package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// 0404 card_placements: these tests prove the trigger design under the real driver (modernc SQLite) with the
// connection settings db.Open uses (foreign_keys on, recursive_triggers left at its default).

const placementMig = "0404_card_placements.sql"

func mustX(t *testing.T, d *DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// mirrorDrift counts cards whose mirrored columns differ from their home placement (or that have no home).
func mirrorDrift(t *testing.T, d *DB) int {
	t.Helper()
	return count(t, d, `SELECT COUNT(*) FROM cards c LEFT JOIN card_placements p ON p.card_id=c.id AND p.is_home=1
		WHERE p.card_id IS NULL OR p.board_id<>c.board_id OR p.list_id<>c.list_id OR p.position IS NOT c.position
		OR p.prev_list_id IS NOT c.prev_list_id OR p.list_changed_at<>c.list_changed_at`)
}

// seedBoards creates two projects with a board each (b1: la, lb, lx archive; b2: l2) and a user.
func seedBoards(t *testing.T, d *DB) {
	t.Helper()
	now := Now()
	for _, q := range []string{
		`INSERT INTO users(id,email,name,role,created_at) VALUES('u1','u1@example.com','U1','boardUser','` + now + `')`,
		`INSERT INTO projects(id,name,created_at) VALUES('p1','P1','` + now + `'),('p2','P2','` + now + `')`,
		`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'B1','` + now + `'),('b2','p2',1,'B2','` + now + `')`,
		`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('la','b1','active',1,'Todo','` + now + `'),
			('lb','b1','active',2,'Doing','` + now + `'),('lx','b1','archive',NULL,NULL,'` + now + `'),('l2','b2','active',1,'Inbox','` + now + `')`,
	} {
		mustX(t, d, q)
	}
}

func TestPlacementsUpgradeBackfill(t *testing.T) {
	d := openWithout(t, map[string]bool{placementMig: true})
	if count(t, d, `SELECT COUNT(*) FROM sqlite_master WHERE name='card_placements'`) != 0 {
		t.Fatal("the old schema should not have card_placements yet")
	}
	seedBoards(t, d)
	old := "2020-01-01T00:00:00Z"
	mustX(t, d, `INSERT INTO cards(id,board_id,list_id,creator_user_id,position,prev_list_id,name,is_closed,is_draft,is_template,list_changed_at,created_at,updated_at) VALUES
		('c1','b1','la','u1',65536,NULL,'one',0,0,0,'2021-01-01T00:00:00Z','2020-06-01T00:00:00Z','`+old+`'),
		('c2','b1','la',NULL,131072,NULL,'two',1,0,0,'2021-01-02T00:00:00Z','2020-06-02T00:00:00Z','`+old+`'),
		('c3','b1','lx','u1',65536,'lb','archived',0,0,0,'2021-01-03T00:00:00Z','2020-06-03T00:00:00Z','`+old+`'),
		('c4','b1','la','u1',NULL,NULL,'no position',0,0,0,'2021-01-04T00:00:00Z','2020-06-04T00:00:00Z','`+old+`'),
		('d1','b1','la','u1',196608,NULL,'draft',0,1,0,'2021-01-05T00:00:00Z','2020-06-05T00:00:00Z','`+old+`'),
		('t1','b2','l2','u1',65536,NULL,'template',0,0,1,'2021-01-06T00:00:00Z','2020-06-06T00:00:00Z','`+old+`'),
		('c5','b2','l2','u1',131072,NULL,'other board',0,0,0,'2021-01-07T00:00:00Z','2020-06-07T00:00:00Z','`+old+`')`)
	fts := count(t, d, `SELECT COUNT(*) FROM search_map`)
	if err := d.migrate(); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM card_placements`); n != 7 {
		t.Fatalf("placements = %d, want one per card (7)", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM cards c WHERE (SELECT COUNT(*) FROM card_placements p WHERE p.card_id=c.id AND p.is_home=1)<>1`); n != 0 {
		t.Fatalf("%d cards without exactly one home placement", n)
	}
	if n := mirrorDrift(t, d); n != 0 {
		t.Fatalf("%d cards differ from their home placement", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM card_placements p JOIN cards c ON c.id=p.card_id WHERE p.placed_at=c.created_at AND p.placed_by IS c.creator_user_id`); n != 7 {
		t.Fatalf("placed_at/placed_by should come from the card's creation (%d of 7)", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM cards WHERE updated_at<>?`, old); n != 0 {
		t.Fatalf("the backfill must not touch cards (%d rows changed)", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM search_map`); n != fts {
		t.Fatalf("the backfill must not touch the search index (%d -> %d)", fts, n)
	}
	if err := d.migrate(); err != nil { // applied once
		t.Fatal(err)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM card_placements`); n != 7 {
		t.Fatalf("a second migrate changed the placements (%d)", n)
	}
	// an upgraded database behaves like a new one
	mustX(t, d, `UPDATE card_placements SET list_id='lb', list_changed_at=? WHERE card_id='c1' AND board_id='b1'`, Now())
	if n := count(t, d, `SELECT COUNT(*) FROM cards WHERE id='c1' AND list_id='lb'`); n != 1 {
		t.Fatal("the mirror does not run on an upgraded database")
	}
	if _, err := d.Exec(`UPDATE cards SET list_id='la' WHERE id='c1'`); err == nil {
		t.Fatal("the guard does not run on an upgraded database")
	}
}

func TestPlacementTriggers(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	// The design relies on nested triggers (placement UPDATE -> mirror UPDATE on cards -> guard + FTS triggers), which
	// SQLite runs whatever recursive_triggers says (that pragma only stops a trigger re-firing itself). Pin the default.
	if n := count(t, d, `PRAGMA recursive_triggers`); n != 0 {
		t.Fatalf("recursive_triggers = %d; the design is proven under the default (0)", n)
	}
	seedBoards(t, d)
	now := Now()
	mustX(t, d, `INSERT INTO cards(id,board_id,list_id,creator_user_id,position,name,description,list_changed_at,created_at) VALUES
		('c1','b1','la','u1',65536,'Alpha','first','`+now+`','`+now+`'),('c2','b1','la','u1',131072,'Beta',NULL,'`+now+`','`+now+`')`)
	mustX(t, d, `INSERT INTO comments(id,card_id,user_id,text,created_at) VALUES('m1','c1','u1','hello there','`+now+`')`)

	t.Run("insert creates the home placement", func(t *testing.T) {
		if n := count(t, d, `SELECT COUNT(*) FROM card_placements WHERE card_id='c1' AND board_id='b1' AND list_id='la' AND position=65536 AND is_home=1 AND placed_by='u1' AND placed_at=?`, now); n != 1 {
			t.Fatal("no home placement for an inserted card")
		}
		if n := mirrorDrift(t, d); n != 0 {
			t.Fatalf("drift %d", n)
		}
	})

	t.Run("direct writes to mirrored columns are rejected", func(t *testing.T) {
		for _, q := range []string{
			`UPDATE cards SET list_id='lb' WHERE id='c1'`,
			`UPDATE cards SET board_id='b2' WHERE id='c1'`,
			`UPDATE cards SET position=1 WHERE id='c1'`,
			`UPDATE cards SET prev_list_id='lb' WHERE id='c1'`,
			`UPDATE cards SET list_changed_at='2001-01-01T00:00:00Z' WHERE id='c1'`,
			`UPDATE cards SET list_id='lb',position=5,updated_at='x' WHERE id='c1'`,
		} {
			_, err := d.Exec(q)
			if err == nil || !strings.Contains(err.Error(), "write the placement") {
				t.Errorf("%s: err = %v, want the guard", q, err)
			}
		}
		if n := count(t, d, `SELECT COUNT(*) FROM cards WHERE id='c1' AND list_id='la' AND position=65536 AND updated_at IS NULL`); n != 1 {
			t.Fatal("a rejected write changed the card")
		}
		// other columns, and a mirrored column set to the value it already has, are fine
		mustX(t, d, `UPDATE cards SET name='Alpha', is_closed=1, updated_at=? WHERE id='c1'`, now)
		mustX(t, d, `UPDATE cards SET is_closed=0, list_id='la' WHERE id='c1'`)
	})

	// Every reindex by notify_card_au deletes the card's search_map row first; log those deletes (a rowid
	// comparison alone is weak: FTS5 reuses the highest rowid after a delete).
	mustX(t, d, `CREATE TABLE fts_churn(ref_id TEXT)`)
	mustX(t, d, `CREATE TRIGGER fts_churn_log AFTER DELETE ON search_map BEGIN INSERT INTO fts_churn VALUES(OLD.ref_id); END`)
	churn := func(t *testing.T) int {
		t.Helper()
		n := count(t, d, `SELECT COUNT(*) FROM fts_churn`)
		mustX(t, d, `DELETE FROM fts_churn`)
		return n
	}

	t.Run("reorder, column move and rebalance mirror without reindexing", func(t *testing.T) {
		churn(t)
		mustX(t, d, `UPDATE card_placements SET position=200000 WHERE card_id='c1' AND board_id='b1'`) // reorder
		mustX(t, d, `UPDATE card_placements SET list_id='lb', prev_list_id='la', list_changed_at='2030-01-01T00:00:00Z', position=7 WHERE card_id='c2' AND board_id='b1'`)
		// a rebalance / list.sort style pass over every placement of a column
		mustX(t, d, `UPDATE card_placements SET position=position*2 WHERE list_id IN ('la','lb')`)
		// a writer that names an unchanged board_id is still a plain mirror (no reindex)
		mustX(t, d, `UPDATE card_placements SET board_id='b1', position=9 WHERE card_id='c1' AND board_id='b1'`)
		if n := mirrorDrift(t, d); n != 0 {
			t.Fatalf("drift %d", n)
		}
		if n := count(t, d, `SELECT COUNT(*) FROM cards WHERE id='c2' AND list_id='lb' AND prev_list_id='la' AND list_changed_at='2030-01-01T00:00:00Z' AND position=14`); n != 1 {
			t.Fatal("the column move was not mirrored")
		}
		if n := churn(t); n != 0 {
			t.Fatalf("a same-board placement write reindexed the search index %d times (notify_card_au fired)", n)
		}
	})

	t.Run("cross-board relocation of the home re-homes and reindexes", func(t *testing.T) {
		churn(t)
		mustX(t, d, `UPDATE card_placements SET board_id='b2', list_id='l2', position=1, prev_list_id=NULL, list_changed_at=? WHERE card_id='c1' AND board_id='b1'`, now)
		if n := count(t, d, `SELECT COUNT(*) FROM cards WHERE id='c1' AND board_id='b2' AND list_id='l2' AND position=1 AND prev_list_id IS NULL`); n != 1 {
			t.Fatal("the cross-board move was not mirrored")
		}
		if n := mirrorDrift(t, d); n != 0 {
			t.Fatalf("drift %d", n)
		}
		if n := churn(t); n != 1 {
			t.Fatalf("moving the home to another board must reindex the card once (%d)", n)
		}
		if n := count(t, d, `SELECT COUNT(*) FROM search_idx WHERE rowid IN (SELECT rid FROM search_map WHERE ref_id IN ('c1','m1')) AND board_id='b2'`); n != 2 {
			t.Fatalf("the card and its comment should be indexed under the new board (%d of 2)", n)
		}
	})

	t.Run("re-homing to another placement", func(t *testing.T) {
		// c2 lives on b1 (home) and gets a second placement on b2
		mustX(t, d, `INSERT INTO card_placements(card_id,board_id,list_id,position,list_changed_at,is_home,placed_by,placed_at) VALUES('c2','b2','l2',5,?,0,'u1',?)`, now, now)
		if n := mirrorDrift(t, d); n != 0 {
			t.Fatalf("an extra placement must not touch the mirror (drift %d)", n)
		}
		if _, err := d.Exec(`INSERT INTO card_placements(card_id,board_id,list_id,position,list_changed_at,is_home,placed_at) VALUES('c2','b1','la',5,?,0,?)`, now, now); err == nil {
			t.Fatal("a card must not be placed twice on one board")
		}
		if _, err := d.Exec(`UPDATE card_placements SET is_home=1 WHERE card_id='c2' AND board_id='b2'`); err == nil {
			t.Fatal("two homes must be refused (cp_home)")
		}
		// a non-home placement moving never touches cards
		mustX(t, d, `UPDATE card_placements SET position=6 WHERE card_id='c2' AND board_id='b2'`)
		if n := count(t, d, `SELECT COUNT(*) FROM cards WHERE id='c2' AND board_id='b1' AND list_id='lb'`); n != 1 {
			t.Fatal("a non-home placement changed the mirror")
		}
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			`UPDATE card_placements SET is_home=0 WHERE card_id='c2' AND is_home=1`,
			`UPDATE card_placements SET is_home=1 WHERE card_id='c2' AND board_id='b2'`,
		} {
			if _, err := tx.Exec(q); err != nil {
				tx.Rollback()
				t.Fatalf("%s: %v", q, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if n := count(t, d, `SELECT COUNT(*) FROM cards WHERE id='c2' AND board_id='b2' AND list_id='l2' AND position=6`); n != 1 {
			t.Fatal("re-homing was not mirrored")
		}
		if n := mirrorDrift(t, d); n != 0 {
			t.Fatalf("drift %d", n)
		}
	})

	t.Run("cascades never orphan placements", func(t *testing.T) {
		// deleting a column that holds only a NON-home placement removes that placement, not the card
		mustX(t, d, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('lc','b1','active',3,'Extra',?)`, now)
		mustX(t, d, `UPDATE card_placements SET list_id='lc' WHERE card_id='c2' AND board_id='b1' AND is_home=0`)
		mustX(t, d, `DELETE FROM lists WHERE id='lc'`)
		if count(t, d, `SELECT COUNT(*) FROM cards WHERE id='c2'`) != 1 || count(t, d, `SELECT COUNT(*) FROM card_placements WHERE card_id='c2'`) != 1 {
			t.Fatal("deleting a column with a non-home placement should drop only that placement")
		}
		// a user delete nulls placed_by without touching cards
		mustX(t, d, `INSERT INTO users(id,email,name,role,created_at) VALUES('u2','u2@example.com','U2','boardUser',?)`, now)
		mustX(t, d, `INSERT INTO cards(id,board_id,list_id,creator_user_id,position,name,list_changed_at,created_at,updated_at) VALUES('c9','b1','la','u2',1,'Gone soon',?,?,'keep')`, now, now)
		mustX(t, d, `DELETE FROM users WHERE id='u2'`)
		if count(t, d, `SELECT COUNT(*) FROM card_placements WHERE card_id='c9' AND placed_by IS NULL`) != 1 || count(t, d, `SELECT COUNT(*) FROM cards WHERE id='c9' AND updated_at='keep'`) != 1 {
			t.Fatal("a user delete should only null placed_by")
		}
		mustX(t, d, `DELETE FROM cards WHERE id='c9'`)
		mustX(t, d, `DELETE FROM lists WHERE id='la'`)  // home column: card and placement cascade together
		mustX(t, d, `DELETE FROM boards WHERE id='b2'`) // c1 (home b2) and c2 (home b2) go with it
		if n := count(t, d, `SELECT COUNT(*) FROM card_placements WHERE card_id NOT IN (SELECT id FROM cards) OR board_id NOT IN (SELECT id FROM boards) OR list_id NOT IN (SELECT id FROM lists)`); n != 0 {
			t.Fatalf("%d orphaned placements", n)
		}
		if n := count(t, d, `SELECT COUNT(*) FROM card_placements`); n != 0 {
			t.Fatalf("%d placements left", n)
		}
	})
}
