package main

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

type Card struct {
	ID          int
	Title       string
	Description string
	Subtasks    string
	Status      string
	Category    string
	CardOrder   int
}

type Subtask struct {
	Completed bool
	Text      string
}

type Category struct {
	ID       int
	Name     string
	Slug     string
	RowOrder int
	Locked   bool
}

type Status struct {
	ID       int
	Name     string
	Slug     string
	ColOrder int
	Locked   bool
}

type StatusColumn struct {
	Status Status
	Cards  []Card
}

type CategoryRow struct {
	Category Category
	Columns  []StatusColumn
	ColCount int
}

type BoardTemplateData struct {
	Rows       []CategoryRow
	Categories []Category
	Statuses   []Status
}

type OrderUpdatePayload struct {
	Category string `json:"category"`
	Status   string `json:"status"`
	Order    []int  `json:"order"`
}

type SlugOrderPayload struct {
	Order []string `json:"order"`
}

// ---------------------------------------------------------------------------
// Globals
// ---------------------------------------------------------------------------

var db *sql.DB
var tmpl *template.Template

// ---------------------------------------------------------------------------
// Startup
// ---------------------------------------------------------------------------

func main() {
	dbUser := getEnv("DB_USER", "user")
	dbPass := getEnv("DB_PASS", "password")
	dbHost := getEnv("DB_HOST", "postgres")
	dbPort := getEnv("DB_PORT", "5432")
	dbName := getEnv("DB_NAME", "kanban")

	connStr := "postgres://" + dbUser + ":" + dbPass + "@" + dbHost + ":" + dbPort + "/" + dbName + "?sslmode=disable"

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal(err)
	}

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(1)

	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("Error closing database: %v", err)
		}
	}()

	if err := runMigrations(); err != nil {
		log.Fatalf("Migration failed: %v", err)
	}

	funcMap := template.FuncMap{
		"split": func(s, sep string) []string {
			s = strings.TrimSpace(s)
			if s == "" {
				return nil
			}
			return strings.Split(s, sep)
		},
		"trim":                strings.TrimSpace,
		"parseSubtasks":       parseSubtasks,
		"hasSubtasks":         hasSubtasks,
		"allSubtasksComplete": allSubtasksComplete,
		"colGridClass": func(n int) string {
			switch {
			case n <= 2:
				return "grid-cols-1 md:grid-cols-2"
			case n == 3:
				return "grid-cols-1 md:grid-cols-3"
			case n == 4:
				return "grid-cols-1 md:grid-cols-2 xl:grid-cols-4"
			default:
				return "grid-cols-1 md:grid-cols-2 xl:grid-cols-5"
			}
		},
	}

	tmpl = template.Must(template.New("").Funcs(funcMap).ParseGlob("templates/*.html"))

	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/board", boardHandler)
	http.HandleFunc("/card", createCardHandler)
	http.HandleFunc("/card/", cardRouter)
	http.HandleFunc("/card/order", updateOrderHandler)
	http.HandleFunc("/category", createCategoryHandler)
	http.HandleFunc("/category/", categoryRouter)
	http.HandleFunc("/status", createStatusHandler)
	http.HandleFunc("/status/", statusRouter)

	serverPort := getEnv("SERVER_PORT", "17808")
	log.Println("Server started on :" + serverPort)
	log.Fatal(http.ListenAndServe(":"+serverPort, nil))
}

func runMigrations() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS cards (
			id          SERIAL PRIMARY KEY,
			title       TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			subtasks    TEXT NOT NULL DEFAULT '',
			status      VARCHAR(50) NOT NULL DEFAULT 'todo',
			card_order  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS categories (
			id        SERIAL PRIMARY KEY,
			name      TEXT NOT NULL,
			slug      TEXT NOT NULL UNIQUE,
			row_order INTEGER NOT NULL DEFAULT 0,
			locked    BOOLEAN NOT NULL DEFAULT false
		)`,
		`CREATE TABLE IF NOT EXISTS statuses (
			id        SERIAL PRIMARY KEY,
			name      TEXT NOT NULL,
			slug      TEXT NOT NULL UNIQUE,
			col_order INTEGER NOT NULL DEFAULT 0,
			locked    BOOLEAN NOT NULL DEFAULT false
		)`,
		`ALTER TABLE cards ADD COLUMN IF NOT EXISTS category VARCHAR(50) NOT NULL DEFAULT 'work'`,
		`ALTER TABLE cards ALTER COLUMN status TYPE VARCHAR(50)`,
		`INSERT INTO categories (name, slug, row_order, locked) VALUES
			('Work',     'work',     1, false),
			('Personal', 'personal', 2, false),
			('Other',    'other',    3, false)
		ON CONFLICT (slug) DO NOTHING`,
		`INSERT INTO statuses (name, slug, col_order, locked) VALUES
			('Tomorrow',       'tomorrow',      1, false),
			('To Do',          'todo',          2, true),
			('In Progress',    'inprogress',    3, false),
			('Needs Feedback', 'needsfeedback', 4, false),
			('Done',           'done',          5, true)
		ON CONFLICT (slug) DO NOTHING`,
		// Migrate any legacy stardom cards to other
		`UPDATE cards SET category = 'other' WHERE category = 'stardom'`,
	}

	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func getEnv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

// ---------------------------------------------------------------------------
// DB helpers
// ---------------------------------------------------------------------------

func getCategories() ([]Category, error) {
	rows, err := db.Query(`SELECT id, name, slug, row_order, locked FROM categories ORDER BY row_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cats []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Slug, &c.RowOrder, &c.Locked); err != nil {
			return nil, err
		}
		cats = append(cats, c)
	}
	return cats, nil
}

func getStatuses() ([]Status, error) {
	rows, err := db.Query(`SELECT id, name, slug, col_order, locked FROM statuses ORDER BY col_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []Status
	for rows.Next() {
		var s Status
		if err := rows.Scan(&s.ID, &s.Name, &s.Slug, &s.ColOrder, &s.Locked); err != nil {
			return nil, err
		}
		statuses = append(statuses, s)
	}
	return statuses, nil
}

func isValidCategory(slug string) bool {
	var exists bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM categories WHERE slug=$1)`, slug).Scan(&exists)
	return err == nil && exists
}

func isValidStatus(slug string) bool {
	var exists bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM statuses WHERE slug=$1)`, slug).Scan(&exists)
	return err == nil && exists
}

func getCardByID(id int) (*Card, error) {
	var card Card
	err := db.QueryRow(
		`SELECT id, title, description, subtasks, status, category, card_order FROM cards WHERE id=$1`,
		id,
	).Scan(&card.ID, &card.Title, &card.Description, &card.Subtasks, &card.Status, &card.Category, &card.CardOrder)
	if err != nil {
		return nil, err
	}
	return &card, nil
}

func buildBoardData() (*BoardTemplateData, error) {
	cats, err := getCategories()
	if err != nil {
		return nil, err
	}

	statuses, err := getStatuses()
	if err != nil {
		return nil, err
	}

	// Build index maps for fast lookup
	catIndex := make(map[string]int, len(cats))
	for i, c := range cats {
		catIndex[c.Slug] = i
	}
	statusIndex := make(map[string]int, len(statuses))
	for i, s := range statuses {
		statusIndex[s.Slug] = i
	}

	// Build rows skeleton
	rows := make([]CategoryRow, len(cats))
	for i, cat := range cats {
		cols := make([]StatusColumn, len(statuses))
		for j, st := range statuses {
			cols[j] = StatusColumn{Status: st}
		}
		rows[i] = CategoryRow{Category: cat, Columns: cols, ColCount: len(cols)}
	}

	// Query cards
	cardRows, err := db.Query(`
		SELECT id, title, description, subtasks, status, category, card_order
		FROM cards
		ORDER BY card_order, id
	`)
	if err != nil {
		return nil, err
	}
	defer cardRows.Close()

	for cardRows.Next() {
		var c Card
		if err := cardRows.Scan(&c.ID, &c.Title, &c.Description, &c.Subtasks, &c.Status, &c.Category, &c.CardOrder); err != nil {
			continue
		}
		catI, okCat := catIndex[c.Category]
		stI, okSt := statusIndex[c.Status]
		if !okCat || !okSt {
			continue
		}
		rows[catI].Columns[stI].Cards = append(rows[catI].Columns[stI].Cards, c)
	}

	return &BoardTemplateData{
		Rows:       rows,
		Categories: cats,
		Statuses:   statuses,
	}, nil
}

// ---------------------------------------------------------------------------
// Subtask helpers
// ---------------------------------------------------------------------------

func parseSubtasks(raw string) []Subtask {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	lines := strings.Split(raw, "\n")
	subtasks := make([]Subtask, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "|", 2)
		completed := false
		text := line

		if len(parts) == 2 {
			completed = isCompletedValue(parts[0])
			text = strings.TrimSpace(parts[1])
		}

		if text == "" {
			continue
		}

		subtasks = append(subtasks, Subtask{Completed: completed, Text: text})
	}

	if len(subtasks) == 0 {
		return nil
	}
	return subtasks
}

func isCompletedValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "checked", "done", "complete", "completed", "x":
		return true
	default:
		return false
	}
}

func hasSubtasks(raw string) bool {
	return len(parseSubtasks(raw)) > 0
}

func allSubtasksComplete(raw string) bool {
	subtasks := parseSubtasks(raw)
	if len(subtasks) == 0 {
		return false
	}
	for _, s := range subtasks {
		if !s.Completed {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Slug utilities
// ---------------------------------------------------------------------------

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = nonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

func uniqueSlug(base string) (string, error) {
	slug := base
	for i := 2; ; i++ {
		var exists bool
		err := db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM categories WHERE slug=$1 UNION SELECT 1 FROM statuses WHERE slug=$1)`,
			slug,
		).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return slug, nil
		}
		slug = base + "-" + strconv.Itoa(i)
	}
}

// ---------------------------------------------------------------------------
// Page handlers
// ---------------------------------------------------------------------------

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data, err := buildBoardData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// boardHandler renders only the board section (used by HTMX after settings changes)
func boardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	data, err := buildBoardData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "board_fragment.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// Card handlers
// ---------------------------------------------------------------------------

func createCardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/card" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	title := r.FormValue("title")
	description := r.FormValue("description")
	subtasks := r.FormValue("subtasks")
	status := r.FormValue("status")
	category := r.FormValue("category")

	if !isValidStatus(status) {
		status = "todo"
	}
	if !isValidCategory(category) {
		category = "work"
	}

	if strings.TrimSpace(title) == "" && strings.TrimSpace(description) == "" && strings.TrimSpace(subtasks) == "" {
		http.Error(w, "Empty card not allowed", http.StatusBadRequest)
		return
	}

	var maxOrder int
	err := db.QueryRow(
		`SELECT COALESCE(MAX(card_order), 0) FROM cards WHERE category=$1 AND status=$2`,
		category, status,
	).Scan(&maxOrder)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	maxOrder++

	var newID int
	err = db.QueryRow(
		`INSERT INTO cards (title, description, subtasks, status, category, card_order) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		title, description, subtasks, status, category, maxOrder,
	).Scan(&newID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	card := Card{
		ID: newID, Title: title, Description: description,
		Subtasks: subtasks, Status: status, Category: category, CardOrder: maxOrder,
	}

	if r.Header.Get("HX-Request") != "" {
		if err := tmpl.ExecuteTemplate(w, "card_fragment.html", card); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func cardRouter(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.NotFound(w, r)
		return
	}

	id, err := strconv.Atoi(parts[2])
	if err != nil {
		http.NotFound(w, r)
		return
	}

	action := parts[3]

	switch action {
	case "move":
		moveCardHandler(w, r, id)
	case "edit":
		editCardHandler(w, r, id)
	case "update":
		updateCardHandler(w, r, id)
	case "delete":
		deleteCardHandler(w, r, id)
	case "view":
		viewCardHandler(w, r, id)
	case "subtask":
		if len(parts) < 5 {
			http.NotFound(w, r)
			return
		}
		index, err := strconv.Atoi(parts[4])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		toggleSubtaskHandler(w, r, id, index)
	default:
		http.NotFound(w, r)
	}
}

func moveCardHandler(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	newStatus := r.FormValue("status")
	newCategory := r.FormValue("category")

	if !isValidStatus(newStatus) {
		http.Error(w, "Invalid status", http.StatusBadRequest)
		return
	}
	if !isValidCategory(newCategory) {
		http.Error(w, "Invalid category", http.StatusBadRequest)
		return
	}

	newOrder, err := strconv.Atoi(r.FormValue("order"))
	if err != nil {
		http.Error(w, "Invalid order", http.StatusBadRequest)
		return
	}

	_, err = db.Exec(`UPDATE cards SET category=$1, status=$2, card_order=$3 WHERE id=$4`, newCategory, newStatus, newOrder, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = db.Exec(`
		WITH OrderedCards AS (
			SELECT id, ROW_NUMBER() OVER (ORDER BY card_order, id) AS new_order
			FROM cards
			WHERE category = $1 AND status = $2
		)
		UPDATE cards SET card_order = OrderedCards.new_order
		FROM OrderedCards WHERE cards.id = OrderedCards.id
	`, newCategory, newStatus)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := w.Write([]byte("OK")); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

func editCardHandler(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	card, err := getCardByID(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "card_edit_fragment.html", card); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func updateCardHandler(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	title := r.FormValue("title")
	description := r.FormValue("description")
	subtasks := r.FormValue("subtasks")

	_, err := db.Exec(`UPDATE cards SET title=$1, description=$2, subtasks=$3 WHERE id=$4`, title, description, subtasks, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	updated, err := getCardByID(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "card_fragment.html", updated); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func deleteCardHandler(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	_, err := db.Exec(`DELETE FROM cards WHERE id=$1`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := w.Write([]byte("OK")); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

func viewCardHandler(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	card, err := getCardByID(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "card_fragment.html", card); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func toggleSubtaskHandler(w http.ResponseWriter, r *http.Request, id int, index int) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	card, err := getCardByID(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	subtasks := parseSubtasks(card.Subtasks)
	if index < 0 || index >= len(subtasks) {
		http.Error(w, "Invalid subtask index", http.StatusBadRequest)
		return
	}

	subtasks[index].Completed = !subtasks[index].Completed

	lines := make([]string, len(subtasks))
	for i, s := range subtasks {
		completed := "0"
		if s.Completed {
			completed = "1"
		}
		lines[i] = completed + "|" + s.Text
	}

	newSubtasks := strings.Join(lines, "\n")
	if _, err := db.Exec(`UPDATE cards SET subtasks=$1 WHERE id=$2`, newSubtasks, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	card.Subtasks = newSubtasks
	if err := tmpl.ExecuteTemplate(w, "card_fragment.html", card); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func updateOrderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload OrderUpdatePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if !isValidCategory(payload.Category) {
		http.Error(w, "Invalid category", http.StatusBadRequest)
		return
	}
	if !isValidStatus(payload.Status) {
		http.Error(w, "Invalid status", http.StatusBadRequest)
		return
	}

	for index, cardID := range payload.Order {
		_, err := db.Exec(
			`UPDATE cards SET category=$1, status=$2, card_order=$3 WHERE id=$4`,
			payload.Category, payload.Status, index+1, cardID,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	_, err := db.Exec(`
		WITH OrderedCards AS (
			SELECT id, ROW_NUMBER() OVER (ORDER BY card_order, id) AS new_order
			FROM cards
			WHERE category = $1 AND status = $2
		)
		UPDATE cards SET card_order = OrderedCards.new_order
		FROM OrderedCards WHERE cards.id = OrderedCards.id
	`, payload.Category, payload.Status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := w.Write([]byte("OK")); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Category handlers
// ---------------------------------------------------------------------------

func createCategoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/category" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}

	base := slugify(name)
	if base == "" {
		http.Error(w, "Invalid name", http.StatusBadRequest)
		return
	}

	slug, err := uniqueSlug(base)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var maxOrder int
	if err := db.QueryRow(`SELECT COALESCE(MAX(row_order), 0) FROM categories`).Scan(&maxOrder); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = db.Exec(`INSERT INTO categories (name, slug, row_order, locked) VALUES ($1, $2, $3, false)`,
		name, slug, maxOrder+1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") != "" {
		boardHandler(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func categoryRouter(w http.ResponseWriter, r *http.Request) {
	// /category/order  OR  /category/{slug}/{action}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	if len(parts) == 2 && parts[1] == "order" {
		reorderCategoriesHandler(w, r)
		return
	}

	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}

	slug := parts[1]
	action := parts[2]

	switch action {
	case "rename":
		renameCategoryHandler(w, r, slug)
	case "delete":
		deleteCategoryHandler(w, r, slug)
	default:
		http.NotFound(w, r)
	}
}

func renameCategoryHandler(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}

	res, err := db.Exec(`UPDATE categories SET name=$1 WHERE slug=$2 AND locked=false`, name, slug)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "Category not found or locked", http.StatusBadRequest)
		return
	}

	if r.Header.Get("HX-Request") != "" {
		boardHandler(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func deleteCategoryHandler(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check locked
	var locked bool
	err := db.QueryRow(`SELECT locked FROM categories WHERE slug=$1`, slug).Scan(&locked)
	if err == sql.ErrNoRows {
		http.Error(w, "Category not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if locked {
		http.Error(w, "Category is locked", http.StatusBadRequest)
		return
	}

	// Count cards
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cards WHERE category=$1`, slug).Scan(&count); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	moveTo := strings.TrimSpace(r.FormValue("move_to"))

	if count > 0 {
		if moveTo == "" {
			// Return 409 with card count so frontend can prompt
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]int{"count": count})
			return
		}
		if !isValidCategory(moveTo) {
			http.Error(w, "Invalid move_to category", http.StatusBadRequest)
			return
		}
		if _, err := db.Exec(`UPDATE cards SET category=$1 WHERE category=$2`, moveTo, slug); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if _, err := db.Exec(`DELETE FROM categories WHERE slug=$1`, slug); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") != "" {
		boardHandler(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func reorderCategoriesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload SlugOrderPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	for i, slug := range payload.Order {
		if _, err := db.Exec(`UPDATE categories SET row_order=$1 WHERE slug=$2`, i+1, slug); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if _, err := w.Write([]byte("OK")); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Status handlers
// ---------------------------------------------------------------------------

func createStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/status" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}

	base := slugify(name)
	if base == "" {
		http.Error(w, "Invalid name", http.StatusBadRequest)
		return
	}

	slug, err := uniqueSlug(base)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var maxOrder int
	if err := db.QueryRow(`SELECT COALESCE(MAX(col_order), 0) FROM statuses`).Scan(&maxOrder); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Insert before "done" — place at maxOrder (done is always last)
	_, err = db.Exec(`INSERT INTO statuses (name, slug, col_order, locked) VALUES ($1, $2, $3, false)`,
		name, slug, maxOrder)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") != "" {
		boardHandler(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func statusRouter(w http.ResponseWriter, r *http.Request) {
	// /status/order  OR  /status/{slug}/{action}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	if len(parts) == 2 && parts[1] == "order" {
		reorderStatusesHandler(w, r)
		return
	}

	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}

	slug := parts[1]
	action := parts[2]

	switch action {
	case "rename":
		renameStatusHandler(w, r, slug)
	case "delete":
		deleteStatusHandler(w, r, slug)
	default:
		http.NotFound(w, r)
	}
}

func renameStatusHandler(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}

	res, err := db.Exec(`UPDATE statuses SET name=$1 WHERE slug=$2 AND locked=false`, name, slug)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "Status not found or locked", http.StatusBadRequest)
		return
	}

	if r.Header.Get("HX-Request") != "" {
		boardHandler(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func deleteStatusHandler(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var locked bool
	err := db.QueryRow(`SELECT locked FROM statuses WHERE slug=$1`, slug).Scan(&locked)
	if err == sql.ErrNoRows {
		http.Error(w, "Status not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if locked {
		http.Error(w, "Status is locked", http.StatusBadRequest)
		return
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cards WHERE status=$1`, slug).Scan(&count); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	moveTo := strings.TrimSpace(r.FormValue("move_to"))

	if count > 0 {
		if moveTo == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]int{"count": count})
			return
		}
		if !isValidStatus(moveTo) {
			http.Error(w, "Invalid move_to status", http.StatusBadRequest)
			return
		}
		if _, err := db.Exec(`UPDATE cards SET status=$1 WHERE status=$2`, moveTo, slug); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if _, err := db.Exec(`DELETE FROM statuses WHERE slug=$1`, slug); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") != "" {
		boardHandler(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func reorderStatusesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload SlugOrderPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Ensure 'done' is always last regardless of what the client sends.
	filtered := make([]string, 0, len(payload.Order))
	hasDone := false
	for _, slug := range payload.Order {
		if slug == "done" {
			hasDone = true
			continue
		}
		filtered = append(filtered, slug)
	}
	if hasDone {
		filtered = append(filtered, "done")
	}

	for i, slug := range filtered {
		if _, err := db.Exec(`UPDATE statuses SET col_order=$1 WHERE slug=$2`, i+1, slug); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if _, err := w.Write([]byte("OK")); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}
