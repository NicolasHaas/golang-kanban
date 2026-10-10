package main

import (
	"context"
	"net/http"
	"strings"
)

// maxHits caps a search; a word that matches everything is not a search.
const maxHits = 100

// searchHit is a card that matched, with where it lives.
type searchHit struct {
	Card
	BoardSlug, BoardName, Lane string
}

// search finds the cards, archived ones included, that contain every word of
// query in their title, description or subtasks, ignoring case. boardID 0
// searches every board. The matching is done here rather than in SQL: the two
// databases disagree on case-insensitive matching outside ASCII, and a board
// is small enough to read whole.
func (s *store) search(ctx context.Context, query string, boardID int) ([]searchHit, bool, error) {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil, false, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.lane_id, c.title, COALESCE(c.description, ''), COALESCE(c.subtasks, ''), c.card_order,
			COALESCE(c.assignee, ''), COALESCE(c.due, ''), COALESCE(c.archived_at, 0), b.slug, b.name, l.name
		FROM cards c JOIN lanes l ON l.id = c.lane_id JOIN boards b ON b.id = l.board_id
		WHERE $1 = 0 OR b.id = $1
		ORDER BY b.id, c.archived_at IS NOT NULL, l.lane_order, c.card_order, c.id`, boardID)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var hits []searchHit
	for rows.Next() {
		var h searchHit
		c := &h.Card
		if err := rows.Scan(&c.ID, &c.LaneID, &c.Title, &c.Description, &c.Subtasks, &c.CardOrder,
			&c.Assignee, &c.Due, &c.ArchivedAt, &h.BoardSlug, &h.BoardName, &h.Lane); err != nil {
			return nil, false, err
		}
		if !matches(c, words) {
			continue
		}
		if len(hits) == maxHits {
			return hits, true, rows.Close()
		}
		hits = append(hits, h)
	}
	return hits, false, rows.Err()
}

func matches(c *Card, words []string) bool {
	var text strings.Builder
	text.WriteString(strings.ToLower(c.Title))
	text.WriteByte('\n')
	text.WriteString(strings.ToLower(c.Description))
	for _, st := range c.SubtaskList() {
		text.WriteByte('\n')
		text.WriteString(strings.ToLower(st.Text))
	}
	for _, w := range words {
		if !strings.Contains(text.String(), w) {
			return false
		}
	}
	return true
}

type searchView struct {
	User      string
	Query     string
	Board     Board // zero when searching every board
	Hits      []searchHit
	Truncated bool
}

// searchHandler is /search?q=...; with board=slug it searches that board.
func (a *app) searchHandler(w http.ResponseWriter, r *http.Request) {
	v := searchView{User: currentUser(r), Query: strings.TrimSpace(r.URL.Query().Get("q"))}
	if slug := r.URL.Query().Get("board"); slug != "" {
		b, err := a.store.boardBySlug(r.Context(), slug)
		if err != nil {
			storeError(w, r, err)
			return
		}
		v.Board = b
	}
	if v.Query != "" {
		var err error
		v.Hits, v.Truncated, err = a.store.search(r.Context(), v.Query, v.Board.ID)
		if err != nil {
			serverError(w, r, err)
			return
		}
	}
	a.render(w, r, "search.html", v)
}
