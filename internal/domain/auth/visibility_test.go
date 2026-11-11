package auth

import (
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

func asUser(e *ops.Env, p *ops.Principal) *ops.Env { c := *e; c.User = p; return &c }

func sx(t *testing.T, e *ops.Env, q string, args ...any) {
	t.Helper()
	if _, err := e.DB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func ids(l []*User) map[string]bool {
	m := map[string]bool{}
	for _, u := range l {
		m[u.ID] = true
	}
	return m
}

func TestGuestsAndWorkersOnlySeeSharedBoardAccounts(t *testing.T) {
	e := newEnv(t)
	admin := mkUser(t, e, "admin@x.io", "admin")
	owner := mkUser(t, e, "owner@x.io", "projectOwner")
	guest := mkUser(t, e, "guest@x.io", "guest")
	worker := mkUser(t, e, "worker@x.io", "worker")
	mate := mkUser(t, e, "mate@x.io", "boardUser")      // shares board b1 with the guest
	viaGroup := mkUser(t, e, "grp@x.io", "boardUser")   // member of group g1 which is on b1
	manager := mkUser(t, e, "mgr@x.io", "projectOwner") // manages the project of b1
	stranger := mkUser(t, e, "stranger@x.io", "boardUser")
	loner := mkUser(t, e, "loner@x.io", "boardUser") // only on b2 (a board nobody restricted can see)

	sx(t, e, `INSERT INTO projects(id,name,created_at) VALUES('p1','P','x'),('p2','Q','x')`)
	sx(t, e, `INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'one','x'),('b2','p2',1,'two','x')`)
	sx(t, e, `INSERT INTO user_groups(id,name,created_at) VALUES('g1','team','x'),('g2','secret','x'),('g3','mine','x')`)
	sx(t, e, `INSERT INTO user_group_members(group_id,user_id) VALUES('g1',?),('g2',?),('g3',?),('g3',?)`, viaGroup.UserID, stranger.UserID, guest.UserID, stranger.UserID)
	sx(t, e, `INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES('pm1','p1',?,'x')`, manager.UserID)
	sx(t, e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m1','b1',?,'viewer','x'),('m2','b1',?,'editor','x'),('m3','b2',?,'editor','x'),('m4','b1',?,'editor','x')`,
		guest.UserID, mate.UserID, loner.UserID, worker.UserID)
	sx(t, e, `INSERT INTO board_memberships(id,board_id,group_id,role,created_at) VALUES('m5','b1','g1','editor','x')`)
	_ = db.Now

	// restricted callers: themselves + users sharing a board (direct, via group, project manager)
	for name, who := range map[string]*ops.Principal{"guest": guest, "worker": worker} {
		c := asUser(e, who)
		got := ids(call[UserList](t, c, "user.list", ListIn{}).Items)
		for _, want := range []*ops.Principal{who, mate, viaGroup, manager} {
			if !got[want.UserID] {
				t.Errorf("%s should see %s", name, want.Name)
			}
		}
		for _, hidden := range []*ops.Principal{admin, owner, stranger, loner} {
			if got[hidden.UserID] {
				t.Errorf("%s must not see %s", name, hidden.Name)
			}
		}
		wantErr(t, c, "user.get", IDIn{ID: stranger.UserID}, 404)
		wantErr(t, c, "user.get", IDIn{ID: admin.UserID}, 404)
		call[*User](t, c, "user.get", IDIn{ID: mate.UserID})
		call[*User](t, c, "user.get", IDIn{ID: "me"})
		// searching cannot reveal hidden accounts either
		if l := call[UserList](t, c, "user.list", ListIn{Q: "stranger"}); len(l.Items) != 0 {
			t.Errorf("%s: search found a hidden user", name)
		}
	}

	// groups: only the guest's own group and the group attached to a shared board; members are filtered too
	gc := asUser(e, guest)
	gl := call[GroupList](t, gc, "group.list", NoInput{})
	gotG := map[string]*Group{}
	for _, g := range gl.Items {
		gotG[g.ID] = g
	}
	if gotG["g1"] == nil || gotG["g3"] == nil || gotG["g2"] != nil || len(gl.Items) != 2 {
		t.Fatalf("guest groups: %+v", gotG)
	}
	for _, m := range gotG["g3"].Members {
		if m.ID == stranger.UserID {
			t.Error("group member list leaks a hidden account")
		}
	}
	if len(gotG["g3"].MemberIDs) != 1 {
		t.Errorf("MemberIDs not filtered: %v", gotG["g3"].MemberIDs)
	}
	wantErr(t, gc, "group.get", GroupIDIn{ID: "g2"}, 404)
	call[*Group](t, gc, "group.get", GroupIDIn{ID: "g1"})

	// everyone else keeps the full directory
	for name, who := range map[string]*ops.Principal{"admin": admin, "projectOwner": owner, "boardUser": stranger} {
		c := asUser(e, who)
		if n := len(call[UserList](t, c, "user.list", ListIn{}).Items); n != 9 {
			t.Errorf("%s sees %d users, want 9", name, n)
		}
		call[*User](t, c, "user.get", IDIn{ID: loner.UserID})
		if n := len(call[GroupList](t, c, "group.list", NoInput{}).Items); n != 3 {
			t.Errorf("%s sees %d groups, want 3", name, n)
		}
	}
}
