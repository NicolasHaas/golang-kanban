package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testBackends lists the databases the store tests run against: a fresh
// SQLite file when the driver is built in, and PostgreSQL when
// KANBAN_TEST_POSTGRES_URL names one.
var testBackends = []struct {
	name string
	open func(t *testing.T) *store
}{
	{"sqlite", openTestSQLite},
	{"postgres", openTestPostgres},
}

func openTestSQLite(t *testing.T) *store {
	t.Helper()
	db, err := sql.Open("sqlite3", sqliteDSN(filepath.Join(t.TempDir(), "kanban.db")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return &store{db: db, dialect: "sqlite"}
}

// A database file whose path has URL syntax in it must still be that file.
func TestSQLitePathIsEscaped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a #b ?c %d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "kanban.db")
	db, err := sql.Open("sqlite3", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the database is not where SQLITE_PATH said: %v", err)
	}
	if got := sqliteDSN("kanban.db"); !strings.HasPrefix(got, "file:kanban.db?") {
		t.Errorf("a relative path: %s", got)
	}
}

// SQLite numbers $n placeholders in the order they first appear rather than
// by n, so "$2 ... $1" would bind the arguments the wrong way round there and
// the right way round in PostgreSQL. Every SQL string literal in the package
// has to introduce its placeholders as $1, $2, ... in order.
func TestPlaceholdersInOrder(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	placeholder := regexp.MustCompile(`\$(\d+)`)
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			next := 1
			for _, m := range placeholder.FindAllStringSubmatch(lit.Value, -1) {
				n, _ := strconv.Atoi(m[1])
				if n > next {
					t.Errorf("%s: $%d appears before $%d", fset.Position(lit.Pos()), n, next)
					return true
				}
				if n == next {
					next++
				}
			}
			return true
		})
	}
}

// forEachBackend runs fn once per available backend, on an empty, migrated
// database.
func forEachBackend(t *testing.T, fn func(t *testing.T, s *store)) {
	for _, backend := range testBackends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)
			if err := s.migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			fn(t, s)
		})
	}
}

// openTestPostgres starts from an empty database. Its name has to end in
// _test, because every table in it is dropped.
func openTestPostgres(t *testing.T) *store {
	t.Helper()
	dsn := os.Getenv("KANBAN_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("KANBAN_TEST_POSTGRES_URL is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(u.Path, "_test") {
		t.Fatal("KANBAN_TEST_POSTGRES_URL must name a database ending in _test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("DROP TABLE IF EXISTS cards, lanes, boards, users, schema_migrations"); err != nil {
		t.Fatal(err)
	}
	return &store{db: db, dialect: "postgres"}
}

func mustBoard(t *testing.T, s *store, slug string) (Board, []Lane) {
	t.Helper()
	b, err := s.boardBySlug(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	lanes, err := s.boardLanes(context.Background(), b.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	return b, lanes
}

// layout draws a board as "To Do: a,b | Done: c".
func layout(t *testing.T, s *store, slug string) string {
	t.Helper()
	_, lanes := mustBoard(t, s, slug)
	var cols []string
	for _, l := range lanes {
		var titles []string
		for _, c := range l.Cards {
			titles = append(titles, c.Title)
		}
		cols = append(cols, l.Name+": "+strings.Join(titles, ","))
	}
	return strings.Join(cols, " | ")
}

func TestMigrateIsIdempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		if err := s.migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := layout(t, s, "main"); got != "To Do:  | In Progress:  | Done: " {
			t.Errorf("a fresh database has the board main with three columns: %s", got)
		}
	})
}

// An installation from before boards has the single cards table and nothing
// else. Its cards must come through onto the board "main", each in the
// column its status named and in order; a card in a status the old board
// could not show comes back in To Do.
func TestMigrateFromSingleTable(t *testing.T) {
	s := openTestPostgres(t)
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE cards (
			id SERIAL PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT,
			subtasks TEXT,
			status VARCHAR(20) NOT NULL DEFAULT 'todo',
			card_order INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO cards (title, status, card_order) VALUES
			('second', 'todo', 2), ('first', 'todo', 1), ('doing', 'inprogress', 1),
			('done', 'done', 1), ('lost', 'archived', 9)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := layout(t, s, "main"); got != "To Do: first,second,lost | In Progress: doing | Done: done" {
		t.Errorf("after migration: %s", got)
	}
}

// A SQLite database from before due dates and the archive, as the earlier
// test builds left it, picks up both and keeps its cards on the board.
func TestMigrateSQLiteToArchive(t *testing.T) {
	s := openTestSQLite(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, migrations[0].sqlite); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY);
		INSERT INTO schema_migrations (version) VALUES (1), (2);
		INSERT INTO cards (lane_id, title, card_order) VALUES (1, 'kept', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := layout(t, s, "main"); got != "To Do: kept | In Progress:  | Done: " {
		t.Errorf("after the upgrade: %s", got)
	}
}

func TestBoards(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		a, err := s.createBoard(ctx, " Team Ä ")
		if err != nil || a.Slug != "team-ae" || a.Name != "Team Ä" {
			t.Fatalf("create: %+v %v", a, err)
		}
		if got := layout(t, s, "team-ae"); got != "To Do:  | In Progress:  | Done: " {
			t.Errorf("a new board has the default columns: %s", got)
		}
		b, err := s.createBoard(ctx, "team ä")
		if err != nil || b.Slug != "team-ae-2" {
			t.Errorf("second board with the same slug: %+v %v", b, err)
		}
		if _, err := s.createBoard(ctx, "  "); !errors.Is(err, errInvalid) {
			t.Errorf("empty name: %v", err)
		}
		if err := s.renameBoard(ctx, a.ID, "Renamed"); err != nil {
			t.Fatal(err)
		}
		if got, _ := mustBoard(t, s, "team-ae"); got.Name != "Renamed" {
			t.Errorf("rename kept the slug and changed the name: %+v", got)
		}

		_, lanes := mustBoard(t, s, "team-ae")
		if _, err := s.createCard(ctx, a.ID, Card{LaneID: lanes[0].ID, Title: "on a"}); err != nil {
			t.Fatal(err)
		}
		if err := s.deleteBoard(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.boardBySlug(ctx, "team-ae"); !errors.Is(err, errNotFound) {
			t.Errorf("deleted board: %v", err)
		}
		var left int
		if err := s.db.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM cards) + (SELECT COUNT(*) FROM lanes WHERE board_id=$1)", a.ID).Scan(&left); err != nil || left != 0 {
			t.Errorf("rows left behind by a deleted board: %d %v", left, err)
		}
	})
}

func TestColumns(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		b, lanes := mustBoard(t, s, "main")
		todo, doing, done := lanes[0].ID, lanes[1].ID, lanes[2].ID

		if err := s.addLane(ctx, b.ID, "Waiting"); err != nil {
			t.Fatal(err)
		}
		if err := s.renameLane(ctx, b.ID, doing, "Doing"); err != nil {
			t.Fatal(err)
		}
		if err := s.moveLane(ctx, b.ID, done, +1); err != nil { // Done after Waiting
			t.Fatal(err)
		}
		if err := s.moveLane(ctx, b.ID, todo, -1); err != nil { // already first: nothing
			t.Fatal(err)
		}
		if got := layout(t, s, "main"); got != "To Do:  | Doing:  | Waiting:  | Done: " {
			t.Errorf("columns: %s", got)
		}

		if _, err := s.createCard(ctx, b.ID, Card{LaneID: doing, Title: "busy"}); err != nil {
			t.Fatal(err)
		}
		if err := s.deleteLane(ctx, b.ID, doing); !errors.Is(err, errInvalid) {
			t.Errorf("deleting a column with cards: %v", err)
		}
		if err := s.deleteLane(ctx, b.ID, todo); err != nil {
			t.Errorf("deleting an empty column: %v", err)
		}
		if err := s.addLane(ctx, b.ID, strings.Repeat("x", maxLaneName+1)); !errors.Is(err, errInvalid) {
			t.Errorf("a name that is too long: %v", err)
		}

		// A column of another board cannot be reached through this one.
		other, err := s.createBoard(ctx, "Other")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.renameLane(ctx, other.ID, doing, "x"); !errors.Is(err, errNotFound) {
			t.Errorf("renaming a column through another board: %v", err)
		}
		if err := s.deleteLane(ctx, other.ID, done); !errors.Is(err, errNotFound) {
			t.Errorf("deleting a column through another board: %v", err)
		}
		if _, err := s.createCard(ctx, other.ID, Card{LaneID: doing, Title: "x"}); !errors.Is(err, errNotFound) {
			t.Errorf("adding a card through another board: %v", err)
		}

		// The last column stays.
		_, otherLanes := mustBoard(t, s, "other")
		for i, l := range otherLanes {
			err := s.deleteLane(ctx, other.ID, l.ID)
			if last := i == len(otherLanes)-1; last != errors.Is(err, errInvalid) {
				t.Errorf("deleting column %d of %d: %v", i+1, len(otherLanes), err)
			}
		}

		for range maxLanes {
			_ = s.addLane(ctx, other.ID, "more")
		}
		if got, _ := s.listLanes(ctx, other.ID); len(got) != maxLanes {
			t.Errorf("a board grew to %d columns", len(got))
		}
	})
}

func TestCardLifecycle(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		main, lanes := mustBoard(t, s, "main")
		todo, done := lanes[0].ID, lanes[2].ID
		other, err := s.createBoard(ctx, "Other")
		if err != nil {
			t.Fatal(err)
		}
		_, otherLanes := mustBoard(t, s, "other")

		ids := map[string]int{}
		for _, title := range []string{"a", "b", "c"} {
			c, err := s.createCard(ctx, main.ID, Card{LaneID: todo, Title: title})
			if err != nil {
				t.Fatal(err)
			}
			ids[title] = c.ID
		}
		x, err := s.createCard(ctx, other.ID, Card{LaneID: otherLanes[0].ID, Title: "x"})
		if err != nil {
			t.Fatal(err)
		}

		// Drag "b" to Done: one request for the destination column only.
		if err := s.reorderLane(ctx, main.ID, done, []int{ids["b"]}); err != nil {
			t.Fatal(err)
		}
		// Reorder within To Do, with an id that was deleted elsewhere and one
		// from another board in the list; both are skipped.
		if err := s.reorderLane(ctx, main.ID, todo, []int{ids["c"], 999999, x.ID, ids["a"]}); err != nil {
			t.Fatal(err)
		}
		if got := layout(t, s, "main"); got != "To Do: c,a | In Progress:  | Done: b" {
			t.Errorf("after reorder: %s", got)
		}
		if got := layout(t, s, "other"); got != "To Do: x | In Progress:  | Done: " {
			t.Errorf("the other board was touched: %s", got)
		}
		if err := s.reorderLane(ctx, main.ID, otherLanes[0].ID, []int{ids["a"]}); !errors.Is(err, errNotFound) {
			t.Errorf("dragging into a column of another board: %v", err)
		}

		c, err := s.updateCard(ctx, ids["c"], cardEdit{Title: "c2", Description: "desc", Subtasks: "1|sub"})
		if err != nil || c.Title != "c2" || c.LaneID != todo || len(c.SubtaskList()) != 1 {
			t.Errorf("update: %+v %v", c, err)
		}
		if _, err := s.updateCard(ctx, 999999, cardEdit{Title: "x"}); !errors.Is(err, errNotFound) {
			t.Errorf("update of a missing card: %v", err)
		}

		// Moved from the editor: to the end of another column of its board,
		// never to a column of another board.
		moved, err := s.updateCard(ctx, ids["a"], cardEdit{Title: "a", LaneID: done})
		if err != nil || moved.LaneID != done {
			t.Errorf("move from the editor: %+v %v", moved, err)
		}
		if got := layout(t, s, "main"); got != "To Do: c2 | In Progress:  | Done: b,a" {
			t.Errorf("after moving from the editor: %s", got)
		}
		if _, err := s.updateCard(ctx, ids["a"], cardEdit{Title: "a", LaneID: otherLanes[0].ID}); !errors.Is(err, errNotFound) {
			t.Errorf("move to another board: %v", err)
		}
		// The phone's arrow buttons: the same move twice is one move.
		for range 2 {
			if got, err := s.moveCardTo(ctx, ids["a"], lanes[1].ID); err != nil || got.LaneID != lanes[1].ID {
				t.Fatalf("move with the buttons: %+v %v", got, err)
			}
		}
		if got := layout(t, s, "main"); got != "To Do: c2 | In Progress: a | Done: b" {
			t.Errorf("after the buttons: %s", got)
		}
		if _, err := s.moveCardTo(ctx, ids["a"], otherLanes[0].ID); !errors.Is(err, errNotFound) {
			t.Errorf("buttons to another board: %v", err)
		}
		if _, err := s.moveCardTo(ctx, 999999, done); !errors.Is(err, errNotFound) {
			t.Errorf("buttons on a missing card: %v", err)
		}
		if picker, err := s.boardLanesOf(ctx, done); err != nil || len(picker) != 3 || picker[2].ID != done {
			t.Errorf("columns for the picker: %+v %v", picker, err)
		}
		if _, err := s.updateCard(ctx, ids["a"], cardEdit{Title: "a", LaneID: todo}); err != nil {
			t.Fatal(err)
		}

		if err := s.deleteCard(ctx, ids["c"]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.getCard(ctx, ids["c"]); !errors.Is(err, errNotFound) {
			t.Errorf("get after delete: %v", err)
		}

		// A row written by hand with NULLs must not break the board.
		if _, err := s.db.ExecContext(ctx, "INSERT INTO cards (lane_id, title) VALUES ($1, 'raw')", lanes[1].ID); err != nil {
			t.Fatal(err)
		}
		if got := layout(t, s, "main"); got != "To Do: a | In Progress: raw | Done: b" {
			t.Errorf("with NULL columns: %s", got)
		}
	})
}

func TestSetSubtask(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		main, lanes := mustBoard(t, s, "main")
		c, err := s.createCard(ctx, main.ID, Card{LaneID: lanes[0].ID, Title: "t", Subtasks: "0|first\n0|a | b"})
		if err != nil {
			t.Fatal(err)
		}
		// The same click arriving twice leaves the line ticked.
		for range 2 {
			got, err := s.setSubtask(ctx, c.ID, 1, "a | b", true)
			if err != nil || got.Subtasks != "0|first\n1|a | b" {
				t.Fatalf("tick: %q %v", got.Subtasks, err)
			}
		}
		if got, err := s.setSubtask(ctx, c.ID, 1, "a | b", false); err != nil || got.Subtasks != "0|first\n0|a | b" {
			t.Errorf("untick: %q %v", got.Subtasks, err)
		}
		// A click on a page drawn before the subtasks were edited.
		for _, tc := range []struct {
			index int
			text  string
		}{{0, "a | b"}, {2, "first"}, {-1, "first"}} {
			if _, err := s.setSubtask(ctx, c.ID, tc.index, tc.text, true); !errors.Is(err, errStale) {
				t.Errorf("index %d text %q: %v", tc.index, tc.text, err)
			}
		}
		if _, err := s.setSubtask(ctx, 999999, 0, "first", true); !errors.Is(err, errNotFound) {
			t.Errorf("missing card: %v", err)
		}
	})
}

func TestArchive(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		main, lanes := mustBoard(t, s, "main")
		var ids []int
		for _, title := range []string{"a", "b", "c"} {
			c, err := s.createCard(ctx, main.ID, Card{LaneID: lanes[0].ID, Title: title})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, c.ID)
		}
		now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
		for range 2 { // a tap that arrives twice archives once
			if err := s.archiveCard(ctx, ids[1], now); err != nil {
				t.Fatal(err)
			}
		}
		if got := layout(t, s, "main"); got != "To Do: a,c | In Progress:  | Done: " {
			t.Errorf("after archiving b: %s", got)
		}
		if boards, _ := s.listBoards(ctx); boards[0].Cards != 2 {
			t.Errorf("the board list counts archived cards: %d", boards[0].Cards)
		}
		archived, err := s.archivedCards(ctx, main.ID)
		if err != nil || len(archived) != 1 || archived[0].Title != "b" || archived[0].Lane != "To Do" || archived[0].ArchivedAt != now.Unix() {
			t.Errorf("archive: %+v %v", archived, err)
		}
		if err := s.archiveCard(ctx, 999999, now); !errors.Is(err, errNotFound) {
			t.Errorf("archiving a missing card: %v", err)
		}

		// An archived card keeps its column from being deleted.
		if err := s.deleteLane(ctx, main.ID, lanes[0].ID); !errors.Is(err, errInvalid) {
			t.Errorf("deleting a column with an archived card: %v", err)
		}

		// Restored, it is back where it was: between a and c.
		if c, err := s.restoreCard(ctx, ids[1]); err != nil || c.ArchivedAt != 0 {
			t.Fatalf("restore: %+v %v", c, err)
		}
		if got := layout(t, s, "main"); got != "To Do: a,b,c | In Progress:  | Done: " {
			t.Errorf("after restoring b: %s", got)
		}
		if _, err := s.restoreCard(ctx, 999999); !errors.Is(err, errNotFound) {
			t.Errorf("restoring a missing card: %v", err)
		}
	})
}

func TestDueDates(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		main, lanes := mustBoard(t, s, "main")
		c, err := s.createCard(ctx, main.ID, Card{LaneID: lanes[0].ID, Title: "t", Due: "2026-10-12"})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := s.getCard(ctx, c.ID); got.Due != "2026-10-12" {
			t.Errorf("created with %q", got.Due)
		}
		if got, err := s.updateCard(ctx, c.ID, cardEdit{Title: "t", Due: "2026-11-01"}); err != nil || got.Due != "2026-11-01" {
			t.Errorf("update: %+v %v", got, err)
		}
		if got, err := s.updateCard(ctx, c.ID, cardEdit{Title: "t"}); err != nil || got.Due != "" {
			t.Errorf("cleared: %+v %v", got, err)
		}
	})
}

func TestSearch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		main, lanes := mustBoard(t, s, "main")
		other, err := s.createBoard(ctx, "Other")
		if err != nil {
			t.Fatal(err)
		}
		_, otherLanes := mustBoard(t, s, "other")
		mk := func(boardID, laneID int, c Card) int {
			c.LaneID = laneID
			got, err := s.createCard(ctx, boardID, c)
			if err != nil {
				t.Fatal(err)
			}
			return got.ID
		}
		mk(main.ID, lanes[0].ID, Card{Title: "Überweisung prüfen", Description: "Rechnung März"})
		mk(main.ID, lanes[1].ID, Card{Title: "Server", Subtasks: "0|Backup prüfen"})
		gone := mk(main.ID, lanes[2].ID, Card{Title: "Old backup"})
		mk(other.ID, otherLanes[0].ID, Card{Title: "Backup NAS"})
		if err := s.archiveCard(ctx, gone, time.Now()); err != nil {
			t.Fatal(err)
		}

		titles := func(q string, boardID int) string {
			t.Helper()
			hits, _, err := s.search(ctx, q, boardID)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, h := range hits {
				out = append(out, h.Title+"@"+h.BoardSlug+"/"+h.Lane)
			}
			return strings.Join(out, ",")
		}
		for _, tc := range []struct {
			q     string
			board int
			want  string
		}{
			{"PRÜFEN", 0, "Überweisung prüfen@main/To Do,Server@main/In Progress"}, // case, beyond ASCII, subtasks
			{"backup", 0, "Server@main/In Progress,Old backup@main/Done,Backup NAS@other/To Do"},
			{"backup", other.ID, "Backup NAS@other/To Do"},
			{"rechnung prüfen", 0, "Überweisung prüfen@main/To Do"}, // every word, anywhere on the card
			{"rechnung server", 0, ""},
			{"   ", 0, ""},
		} {
			if got := titles(tc.q, tc.board); got != tc.want {
				t.Errorf("search %q board %d:\n got %s\nwant %s", tc.q, tc.board, got, tc.want)
			}
		}

		for i := range maxHits + 5 {
			mk(main.ID, lanes[0].ID, Card{Title: "bulk " + strconv.Itoa(i)})
		}
		if hits, truncated, err := s.search(ctx, "bulk", 0); err != nil || len(hits) != maxHits || !truncated {
			t.Errorf("a search that matches too much: %d %v %v", len(hits), truncated, err)
		}
	})
}

func TestAssignees(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		main, lanes := mustBoard(t, s, "main")
		c, err := s.createCard(ctx, main.ID, Card{LaneID: lanes[0].ID, Title: "task"})
		if err != nil {
			t.Fatal(err)
		}
		ada, nobody := "ada@example.com", ""

		if _, err := s.updateCard(ctx, c.ID, cardEdit{Title: "task", Assignee: &ada}); !errors.Is(err, errInvalid) {
			t.Errorf("assigning somebody who never signed in: %v", err)
		}
		for range 2 { // remembering twice is harmless
			if err := s.rememberUser(ctx, ada); err != nil {
				t.Fatal(err)
			}
		}
		if users, err := s.listUsers(ctx); err != nil || len(users) != 1 || users[0] != ada {
			t.Errorf("users: %v %v", users, err)
		}
		if got, err := s.updateCard(ctx, c.ID, cardEdit{Title: "task", Assignee: &ada}); err != nil || got.Assignee != ada {
			t.Errorf("assign: %+v %v", got, err)
		}
		// Assignees switched off: the stored one is left alone.
		if got, err := s.updateCard(ctx, c.ID, cardEdit{Title: "task 2"}); err != nil || got.Assignee != ada {
			t.Errorf("update without assignees: %+v %v", got, err)
		}
		if got, err := s.updateCard(ctx, c.ID, cardEdit{Title: "task", Assignee: &nobody}); err != nil || got.Assignee != "" {
			t.Errorf("unassign: %+v %v", got, err)
		}
	})
}

func TestExportImportRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		main, lanes := mustBoard(t, s, "main")
		if err := s.rememberUser(ctx, "ada@example.com"); err != nil {
			t.Fatal(err)
		}
		for _, c := range []Card{
			{LaneID: lanes[0].ID, Title: "due", Due: "2026-12-24"},
			{LaneID: lanes[1].ID, Title: "archived", ArchivedAt: 1791000000},
			{LaneID: lanes[0].ID, Title: "one", Subtasks: "1|done\n0|open"},
			{LaneID: lanes[0].ID, Title: "two", Description: "line\nline"},
			{LaneID: lanes[2].ID, Title: "three", Assignee: "ada@example.com"},
		} {
			if _, err := s.createCard(ctx, main.ID, c); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.renameLane(ctx, main.ID, lanes[1].ID, "Doing"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.createBoard(ctx, "Empty"); err != nil {
			t.Fatal(err)
		}

		var first bytes.Buffer
		if err := s.export(ctx, &first, ""); err != nil {
			t.Fatal(err)
		}
		snap, err := readSnapshot(bytes.NewReader(first.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.importSnapshot(ctx, snap, false); err == nil {
			t.Fatal("import over existing boards without -replace was accepted")
		}
		if err := s.importSnapshot(ctx, snap, true); err != nil {
			t.Fatal(err)
		}

		var second bytes.Buffer
		if err := s.export(ctx, &second, ""); err != nil {
			t.Fatal(err)
		}
		// Everything but the timestamp has to come back the same.
		strip := func(b []byte) string { return string(b[bytes.Index(b, []byte(`"boards"`)):]) }
		if strip(first.Bytes()) != strip(second.Bytes()) {
			t.Errorf("round trip changed the boards:\n%s\n---\n%s", first.String(), second.String())
		}

		var one bytes.Buffer
		if err := s.export(ctx, &one, "empty"); err != nil || strings.Contains(one.String(), `"main"`) {
			t.Errorf("export of one board: %v\n%s", err, one.String())
		}
		if err := s.export(ctx, &one, "nope"); !errors.Is(err, errNotFound) {
			t.Errorf("export of a missing board: %v", err)
		}
	})
}

func TestHandlersAgainstDatabase(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		a, err := newApp(s, "X-Forwarded-Email")
		if err != nil {
			t.Fatal(err)
		}
		as := func(req *http.Request) *httptest.ResponseRecorder {
			req.Header.Set("X-Forwarded-Email", "Ada@Example.com")
			return do(a, req)
		}

		rr := as(postForm("/boards", url.Values{"name": {"Second Board"}}))
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/b/second-board" {
			t.Fatalf("create board: %d %v", rr.Code, rr.Header())
		}
		_, lanes := mustBoard(t, s, "second-board")
		_, mainLanes := mustBoard(t, s, "main")

		req := postForm("/b/second-board/card", url.Values{"title": {"from htmx"}, "lane": {itoa(lanes[1].ID)}})
		req.Header.Set("HX-Request", "true")
		rr = as(req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "from htmx") ||
			rr.Header().Get("HX-Retarget") != "#lane-"+itoa(lanes[1].ID) {
			t.Fatalf("create card: %d %v %s", rr.Code, rr.Header(), rr.Body.String())
		}
		if rr := as(postForm("/b/second-board/card", url.Values{"title": {"x"}, "lane": {itoa(mainLanes[0].ID)}})); rr.Code != http.StatusNotFound {
			t.Errorf("a card into another board's column: %d", rr.Code)
		}
		if rr := as(httptest.NewRequest(http.MethodGet, "/b/second-board", nil)); !strings.Contains(rr.Body.String(), "from htmx") {
			t.Errorf("board page: %d", rr.Code)
		}
		if rr := as(httptest.NewRequest(http.MethodGet, "/", nil)); !strings.Contains(rr.Body.String(), "Second Board") {
			t.Errorf("board list: %d", rr.Code)
		}

		// The signed-in user was remembered and can take the card.
		card := lanesCards(t, s, "second-board")[0]
		rr = as(postForm("/card/"+itoa(card.ID)+"/update", url.Values{"title": {"from htmx"}, "assignee": {"ada@example.com"}}))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Assigned to ada@example.com") {
			t.Errorf("assign: %d %s", rr.Code, rr.Body.String())
		}
		if rr := as(postForm("/card/"+itoa(card.ID)+"/update", url.Values{"title": {"x"}, "assignee": {"eve@example.com"}})); rr.Code != http.StatusBadRequest {
			t.Errorf("assigning a stranger: %d", rr.Code)
		}

		// A subtask ticked from the card face, and a stale tick refused.
		if rr := as(postForm("/card/"+itoa(card.ID)+"/update", url.Values{"title": {"from htmx"}, "subtasks": {"0|one"}, "assignee": {"ada@example.com"}})); rr.Code != http.StatusOK {
			t.Fatalf("add subtask: %d", rr.Code)
		}
		rr = as(postForm("/card/"+itoa(card.ID)+"/subtask", url.Values{"index": {"0"}, "text": {"one"}, "done": {"1"}}))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "subtask done") {
			t.Errorf("tick: %d %s", rr.Code, rr.Body.String())
		}
		if rr := as(postForm("/card/"+itoa(card.ID)+"/subtask", url.Values{"index": {"0"}, "text": {"two"}, "done": {"1"}})); rr.Code != http.StatusConflict {
			t.Errorf("stale tick: %d", rr.Code)
		}
		if rr := as(postForm("/card/"+itoa(card.ID)+"/subtask", url.Values{"index": {"0"}, "text": {"one"}, "done": {"yes"}})); rr.Code != http.StatusBadRequest {
			t.Errorf("bad state: %d", rr.Code)
		}

		// Moved from the editor: the page is told to move the card.
		rr = as(postForm("/card/"+itoa(card.ID)+"/update", url.Values{"title": {"from htmx"}, "lane": {itoa(lanes[2].ID)}}))
		if want := `{"cardMoved":{"id":` + itoa(card.ID) + `,"lane":` + itoa(lanes[2].ID) + `}}`; rr.Code != http.StatusOK || rr.Header().Get("HX-Trigger-After-Swap") != want {
			t.Errorf("move from the editor: %d %q", rr.Code, rr.Header().Get("HX-Trigger-After-Swap"))
		}
		rr = as(postForm("/card/"+itoa(card.ID)+"/update", url.Values{"title": {"from htmx"}}))
		if rr.Header().Get("HX-Trigger-After-Swap") != "" {
			t.Error("a save without a column asked the page to move the card")
		}
		if rr := as(httptest.NewRequest(http.MethodGet, "/card/"+itoa(card.ID)+"/edit", nil)); !strings.Contains(rr.Body.String(), `<select name="lane">`) {
			t.Errorf("editor without a column picker: %d", rr.Code)
		}
		if rr := as(postForm("/card/"+itoa(card.ID)+"/update", url.Values{"title": {"from htmx"}, "lane": {itoa(lanes[1].ID)}})); rr.Code != http.StatusOK {
			t.Fatalf("move back: %d", rr.Code)
		}

		// The phone's arrow buttons.
		if rr := as(postForm("/card/"+itoa(card.ID)+"/move", url.Values{"lane": {itoa(lanes[0].ID)}})); rr.Code != http.StatusOK {
			t.Errorf("move: %d", rr.Code)
		}
		if rr := as(postForm("/card/"+itoa(card.ID)+"/move", url.Values{"lane": {itoa(mainLanes[0].ID)}})); rr.Code != http.StatusNotFound {
			t.Errorf("move to another board: %d", rr.Code)
		}
		if rr := as(postForm("/card/"+itoa(card.ID)+"/move", url.Values{"lane": {"left"}})); rr.Code != http.StatusBadRequest {
			t.Errorf("move without a column: %d", rr.Code)
		}
		if rr := as(postForm("/card/"+itoa(card.ID)+"/move", url.Values{"lane": {itoa(lanes[1].ID)}})); rr.Code != http.StatusOK {
			t.Fatalf("move back: %d", rr.Code)
		}

		// Archive, undo, and the archive page.
		if rr := as(postForm("/card/"+itoa(card.ID)+"/archive", nil)); rr.Code != http.StatusOK {
			t.Errorf("archive: %d", rr.Code)
		}
		if rr := as(httptest.NewRequest(http.MethodGet, "/b/second-board/archive", nil)); !strings.Contains(rr.Body.String(), "from htmx") {
			t.Errorf("archive page: %d", rr.Code)
		}
		rr = as(postForm("/card/"+itoa(card.ID)+"/restore", nil))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `id="card-`+itoa(card.ID)+`"`) {
			t.Errorf("undo answers the card: %d %s", rr.Code, rr.Body.String())
		}
		_ = as(postForm("/card/"+itoa(card.ID)+"/archive", nil))
		rr = as(postForm("/card/"+itoa(card.ID)+"/restore", url.Values{"next": {"/b/second-board/archive"}}))
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/b/second-board/archive" {
			t.Errorf("restore from the archive page: %d %v", rr.Code, rr.Header())
		}
		for _, next := range []string{"https://evil.example/", "//evil.example/", "/\\evil.example/"} {
			rr := as(postForm("/card/"+itoa(card.ID)+"/restore", url.Values{"next": {next}}))
			if rr.Code == http.StatusSeeOther {
				t.Errorf("followed next=%q to %s", next, rr.Header().Get("Location"))
			}
		}

		// A due date through the editor, and a bad one refused.
		if rr := as(postForm("/card/"+itoa(card.ID)+"/update", url.Values{"title": {"from htmx"}, "due": {"2026-02-30"}})); rr.Code != http.StatusBadRequest {
			t.Errorf("an impossible date: %d", rr.Code)
		}
		if rr := as(postForm("/card/"+itoa(card.ID)+"/update", url.Values{"title": {"from htmx"}, "due": {"2020-01-01"}})); !strings.Contains(rr.Body.String(), "due-overdue") {
			t.Errorf("an overdue card: %d %s", rr.Code, rr.Body.String())
		}

		// Search, on one board and on all.
		if rr := as(httptest.NewRequest(http.MethodGet, "/search?q=HTMX&board=second-board", nil)); !strings.Contains(rr.Body.String(), `href="/b/second-board#card-`+itoa(card.ID)+`"`) {
			t.Errorf("search: %d %s", rr.Code, rr.Body.String())
		}
		if rr := as(httptest.NewRequest(http.MethodGet, "/search?q=nothing+like+it", nil)); !strings.Contains(rr.Body.String(), "No card has all of those words") {
			t.Errorf("search without hits: %d", rr.Code)
		}
		if rr := as(httptest.NewRequest(http.MethodGet, "/search?q=x&board=nope", nil)); rr.Code != http.StatusNotFound {
			t.Errorf("search on a missing board: %d", rr.Code)
		}

		// Columns through the settings page.
		if rr := as(postForm("/b/second-board/columns", url.Values{"name": {"Waiting"}})); rr.Code != http.StatusSeeOther {
			t.Errorf("add column: %d", rr.Code)
		}
		if rr := as(postForm("/b/second-board/columns/"+itoa(lanes[1].ID)+"/delete", nil)); rr.Code != http.StatusBadRequest {
			t.Errorf("delete a column with cards: %d", rr.Code)
		}
		if rr := as(postForm("/b/second-board/columns/"+itoa(lanes[0].ID)+"/move", url.Values{"dir": {"right"}})); rr.Code != http.StatusSeeOther {
			t.Errorf("move column: %d", rr.Code)
		}
		if got := layout(t, s, "second-board"); got != "In Progress: from htmx | To Do:  | Done:  | Waiting: " {
			t.Errorf("columns after settings: %s", got)
		}
		if rr := as(httptest.NewRequest(http.MethodGet, "/b/second-board/columns", nil)); !strings.Contains(rr.Body.String(), "Waiting") {
			t.Errorf("columns page: %d", rr.Code)
		}

		if rr := as(postForm("/b/second-board/delete", url.Values{"confirm": {"second board"}})); rr.Code != http.StatusBadRequest {
			t.Errorf("delete with the wrong name: %d", rr.Code)
		}
		if rr := as(postForm("/b/second-board/delete", url.Values{"confirm": {"Second Board"}})); rr.Code != http.StatusSeeOther {
			t.Errorf("delete: %d", rr.Code)
		}
		if rr := as(httptest.NewRequest(http.MethodGet, "/b/second-board", nil)); rr.Code != http.StatusNotFound {
			t.Errorf("deleted board still there: %d", rr.Code)
		}
		if rr := do(a, httptest.NewRequest(http.MethodGet, "/readyz", nil)); rr.Code != http.StatusOK {
			t.Errorf("readyz: %d", rr.Code)
		}
	})
}

func lanesCards(t *testing.T, s *store, slug string) []Card {
	t.Helper()
	_, lanes := mustBoard(t, s, slug)
	var cards []Card
	for _, l := range lanes {
		cards = append(cards, l.Cards...)
	}
	return cards
}

func itoa(i int) string { return strconv.Itoa(i) }
