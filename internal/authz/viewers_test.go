package authz_test

import (
	"testing"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// ViewersAmong must agree with BoardLevel (>= View) for every kind of user, including stored-account states
// (deactivated, deleted) that BoardLevel's callers check before building a principal.
func TestViewersAmongAgreesWithBoardLevel(t *testing.T) {
	e := testutil.Env(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	now := "2026-01-01T00:00:00Z"
	users := []struct{ id, role string }{
		{"admin", "admin"}, {"mgr", "user"}, {"member", "user"}, {"viewer", "user"}, {"grp", "user"}, {"outsider", "user"},
		{"guest", "guest"}, {"guestm", "guest"}, {"worker", "worker"}, {"deact", "user"}, {"deleted", "user"}, {"deactadmin", "admin"},
	}
	for _, u := range users {
		exec(`INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,?,?)`, u.id, u.id+"@x.io", u.id, u.role, now)
	}
	exec(`UPDATE users SET is_deactivated=1 WHERE id IN ('deact','deactadmin')`)
	exec(`UPDATE users SET deleted_at=? WHERE id='deleted'`, now)
	exec(`INSERT INTO projects(id,name,created_at) VALUES('p1','P',?)`, now)
	exec(`INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES('pm1','p1','mgr',?)`, now)
	exec(`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('priv','p1',1,'Private',?),('pub','p1',2,'Public',?)`, now, now)
	exec(`UPDATE boards SET is_public=1 WHERE id='pub'`)
	exec(`INSERT INTO user_groups(id,name,created_at) VALUES('g1','Team',?)`, now)
	exec(`INSERT INTO user_group_members(group_id,user_id) VALUES('g1','grp')`)
	for i, m := range []struct{ user, group, role string }{
		{"member", "", "editor"}, {"viewer", "", "viewer"}, {"", "g1", "viewer"}, {"guestm", "", "viewer"}, {"worker", "", "viewer"},
		{"deact", "", "editor"}, {"deleted", "", "editor"},
	} {
		var u, g any
		if m.user != "" {
			u = m.user
		}
		if m.group != "" {
			g = m.group
		}
		exec(`INSERT INTO board_memberships(id,board_id,user_id,group_id,role,created_at) VALUES(?,?,?,?,?,?)`, "m"+string(rune('a'+i)), "priv", u, g, m.role, now)
	}
	var ids []string
	for _, u := range users {
		ids = append(ids, u.id)
	}
	ids = append(ids, "nobody") // unknown ids are never viewers
	for _, board := range []string{"priv", "pub", "missing"} {
		got := authz.ViewersAmong(e, board, ids)
		for _, u := range users {
			var role string
			var deact bool
			want := false
			if e.DB.QueryRow(`SELECT role,is_deactivated FROM users WHERE id=? AND deleted_at IS NULL`, u.id).Scan(&role, &deact) == nil && !deact {
				want = authz.BoardLevel(testutil.As(e, &ops.Principal{UserID: u.id, Role: role}), board) >= authz.View
			}
			if got[u.id] != want {
				t.Errorf("board %s user %s: ViewersAmong=%v, BoardLevel says %v", board, u.id, got[u.id], want)
			}
		}
		if got["nobody"] {
			t.Errorf("unknown user counted as a viewer")
		}
	}
	// spot checks so the agreement above is not vacuous
	priv := authz.ViewersAmong(e, "priv", ids)
	for _, id := range []string{"admin", "mgr", "member", "viewer", "grp", "guestm", "worker"} {
		if !priv[id] {
			t.Errorf("%s should view the private board", id)
		}
	}
	for _, id := range []string{"outsider", "guest", "deact", "deleted", "deactadmin"} {
		if priv[id] {
			t.Errorf("%s must not view the private board", id)
		}
	}
	if pub := authz.ViewersAmong(e, "pub", ids); !pub["outsider"] || !pub["guest"] || pub["deact"] {
		t.Errorf("public board: %v", pub)
	}
	if len(authz.ViewersAmong(e, "priv", nil)) != 0 {
		t.Error("empty input must give an empty result")
	}
}
