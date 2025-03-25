package main

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	_ "github.com/lib/pq"
)

type Card struct {
	ID          int
	Title       string
	Description string
	Subtasks    string
	Status      string // "todo", "inprogress", "done"
	CardOrder   int
}

var db *sql.DB
var tmpl *template.Template

// OrderUpdatePayload is used for full ordering update.
type OrderUpdatePayload struct {
	Status string `json:"status"`
	Order  []int  `json:"order"`
}

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
	defer db.Close()

	funcMap := template.FuncMap{
		"split": func(s, sep string) []string {
			s = strings.TrimSpace(s)
			if s == "" {
				return nil
			}
			return strings.Split(s, sep)
		},
		"trim": strings.TrimSpace,
	}
	tmpl = template.Must(template.New("").Funcs(funcMap).ParseGlob("templates/*.html"))

	// Favicon handler to avoid 404.
	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/card", createCardHandler)
	http.HandleFunc("/card/", cardRouter)
	http.HandleFunc("/card/order", updateOrderHandler)

	serverPort := getEnv("SERVER_PORT", "17808")
	log.Println("Server started on :" + serverPort)
	log.Fatal(http.ListenAndServe(":"+serverPort, nil))
}

func getEnv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	cardsByStatus := map[string][]Card{
		"todo":       {},
		"inprogress": {},
		"done":       {},
	}
	rows, err := db.Query("SELECT id, title, description, subtasks, status, card_order FROM cards ORDER BY status, card_order")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.ID, &c.Title, &c.Description, &c.Subtasks, &c.Status, &c.CardOrder); err != nil {
			continue
		}
		cardsByStatus[c.Status] = append(cardsByStatus[c.Status], c)
	}
	tmpl.ExecuteTemplate(w, "index.html", cardsByStatus)
}

func createCardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/card" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	title := r.FormValue("title")
	description := r.FormValue("description")
	subtasks := r.FormValue("subtasks")
	status := r.FormValue("status")
	if strings.TrimSpace(title) == "" && strings.TrimSpace(description) == "" && strings.TrimSpace(subtasks) == "" {
		http.Error(w, "Empty card not allowed", http.StatusBadRequest)
		return
	}
	var maxOrder int
	_ = db.QueryRow("SELECT COALESCE(MAX(card_order), 0) FROM cards WHERE status=$1", status).Scan(&maxOrder)
	maxOrder++
	var newID int
	err := db.QueryRow("INSERT INTO cards (title, description, subtasks, status, card_order) VALUES ($1, $2, $3, $4, $5) RETURNING id",
		title, description, subtasks, status, maxOrder).Scan(&newID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	card := Card{ID: newID, Title: title, Description: description, Subtasks: subtasks, Status: status, CardOrder: maxOrder}
	if r.Header.Get("HX-Request") != "" {
		tmpl.ExecuteTemplate(w, "card_fragment.html", card)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// cardRouter dispatches requests based on URL segments: /card/{id}/{action}
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
	newOrder, err := strconv.Atoi(r.FormValue("order"))
	if err != nil {
		http.Error(w, "Invalid order", http.StatusBadRequest)
		return
	}
	_, err = db.Exec("UPDATE cards SET status=$1, card_order=$2 WHERE id=$3", newStatus, newOrder, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Recalculate order for the destination lane.
	_, err = db.Exec(`
        WITH OrderedCards AS (
            SELECT id, ROW_NUMBER() OVER (ORDER BY card_order, id) AS new_order
            FROM cards
            WHERE status = $1
        )
        UPDATE cards SET card_order = OrderedCards.new_order
        FROM OrderedCards
        WHERE cards.id = OrderedCards.id;
    `, newStatus)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("OK"))
}

func editCardHandler(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var card Card
	err := db.QueryRow("SELECT id, title, description, subtasks, status, card_order FROM cards WHERE id=$1", id).
		Scan(&card.ID, &card.Title, &card.Description, &card.Subtasks, &card.Status, &card.CardOrder)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tmpl.ExecuteTemplate(w, "card_edit_fragment.html", card)
}

func updateCardHandler(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	title := r.FormValue("title")
	description := r.FormValue("description")
	subtasks := r.FormValue("subtasks")
	_, err := db.Exec("UPDATE cards SET title=$1, description=$2, subtasks=$3 WHERE id=$4", title, description, subtasks, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var updated Card
	err = db.QueryRow("SELECT id, title, description, subtasks, status, card_order FROM cards WHERE id=$1", id).
		Scan(&updated.ID, &updated.Title, &updated.Description, &updated.Subtasks, &updated.Status, &updated.CardOrder)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tmpl.ExecuteTemplate(w, "card_fragment.html", updated)
}

func deleteCardHandler(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	_, err := db.Exec("DELETE FROM cards WHERE id=$1", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("OK"))
}

func viewCardHandler(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var card Card
	err := db.QueryRow("SELECT id, title, description, subtasks, status, card_order FROM cards WHERE id=$1", id).
		Scan(&card.ID, &card.Title, &card.Description, &card.Subtasks, &card.Status, &card.CardOrder)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tmpl.ExecuteTemplate(w, "card_fragment.html", card)
}

func updateOrderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload OrderUpdatePayload
	err := json.NewDecoder(r.Body).Decode(&payload)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	for index, cardId := range payload.Order {
		_, err := db.Exec("UPDATE cards SET status=$1, card_order=$2 WHERE id=$3", payload.Status, index+1, cardId)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Write([]byte("OK"))
}
