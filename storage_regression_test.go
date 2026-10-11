package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStorageBackupEdges(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		b, lanes := mustBoard(t, s, "main")
		// Legacy installations can contain genuinely empty cards; backups must preserve them.
		if _, err := s.createCard(ctx, b.ID, Card{LaneID: lanes[0].ID}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := s.export(ctx, &out, ""); err != nil {
			t.Fatal(err)
		}
		snap, err := readSnapshot(&out)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.importSnapshot(ctx, snap, true); err != nil {
			t.Fatal(err)
		}
		if cards := lanesCards(t, s, "main"); len(cards) != 1 || !isEmpty(cards[0]) {
			t.Fatalf("legacy card lost: %+v", cards)
		}
	})
}

func TestStorageLongSlugRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx := context.Background()
		for range 2 {
			if _, err := s.createBoard(ctx, strings.Repeat("a", 50)); err != nil {
				t.Fatal(err)
			}
		}
		var out bytes.Buffer
		if err := s.export(ctx, &out, ""); err != nil {
			t.Fatal(err)
		}
		snap, err := readSnapshot(&out)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.importSnapshot(ctx, snap, true); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStorageFlagOnlyUI(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		a, err := newApp(s, "")
		if err != nil {
			t.Fatal(err)
		}
		b, lanes := mustBoard(t, s, "main")
		c, err := s.createCard(context.Background(), b.ID, Card{LaneID: lanes[0].ID, Title: "kept"})
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{"0|", "1| \n0|"} {
			for _, path := range []string{"/b/main/card", "/card/" + itoa(c.ID) + "/update"} {
				rr := do(a, postForm(path, url.Values{"lane": {itoa(lanes[0].ID)}, "subtasks": {raw}}))
				if rr.Code != http.StatusBadRequest {
					t.Errorf("%s subtasks %q: %d", path, raw, rr.Code)
				}
			}
		}
		got, err := s.getCard(context.Background(), c.ID)
		if err != nil || got.Title != "kept" {
			t.Fatalf("rejected edit changed card: %+v %v", got, err)
		}
	})
}

func TestStorageSnapshotDueNormalization(t *testing.T) {
	snap, err := readSnapshot(strings.NewReader(`{"version":1,"boards":[{"slug":"main","name":"Main","columns":[{"name":"To Do","cards":[{"title":"Dated","due":" 2026-10-12 "}]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Boards[0].Columns[0].Cards[0].Due; got != "2026-10-12" {
		t.Fatalf("validated due date is not canonical: %q", got)
	}
}

func TestStorageTrailingJSON(t *testing.T) {
	for _, suffix := range []string{" garbage", ` {"version":2}`, " null"} {
		if _, err := readSnapshot(strings.NewReader(`{"version":1,"boards":[]}` + suffix)); err == nil {
			t.Errorf("accepted %q", suffix)
		}
	}
	if _, err := readSnapshot(strings.NewReader("{\"version\":1,\"boards\":[]} \n\t")); err != nil {
		t.Fatal(err)
	}
}

// Wait for database-observed blocking, never scheduler timing, before releasing a held row.
func waitStorageLocks(t *testing.T, s *store, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		err := s.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operations did not reach %d database locks", n)
}

func storageBlock(t *testing.T, s *store, query string, id int) *sql.Tx {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	var found int
	if err := tx.QueryRow(query, id).Scan(&found); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestStorageConcurrentLaneDelete(t *testing.T) {
	for _, incoming := range []string{"create", "move"} {
		t.Run(incoming, func(t *testing.T) {
			s := openTestPostgres(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.migrate(ctx); err != nil {
				t.Fatal(err)
			}
			b, lanes := mustBoard(t, s, "main")
			c, err := s.createCard(ctx, b.ID, Card{LaneID: lanes[0].ID, Title: "survive"})
			if err != nil {
				t.Fatal(err)
			}
			blocker := storageBlock(t, s, "SELECT id FROM lanes WHERE id=$1 FOR UPDATE", lanes[1].ID)
			result := make(chan error, 1)
			go func() { result <- s.deleteLane(ctx, b.ID, lanes[1].ID) }()
			waitStorageLocks(t, s, 1)
			// Commit a card into the target while deletion is waiting on its lane lock.
			// Raw SQL makes the final insert/update and FK lock interleaving exact.
			if incoming == "create" {
				_, err = blocker.Exec("INSERT INTO cards (lane_id,title) VALUES ($1,'survive')", lanes[1].ID)
			} else {
				_, err = blocker.Exec("UPDATE cards SET lane_id=$1 WHERE id=$2", lanes[1].ID, c.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, errInvalid) {
				t.Fatalf("delete accepted a concurrently populated lane: %v", err)
			}
			var count int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM cards WHERE lane_id=$1", lanes[1].ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("card lost: %d %v", count, err)
			}
		})
	}
}

func TestStorageDestinationDeletedWhileWaiting(t *testing.T) {
	for _, operation := range []string{"create", "move", "edit", "reorder"} {
		t.Run(operation, func(t *testing.T) {
			s := openTestPostgres(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.migrate(ctx); err != nil {
				t.Fatal(err)
			}
			b, lanes := mustBoard(t, s, "main")
			c, err := s.createCard(ctx, b.ID, Card{LaneID: lanes[0].ID, Title: "kept"})
			if err != nil {
				t.Fatal(err)
			}
			blocker := storageBlock(t, s, "SELECT id FROM lanes WHERE id=$1 FOR UPDATE", lanes[1].ID)
			result := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "create":
					_, err = s.createCard(ctx, b.ID, Card{LaneID: lanes[1].ID, Title: "new"})
				case "move":
					_, err = s.moveCardTo(ctx, c.ID, lanes[1].ID)
				case "edit":
					_, err = s.updateCard(ctx, c.ID, cardEdit{Title: "changed", LaneID: lanes[1].ID})
				case "reorder":
					err = s.reorderLane(ctx, b.ID, lanes[1].ID, []int{c.ID})
				}
				result <- err
			}()
			waitStorageLocks(t, s, 1)
			if _, err := blocker.Exec("DELETE FROM lanes WHERE id=$1", lanes[1].ID); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, errNotFound) {
				t.Fatalf("missing destination: %v", err)
			}
			got, err := s.getCard(ctx, c.ID)
			if err != nil || got.Title != "kept" || got.LaneID != lanes[0].ID {
				t.Fatalf("failed move changed card: %+v %v", got, err)
			}
		})
	}
}

func TestStorageConcurrentLaneLimits(t *testing.T) {
	for _, operation := range []string{"delete", "add"} {
		t.Run(operation, func(t *testing.T) {
			s := openTestPostgres(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.migrate(ctx); err != nil {
				t.Fatal(err)
			}
			b, lanes := mustBoard(t, s, "main")
			var blocker *sql.Tx
			if operation == "delete" {
				if err := s.deleteLane(ctx, b.ID, lanes[2].ID); err != nil {
					t.Fatal(err)
				}
				blocker = storageBlock(t, s, "SELECT id FROM lanes WHERE id=$1 FOR UPDATE", lanes[0].ID)
				var id int
				if err := blocker.QueryRow("SELECT id FROM lanes WHERE id=$1 FOR UPDATE", lanes[1].ID).Scan(&id); err != nil {
					t.Fatal(err)
				}
			} else {
				for range maxLanes - 4 {
					if err := s.addLane(ctx, b.ID, "extra"); err != nil {
						t.Fatal(err)
					}
				}
				blocker = storageBlock(t, s, "SELECT id FROM boards WHERE id=$1 FOR UPDATE", b.ID)
			}
			results := make(chan error, 2)
			for i := range 2 {
				go func() {
					if operation == "delete" {
						results <- s.deleteLane(ctx, b.ID, lanes[i].ID)
					} else {
						results <- s.addLane(ctx, b.ID, "last")
					}
				}()
			}
			waitStorageLocks(t, s, 2)
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			successes, rejected := 0, 0
			for range 2 {
				err := <-results
				if err == nil {
					successes++
				} else if errors.Is(err, errInvalid) {
					rejected++
				} else {
					t.Fatal(err)
				}
			}
			if successes != 1 || rejected != 1 {
				t.Fatalf("successes=%d rejected=%d", successes, rejected)
			}
			got, err := s.listLanes(ctx, b.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if operation == "add" {
				want = maxLanes
			}
			if len(got) != want {
				t.Fatalf("lanes=%d want=%d", len(got), want)
			}
		})
	}
}

func TestStorageConcurrentCardRMW(t *testing.T) {
	for _, operation := range []string{"ticks", "edit", "move"} {
		t.Run(operation, func(t *testing.T) {
			s := openTestPostgres(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.migrate(ctx); err != nil {
				t.Fatal(err)
			}
			b, lanes := mustBoard(t, s, "main")
			c, err := s.createCard(ctx, b.ID, Card{LaneID: lanes[0].ID, Title: "before", Subtasks: "0|one\n0|two"})
			if err != nil {
				t.Fatal(err)
			}
			blocker := storageBlock(t, s, "SELECT id FROM cards WHERE id=$1 FOR UPDATE", c.ID)
			results := make(chan error, 2)
			go func() { _, err := s.setSubtask(ctx, c.ID, 0, "one", true); results <- err }()
			if operation == "ticks" {
				go func() { _, err := s.setSubtask(ctx, c.ID, 1, "two", true); results <- err }()
				waitStorageLocks(t, s, 2)
			} else {
				// A prior committed edit must be visible to the blocked read-modify-write.
				if operation == "edit" {
					go func() {
						_, err := s.updateCard(ctx, c.ID, cardEdit{Title: "after", Subtasks: c.Subtasks})
						results <- err
					}()
				} else {
					go func() { _, err := s.moveCardTo(ctx, c.ID, lanes[0].ID); results <- err }()
				}
				waitStorageLocks(t, s, 2)
				if _, err := blocker.Exec("UPDATE cards SET assignee='ada@example.com', lane_id=$1 WHERE id=$2", lanes[1].ID, c.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.getCard(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "ticks" && got.Subtasks != "1|one\n1|two" {
				t.Fatalf("lost tick: %q", got.Subtasks)
			}
			wantLane := lanes[1].ID
			if operation == "move" {
				wantLane = lanes[0].ID
			}
			if operation != "ticks" && (got.Assignee != "ada@example.com" || got.LaneID != wantLane) {
				t.Fatalf("stale fields overwritten: %+v", got)
			}
		})
	}
}

func TestStorageCardLaneLockOrder(t *testing.T) {
	for _, operation := range []string{"reorder", "deleteBoard"} {
		t.Run(operation, func(t *testing.T) {
			s := openTestPostgres(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.migrate(ctx); err != nil {
				t.Fatal(err)
			}
			b, lanes := mustBoard(t, s, "main")
			c, err := s.createCard(ctx, b.ID, Card{LaneID: lanes[0].ID, Title: "kept"})
			if err != nil {
				t.Fatal(err)
			}
			blocker := storageBlock(t, s, "SELECT id FROM lanes WHERE id=$1 FOR UPDATE", lanes[1].ID)
			results := make(chan error, 2)
			go func() { _, err := s.moveCardTo(ctx, c.ID, lanes[1].ID); results <- err }()
			waitStorageLocks(t, s, 1)
			go func() {
				if operation == "reorder" {
					results <- s.reorderLane(ctx, b.ID, lanes[1].ID, []int{c.ID})
				} else {
					results <- s.deleteBoard(ctx, b.ID)
				}
			}()
			waitStorageLocks(t, s, 2)
			var cardWaiters int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id FROM cards WHERE lane_id%'`).Scan(&cardWaiters); err != nil {
				t.Fatal(err)
			}
			if cardWaiters != 1 {
				t.Fatalf("%s must lock cards before lanes; card waiters=%d", operation, cardWaiters)
			}
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Wrap the existing driver only to pause export between its lane and card reads.
// Preserve BeginTx so PostgreSQL's requested isolation reaches the driver.
var storageDriverID atomic.Int64

type storageExportDriver struct {
	driver.Driver
	pause func(string)
}
type storageExportConn struct {
	driver.Conn
	pause func(string)
}

func (d storageExportDriver) Open(dsn string) (driver.Conn, error) {
	c, err := d.Driver.Open(dsn)
	return storageExportConn{c, d.pause}, err
}
func (c storageExportConn) Prepare(query string) (driver.Stmt, error) {
	c.pause(query)
	return c.Conn.Prepare(query)
}
func (c storageExportConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c, ok := c.Conn.(driver.ConnBeginTx); ok {
		return c.BeginTx(ctx, opts)
	}
	return nil, errors.New("test driver does not support BeginTx")
}

func TestStorageConsistentExport(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *store) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b, lanes := mustBoard(t, s, "main")
		c, err := s.createCard(ctx, b.ID, Card{LaneID: lanes[0].ID, Title: "must survive backup"})
		if err != nil {
			t.Fatal(err)
		}
		reached, proceed := make(chan struct{}), make(chan struct{})
		driverName := fmt.Sprintf("storage-export-%d", storageDriverID.Add(1))
		sql.Register(driverName, storageExportDriver{s.db.Driver(), func(query string) {
			if strings.Contains(query, "SELECT "+cardColumns) && strings.Contains(query, "WHERE lane_id IN") {
				close(reached)
				select {
				case <-proceed:
				case <-ctx.Done():
				}
			}
		}})
		dsn := os.Getenv("KANBAN_TEST_POSTGRES_URL")
		if s.dialect == "sqlite" {
			var seq int
			var name, file string
			if err := s.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &file); err != nil {
				t.Fatal(err)
			}
			dsn = sqliteDSN(file)
		}
		db, err := sql.Open(driverName, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		exporter := &store{db: db, dialect: s.dialect}
		var out bytes.Buffer
		result := make(chan error, 1)
		go func() { result <- exporter.export(ctx, &out, "") }()
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := s.addLane(ctx, b.ID, "New"); err != nil {
			close(proceed)
			t.Fatal(err)
		}
		updated, err := s.listLanes(ctx, b.ID)
		if err != nil {
			close(proceed)
			t.Fatal(err)
		}
		if _, err := s.moveCardTo(ctx, c.ID, updated[len(updated)-1].ID); err != nil {
			close(proceed)
			t.Fatal(err)
		}
		close(proceed)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		snap, err := readSnapshot(&out)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, b := range snap.Boards {
			for _, l := range b.Columns {
				for _, c := range l.Cards {
					if c.Title == "must survive backup" {
						count++
					}
				}
			}
		}
		if count != 1 || len(snap.Boards[0].Columns) != 3 {
			t.Fatalf("incoherent backup: %+v", snap)
		}
	})
}
