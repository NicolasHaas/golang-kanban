package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// newTestApp builds the app without a database. Every test using it exercises
// a path that must be decided before the database is touched: reaching it
// would dereference the nil *sql.DB and panic rather than pass.
func newTestApp(t *testing.T) *app {
	t.Helper()
	a, err := newApp(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func do(a *app, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, req)
	return rr
}

func TestGetEnv(t *testing.T) {
	if got := getEnv("NONEXISTENT_KEY_123", "default_value"); got != "default_value" {
		t.Errorf("unset: got %q", got)
	}
	t.Setenv("TEST_KEY", "test_value")
	if got := getEnv("TEST_KEY", "default_value"); got != "test_value" {
		t.Errorf("set: got %q", got)
	}
	t.Setenv("EMPTY_KEY", "")
	if got := getEnv("EMPTY_KEY", "default_value"); got != "default_value" {
		t.Errorf("empty: got %q", got)
	}
}

// A password with URL syntax in it has to reach the driver unchanged, not
// end the userinfo early and turn the rest into a host.
func TestDSNEscapesCredentials(t *testing.T) {
	t.Setenv("DB_USER", "kan ban")
	t.Setenv("DB_PASS", "p@ss/w:rd?#")
	t.Setenv("DB_HOST", "db.example")
	t.Setenv("DB_PORT", "6543")
	t.Setenv("DB_NAME", "boards")
	t.Setenv("DB_SSLMODE", "require")

	u, err := url.Parse(dsnFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := u.User.Password()
	if u.User.Username() != "kan ban" || pass != "p@ss/w:rd?#" {
		t.Errorf("credentials: got %q / %q", u.User.Username(), pass)
	}
	if u.Host != "db.example:6543" || u.Path != "/boards" || u.Query().Get("sslmode") != "require" {
		t.Errorf("got host %q path %q query %q", u.Host, u.Path, u.RawQuery)
	}
}

func TestSubtaskList(t *testing.T) {
	c := Card{Subtasks: "1|done one\r\n0|open one\n\n  \nno flag\n0|a | b\n1|\n"}
	want := []Subtask{
		{Done: true, Text: "done one"},
		{Done: false, Text: "open one"},
		{Done: false, Text: "no flag"},
		{Done: false, Text: "a | b"},
	}
	got := c.SubtaskList()
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRouting(t *testing.T) {
	a := newTestApp(t)
	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/wrong-path", http.StatusNotFound},
		{http.MethodGet, "/boards", http.StatusMethodNotAllowed},
		{http.MethodGet, "/b/main/card", http.StatusMethodNotAllowed},
		{http.MethodGet, "/b/main/order", http.StatusMethodNotAllowed},
		{http.MethodGet, "/b/main/delete", http.StatusMethodNotAllowed},
		{http.MethodGet, "/b/main/x", http.StatusNotFound},
		{http.MethodGet, "/card/1", http.StatusNotFound},
		{http.MethodGet, "/card/abc/edit", http.StatusNotFound},
		{http.MethodGet, "/card/0/edit", http.StatusNotFound},
		{http.MethodGet, "/card/1/invalid", http.StatusNotFound},
		{http.MethodPost, "/card/1/edit", http.StatusMethodNotAllowed},
		{http.MethodGet, "/card/1/view", http.StatusNotFound},
		{http.MethodGet, "/b/main/columns/1/move", http.StatusMethodNotAllowed},
		{http.MethodGet, "/card/1/update", http.StatusMethodNotAllowed},
		{http.MethodGet, "/card/1/delete", http.StatusMethodNotAllowed},
		{http.MethodGet, "/favicon.ico", http.StatusNoContent},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/static/", http.StatusNotFound},
		{http.MethodGet, "/static/vendor/", http.StatusNotFound},
		{http.MethodGet, "/static/missing.js", http.StatusNotFound},
	}
	for _, tt := range tests {
		rr := do(a, httptest.NewRequest(tt.method, tt.path, nil))
		if rr.Code != tt.want {
			t.Errorf("%s %s: got %d, want %d", tt.method, tt.path, rr.Code, tt.want)
		}
	}
}

func postForm(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestCreateRejectsEmptyCard(t *testing.T) {
	a := newTestApp(t)
	rr := do(a, postForm("/b/main/card", url.Values{"title": {"  "}, "description": {"\n"}, "lane": {"1"}}))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("got %d", rr.Code)
	}
}

func TestUpdateRejectsEmptyCard(t *testing.T) {
	a := newTestApp(t)
	rr := do(a, postForm("/card/1/update", url.Values{"title": {""}}))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("got %d", rr.Code)
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	a := newTestApp(t)
	big := strings.Repeat("x", maxBody+1)
	rr := do(a, postForm("/b/main/card", url.Values{"title": {big}}))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("got %d", rr.Code)
	}
}

// A form on another site must not be able to make the reader's browser write
// to the board.
func TestCrossSiteWriteIsRefused(t *testing.T) {
	a := newTestApp(t)
	req := postForm("/card/1/delete", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rr := do(a, req); rr.Code != http.StatusForbidden {
		t.Errorf("Sec-Fetch-Site cross-site: got %d", rr.Code)
	}

	req = postForm("/card/1/delete", nil)
	req.Header.Set("Origin", "https://evil.example")
	if rr := do(a, req); rr.Code != http.StatusForbidden {
		t.Errorf("foreign Origin: got %d", rr.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	a := newTestApp(t)
	rr := do(a, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	for header, want := range map[string]string{
		"Content-Security-Policy": csp,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	} {
		if got := rr.Header().Get(header); got != want {
			t.Errorf("%s: got %q", header, got)
		}
	}
}

func TestSlugify(t *testing.T) {
	for name, want := range map[string]string{
		"Kanban Board":          "kanban-board",
		"  Übersicht Gäste  ":   "uebersicht-gaeste",
		"Straße / Team #2":      "strasse-team-2",
		"🚀":                     "board",
		"---":                   "board",
		strings.Repeat("a", 80): strings.Repeat("a", 50),
	} {
		if got := slugify(name); got != want {
			t.Errorf("slugify(%q) = %q, want %q", name, got, want)
		}
		if !validSlug(slugify(name)) {
			t.Errorf("slugify(%q) is not a valid slug", name)
		}
	}
	for _, s := range []string{"", "Main", "a b", "-a", "a--b", "a/b"} {
		if validSlug(s) {
			t.Errorf("validSlug(%q) = true", s)
		}
	}
}

func TestSubtasksRoundTrip(t *testing.T) {
	in := []Subtask{{Done: true, Text: " one "}, {Text: "two\nlines"}, {Text: "   "}, {Text: "a|b"}}
	got := Card{Subtasks: formatSubtasks(in)}.SubtaskList()
	want := []Subtask{{Done: true, Text: "one"}, {Text: "two lines"}, {Text: "a|b"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A drag carries the column it ended in. Anything that is not a column id is
// refused before the database is asked.
func TestUpdateOrderRejectsBadPayload(t *testing.T) {
	a := newTestApp(t)
	for _, body := range []string{"{", `{"lane":0,"order":[1]}`, `{"lane":"todo","order":[1]}`, `{"order":[1]}`} {
		rr := do(a, httptest.NewRequest(http.MethodPost, "/b/main/order", strings.NewReader(body)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d", body, rr.Code)
		}
	}
}

func TestCreateCardNeedsAColumn(t *testing.T) {
	a := newTestApp(t)
	rr := do(a, postForm("/b/main/card", url.Values{"title": {"x"}, "lane": {"todo"}}))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("got %d", rr.Code)
	}
}

// With AUTH_HEADER set, a request that did not come through the sign-in
// proxy is refused before it reaches a handler, except for what an
// orchestrator and a browser fetching assets need.
func TestSignInRequired(t *testing.T) {
	a, err := newApp(nil, "X-Forwarded-Email")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/b/main", "/card/1/edit"} {
		if rr := do(a, httptest.NewRequest(http.MethodGet, path, nil)); rr.Code != http.StatusForbidden {
			t.Errorf("%s without a user: %d", path, rr.Code)
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Forwarded-Email", "not an address")
		if rr := do(a, req); rr.Code != http.StatusForbidden {
			t.Errorf("%s with a malformed user: %d", path, rr.Code)
		}
	}
	for _, path := range []string{"/healthz", "/favicon.ico"} {
		if rr := do(a, httptest.NewRequest(http.MethodGet, path, nil)); rr.Code >= 400 {
			t.Errorf("%s: %d", path, rr.Code)
		}
	}
}

func TestInitials(t *testing.T) {
	for email, want := range map[string]string{
		"ada.lovelace@example.com": "AL",
		"grace@example.com":        "G",
		"a-b-c@example.com":        "AB",
		"élodie@example.com":       "É",
		"@example.com":             "?",
	} {
		if got := initials(email); got != want {
			t.Errorf("initials(%q) = %q, want %q", email, got, want)
		}
	}
}

// The templates are embedded, so a mistake in one is a broken binary. Render
// every one with awkward data, with and without assignees, then fetch every
// asset the pages link to.
func TestTemplatesAndAssets(t *testing.T) {
	for _, header := range []string{"", "X-Forwarded-Email"} {
		a, err := newApp(nil, header)
		if err != nil {
			t.Fatal(err)
		}
		card := Card{ID: 7, LaneID: 3, Title: "<script>x</script>", Description: "line one\nline two", Subtasks: "1|done\nno flag",
			Assignee: "ada.lovelace@example.com", Due: "2001-01-01"}
		render := func(name string, data any) string {
			t.Helper()
			rr := httptest.NewRecorder()
			a.render(rr, httptest.NewRequest(http.MethodGet, "/", nil), name, data)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
			}
			return rr.Body.String()
		}
		want := func(page string, parts ...string) {
			t.Helper()
			for _, p := range parts {
				if !strings.Contains(page, p) {
					t.Errorf("header %q: missing %q", header, p)
				}
			}
		}

		board := render("board.html", boardView{
			User:  "ada.lovelace@example.com",
			Board: Board{ID: 1, Slug: "main", Name: "Main <b>"},
			Lanes: []Lane{{ID: 3, Name: "Doing", Cards: []Card{card}}, {ID: 4, Name: "Done", Cards: []Card{}}},
		})
		want(board, `id="card-7"`, `id="lane-3"`, `data-lane="4"`, "Doing", "no flag", `id="subtask-row-template"`,
			`hx-post="/b/main/card"`, `data-order-url="/b/main/order"`, "Main &lt;b&gt;", `href="/b/main/columns"`,
			`class="lane-tab active" role="tab" data-tab="3"`, `class="lane active" data-tab="3"`, `class="lane" data-tab="4"`,
			`data-count-for="3">1<`, `data-count-for="4">0<`)
		if strings.Contains(board, "<script>x</script>") {
			t.Error("card title was not escaped")
		}
		if strings.Contains(board, "<script>") || strings.Contains(board, " onclick=") || strings.Contains(board, " style=") {
			t.Error("board has inline script or style, which the CSP blocks")
		}
		if got := strings.Contains(board, `title="Assigned to ada.lovelace@example.com"`); got != (header != "") {
			t.Errorf("header %q: assignee shown = %v", header, got)
		}

		archive := render("archive.html", archiveView{Board: Board{Slug: "main", Name: "Main"},
			Cards: []archivedCard{{Card: Card{ID: 9, Title: "old", ArchivedAt: 1791000000}, Lane: "Done"}}})
		want(archive, `action="/card/9/restore"`, `action="/card/9/delete" data-confirm=`, `name="next" value="/b/main/archive"`, "from Done")
		search := render("search.html", searchView{Query: "a b", Board: Board{Slug: "main", Name: "Main"},
			Hits: []searchHit{{Card: Card{ID: 4, Title: "found"}, BoardSlug: "main", BoardName: "Main", Lane: "Doing"}}})
		want(search, `href="/b/main#card-4"`, `name="q" value="a b"`, `href="/search?q=a%20b"`, "Main › Doing")

		boards := render("boards.html", boardsView{Boards: []Board{{ID: 1, Slug: "main", Name: "Main", Cards: 1}}})
		want(boards, `action="/b/main/delete"`, "1 card<")

		columns := render("columns.html", lanesView{Board: Board{Slug: "main", Name: "Main"}, Max: maxLanes,
			Lanes: []Lane{{ID: 3, Name: "Doing", Count: 2}, {ID: 4, Name: "Done"}}})
		want(columns, `action="/b/main/columns/3/rename"`, `action="/b/main/columns/4/delete"`, `action="/b/main/columns"`)

		edit := render("card_edit_fragment.html", editView{Card: card,
			Lanes: []Lane{{ID: 3, Name: "Doing"}, {ID: 4, Name: "Done"}},
			Users: []string{"ada.lovelace@example.com", "grace@example.com"}})
		want(edit, "/card/7/update", `name="due" value="2001-01-01"`, `hx-disabled-elt="find button[type=submit]"`, `<select name="lane">`, `<option value="3" selected>Doing</option>`, `<option value="4">Done</option>`)
		single := render("card_edit_fragment.html", editView{Card: card, Lanes: []Lane{{ID: 3, Name: "Doing"}}})
		if strings.Contains(single, `name="lane"`) {
			t.Error("a column picker with nothing to pick from")
		}
		if got := strings.Contains(edit, `<option value="ada.lovelace@example.com" selected>`); got != (header != "") {
			t.Errorf("header %q: assignee picker shown = %v", header, got)
		}
		face := render("card_fragment.html", card)
		want(face, "/card/7/edit", `data-move="-1"`, `data-move="1"`, `class="icon-btn archive-btn"`, "due-overdue", `title="Due 2001-01-01"`, `hx-post="/card/7/subtask"`, `name="index" value="1"`, `name="text" value="no flag"`,
			`<button type="submit" class="subtask done"`)
		// The done line asks to be unticked, the open one to be ticked.
		if strings.Count(face, `name="done" value="0"`) != 1 || strings.Count(face, `name="done" value="1"`) != 1 {
			t.Errorf("each subtask asks for the other state:\n%s", face)
		}

		assets := regexp.MustCompile(`(?:src|href)="(/static/[^"]+)"`).FindAllStringSubmatch(board, -1)
		if len(assets) < 5 {
			t.Fatalf("expected the page to link its assets, found %d", len(assets))
		}
		for _, m := range assets {
			path := strings.ReplaceAll(m[1], "&amp;", "&")
			rr := do(a, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Errorf("%s: %d", path, rr.Code)
			}
			if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
				t.Errorf("%s: Cache-Control %q", path, cc)
			}
		}
	}
}

func TestReadSnapshotRefusesBadFiles(t *testing.T) {
	board := func(columns string) string {
		return `{"version":1,"boards":[{"slug":"a","name":"x","columns":` + columns + `}]}`
	}
	for name, doc := range map[string]string{
		"not json":       `{`,
		"wrong version":  `{"version":2,"boards":[]}`,
		"unknown field":  `{"version":1,"boards":[],"colour":"red"}`,
		"bad slug":       `{"version":1,"boards":[{"slug":"Main","name":"x","columns":[{"name":"c","cards":[]}]}]}`,
		"duplicate slug": `{"version":1,"boards":[{"slug":"a","name":"x","columns":[{"name":"c","cards":[]}]},{"slug":"a","name":"y","columns":[{"name":"c","cards":[]}]}]}`,
		"empty name":     `{"version":1,"boards":[{"slug":"a","name":" ","columns":[{"name":"c","cards":[]}]}]}`,
		"no columns":     board(`[]`),
		"empty column":   board(`[{"name":" ","cards":[]}]`),
		"bad assignee":   board(`[{"name":"c","cards":[{"title":"t","assignee":"a b"}]}]`),
	} {
		if _, err := readSnapshot(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	snap, err := readSnapshot(strings.NewReader(board(`[{"name":"c","cards":[{"title":"t","assignee":" Ada@Example.com ","subtasks":[{"done":true,"text":"s"}]}]}]`)))
	if err != nil || snap.Boards[0].Columns[0].Cards[0].Assignee != "ada@example.com" {
		t.Errorf("valid file: %+v %v", snap, err)
	}
}

func TestLinkify(t *testing.T) {
	for in, want := range map[string]string{
		"plain <b>text</b> & more":                     "plain &lt;b&gt;text&lt;/b&gt; &amp; more",
		"see https://example.com.":                     `see <a href="https://example.com" target="_blank" rel="noopener noreferrer nofollow">https://example.com</a>.`,
		"(http://x.org/a?b=1&c=2)":                     `(<a href="http://x.org/a?b=1&amp;c=2" target="_blank" rel="noopener noreferrer nofollow">http://x.org/a?b=1&amp;c=2</a>)`,
		"https://en.wikipedia.org/wiki/Go_(game), yes": `<a href="https://en.wikipedia.org/wiki/Go_(game)" target="_blank" rel="noopener noreferrer nofollow">https://en.wikipedia.org/wiki/Go_(game)</a>, yes`,
		"two http://a.io\nhttps://b.io": `two <a href="http://a.io" target="_blank" rel="noopener noreferrer nofollow">http://a.io</a>` + "\n" +
			`<a href="https://b.io" target="_blank" rel="noopener noreferrer nofollow">https://b.io</a>`,
		// Quotes and angle brackets end an address, so nothing can break out
		// of the attribute; everything after is escaped text.
		`https://x.io/"onmouseover="alert(1)`: `<a href="https://x.io/" target="_blank" rel="noopener noreferrer nofollow">https://x.io/</a>&#34;onmouseover=&#34;alert(1)`,
		"https://x.io/<script>":               `<a href="https://x.io/" target="_blank" rel="noopener noreferrer nofollow">https://x.io/</a>&lt;script&gt;`,
		// Not addresses this turns into links.
		"javascript:alert(1)": "javascript:alert(1)",
		"ftp://x.io":          "ftp://x.io",
		"https://":            "https://",
		"http://...":          "http://...",
	} {
		if got := string(linkify(in)); got != want {
			t.Errorf("linkify(%q)\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestDue(t *testing.T) {
	// Midnight just past, the day after the clocks went forward: whole days
	// are still whole days.
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Skip("no time zone database:", err)
	}
	now := time.Date(2026, 3, 30, 0, 30, 0, 0, zurich)
	for date, want := range map[string]dueBadge{
		"2026-03-28": {Label: "Sat 28 Mar", State: "overdue"},
		"2026-03-29": {Label: "Yesterday", State: "overdue"},
		"2026-03-30": {Label: "Today", State: "today"},
		"2026-03-31": {Label: "Tomorrow", State: "soon"},
		"2026-04-01": {Label: "Wed 1 Apr", State: "soon"},
		"2026-04-02": {Label: "Thu 2 Apr", State: "later"},
		"2027-01-05": {Label: "5 Jan 2027", State: "later"},
	} {
		got, ok := due(date, now)
		want.Date = date
		if !ok || got != want {
			t.Errorf("due(%s) = %+v, want %+v", date, got, want)
		}
	}
	if _, ok := due("", now); ok {
		t.Error("a card without a date has a badge")
	}
	for in, want := range map[string]string{"": "", " 2026-01-31 ": "2026-01-31"} {
		if got, err := cleanDue(in); err != nil || got != want {
			t.Errorf("cleanDue(%q) = %q %v", in, got, err)
		}
	}
	for _, in := range []string{"2026-02-30", "31.01.2026", "tomorrow", "2026-1-5"} {
		if _, err := cleanDue(in); !errors.Is(err, errInvalid) {
			t.Errorf("cleanDue(%q) accepted", in)
		}
	}
}
