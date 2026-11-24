package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// An existing database upgraded across 0103 (projectOwner -> boardUser) and 0202 (projects.board_creation).
func TestProjectOwnerRoleAndBoardCreationMigrations(t *testing.T) {
	skip := map[string]bool{"0103_drop_project_owner_role.sql": true, "0202_project_board_creation.sql": true}
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
	if cols(t, d, "projects")["board_creation"] {
		t.Fatal("previous schema should not have board_creation")
	}
	now := Now()
	for _, q := range []string{
		`INSERT INTO users(id,email,name,role,created_at) VALUES('u1','a@x.io','Ann','projectOwner','` + now + `'),('u2','b@x.io','Bo','admin','` + now + `'),
		  ('u3','c@x.io','Cy','guest','` + now + `'),('u4','d@x.io','Di','worker','` + now + `'),('u5','e@x.io','Ed','boardUser','` + now + `')`,
		`INSERT INTO projects(id,name,owner_manager_id,created_at) VALUES('p1','Proj','m1','` + now + `')`,
		`INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES('m1','p1','u1','` + now + `')`,
		`INSERT INTO config(key,value) VALUES('oidc.ownerGroup','legacy-owners'),('oidc.adminGroup','my-admins')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := d.migrate(); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	want := map[string]string{"u1": "boardUser", "u2": "admin", "u3": "guest", "u4": "worker", "u5": "boardUser"}
	for id, role := range want {
		var got string
		if err := d.QueryRow(`SELECT role FROM users WHERE id=?`, id).Scan(&got); err != nil || got != role {
			t.Errorf("%s: role %q, want %q (%v)", id, got, role, err)
		}
	}
	if n := count(t, d, `SELECT COUNT(*) FROM users WHERE role='projectOwner'`); n != 0 {
		t.Fatalf("%d projectOwner accounts left", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM project_managers WHERE user_id='u1' AND project_id='p1'`); n != 1 {
		t.Fatal("the former owner's manager row must stay")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM config WHERE key='oidc.ownerGroup'`); n != 0 {
		t.Fatal("stale oidc.ownerGroup row survived")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM config WHERE key='oidc.adminGroup' AND value='my-admins'`); n != 1 {
		t.Fatal("other config rows must stay")
	}
	var bc string
	if err := d.QueryRow(`SELECT board_creation FROM projects WHERE id='p1'`).Scan(&bc); err != nil || bc != "owners" {
		t.Fatalf("existing project board_creation %q (%v), want owners", bc, err)
	}
	if _, err := d.Exec(`INSERT INTO projects(id,name,created_at) VALUES('p2','New','` + now + `')`); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT board_creation FROM projects WHERE id='p2'`).Scan(&bc); err != nil || bc != "owners" {
		t.Fatalf("new project default %q (%v), want owners", bc, err)
	}
}
