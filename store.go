package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	errNotFound = errors.New("not found")
	errInvalid  = errors.New("invalid")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errInvalid}, args...)...)
}

// defaultLanes are the columns a new board starts with.
var defaultLanes = []string{"To Do", "In Progress", "Done"}

const (
	maxBoardName = 100
	maxLaneName  = 50
	maxLanes     = 20
	maxEmail     = 254
)

type Board struct {
	ID    int
	Slug  string
	Name  string
	Cards int // only filled in by listBoards
}

// Lane is a column of a board. The UI calls them columns; the table is
// called lanes because COLUMNS is a keyword in SQL.
type Lane struct {
	ID      int
	BoardID int
	Name    string
	Order   int
	Count   int    // cards on the board; only filled in by listLanes
	Total   int    // the same with archived cards; only filled in by listLanes
	Cards   []Card // only filled in by boardLanes
}

type Card struct {
	ID          int
	LaneID      int
	Title       string
	Description string
	Subtasks    string // one per line, "1|text" when done, "0|text" when not
	CardOrder   int
	Assignee    string // an email address, or empty
	Due         string // YYYY-MM-DD, or empty
	ArchivedAt  int64  // unix seconds; 0 while the card is on the board
}

type Subtask struct {
	Done bool   `json:"done"`
	Text string `json:"text"`
}

// SubtaskList parses Subtasks for the templates. It is lenient on purpose: a
// line without the "|" flag is an open subtask, and a "|" inside the text is
// kept as part of the text.
func (c Card) SubtaskList() []Subtask {
	var out []Subtask
	for _, line := range strings.Split(c.Subtasks, "\n") {
		line = strings.TrimSpace(line)
		flag, text, ok := strings.Cut(line, "|")
		if !ok {
			flag, text = "0", line
		}
		if text = strings.TrimSpace(text); text != "" {
			out = append(out, Subtask{Done: flag == "1", Text: text})
		}
	}
	return out
}

func formatSubtasks(list []Subtask) string {
	lines := make([]string, 0, len(list))
	for _, s := range list {
		// Fields also folds a newline in the text, which would split the line.
		text := strings.Join(strings.Fields(s.Text), " ")
		if text == "" {
			continue
		}
		flag := "0"
		if s.Done {
			flag = "1"
		}
		lines = append(lines, flag+"|"+text)
	}
	return strings.Join(lines, "\n")
}

func isEmpty(c Card) bool {
	return strings.TrimSpace(c.Title) == "" &&
		strings.TrimSpace(c.Description) == "" &&
		len(c.SubtaskList()) == 0
}

// cleanName trims a board or column name and checks its length.
func cleanName(what, name string, max int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > max {
		return "", invalid("a %s name has 1 to %d characters", what, max)
	}
	return name, nil
}

// cleanEmail normalises an address from the sign-in proxy or an import.
func cleanEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > maxEmail || strings.ContainsAny(s, " \t\r\n<>\"") {
		return "", invalid("%q is not an email address", s)
	}
	return s, nil
}

// store is the database. The SQL is written once for both backends: numbered
// $n placeholders, RETURNING and ON CONFLICT mean the same thing in
// PostgreSQL and in SQLite. Only the schema differs, see migrations.
//
// One rule comes from SQLite: it numbers $n placeholders in the order they
// first appear, not by n. Every query therefore uses $1, $2, ... in that
// order, and TestPlaceholdersInOrder checks it.
type store struct {
	db      *sql.DB
	dialect string // "postgres" or "sqlite"
}

// querier is what *sql.DB and *sql.Tx have in common, so a helper can run
// inside a transaction or outside one.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SQLite write transactions already take the database's writer lock.
func (s *store) forUpdate() string {
	if s.dialect == "postgres" {
		return " FOR UPDATE"
	}
	return ""
}

func (s *store) lockBoard(ctx context.Context, q querier, id int) error {
	var found int
	err := q.QueryRowContext(ctx, "SELECT id FROM boards WHERE id=$1"+s.forUpdate(), id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return errNotFound
	}
	return err
}

// --- Migrations

// migration is one step of the schema, in both dialects. Steps are applied in
// order, each once, recorded in schema_migrations. Never edit a step that has
// been released; add the next one.
type migration struct {
	postgres string
	sqlite   string
}

var migrations = []migration{
	// 1: the single table every release before boards had. IF NOT EXISTS
	// because existing installations already have it.
	{
		postgres: `
			CREATE TABLE IF NOT EXISTS cards (
				id SERIAL PRIMARY KEY,
				title TEXT NOT NULL,
				description TEXT,
				subtasks TEXT,
				status VARCHAR(20) NOT NULL DEFAULT 'todo',
				card_order INTEGER NOT NULL DEFAULT 0
			)`,
		// SQLite has no installations from before boards, so its first step
		// is the whole schema and its second one is empty.
		sqlite: `
			CREATE TABLE boards (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				slug TEXT NOT NULL UNIQUE,
				name TEXT NOT NULL
			);
			CREATE TABLE lanes (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				board_id INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
				name TEXT NOT NULL,
				lane_order INTEGER NOT NULL
			);
			CREATE TABLE cards (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				lane_id INTEGER NOT NULL REFERENCES lanes(id) ON DELETE CASCADE,
				title TEXT NOT NULL,
				description TEXT,
				subtasks TEXT,
				card_order INTEGER NOT NULL DEFAULT 0,
				assignee TEXT
			);
			CREATE TABLE users (email TEXT PRIMARY KEY);
			INSERT INTO boards (id, slug, name) VALUES (1, 'main', 'Kanban Board');
			INSERT INTO lanes (board_id, name, lane_order) VALUES (1, 'To Do', 1), (1, 'In Progress', 2), (1, 'Done', 3);
			CREATE INDEX lanes_board ON lanes (board_id, lane_order);
			CREATE INDEX cards_lane ON cards (lane_id, card_order)`,
	},
	// 2: boards, their columns and assignees. Every existing card lands on the
	// board "main", in the column its status named; a card with a status no
	// column was drawn for, which the old version could not show at all,
	// lands in To Do.
	{
		postgres: `
			CREATE TABLE boards (
				id SERIAL PRIMARY KEY,
				slug TEXT NOT NULL UNIQUE,
				name TEXT NOT NULL
			);
			CREATE TABLE lanes (
				id SERIAL PRIMARY KEY,
				board_id INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
				name TEXT NOT NULL,
				lane_order INTEGER NOT NULL
			);
			CREATE TABLE users (email TEXT PRIMARY KEY);
			INSERT INTO boards (slug, name) VALUES ('main', 'Kanban Board');
			INSERT INTO lanes (board_id, name, lane_order)
				SELECT b.id, l.name, l.n
				FROM boards b, (VALUES ('To Do', 1), ('In Progress', 2), ('Done', 3)) AS l(name, n);
			ALTER TABLE cards ADD COLUMN lane_id INTEGER REFERENCES lanes(id) ON DELETE CASCADE;
			ALTER TABLE cards ADD COLUMN assignee TEXT;
			UPDATE cards SET lane_id = lanes.id FROM lanes
				WHERE lanes.lane_order = CASE cards.status WHEN 'inprogress' THEN 2 WHEN 'done' THEN 3 ELSE 1 END;
			ALTER TABLE cards ALTER COLUMN lane_id SET NOT NULL;
			ALTER TABLE cards DROP COLUMN status;
			CREATE INDEX lanes_board ON lanes (board_id, lane_order);
			CREATE INDEX cards_lane ON cards (lane_id, card_order)`,
	},
	// 3: due dates and the archive. A due date is text, YYYY-MM-DD, because
	// the two databases agree on nothing else about dates; archived_at is unix
	// seconds, NULL while the card is on the board.
	{
		postgres: `
			ALTER TABLE cards ADD COLUMN due TEXT;
			ALTER TABLE cards ADD COLUMN archived_at BIGINT`,
		sqlite: `
			ALTER TABLE cards ADD COLUMN due TEXT;
			ALTER TABLE cards ADD COLUMN archived_at INTEGER`,
	},
}

// migrate brings the schema up to date, in one transaction: a failing step
// leaves the database as it was. On PostgreSQL an advisory lock keeps two
// instances starting at once from both applying the same step.
func (s *store) migrate(ctx context.Context) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if s.dialect == "postgres" {
			if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(17808)"); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)"); err != nil {
			return err
		}
		var current int
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
			return err
		}
		for i := current; i < len(migrations); i++ {
			stmt := migrations[i].postgres
			if s.dialect == "sqlite" {
				stmt = migrations[i].sqlite
			}
			if strings.TrimSpace(stmt) != "" {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("migration %d: %w", i+1, err)
				}
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", i+1); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- Boards

// slugify turns a board name into the part of its URL: lower case ASCII
// letters, digits and dashes. German umlauts are spelled out rather than
// dropped, anything else that is not a letter or digit becomes a dash.
func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == 'ä':
			b.WriteString("ae")
		case r == 'ö':
			b.WriteString("oe")
		case r == 'ü':
			b.WriteString("ue")
		case r == 'ß':
			b.WriteString("ss")
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
			continue
		}
		dash = false
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 50 {
		slug = strings.TrimRight(slug[:50], "-")
	}
	if slug == "" {
		slug = "board"
	}
	return slug
}

func validSlug(s string) bool {
	if s == "" || len(s) > 64 || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || strings.Contains(s, "--") {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789-", r) {
			return false
		}
	}
	return true
}

func (s *store) listBoards(ctx context.Context) ([]Board, error) {
	return listBoards(ctx, s.db)
}

func listBoards(ctx context.Context, q querier) ([]Board, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT b.id, b.slug, b.name, COUNT(c.id)
		FROM boards b
		LEFT JOIN lanes l ON l.board_id = b.id
		LEFT JOIN cards c ON c.lane_id = l.id AND c.archived_at IS NULL
		GROUP BY b.id, b.slug, b.name
		ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var boards []Board
	for rows.Next() {
		var b Board
		if err := rows.Scan(&b.ID, &b.Slug, &b.Name, &b.Cards); err != nil {
			return nil, err
		}
		boards = append(boards, b)
	}
	return boards, rows.Err()
}

func boardBySlug(ctx context.Context, q querier, slug string) (Board, error) {
	var b Board
	err := q.QueryRowContext(ctx, "SELECT id, slug, name FROM boards WHERE slug=$1", slug).Scan(&b.ID, &b.Slug, &b.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return b, errNotFound
	}
	return b, err
}

func (s *store) boardBySlug(ctx context.Context, slug string) (Board, error) {
	return boardBySlug(ctx, s.db, slug)
}

// createBoard adds a board with the default columns, its slug made from the
// name and numbered when that slug is taken.
func (s *store) createBoard(ctx context.Context, name string) (Board, error) {
	name, err := cleanName("board", name, maxBoardName)
	if err != nil {
		return Board{}, err
	}
	b := Board{Name: name}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		base := slugify(name)
		for i := 1; ; i++ {
			b.Slug = base
			if i > 1 {
				b.Slug = fmt.Sprintf("%s-%d", base, i)
			}
			_, err := boardBySlug(ctx, tx, b.Slug)
			if errors.Is(err, errNotFound) {
				break
			}
			if err != nil {
				return err
			}
		}
		if err := insertBoard(ctx, tx, &b); err != nil {
			return err
		}
		for _, name := range defaultLanes {
			if _, err := insertLane(ctx, tx, b.ID, name); err != nil {
				return err
			}
		}
		return nil
	})
	return b, err
}

func insertBoard(ctx context.Context, q querier, b *Board) error {
	return q.QueryRowContext(ctx, "INSERT INTO boards (slug, name) VALUES ($1, $2) RETURNING id", b.Slug, b.Name).Scan(&b.ID)
}

// renameBoard changes the name and keeps the slug, so links keep working.
func (s *store) renameBoard(ctx context.Context, id int, name string) error {
	name, err := cleanName("board", name, maxBoardName)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE boards SET name=$1 WHERE id=$2", name, id)
	return err
}

func (s *store) deleteBoard(ctx context.Context, id int) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return s.deleteBoardRows(ctx, tx, id) })
}

// deleteBoardRows removes cards and columns explicitly rather than relying on ON
// DELETE CASCADE, which SQLite only honours with foreign keys switched on.
func (s *store) deleteBoardRows(ctx context.Context, q querier, id int) error {
	if err := s.lockBoard(ctx, q, id); err != nil {
		return err
	}
	// Like a multi-card drag, acquire cards in id order before touching lanes.
	if err := s.lockCards(ctx, q, id, nil); err != nil {
		return err
	}
	for _, stmt := range []string{
		"DELETE FROM cards WHERE lane_id IN (SELECT id FROM lanes WHERE board_id=$1)",
		"DELETE FROM lanes WHERE board_id=$1",
		"DELETE FROM boards WHERE id=$1",
	} {
		if _, err := q.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
	}
	return nil
}

// --- Columns

// listLanes returns a board's columns in order, with how many cards each has.
func listLanes(ctx context.Context, q querier, boardID int) ([]Lane, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT l.id, l.board_id, l.name, l.lane_order,
			COUNT(CASE WHEN c.archived_at IS NULL THEN c.id END), COUNT(c.id)
		FROM lanes l LEFT JOIN cards c ON c.lane_id = l.id
		WHERE l.board_id = $1
		GROUP BY l.id, l.board_id, l.name, l.lane_order
		ORDER BY l.lane_order, l.id`, boardID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var lanes []Lane
	for rows.Next() {
		var l Lane
		if err := rows.Scan(&l.ID, &l.BoardID, &l.Name, &l.Order, &l.Count, &l.Total); err != nil {
			return nil, err
		}
		lanes = append(lanes, l)
	}
	return lanes, rows.Err()
}

func (s *store) listLanes(ctx context.Context, boardID int) ([]Lane, error) {
	return listLanes(ctx, s.db, boardID)
}

// laneOf returns column id of board boardID; a column of another board is
// errNotFound, so no request can reach across boards.
func (s *store) laneOf(ctx context.Context, q querier, boardID, id int) (Lane, error) {
	var l Lane
	err := q.QueryRowContext(ctx, "SELECT id, board_id, name, lane_order FROM lanes WHERE id=$1 AND board_id=$2"+s.forUpdate(), id, boardID).
		Scan(&l.ID, &l.BoardID, &l.Name, &l.Order)
	if errors.Is(err, sql.ErrNoRows) {
		return l, errNotFound
	}
	return l, err
}

// boardLanes returns a board's columns with their cards, each in order:
// the cards on the board, or with withArchived the archived ones as well.
func (s *store) boardLanes(ctx context.Context, boardID int, withArchived bool) ([]Lane, error) {
	return boardLanes(ctx, s.db, boardID, withArchived)
}

func boardLanes(ctx context.Context, q querier, boardID int, withArchived bool) ([]Lane, error) {
	lanes, err := listLanes(ctx, q, boardID)
	if err != nil {
		return nil, err
	}
	index := make(map[int]int, len(lanes))
	for i, l := range lanes {
		index[l.ID] = i
		lanes[i].Cards = []Card{}
	}
	onBoard := " AND archived_at IS NULL"
	if withArchived {
		onBoard = ""
	}
	rows, err := q.QueryContext(ctx, "SELECT "+cardColumns+` FROM cards
		WHERE lane_id IN (SELECT id FROM lanes WHERE board_id=$1)`+onBoard+`
		ORDER BY card_order, id`, boardID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		if i, ok := index[c.LaneID]; ok {
			lanes[i].Cards = append(lanes[i].Cards, c)
		}
	}
	return lanes, rows.Err()
}

func (s *store) addLane(ctx context.Context, boardID int, name string) error {
	name, err := cleanName("column", name, maxLaneName)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := s.lockBoard(ctx, tx, boardID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM lanes WHERE board_id=$1", boardID).Scan(&n); err != nil {
			return err
		}
		if n >= maxLanes {
			return invalid("a board has at most %d columns", maxLanes)
		}
		_, err := insertLane(ctx, tx, boardID, name)
		return err
	})
}

// insertLane appends a column at the right end of the board.
func insertLane(ctx context.Context, q querier, boardID int, name string) (int, error) {
	var order int
	if err := q.QueryRowContext(ctx, "SELECT COALESCE(MAX(lane_order), 0) + 1 FROM lanes WHERE board_id=$1", boardID).Scan(&order); err != nil {
		return 0, err
	}
	var id int
	err := q.QueryRowContext(ctx, "INSERT INTO lanes (board_id, name, lane_order) VALUES ($1, $2, $3) RETURNING id",
		boardID, name, order).Scan(&id)
	return id, err
}

func (s *store) renameLane(ctx context.Context, boardID, id int, name string) error {
	name, err := cleanName("column", name, maxLaneName)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, "UPDATE lanes SET name=$1 WHERE id=$2 AND board_id=$3", name, id, boardID)
	return mustAffect(res, err)
}

// moveLane swaps a column with its neighbour, delta -1 for left and +1 for
// right, and numbers the board's columns afresh.
func (s *store) moveLane(ctx context.Context, boardID, id, delta int) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockBoard(ctx, tx, boardID); err != nil {
			return err
		}
		lanes, err := listLanes(ctx, tx, boardID)
		if err != nil {
			return err
		}
		from := -1
		for i, l := range lanes {
			if l.ID == id {
				from = i
			}
		}
		if from < 0 {
			return errNotFound
		}
		to := from + delta
		if to < 0 || to >= len(lanes) {
			return nil // already at the edge
		}
		lanes[from], lanes[to] = lanes[to], lanes[from]
		for i, l := range lanes {
			if _, err := tx.ExecContext(ctx, "UPDATE lanes SET lane_order=$1 WHERE id=$2", i+1, l.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// deleteLane removes an empty column. One with cards is refused rather than
// taking them along: they are moved or deleted first, by someone who sees
// them.
func (s *store) deleteLane(ctx context.Context, boardID, id int) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// Board before lane: serialize column limits/order, then block incoming
		// cards (including FK inserts) before checking whether the lane is empty.
		if err := s.lockBoard(ctx, tx, boardID); err != nil {
			return err
		}
		if _, err := s.laneOf(ctx, tx, boardID, id); err != nil {
			return err
		}
		lanes, err := listLanes(ctx, tx, boardID)
		if err != nil {
			return err
		}
		for _, l := range lanes {
			if l.ID != id {
				continue
			}
			if l.Count > 0 {
				return invalid("column %q still has %d cards; move or archive them first", l.Name, l.Count)
			}
			if l.Total > 0 {
				return invalid("column %q has %d archived cards; restore or delete them in the archive first", l.Name, l.Total)
			}
			if len(lanes) == 1 {
				return invalid("a board needs at least one column")
			}
			_, err := tx.ExecContext(ctx, "DELETE FROM lanes WHERE id=$1", id)
			return err
		}
		return errNotFound
	})
}

// --- Cards

// description, subtasks and assignee are nullable, so every read coalesces
// them: a row written by hand with a NULL must not make the board fail to load.
const cardColumns = "id, lane_id, title, COALESCE(description, ''), COALESCE(subtasks, ''), card_order, " +
	"COALESCE(assignee, ''), COALESCE(due, ''), COALESCE(archived_at, 0)"

type scanner interface{ Scan(dest ...any) error }

func scanCard(s scanner) (Card, error) {
	var c Card
	err := s.Scan(&c.ID, &c.LaneID, &c.Title, &c.Description, &c.Subtasks, &c.CardOrder, &c.Assignee, &c.Due, &c.ArchivedAt)
	return c, err
}

func (s *store) getCard(ctx context.Context, id int) (Card, error) {
	return getCard(ctx, s.db, id, "")
}

func getCard(ctx context.Context, q querier, id int, lock string) (Card, error) {
	c, err := scanCard(q.QueryRowContext(ctx, "SELECT "+cardColumns+" FROM cards WHERE id=$1"+lock, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, errNotFound
	}
	return c, err
}

// createCard adds c to column c.LaneID, which has to be on board boardID.
func (s *store) createCard(ctx context.Context, boardID int, c Card) (Card, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.laneOf(ctx, tx, boardID, c.LaneID); err != nil {
			return err
		}
		return insertCard(ctx, tx, &c)
	})
	return c, err
}

// insertCard appends the card at the end of its column.
func insertCard(ctx context.Context, q querier, c *Card) error {
	err := q.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(card_order), 0) + 1 FROM cards WHERE lane_id=$1", c.LaneID).Scan(&c.CardOrder)
	if err != nil {
		return err
	}
	return q.QueryRowContext(ctx,
		`INSERT INTO cards (lane_id, title, description, subtasks, card_order, assignee, due, archived_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		c.LaneID, c.Title, c.Description, c.Subtasks, c.CardOrder, nullable(c.Assignee), nullable(c.Due), nullableTime(c.ArchivedAt)).Scan(&c.ID)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(unix int64) any {
	if unix == 0 {
		return nil
	}
	return unix
}

// cardEdit is what the card editor saves.
type cardEdit struct {
	Title, Description, Subtasks string
	Due                          string // YYYY-MM-DD or empty; see cleanDue
	// Assignee is nil when assignees are switched off, which leaves whatever
	// is stored alone; otherwise an address that has signed in before, or
	// empty for nobody.
	Assignee *string
	// LaneID moves the card to the end of another column of its board; 0 or
	// its own column leaves it where it is.
	LaneID int
}

// updateCard saves an edit in one statement, so text, assignee and column
// change together or not at all.
func (s *store) updateCard(ctx context.Context, id int, e cardEdit) (Card, error) {
	var c Card
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		c, err = getCard(ctx, tx, id, s.forUpdate())
		if err != nil {
			return err
		}
		if e.Assignee != nil {
			if *e.Assignee != "" {
				var known int
				if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE email=$1", *e.Assignee).Scan(&known); err != nil {
					return err
				}
				if known == 0 {
					return invalid("%s has not signed in yet and cannot be assigned", *e.Assignee)
				}
			}
			c.Assignee = *e.Assignee
		}
		if err := s.relocate(ctx, tx, &c, e.LaneID); err != nil {
			return err
		}
		c.Title, c.Description, c.Subtasks, c.Due = e.Title, e.Description, e.Subtasks, e.Due
		_, err = tx.ExecContext(ctx, `
			UPDATE cards SET title=$1, description=$2, subtasks=$3, due=$4, assignee=$5, lane_id=$6, card_order=$7
			WHERE id=$8`, c.Title, c.Description, c.Subtasks, nullable(c.Due), nullable(c.Assignee), c.LaneID, c.CardOrder, id)
		return err
	})
	return c, err
}

// relocate points c at the end of column laneID, which has to be on the same
// board; 0 or its own column leaves it where it is. The caller saves it.
func (s *store) relocate(ctx context.Context, q querier, c *Card, laneID int) error {
	if laneID == 0 || laneID == c.LaneID {
		return nil
	}
	var boardID int
	err := q.QueryRowContext(ctx, "SELECT board_id FROM lanes WHERE id=$1", c.LaneID).Scan(&boardID)
	if err != nil {
		return err
	}
	if _, err := s.laneOf(ctx, q, boardID, laneID); err != nil {
		return err
	}
	c.LaneID = laneID
	return q.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(card_order), 0) + 1 FROM cards WHERE lane_id=$1", laneID).Scan(&c.CardOrder)
}

// moveCardTo puts a card at the end of column laneID of its board. The
// column is named rather than "the next one", so a request that arrives
// twice leaves the card where the first one put it.
func (s *store) moveCardTo(ctx context.Context, id, laneID int) (Card, error) {
	var c Card
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		c, err = getCard(ctx, tx, id, s.forUpdate())
		if err != nil {
			return err
		}
		if laneID == c.LaneID {
			return nil
		}
		if err := s.relocate(ctx, tx, &c, laneID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE cards SET lane_id=$1, card_order=$2 WHERE id=$3", c.LaneID, c.CardOrder, id)
		return err
	})
	return c, err
}

// boardLanesOf returns the columns of the board column laneID is on, for the
// editor's column picker.
func (s *store) boardLanesOf(ctx context.Context, laneID int) ([]Lane, error) {
	var boardID int
	err := s.db.QueryRowContext(ctx, "SELECT board_id FROM lanes WHERE id=$1", laneID).Scan(&boardID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.listLanes(ctx, boardID)
}

// errStale is a change made against a version of the card that is no
// longer the one stored.
var errStale = errors.New("the card has changed since this page was loaded")

// setSubtask marks subtask index of card id done or not. text is the line the
// click was made on: if the card's subtasks were edited since the page was
// drawn, the line at index may be a different one, and nothing is changed.
func (s *store) setSubtask(ctx context.Context, id, index int, text string, done bool) (Card, error) {
	var c Card
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		c, err = getCard(ctx, tx, id, s.forUpdate())
		if err != nil {
			return err
		}
		list := c.SubtaskList()
		if index < 0 || index >= len(list) || list[index].Text != text {
			return errStale
		}
		list[index].Done = done
		c.Subtasks = formatSubtasks(list)
		_, err = tx.ExecContext(ctx, "UPDATE cards SET subtasks=$1 WHERE id=$2", c.Subtasks, id)
		return err
	})
	return c, err
}

// archiveCard takes a card off the board and keeps it, column and position
// included, so restoreCard puts back the same card in the same place.
func (s *store) archiveCard(ctx context.Context, id int, now time.Time) error {
	res, err := s.db.ExecContext(ctx, "UPDATE cards SET archived_at=$1 WHERE id=$2 AND archived_at IS NULL", now.Unix(), id)
	if err := mustAffect(res, err); !errors.Is(err, errNotFound) {
		return err
	}
	// Archived already, by a tap that arrived twice: done all the same.
	_, err = s.getCard(ctx, id)
	return err
}

func (s *store) restoreCard(ctx context.Context, id int) (Card, error) {
	if _, err := s.db.ExecContext(ctx, "UPDATE cards SET archived_at=NULL WHERE id=$1", id); err != nil {
		return Card{}, err
	}
	return s.getCard(ctx, id)
}

// archivedCard is a card in a board's archive, with the column it left.
type archivedCard struct {
	Card
	Lane string
}

// archivedCards lists a board's archive, most recently archived first.
func (s *store) archivedCards(ctx context.Context, boardID int) ([]archivedCard, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.lane_id, c.title, COALESCE(c.description, ''), COALESCE(c.subtasks, ''), c.card_order,
			COALESCE(c.assignee, ''), COALESCE(c.due, ''), COALESCE(c.archived_at, 0), l.name
		FROM cards c JOIN lanes l ON l.id = c.lane_id
		WHERE l.board_id = $1 AND c.archived_at IS NOT NULL
		ORDER BY c.archived_at DESC, c.id DESC`, boardID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cards []archivedCard
	for rows.Next() {
		var a archivedCard
		c := &a.Card
		if err := rows.Scan(&c.ID, &c.LaneID, &c.Title, &c.Description, &c.Subtasks, &c.CardOrder,
			&c.Assignee, &c.Due, &c.ArchivedAt, &a.Lane); err != nil {
			return nil, err
		}
		cards = append(cards, a)
	}
	return cards, rows.Err()
}

func (s *store) deleteCard(ctx context.Context, id int) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM cards WHERE id=$1", id)
	return err
}

// reorderLane puts the listed cards into column laneID in the given order. A
// card that came from another column leaves a gap there, which is harmless
// because columns are sorted by card_order, not numbered contiguously. Ids
// that are not on this board, or no longer exist because they were deleted in
// another tab, are skipped.
func (s *store) reorderLane(ctx context.Context, boardID, laneID int, ids []int) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// Card before destination lane, as in updateCard/moveCardTo. Lock
		// multiple cards in id order so overlapping drags cannot deadlock.
		if len(ids) > 0 {
			if err := s.lockCards(ctx, tx, boardID, ids); err != nil {
				return err
			}
		}
		if _, err := s.laneOf(ctx, tx, boardID, laneID); err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, `
			UPDATE cards SET lane_id=$1, card_order=$2
			WHERE id=$3 AND lane_id IN (SELECT id FROM lanes WHERE board_id=$4)`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for i, id := range ids {
			if _, err := stmt.ExecContext(ctx, laneID, i+1, id, boardID); err != nil {
				return err
			}
		}
		return nil
	})
}

func mustAffect(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errNotFound
	}
	return nil
}

// nil ids locks every card on the board, for board deletion/replacement.
func (s *store) lockCards(ctx context.Context, q querier, boardID int, ids []int) error {
	if s.dialect != "postgres" {
		return nil
	}
	query := "SELECT id FROM cards WHERE lane_id IN (SELECT id FROM lanes WHERE board_id=$1)"
	args := []any{boardID}
	if ids != nil {
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			placeholders[i] = fmt.Sprintf("$%d", i+2)
			args = append(args, id)
		}
		query += " AND id IN (" + strings.Join(placeholders, ",") + ")"
	}
	rows, err := q.QueryContext(ctx, query+" ORDER BY id FOR UPDATE", args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

// --- Users

// rememberUser records an address the sign-in proxy vouched for, so it can be
// offered as an assignee.
func rememberUser(ctx context.Context, q querier, email string) error {
	_, err := q.ExecContext(ctx, "INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO NOTHING", email)
	return err
}

func (s *store) rememberUser(ctx context.Context, email string) error {
	return rememberUser(ctx, s.db, email)
}

func (s *store) listUsers(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT email FROM users ORDER BY email")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var users []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}
