package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// newMigrations are the card-type removal migrations under test. An existing database has every
// other migration applied (including the 0600 search triggers) when these run, so the test builds
// exactly that state, fills it with boards and cards that use the dropped columns, then upgrades.
var newMigrations = map[string]bool{"0306_drop_board_card_type.sql": true, "0401_drop_card_type.sql": true}

func cols(t *testing.T, d *DB, table string) map[string]bool {
	t.Helper()
	rows, err := d.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	return out
}

func count(t *testing.T, d *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestDropCardTypeMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	sdb, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	d := &DB{sdb}
	defer d.Close()
	if _, err := d.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	ents, _ := migFS.ReadDir("migrations")
	for _, e := range ents {
		if newMigrations[e.Name()] {
			continue
		}
		b, _ := migFS.ReadFile("migrations/" + e.Name())
		if _, err := d.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if _, err := d.Exec(`INSERT INTO schema_migrations VALUES(?,?)`, e.Name(), Now()); err != nil {
			t.Fatal(err)
		}
	}
	if !cols(t, d, "cards")["type"] || !cols(t, d, "boards")["default_card_type"] {
		t.Fatal("previous schema should still have the card-type columns")
	}

	now := Now()
	seed := []string{
		`INSERT INTO users(id,email,name,role,created_at) VALUES('u1','a@x.io','Ann','admin','` + now + `')`,
		`INSERT INTO projects(id,name,created_at) VALUES('p1','Proj','` + now + `')`,
		`INSERT INTO boards(id,project_id,position,name,default_view,default_card_type,limit_card_types_to_default,color,agent_prompt,created_at)
		  VALUES('b1','p1',1,'Stories','grid','story',1,'#112233','be brief','` + now + `'),('b2','p1',2,'Plain','kanban','project',0,NULL,NULL,'` + now + `')`,
		`INSERT INTO lists(id,board_id,type,position,name,description,wip_limit,created_at) VALUES('l1','b1','active',1,'Todo','brief',3,'` + now + `'),('l2','b2','active',1,'Doing',NULL,NULL,'` + now + `')`,
		`INSERT INTO labels(id,board_id,name,color,position,created_at) VALUES('lb1','b1','bug','crimson',1,'` + now + `')`,
		`INSERT INTO cards(id,board_id,list_id,creator_user_id,type,position,name,description,due_date,list_changed_at,created_at,claimed_by,claimed_until)
		  VALUES('c1','b1','l1','u1','story',65536,'Write the story','zebra details','2030-01-02T00:00:00Z','` + now + `','` + now + `','u1','2030-01-01T00:00:00Z'),
		        ('c2','b2','l2',NULL,'project',65536,'Ship it',NULL,NULL,'` + now + `','` + now + `',NULL,NULL)`,
		`INSERT INTO card_labels(card_id,label_id) VALUES('c1','lb1')`,
		`INSERT INTO card_memberships(card_id,user_id) VALUES('c1','u1')`,
		`INSERT INTO comments(id,card_id,user_id,text,created_at) VALUES('m1','c1','u1','looks good','` + now + `')`,
	}
	for _, q := range seed {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	triggers := func() int {
		return count(t, d, `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND tbl_name IN ('cards','boards')`)
	}
	indexes := func() int {
		return count(t, d, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name IN ('cards','boards')`)
	}
	trigBefore, idxBefore := triggers(), indexes()
	if trigBefore == 0 || idxBefore == 0 {
		t.Fatalf("expected search triggers and indexes before upgrade: %d %d", trigBefore, idxBefore)
	}

	if err := d.migrate(); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	cc, bc := cols(t, d, "cards"), cols(t, d, "boards")
	if cc["type"] || bc["default_card_type"] || bc["limit_card_types_to_default"] {
		t.Fatalf("card-type columns survived: cards=%v boards=%v", cc, bc)
	}
	for _, c := range []string{"claimed_by", "template_project_id", "recurrence", "share_token"} {
		if !cc[c] {
			t.Errorf("cards.%s lost", c)
		}
	}
	for _, c := range []string{"color", "agent_prompt", "template_visibility", "default_view", "has_inbox"} {
		if !bc[c] {
			t.Errorf("boards.%s lost", c)
		}
	}
	if n := count(t, d, `SELECT COUNT(*) FROM schema_migrations WHERE name IN ('0306_drop_board_card_type.sql','0401_drop_card_type.sql')`); n != 2 {
		t.Fatalf("new migrations recorded %d times", n)
	}

	var name, desc, due, claimed, view, color, prompt string
	if err := d.QueryRow(`SELECT name,description,due_date,claimed_by FROM cards WHERE id='c1'`).Scan(&name, &desc, &due, &claimed); err != nil {
		t.Fatal(err)
	}
	if name != "Write the story" || desc != "zebra details" || due != "2030-01-02T00:00:00Z" || claimed != "u1" {
		t.Fatalf("card data changed: %q %q %q %q", name, desc, due, claimed)
	}
	if err := d.QueryRow(`SELECT default_view,color,agent_prompt FROM boards WHERE id='b1'`).Scan(&view, &color, &prompt); err != nil {
		t.Fatal(err)
	}
	if view != "grid" || color != "#112233" || prompt != "be brief" {
		t.Fatalf("board data changed: %q %q %q", view, color, prompt)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM cards`); n != 2 {
		t.Fatalf("cards: %d", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM boards`); n != 2 {
		t.Fatalf("boards: %d", n)
	}
	if count(t, d, `SELECT COUNT(*) FROM card_labels WHERE card_id='c1'`) != 1 || count(t, d, `SELECT COUNT(*) FROM card_memberships WHERE card_id='c1'`) != 1 ||
		count(t, d, `SELECT COUNT(*) FROM comments WHERE card_id='c1'`) != 1 {
		t.Fatal("card relations lost")
	}
	if triggers() != trigBefore || indexes() != idxBefore {
		t.Fatalf("triggers %d->%d, indexes %d->%d", trigBefore, triggers(), idxBefore, indexes())
	}
	var ic string
	if err := d.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil || ic != "ok" {
		t.Fatalf("integrity_check: %q %v", ic, err)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("foreign_key_check: %d violations", n)
	}
	// The search index is still maintained: old rows are found, and the triggers fire for new cards.
	if n := count(t, d, `SELECT COUNT(*) FROM search_idx WHERE search_idx MATCH 'zebra'`); n != 1 {
		t.Fatalf("existing card missing from search: %d", n)
	}
	if _, err := d.Exec(`INSERT INTO cards(id,board_id,list_id,position,name,description,list_changed_at,created_at) VALUES('c3','b1','l1',131072,'After upgrade','quokka',?,?)`, now, now); err != nil {
		t.Fatalf("insert after upgrade: %v", err)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM search_idx WHERE search_idx MATCH 'quokka'`); n != 1 {
		t.Fatalf("search trigger did not fire after upgrade: %d", n)
	}
	// Foreign keys still cascade through the dropped-from tables.
	if _, err := d.Exec(`DELETE FROM boards WHERE id='b1'`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM cards WHERE board_id='b1'`); n != 0 {
		t.Fatalf("cascade: %d cards left", n)
	}
	// A second run is a no-op.
	if err := d.migrate(); err != nil {
		t.Fatal(err)
	}
}

// 0403: open cards that sit in a closed-type column are closed once; drafts, templates, cards elsewhere and
// updated_at are left alone.
func TestClosedColumnBackfill(t *testing.T) {
	d := openWithout(t, map[string]bool{"0403_closed_column_backfill.sql": true})
	now := Now()
	old := "2020-01-01T00:00:00Z"
	for _, q := range []string{
		`INSERT INTO projects(id,name,created_at) VALUES('p1','Proj','` + now + `')`,
		`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'B','` + now + `')`,
		`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('la','b1','active',1,'Todo','` + now + `'),('lc','b1','closed',2,'Done','` + now + `'),('lx','b1','archive',NULL,NULL,'` + now + `')`,
		`INSERT INTO cards(id,board_id,list_id,position,name,is_closed,is_draft,is_template,list_changed_at,created_at,updated_at) VALUES
			('open-in-done','b1','lc',1,'A',0,0,0,'` + now + `','` + now + `','` + old + `'),
			('closed-in-done','b1','lc',2,'B',1,0,0,'` + now + `','` + now + `','` + old + `'),
			('open-todo','b1','la',1,'C',0,0,0,'` + now + `','` + now + `','` + old + `'),
			('closed-todo','b1','la',2,'D',1,0,0,'` + now + `','` + now + `','` + old + `'),
			('archived','b1','lx',1,'E',0,0,0,'` + now + `','` + now + `','` + old + `'),
			('draft','b1','lc',3,'F',0,1,0,'` + now + `','` + now + `','` + old + `'),
			('tmpl','b1','lc',4,'G',0,0,1,'` + now + `','` + now + `','` + old + `')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := d.migrate(); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	want := map[string]int{"open-in-done": 1, "closed-in-done": 1, "open-todo": 0, "closed-todo": 1, "archived": 0, "draft": 0, "tmpl": 0}
	for id, w := range want {
		if n := count(t, d, `SELECT is_closed FROM cards WHERE id=?`, id); n != w {
			t.Errorf("%s: is_closed=%d, want %d", id, n, w)
		}
	}
	if n := count(t, d, `SELECT COUNT(*) FROM cards WHERE updated_at<>?`, old); n != 0 {
		t.Fatalf("the backfill must not touch updated_at (%d rows changed)", n)
	}
	if err := d.migrate(); err != nil { // applied once
		t.Fatal(err)
	}
}

// openWithout builds a database with every migration applied except skip (the state of an existing install).
func openWithout(t *testing.T, skip map[string]bool) *DB {
	t.Helper()
	sdb, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "old.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	d := &DB{sdb}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	ents, _ := migFS.ReadDir("migrations")
	for _, e := range ents {
		if skip[e.Name()] {
			continue
		}
		b, _ := migFS.ReadFile("migrations/" + e.Name())
		if _, err := d.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if _, err := d.Exec(`INSERT INTO schema_migrations VALUES(?,?)`, e.Name(), Now()); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// Watching (0102, 0308, 0402): existing card and board subscriptions survive, the column table exists, the mute
// column defaults to 0 and "Watch cards I create" is switched on for everyone once.
func TestWatchMigrations(t *testing.T) {
	d := openWithout(t, map[string]bool{"0102_watch_own_cards_default.sql": true, "0308_list_subscriptions.sql": true, "0402_card_subscription_mute.sql": true})
	if cols(t, d, "card_subscriptions")["muted"] || count(t, d, `SELECT COUNT(*) FROM sqlite_master WHERE name='list_subscriptions'`) != 0 {
		t.Fatal("the old schema should not have the watch changes yet")
	}
	now := Now()
	for _, q := range []string{
		`INSERT INTO users(id,email,name,role,created_at) VALUES('u1','a@x.io','Ann','user','` + now + `'),('u2','b@x.io','Ben','user','` + now + `')`,
		`UPDATE users SET subscribe_own_cards=0, subscribe_to_card_when_commenting=0 WHERE id='u2'`,
		`INSERT INTO projects(id,name,created_at) VALUES('p1','Proj','` + now + `')`,
		`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'B','` + now + `')`,
		`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('l1','b1','active',1,'Todo','` + now + `')`,
		`INSERT INTO cards(id,board_id,list_id,position,name,list_changed_at,created_at) VALUES('c1','b1','l1',1,'C','` + now + `','` + now + `')`,
		`INSERT INTO card_subscriptions(card_id,user_id,is_permanent) VALUES('c1','u1',1),('c1','u2',0)`,
		`INSERT INTO board_subscriptions(board_id,user_id) VALUES('b1','u2')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := d.migrate(); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM card_subscriptions WHERE card_id='c1' AND muted=0`); n != 2 {
		t.Fatalf("card subscriptions after upgrade (unmuted): %d", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM card_subscriptions WHERE user_id='u1' AND is_permanent=1`); n != 1 {
		t.Fatal("is_permanent lost")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM board_subscriptions WHERE board_id='b1' AND user_id='u2'`); n != 1 {
		t.Fatal("board subscription lost")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM users WHERE subscribe_own_cards=1`); n != 2 {
		t.Fatalf("subscribe_own_cards flipped for %d of 2 users", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM users WHERE id='u2' AND subscribe_to_card_when_commenting=0`); n != 1 {
		t.Fatal("the comment preference must be left alone")
	}
	if _, err := d.Exec(`INSERT INTO list_subscriptions(list_id,user_id,created_at) VALUES('l1','u1',?)`, now); err != nil {
		t.Fatalf("list_subscriptions: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO card_subscriptions(card_id,user_id) VALUES('c1','u3x')`); err == nil {
		t.Fatal("foreign keys should still hold")
	}
	if _, err := d.Exec(`DELETE FROM lists WHERE id='l1'`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM list_subscriptions`); n != 0 {
		t.Fatalf("list watches must cascade with the list: %d", n)
	}
	if err := d.migrate(); err != nil {
		t.Fatal(err)
	}
}

// 0310: lists gain archive_after (auto-archive), NULL for every existing column; nothing is archived by the upgrade.
func TestListArchiveAfterMigration(t *testing.T) {
	d := openWithout(t, map[string]bool{"0310_list_archive_after.sql": true})
	if cols(t, d, "lists")["archive_after"] {
		t.Fatal("column exists before the migration")
	}
	now := Now()
	for _, q := range []string{
		`INSERT INTO projects(id,name,created_at) VALUES('p1','Proj','` + now + `')`,
		`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'B','` + now + `')`,
		`INSERT INTO lists(id,board_id,type,position,name,wip_limit,created_at) VALUES('la','b1','active',1,'Todo',3,'` + now + `')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := d.migrate(); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !cols(t, d, "lists")["archive_after"] {
		t.Fatal("archive_after missing")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM lists WHERE archive_after IS NULL AND wip_limit=3`); n != 1 {
		t.Fatalf("existing column: %d rows off with the WIP limit kept", n)
	}
	if err := d.migrate(); err != nil { // applied once
		t.Fatal(err)
	}
}
