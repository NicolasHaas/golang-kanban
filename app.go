package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// maxBody caps every request body. The largest legitimate one is a card with
// a long description; anything bigger is refused before it is read.
const maxBody = 1 << 20

// csp locks scripts, styles and requests to this origin. There is no inline
// script or style on any page, so nothing needs 'unsafe-inline'.
const csp = "default-src 'self'; img-src 'self' data:; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

type app struct {
	store         *store
	tmpl          *template.Template
	static        fs.FS
	staticVersion string

	// authHeader is the request header the sign-in proxy puts the user's
	// email address in. Empty means there is no sign-in in front, and no
	// assignees.
	authHeader string
}

func newApp(s *store, authHeader string) (*app, error) {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	version, err := digest(static)
	if err != nil {
		return nil, err
	}
	a := &app{store: s, static: static, staticVersion: version, authHeader: authHeader}
	funcs := template.FuncMap{
		// asset gives a static file's URL with the build's digest in it, so a
		// browser caches it for a year and still picks up a new release.
		"asset":     func(path string) string { return "/static/" + path + "?v=" + version },
		"assigning": func() bool { return a.authHeader != "" },
		"initials":  initials,
		"localpart": func(email string) string { name, _, _ := strings.Cut(email, "@"); return name },
		"linkify":   linkify,
		// archivedIn is how many of a column's cards are in the archive.
		"archivedIn": func(l Lane) int { return l.Total - l.Count },
		"due": func(date string) *dueBadge {
			b, ok := due(date, time.Now())
			if !ok {
				return nil
			}
			return &b
		},
		"date": func(unix int64) string { return time.Unix(unix, 0).Format("2 Jan 2006, 15:04") },
	}
	a.tmpl, err = template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.boardsHandler)
	mux.HandleFunc("POST /boards", a.createBoardHandler)
	mux.HandleFunc("GET /b/{board}", a.boardHandler)
	mux.HandleFunc("POST /b/{board}/rename", a.renameBoardHandler)
	mux.HandleFunc("POST /b/{board}/delete", a.deleteBoardHandler)
	mux.HandleFunc("GET /b/{board}/columns", a.lanesHandler)
	mux.HandleFunc("POST /b/{board}/columns", a.addLaneHandler)
	mux.HandleFunc("POST /b/{board}/columns/{lane}/rename", a.renameLaneHandler)
	mux.HandleFunc("POST /b/{board}/columns/{lane}/move", a.moveLaneHandler)
	mux.HandleFunc("POST /b/{board}/columns/{lane}/delete", a.deleteLaneHandler)
	mux.HandleFunc("POST /b/{board}/card", a.createCardHandler)
	mux.HandleFunc("POST /b/{board}/order", a.updateOrderHandler)
	mux.HandleFunc("GET /b/{board}/archive", a.archiveHandler)
	mux.HandleFunc("GET /search", a.searchHandler)
	mux.HandleFunc("GET /card/{id}/edit", a.editCardHandler)
	mux.HandleFunc("POST /card/{id}/update", a.updateCardHandler)
	mux.HandleFunc("POST /card/{id}/subtask", a.subtaskHandler)
	mux.HandleFunc("POST /card/{id}/move", a.moveCardHandler)
	mux.HandleFunc("POST /card/{id}/archive", a.archiveCardHandler)
	mux.HandleFunc("POST /card/{id}/restore", a.restoreCardHandler)
	mux.HandleFunc("POST /card/{id}/delete", a.deleteCardHandler)
	mux.Handle("GET /static/", a.staticHandler())
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", a.readyHandler)

	// CrossOriginProtection refuses POSTs that a page on another origin makes
	// the browser send, using Sec-Fetch-Site and Origin; no CSRF token needed.
	return secureHeaders(http.NewCrossOriginProtection().Handler(limitBody(a.identify(mux))))
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

type userKey struct{}

// currentUser is the signed-in address, or empty without a sign-in in front.
func currentUser(r *http.Request) string {
	u, _ := r.Context().Value(userKey{}).(string)
	return u
}

// identify reads who is signed in from the header the proxy sets. With a
// header configured, a request without it did not come through the proxy
// and is refused; only the health checks and static files are open, for the
// orchestrator and the proxy's own pages.
func (a *app) identify(next http.Handler) http.Handler {
	if a.authHeader == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" || p == "/readyz" || p == "/favicon.ico" || strings.HasPrefix(p, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		email, err := cleanEmail(r.Header.Get(a.authHeader))
		if err != nil {
			slog.Warn("refused a request without a signed-in user", "header", a.authHeader, "path", p)
			http.Error(w, "Sign-in required.", http.StatusForbidden)
			return
		}
		if err := a.store.rememberUser(r.Context(), email); err != nil {
			serverError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, email)))
	})
}

// initials are what the assignee bubble shows: "ada.lovelace@x" is "AL".
func initials(email string) string {
	name, _, _ := strings.Cut(email, "@")
	var out []rune
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == '+' }) {
		out = append(out, []rune(strings.ToUpper(part))[0])
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

func (a *app) staticHandler() http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(a.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") { // no directory listings
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("v") == a.staticVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func (a *app) readyHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.db.PingContext(ctx); err != nil {
		slog.Warn("readiness check failed", "err", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, "ok\n")
}

// --- Boards

type boardsView struct {
	User   string
	Boards []Board
}

func (a *app) boardsHandler(w http.ResponseWriter, r *http.Request) {
	boards, err := a.store.listBoards(r.Context())
	if err != nil {
		serverError(w, r, err)
		return
	}
	a.render(w, r, "boards.html", boardsView{User: currentUser(r), Boards: boards})
}

func (a *app) createBoardHandler(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	b, err := a.store.createBoard(r.Context(), r.PostForm.Get("name"))
	if err != nil {
		storeError(w, r, err)
		return
	}
	http.Redirect(w, r, "/b/"+b.Slug, http.StatusSeeOther)
}

// board loads the board named in the URL, answering 404 itself when there is
// none.
func (a *app) board(w http.ResponseWriter, r *http.Request) (Board, bool) {
	b, err := a.store.boardBySlug(r.Context(), r.PathValue("board"))
	if err != nil {
		storeError(w, r, err)
		return b, false
	}
	return b, true
}

// boardView is what board.html renders. New and Blank are zero values that
// the add-card form and the subtask row template are drawn from.
type boardView struct {
	User  string
	Board Board
	Lanes []Lane
	New   editView
	Blank Subtask
}

func (a *app) boardHandler(w http.ResponseWriter, r *http.Request) {
	b, ok := a.board(w, r)
	if !ok {
		return
	}
	lanes, err := a.store.boardLanes(r.Context(), b.ID, false)
	if err != nil {
		serverError(w, r, err)
		return
	}
	a.render(w, r, "board.html", boardView{User: currentUser(r), Board: b, Lanes: lanes})
}

func (a *app) renameBoardHandler(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	b, ok := a.board(w, r)
	if !ok {
		return
	}
	if err := a.store.renameBoard(r.Context(), b.ID, r.PostForm.Get("name")); err != nil {
		storeError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// deleteBoardHandler wants the board's name typed out, because it takes every
// card on the board with it and nothing brings them back.
func (a *app) deleteBoardHandler(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	b, ok := a.board(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(r.PostForm.Get("confirm")) != b.Name {
		http.Error(w, "The name typed did not match the board's name; nothing was deleted.", http.StatusBadRequest)
		return
	}
	if err := a.store.deleteBoard(r.Context(), b.ID); err != nil {
		serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- Columns

type lanesView struct {
	User  string
	Board Board
	Lanes []Lane
	Max   int
}

func (a *app) lanesHandler(w http.ResponseWriter, r *http.Request) {
	b, ok := a.board(w, r)
	if !ok {
		return
	}
	lanes, err := a.store.listLanes(r.Context(), b.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	a.render(w, r, "columns.html", lanesView{User: currentUser(r), Board: b, Lanes: lanes, Max: maxLanes})
}

// laneAction parses the form and the board for a column change, runs fn and
// sends the browser back to the column settings.
func (a *app) laneAction(w http.ResponseWriter, r *http.Request, fn func(b Board, laneID int) error) {
	if !parseForm(w, r) {
		return
	}
	b, ok := a.board(w, r)
	if !ok {
		return
	}
	laneID := 0
	if v := r.PathValue("lane"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		laneID = id
	}
	if err := fn(b, laneID); err != nil {
		storeError(w, r, err)
		return
	}
	http.Redirect(w, r, "/b/"+b.Slug+"/columns", http.StatusSeeOther)
}

func (a *app) addLaneHandler(w http.ResponseWriter, r *http.Request) {
	a.laneAction(w, r, func(b Board, _ int) error {
		return a.store.addLane(r.Context(), b.ID, r.PostForm.Get("name"))
	})
}

func (a *app) renameLaneHandler(w http.ResponseWriter, r *http.Request) {
	a.laneAction(w, r, func(b Board, id int) error {
		return a.store.renameLane(r.Context(), b.ID, id, r.PostForm.Get("name"))
	})
}

func (a *app) moveLaneHandler(w http.ResponseWriter, r *http.Request) {
	a.laneAction(w, r, func(b Board, id int) error {
		delta := 1
		if r.PostForm.Get("dir") == "left" {
			delta = -1
		}
		return a.store.moveLane(r.Context(), b.ID, id, delta)
	})
}

func (a *app) deleteLaneHandler(w http.ResponseWriter, r *http.Request) {
	a.laneAction(w, r, func(b Board, id int) error {
		return a.store.deleteLane(r.Context(), b.ID, id)
	})
}

// --- Cards

func (a *app) createCardHandler(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	c := Card{
		Title:       strings.TrimSpace(r.PostForm.Get("title")),
		Description: r.PostForm.Get("description"),
		Subtasks:    r.PostForm.Get("subtasks"),
	}
	if isEmpty(c) {
		http.Error(w, "Empty card not allowed", http.StatusBadRequest)
		return
	}
	var err error
	if c.Due, err = cleanDue(r.PostForm.Get("due")); err != nil {
		storeError(w, r, err)
		return
	}
	if c.LaneID, err = strconv.Atoi(r.PostForm.Get("lane")); err != nil {
		http.Error(w, "Invalid column", http.StatusBadRequest)
		return
	}
	b, ok := a.board(w, r)
	if !ok {
		return
	}
	c, err = a.store.createCard(r.Context(), b.ID, c)
	if err != nil {
		storeError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/b/"+b.Slug, http.StatusSeeOther)
		return
	}
	// The add form can be opened from any column; the card goes to the end of
	// the one it was opened from.
	w.Header().Set("HX-Retarget", "#lane-"+strconv.Itoa(c.LaneID))
	w.Header().Set("HX-Reswap", "beforeend")
	a.render(w, r, "card_fragment.html", c)
}

// editView is a card with the columns it can move to and the people it can
// be assigned to.
type editView struct {
	Card
	Lanes []Lane
	Users []string
}

func (a *app) editCardHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(w, r)
	if !ok {
		return
	}
	c, err := a.store.getCard(r.Context(), id)
	if err != nil {
		storeError(w, r, err)
		return
	}
	view := editView{Card: c}
	if view.Lanes, err = a.store.boardLanesOf(r.Context(), c.LaneID); err != nil {
		serverError(w, r, err)
		return
	}
	if a.authHeader != "" {
		if view.Users, err = a.store.listUsers(r.Context()); err != nil {
			serverError(w, r, err)
			return
		}
	}
	a.render(w, r, "card_edit_fragment.html", view)
}

func (a *app) updateCardHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(w, r)
	if !ok || !parseForm(w, r) {
		return
	}
	e := cardEdit{
		Title:       strings.TrimSpace(r.PostForm.Get("title")),
		Description: r.PostForm.Get("description"),
		Subtasks:    r.PostForm.Get("subtasks"),
	}
	if isEmpty(Card{Title: e.Title, Description: e.Description, Subtasks: e.Subtasks}) {
		http.Error(w, "Empty card not allowed", http.StatusBadRequest)
		return
	}
	var err error
	if e.Due, err = cleanDue(r.PostForm.Get("due")); err != nil {
		storeError(w, r, err)
		return
	}
	if v := r.PostForm.Get("lane"); v != "" {
		laneID, err := strconv.Atoi(v)
		if err != nil {
			http.Error(w, "Invalid column", http.StatusBadRequest)
			return
		}
		e.LaneID = laneID
	}
	// Without a sign-in in front there are no assignees, and whatever is
	// stored is left as it is.
	if a.authHeader != "" {
		v := ""
		if raw := r.PostForm.Get("assignee"); raw != "" {
			email, err := cleanEmail(raw)
			if err != nil {
				storeError(w, r, err)
				return
			}
			v = email
		}
		e.Assignee = &v
	}
	c, err := a.store.updateCard(r.Context(), id, e)
	if err != nil {
		storeError(w, r, err)
		return
	}
	// The card is swapped in where it was; the page then moves it to the end
	// of its column if that is another one now (app.js, "cardMoved").
	if e.LaneID != 0 {
		trigger, _ := json.Marshal(map[string]any{"cardMoved": map[string]int{"id": c.ID, "lane": c.LaneID}})
		w.Header().Set("HX-Trigger-After-Swap", string(trigger))
	}
	a.render(w, r, "card_fragment.html", c)
}

// archiveCardHandler takes a card off the board. The page offers to undo it,
// which is restoreCardHandler.
func (a *app) archiveCardHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(w, r)
	if !ok {
		return
	}
	if err := a.store.archiveCard(r.Context(), id, time.Now()); err != nil {
		storeError(w, r, err)
		return
	}
	_, _ = io.WriteString(w, "OK")
}

// restoreCardHandler puts an archived card back where it was. From the
// archive page it is a plain form and goes back there; from the undo on the
// board it answers the card, for the page to put back in place.
func (a *app) restoreCardHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(w, r)
	if !ok || !parseForm(w, r) {
		return
	}
	c, err := a.store.restoreCard(r.Context(), id)
	if err != nil {
		storeError(w, r, err)
		return
	}
	if redirectBack(w, r) {
		return
	}
	a.render(w, r, "card_fragment.html", c)
}

// redirectBack sends a plain form back to the page in its "next" field. Only
// a path on this site is followed, so a form elsewhere cannot use it to send
// somebody off the site.
func redirectBack(w http.ResponseWriter, r *http.Request) bool {
	next := r.PostForm.Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return false
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
	return true
}

type archiveView struct {
	User  string
	Board Board
	Cards []archivedCard
}

func (a *app) archiveHandler(w http.ResponseWriter, r *http.Request) {
	b, ok := a.board(w, r)
	if !ok {
		return
	}
	cards, err := a.store.archivedCards(r.Context(), b.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	a.render(w, r, "archive.html", archiveView{User: currentUser(r), Board: b, Cards: cards})
}

// moveCardHandler puts a card at the end of another column of its board: the
// left and right buttons on a phone, where the columns are tabs and a card
// cannot be dragged from one to the other.
func (a *app) moveCardHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(w, r)
	if !ok || !parseForm(w, r) {
		return
	}
	laneID, err := strconv.Atoi(r.PostForm.Get("lane"))
	if err != nil || laneID <= 0 {
		http.Error(w, "Invalid column", http.StatusBadRequest)
		return
	}
	if _, err := a.store.moveCardTo(r.Context(), id, laneID); err != nil {
		storeError(w, r, err)
		return
	}
	_, _ = io.WriteString(w, "OK")
}

// subtaskHandler ticks or unticks one subtask from the card face.
func (a *app) subtaskHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(w, r)
	if !ok || !parseForm(w, r) {
		return
	}
	index, err := strconv.Atoi(r.PostForm.Get("index"))
	done := r.PostForm.Get("done")
	if err != nil || (done != "0" && done != "1") {
		http.Error(w, "Invalid subtask", http.StatusBadRequest)
		return
	}
	c, err := a.store.setSubtask(r.Context(), id, index, r.PostForm.Get("text"), done == "1")
	if err != nil {
		storeError(w, r, err)
		return
	}
	a.render(w, r, "card_fragment.html", c)
}

func (a *app) deleteCardHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(w, r)
	if !ok || !parseForm(w, r) {
		return
	}
	// A card that is already gone answers OK as well: the page removes it,
	// which is the right outcome when it was deleted in another tab.
	if err := a.store.deleteCard(r.Context(), id); err != nil {
		serverError(w, r, err)
		return
	}
	if redirectBack(w, r) {
		return
	}
	_, _ = io.WriteString(w, "OK")
}

type OrderUpdatePayload struct {
	Lane  int   `json:"lane"`
	Order []int `json:"order"`
}

// updateOrderHandler saves a drag: the column a card was dropped in, with
// its cards in their new order. A column of another board is a 404.
func (a *app) updateOrderHandler(w http.ResponseWriter, r *http.Request) {
	var payload OrderUpdatePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Lane <= 0 {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	b, ok := a.board(w, r)
	if !ok {
		return
	}
	if err := a.store.reorderLane(r.Context(), b.ID, payload.Lane, payload.Order); err != nil {
		storeError(w, r, err)
		return
	}
	_, _ = io.WriteString(w, "OK")
}

// render executes the template into a buffer first, so a template error is a
// clean 500 instead of half a page followed by an error message.
func (a *app) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func cardID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

// parseForm reads the body once, so an oversized request is a 413 rather than
// a form that silently comes back empty.
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "Invalid form", http.StatusBadRequest)
		}
		return false
	}
	return true
}

// storeError answers a store error: 404 for something that does not exist,
// 400 with the reason for input the store refused, 500 for anything else.
func storeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errNotFound):
		http.NotFound(w, r)
	case errors.Is(err, errStale):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errInvalid):
		http.Error(w, strings.TrimPrefix(err.Error(), errInvalid.Error()+": "), http.StatusBadRequest)
	default:
		serverError(w, r, err)
	}
}

// serverError logs the cause and tells the client nothing about it: database
// errors carry table names and query fragments that are no business of a
// browser's.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}

// digest is a short hash over every embedded static file, used as the cache
// buster in asset URLs.
func digest(fsys fs.FS) (string, error) {
	sum := sha256.New()
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(sum, path+"\x00")
		_, _ = sum.Write(b)
		return nil
	})
	return hex.EncodeToString(sum.Sum(nil))[:12], err
}
