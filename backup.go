package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// snapshotVersion is the format of the export file. Bump it on any change an
// older import could misread.
const snapshotVersion = 1

// snapshot is the export file: the same JSON whichever backend wrote it, so
// it also moves boards between SQLite and PostgreSQL. Ids are not kept, the
// importing database hands out its own; order is the order in the file.
type snapshot struct {
	Version    int             `json:"version"`
	ExportedAt time.Time       `json:"exported_at"`
	Boards     []boardSnapshot `json:"boards"`
}

type boardSnapshot struct {
	Slug    string         `json:"slug"`
	Name    string         `json:"name"`
	Columns []laneSnapshot `json:"columns"`
}

type laneSnapshot struct {
	Name  string         `json:"name"`
	Cards []cardSnapshot `json:"cards"`
}

type cardSnapshot struct {
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Subtasks    []Subtask `json:"subtasks,omitempty"`
	Assignee    string    `json:"assignee,omitempty"`
	Due         string    `json:"due,omitempty"`
	ArchivedAt  int64     `json:"archived_at,omitempty"` // unix seconds
}

// export writes every board, or only the one with the given slug, as JSON.
func (s *store) export(ctx context.Context, w io.Writer, only string) error {
	// Every query sees the same committed state, even as lanes/cards move.
	opts := &sql.TxOptions{ReadOnly: true}
	if s.dialect == "postgres" {
		opts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	boards, err := listBoards(ctx, tx)
	if err != nil {
		return err
	}
	snap := snapshot{Version: snapshotVersion, ExportedAt: time.Now().UTC(), Boards: []boardSnapshot{}}
	for _, b := range boards {
		if only != "" && b.Slug != only {
			continue
		}
		lanes, err := boardLanes(ctx, tx, b.ID, true)
		if err != nil {
			return err
		}
		bs := boardSnapshot{Slug: b.Slug, Name: b.Name, Columns: []laneSnapshot{}}
		for _, l := range lanes {
			ls := laneSnapshot{Name: l.Name, Cards: []cardSnapshot{}}
			for _, c := range l.Cards {
				ls.Cards = append(ls.Cards, cardSnapshot{
					Title:       c.Title,
					Description: c.Description,
					Subtasks:    c.SubtaskList(),
					Assignee:    c.Assignee,
					Due:         c.Due,
					ArchivedAt:  c.ArchivedAt,
				})
			}
			bs.Columns = append(bs.Columns, ls)
		}
		snap.Boards = append(snap.Boards, bs)
	}
	if only != "" && len(snap.Boards) == 0 {
		return fmt.Errorf("board %q: %w", only, errNotFound)
	}
	// Release the transaction before writing to a potentially slow output.
	if err := tx.Commit(); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}

// readSnapshot parses and checks an export file completely before anything
// is written, so a bad file is refused as a whole. Assignees are normalised
// in place.
func readSnapshot(r io.Reader) (snapshot, error) {
	var snap snapshot
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return snap, fmt.Errorf("reading snapshot: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return snap, fmt.Errorf("reading snapshot: trailing data (expected EOF): %v", err)
	}
	if snap.Version != snapshotVersion {
		return snap, fmt.Errorf("snapshot version %d, this build reads version %d", snap.Version, snapshotVersion)
	}
	seen := map[string]bool{}
	for i, b := range snap.Boards {
		if !validSlug(b.Slug) {
			return snap, fmt.Errorf("board %d: invalid slug %q", i+1, b.Slug)
		}
		if seen[b.Slug] {
			return snap, fmt.Errorf("board %q appears twice", b.Slug)
		}
		seen[b.Slug] = true
		if _, err := cleanName("board", b.Name, maxBoardName); err != nil {
			return snap, fmt.Errorf("board %q: %w", b.Slug, err)
		}
		if len(b.Columns) == 0 || len(b.Columns) > maxLanes {
			return snap, fmt.Errorf("board %q: a board has 1 to %d columns", b.Slug, maxLanes)
		}
		for _, l := range b.Columns {
			if _, err := cleanName("column", l.Name, maxLaneName); err != nil {
				return snap, fmt.Errorf("board %q: %w", b.Slug, err)
			}
			for k, c := range l.Cards {
				// Empty cards are valid backups of legacy rows, though the UI
				// refuses to create them. Never make old data unrestoreable.
				due, err := cleanDue(c.Due)
				if err != nil || c.ArchivedAt < 0 {
					return snap, fmt.Errorf("board %q column %q card %d: invalid due date or archive time", b.Slug, l.Name, k+1)
				}
				l.Cards[k].Due = due
				if c.Assignee != "" {
					email, err := cleanEmail(c.Assignee)
					if err != nil {
						return snap, fmt.Errorf("board %q column %q card %d: %w", b.Slug, l.Name, k+1, err)
					}
					l.Cards[k].Assignee = email
				}
			}
		}
	}
	return snap, nil
}

// importSnapshot writes the boards of snap in one transaction. A board whose
// slug already exists is refused, unless replace is set: then it is deleted
// with its cards first. Boards not in the file are left alone. Assignees are
// added to the people that can be assigned.
func (s *store) importSnapshot(ctx context.Context, snap snapshot, replace bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, bs := range snap.Boards {
			existing, err := boardBySlug(ctx, tx, bs.Slug)
			switch {
			case errors.Is(err, errNotFound):
			case err != nil:
				return err
			case !replace:
				return fmt.Errorf("board %q already exists; import with -replace to overwrite it", bs.Slug)
			default:
				if err := s.deleteBoardRows(ctx, tx, existing.ID); err != nil {
					return err
				}
			}
			b := Board{Slug: bs.Slug, Name: bs.Name}
			if err := insertBoard(ctx, tx, &b); err != nil {
				return err
			}
			for _, ls := range bs.Columns {
				laneID, err := insertLane(ctx, tx, b.ID, ls.Name)
				if err != nil {
					return err
				}
				for _, cs := range ls.Cards {
					if cs.Assignee != "" {
						if err := rememberUser(ctx, tx, cs.Assignee); err != nil {
							return err
						}
					}
					c := Card{
						LaneID:      laneID,
						Title:       cs.Title,
						Description: cs.Description,
						Subtasks:    formatSubtasks(cs.Subtasks),
						Assignee:    cs.Assignee,
						Due:         cs.Due,
						ArchivedAt:  cs.ArchivedAt,
					}
					if err := insertCard(ctx, tx, &c); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}
