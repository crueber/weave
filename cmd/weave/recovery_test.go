package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/db"
	_ "modernc.org/sqlite"
)

// oldSchema is the subset of a v0.2.1 database (0001_core + 0800 users.is_agent) that preflight may read. Any query
// touching a column added later fails against it, which is the point: preflight runs on a not yet upgraded database.
const oldSchema = `
CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL);
INSERT INTO schema_migrations VALUES('0001_core.sql','2026-01-01T00:00:00Z'),('0200_projects.sql','2026-01-01T00:00:00Z'),('0800_agents_events.sql','2026-01-01T00:00:00Z');
CREATE TABLE users (id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE COLLATE NOCASE, name TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'boardUser',
  is_deactivated INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT, deleted_at TEXT, is_agent INTEGER NOT NULL DEFAULT 0);
CREATE TABLE user_groups (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL);
CREATE TABLE user_group_members (group_id TEXT NOT NULL, user_id TEXT NOT NULL, PRIMARY KEY(group_id,user_id));
CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT NOT NULL, owner_manager_id TEXT, created_at TEXT NOT NULL, updated_at TEXT);
CREATE TABLE project_managers (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, user_id TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(project_id,user_id));
CREATE TABLE boards (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, position REAL NOT NULL, name TEXT NOT NULL, is_public INTEGER NOT NULL DEFAULT 0,
  is_template INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE TABLE board_memberships (id TEXT PRIMARY KEY, board_id TEXT NOT NULL, user_id TEXT, group_id TEXT, role TEXT NOT NULL DEFAULT 'editor',
  can_comment INTEGER, created_at TEXT NOT NULL);

INSERT INTO users(id,email,name,role,is_deactivated,created_at) VALUES
  ('ada','ada@example.com','Ada Admin','admin',0,'t'),('bob','bob@example.com','Bob Admin','admin',0,'t'),
  ('cat','cat@example.com','Cat Gone','boardUser',1,'t'),('dan','dan@example.com','Dan Doer','boardUser',0,'t'),
  ('eve','eve@example.com','Eve Retired','admin',1,'t');
INSERT INTO user_groups VALUES('g','Ops','t');
INSERT INTO user_group_members VALUES('g','bob');
INSERT INTO projects(id,name,created_at) VALUES('p1','Roadmap','t'),('p2','Orphan Garden','t'),('p3','Admin Tower','t');
INSERT INTO project_managers VALUES('m1','p1','dan','t'),('m2','p2','cat','t'),('m3','p3','ada','t');
INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'Plan','t'),('b2','p2',1,'Beds','t'),('b3','p3',1,'Keys','t');
INSERT INTO board_memberships(id,board_id,group_id,role,created_at) VALUES('bm1','b1','g','viewer','t');
`

func seedOld(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "weave.db")
	live, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	if _, err := live.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	return path, live // the open connection plays the old server that is still running
}

func snapshot(t *testing.T, d *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{`SELECT name FROM schema_migrations ORDER BY name`, `SELECT type||' '||name FROM sqlite_master ORDER BY name`,
		`SELECT id||project_id||user_id FROM project_managers ORDER BY id`} {
		rows, err := d.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			b.WriteString(s + "\n")
		}
		rows.Close()
	}
	return b.String()
}

func TestPreflightOnOldDatabaseIsReadOnly(t *testing.T) {
	path, live := seedOld(t)
	before := snapshot(t, live)
	var out, errb bytes.Buffer
	code := preflight([]string{"--db", path}, &out, &errb)
	s := out.String()
	if code != 1 {
		t.Fatalf("exit %d (want 1: action needed); stderr %s\n%s", code, errb.String(), s)
	}
	for _, want := range []string{
		"(a) Projects with no active manager: 1",
		"- Orphan Garden [p2] 1 board (1 without an active board owner); former managers: Cat Gone <cat@example.com> (deactivated)",
		"Ada Admin <ada@example.com>: 2 of 3 projects", // ada manages Admin Tower only
		"Bob Admin <bob@example.com>: 2 of 3 projects", // bob sees Roadmap through a group membership
		"    - Roadmap [p1] 1 board",
		"(c) Boards only admins manage (for information; does not change the exit code): 1",
		"- Admin Tower / Keys [b3]: Ada Admin <ada@example.com>",
		"Summary: 1 project without an active manager, 4 projects out of an admin's reach (1 board only admins manage): action needed before upgrading.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Eve Retired") {
		t.Error("deactivated admins are not reported")
	}
	if after := snapshot(t, live); after != before {
		t.Fatalf("preflight changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// one person, JSON
	out.Reset()
	if code := preflight([]string{"--db", path, "--email", "BOB@example.com", "--json"}, &out, &errb); code != 1 {
		t.Fatalf("json exit %d: %s", code, errb.String())
	}
	var j struct {
		Item struct {
			Admins []struct {
				Email       string
				Unreachable []struct{ ID string }
			}
		}
		NeedsAction bool
	}
	if err := json.Unmarshal(out.Bytes(), &j); err != nil || !j.NeedsAction || len(j.Item.Admins) != 1 || len(j.Item.Admins[0].Unreachable) != 2 {
		t.Fatalf("json: %v %s", err, out.String())
	}

	// usage and missing database
	if code := preflight([]string{"--bogus"}, &out, &errb); code != 2 {
		t.Errorf("usage exit %d", code)
	}
	if code := preflight([]string{"--db", filepath.Join(t.TempDir(), "missing.db")}, &out, &errb); code != 1 {
		t.Errorf("missing db exit %d", code)
	}
}

func TestPreflightCleanInstanceExitsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weave.db")
	d, err := db.Open(path) // a current schema works too
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES('a','a@x.io','A','admin','t'),('u','u@x.io','U','boardUser','t')`)
	_, _ = d.Exec(`INSERT INTO projects(id,name,created_at) VALUES('p','P','t')`)
	_, _ = d.Exec(`INSERT INTO project_managers VALUES('m1','p','a','t'),('m2','p','u','t')`)
	// an admin who manages a project of their own, with a board: only admins manage it, which is informational
	_, _ = d.Exec(`INSERT INTO projects(id,name,created_at) VALUES('q','Mine','t')`)
	_, _ = d.Exec(`INSERT INTO project_managers VALUES('m3','q','a','t')`)
	_, _ = d.Exec(`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b','q',1,'Solo','t')`)
	d.Close()
	var out, errb bytes.Buffer
	if code := preflight([]string{"--db", path}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "OK: nothing to fix before upgrading") || !strings.Contains(out.String(), "- Mine / Solo [b]: A <a@x.io>") {
		t.Fatal(out.String())
	}
}

func TestAdoptCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WEAVE_DATA_DIR", dir)
	d, err := db.Open(dir + "/weave.db")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES('u','u@x.io','U','boardUser','t'),('g','g@x.io','G','guest','t')`)
	_, _ = d.Exec(`INSERT INTO projects(id,name,created_at) VALUES('p','P','t')`)
	d.Close()
	var out, errb bytes.Buffer
	if code := adopt([]string{"--project", "p"}, &out, &errb); code != 2 {
		t.Errorf("usage exit %d", code)
	}
	if code := adopt([]string{"--project", "p", "--email", "g@x.io"}, &out, &errb); code != 1 {
		t.Errorf("guest adopt exit %d", code)
	}
	if code := adopt([]string{"--project", "p", "--email", "u@x.io"}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"source":"host"`) {
		t.Fatalf("adopt exit %d: %s %s", code, out.String(), errb.String())
	}
	d, _ = db.Open(dir + "/weave.db")
	defer d.Close()
	var n int
	_ = d.QueryRow(`SELECT COUNT(*) FROM project_recoveries WHERE project_id='p' AND user_id='u' AND source='host' AND actor_id IS NULL`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows %d", n)
	}
}
