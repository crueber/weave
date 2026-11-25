package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// An existing database upgraded across 0901 (api_keys): the old one-key-per-user columns are backfilled into rows that keep
// the hash, prefix, created_at and expiry; users without a key get none; the legacy columns stay readable for a rollback.
func TestAPIKeysMigrationBackfill(t *testing.T) {
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
		if e.Name() == "0901_api_keys.sql" {
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
	if len(cols(t, d, "users")) == 0 || cols(t, d, "users")["api_key_hash"] != true {
		t.Fatal("previous schema should still have the one-key columns")
	}
	for _, q := range []string{
		`INSERT INTO users(id,email,name,role,created_at,api_key_hash,api_key_prefix,api_key_created_at) VALUES('u1','a@example.com','Ann','boardUser','2026-01-01T00:00:00Z','h-forever','wk_aaaaa','2026-02-01T00:00:00Z')`,
		`INSERT INTO users(id,email,name,role,created_at,is_agent,api_key_hash,api_key_prefix,api_key_created_at) VALUES('ag1','agent-ag1@agents.invalid','Bot','boardUser','2026-01-01T00:00:00Z',1,'h-agent','wk_bbbbb','2026-03-01T00:00:00Z')`,
		`INSERT INTO users(id,email,name,role,created_at,api_key_hash,api_key_prefix,api_key_created_at,api_key_expires_at) VALUES('u2','b@example.com','Bo','admin','2026-01-01T00:00:00Z','h-expiring','wk_ccccc','2026-04-01T00:00:00Z','2099-01-01T00:00:00Z')`,
		`INSERT INTO users(id,email,name,role,created_at,api_key_hash) VALUES('u3','c@example.com','Cy','boardUser','2026-01-01T00:00:00Z','h-nodates')`,
		`INSERT INTO users(id,email,name,role,created_at) VALUES('u4','d@example.com','Di','boardUser','2026-01-01T00:00:00Z')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := d.migrate(); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	type row struct {
		id, user, name, hash, prefix, created string
		exp, used                             *string
	}
	get := func(user string) (r row) {
		err := d.QueryRow(`SELECT id,user_id,name,key_hash,prefix,created_at,expires_at,last_used_at FROM api_keys WHERE user_id=?`, user).
			Scan(&r.id, &r.user, &r.name, &r.hash, &r.prefix, &r.created, &r.exp, &r.used)
		if err != nil {
			t.Fatalf("%s: %v", user, err)
		}
		return
	}
	if r := get("u1"); r.id != "u1" || r.name != "Existing key" || r.hash != "h-forever" || r.prefix != "wk_aaaaa" || r.created != "2026-02-01T00:00:00Z" || r.exp != nil || r.used != nil {
		t.Errorf("user key: %+v", r)
	}
	if r := get("ag1"); r.name != "Agent key" || r.hash != "h-agent" || r.exp != nil {
		t.Errorf("agent key: %+v", r)
	}
	if r := get("u2"); r.exp == nil || *r.exp != "2099-01-01T00:00:00Z" || r.hash != "h-expiring" {
		t.Errorf("expiring key lost its expiry: %+v", r)
	}
	if r := get("u3"); r.created != "2026-01-01T00:00:00Z" || r.prefix != "" {
		t.Errorf("key without dates falls back to the account's created_at: %+v", r)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM api_keys`); n != 4 {
		t.Errorf("%d rows, want 4 (u4 has no key)", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM users WHERE api_key_hash IS NOT NULL`); n != 4 {
		t.Error("legacy columns must stay untouched so a rollback still works")
	}
	// the user's cascade and the unique hash
	if _, err := d.Exec(`INSERT INTO api_keys(id,user_id,name,key_hash,prefix,created_at) VALUES('x','u4','n','h-forever','p','t')`); err == nil {
		t.Error("key_hash must be unique")
	}
	if _, err := d.Exec(`DELETE FROM users WHERE id='u1'`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM api_keys WHERE user_id='u1'`); n != 0 {
		t.Error("deleting a user must delete their keys")
	}
}
