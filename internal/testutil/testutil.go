// Package testutil builds an in-memory-ish Env for op tests.
package testutil

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

var (
	tplOnce  sync.Once
	tplBytes []byte
	tplErr   error
)

func templateDB() ([]byte, error) {
	tplOnce.Do(func() {
		dir, err := os.MkdirTemp("", "weave-test-tpl-*")
		if err != nil {
			tplErr = err
			return
		}
		defer os.RemoveAll(dir)
		p := filepath.Join(dir, "tpl.db")
		d, err := db.Open(p)
		if err != nil {
			tplErr = err
			return
		}
		_, _ = d.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		if err := d.Close(); err != nil {
			tplErr = err
			return
		}
		tplBytes, tplErr = os.ReadFile(p)
	})
	return tplBytes, tplErr
}

// Env returns an Env on a fresh temp SQLite DB, with the given principal (nil = anonymous).
func Env(t testing.TB, u *ops.Principal) *ops.Env {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.db")
	if b, err := templateDB(); err == nil {
		if werr := os.WriteFile(p, b, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}
	d, err := db.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	t.Cleanup(func() { CheckPlacements(t, d) }) // runs before the Close above (cleanups are LIFO)
	return &ops.Env{Ctx: context.Background(), DB: d, User: u, Hub: realtime.New(), Svc: map[string]any{}}
}

// As returns a copy of e acting as another principal.
func As(e *ops.Env, u *ops.Principal) *ops.Env { c := *e; c.User = u; return &c }

// placementChecks are the card_placements invariants (migration 0404, docs/DESIGN.md "Placements"): each query
// returns the offending card ids. The mirror triggers and the guard keep most of them; the check catches a writer
// that bypasses both (a cleared home, a placement on another board's column, an extra placement of a draft).
var placementChecks = []struct{ what, q string }{
	{"more than one placement on one board", `SELECT card_id FROM card_placements GROUP BY card_id, board_id HAVING COUNT(*)>1`},
	{"not exactly one home placement", `SELECT c.id FROM cards c WHERE (SELECT COUNT(*) FROM card_placements p WHERE p.card_id=c.id AND p.is_home=1)<>1`},
	{"cards columns differ from the home placement", `SELECT c.id FROM cards c JOIN card_placements p ON p.card_id=c.id AND p.is_home=1
		WHERE p.board_id<>c.board_id OR p.list_id<>c.list_id OR p.position IS NOT c.position OR p.prev_list_id IS NOT c.prev_list_id
		OR p.list_changed_at<>c.list_changed_at`},
	{"placement column belongs to another board", `SELECT p.card_id FROM card_placements p JOIN lists l ON l.id=p.list_id WHERE l.board_id<>p.board_id`},
	{"draft or card template placed on more than one board", `SELECT c.id FROM cards c WHERE (c.is_draft=1 OR c.is_template=1)
		AND (SELECT COUNT(*) FROM card_placements p WHERE p.card_id=c.id)>1`},
}

// CheckPlacements fails the test when the database breaks a card_placements invariant: at most one placement per
// (card, board), exactly one home per card, the home equal to the cards columns, each placement's column on the
// placement's board, drafts and card templates never placed twice. Env runs it when the test ends (skipped when the
// test already failed); tests that open a database themselves call it explicitly.
func CheckPlacements(t testing.TB, d *db.DB) {
	t.Helper()
	if t.Failed() {
		return
	}
	for _, c := range placementChecks {
		ids, err := queryIDs(d.DB, c.q)
		if err != nil {
			t.Errorf("CheckPlacements (%s): %v", c.what, err)
			continue
		}
		if len(ids) > 0 {
			t.Errorf("CheckPlacements: %s: %d card(s), e.g. %v", c.what, len(ids), ids[:min(len(ids), 5)])
		}
	}
}

func queryIDs(d *sql.DB, q string) ([]string, error) {
	rows, err := d.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Place adds an extra (non-home) placement of cardID on boardID's column listID at position, written by SQL: the
// tests of the multi-board read paths need cards on several boards before the ops that place cards exist. placed_at
// is the current time, so later placements sort after earlier ones.
func Place(t testing.TB, d *db.DB, cardID, boardID, listID string, position float64) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO card_placements(card_id,board_id,list_id,position,list_changed_at,is_home,placed_at) VALUES(?,?,?,?,?,0,?)`,
		cardID, boardID, listID, position, db.Now(), db.Now()); err != nil {
		t.Fatalf("place %s on %s: %v", cardID, boardID, err)
	}
}
