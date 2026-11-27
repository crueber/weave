package auth

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// Persona tests for owner-created agents (internal/authz/agents.go).
type agentWorld struct {
	e                                        *ops.Env
	admin, owner, pm, editor, viewer, commen *ops.Env
	outsider, anon                           *ops.Env
	ownerID, pmID                            string
}

func newAgentWorld(t *testing.T) *agentWorld {
	t.Helper()
	e := newEnv(t)
	w := &agentWorld{e: e}
	as := func(p *ops.Principal) *ops.Env { return testutil.As(e, p) }
	w.admin = as(mkUser(t, e, "root@x.io", "admin"))
	o := mkUser(t, e, "owner@x.io", "boardUser")
	pm := mkUser(t, e, "pm@x.io", "boardUser")
	ed, vw, cm := mkUser(t, e, "ed@x.io", "boardUser"), mkUser(t, e, "vw@x.io", "boardUser"), mkUser(t, e, "cm@x.io", "boardUser")
	w.owner, w.pm, w.editor, w.viewer, w.commen = as(o), as(pm), as(ed), as(vw), as(cm)
	w.outsider, w.anon = as(mkUser(t, e, "out@x.io", "boardUser")), as(nil)
	w.ownerID, w.pmID = o.UserID, pm.UserID
	x := func(q string, args ...any) {
		t.Helper()
		if _, err := e.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	now := db.Now()
	x(`INSERT INTO projects(id,name,created_at) VALUES('p1','Mine',?),('p2','Theirs',?)`, now, now)
	x(`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'One',?),('b2','p1',2,'Two',?),('b3','p2',1,'Three',?)`, now, now, now)
	x(`INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES(?,?,?,?),(?,?,?,?)`, db.NewID(), "p1", o.UserID, now, db.NewID(), "p2", pm.UserID, now)
	for u, role := range map[string]string{ed.UserID: "editor", vw.UserID: "viewer"} {
		x(`INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`, db.NewID(), "b1", u, role, now)
	}
	x(`INSERT INTO board_memberships(id,board_id,user_id,role,can_comment,created_at) VALUES(?,?,?,?,1,?)`, db.NewID(), "b1", cm.UserID, "viewer", now)
	return w
}

func boardsOf(a AgentOut) string {
	var ids []string
	for _, b := range a.Boards {
		ids = append(ids, b.BoardID)
	}
	return strings.Join(ids, ",")
}

func listOf(t *testing.T, e *ops.Env) map[string]AgentOut {
	t.Helper()
	out := map[string]AgentOut{}
	for _, a := range call[[]AgentOut](t, e, "agent.list", AgentListIn{}) {
		if a.Key != nil {
			t.Fatal("agent.list must never return a key")
		}
		out[a.ID] = a
	}
	return out
}

func TestOwnerCreatesAgents(t *testing.T) {
	w := newAgentWorld(t)

	// who may create an agent for b1 (Manage on the board)
	a := call[AgentOut](t, w.owner, "agent.create", AgentCreateIn{Name: "Triage", BoardIDs: []string{"b1"}})
	if a.Role != "boardUser" || a.CreatedBy != w.ownerID || boardsOf(a) != "b1" || a.Key == nil || !a.CanManage {
		t.Fatalf("owner create: %+v", a)
	}
	wantErr(t, w.owner, "agent.create", AgentCreateIn{Name: "x"}, 422)                                               // boards required
	wantErr(t, w.owner, "agent.create", AgentCreateIn{Name: "x", BoardIDs: []string{"b1"}, Role: "worker"}, 422)     // role is admin-only
	wantErr(t, w.owner, "agent.create", AgentCreateIn{Name: "x", BoardIDs: []string{"b1"}, BoardRole: "owner"}, 422) // never more than editor
	wantErr(t, w.owner, "agent.create", AgentCreateIn{Name: "x", BoardIDs: []string{"b3"}}, 404)                     // not their board
	wantErr(t, w.owner, "agent.create", AgentCreateIn{Name: "x", BoardIDs: []string{"b1", "b3"}}, 404)               // every board must be managed
	for name, env := range map[string]*ops.Env{"editor": w.editor, "viewer": w.viewer, "commenter": w.commen} {
		if _, err := ops.Call(env, "agent.create", &AgentCreateIn{Name: "x", BoardIDs: []string{"b1"}}); err == nil {
			t.Fatalf("%s created an agent", name)
		}
	}
	wantErr(t, w.outsider, "agent.create", AgentCreateIn{Name: "x", BoardIDs: []string{"b1"}}, 404)
	wantErr(t, w.anon, "agent.create", AgentCreateIn{Name: "x", BoardIDs: []string{"b1"}}, 401)
	// admins may still create an account without boards (account management)
	call[AgentOut](t, w.admin, "agent.create", AgentCreateIn{Name: "Solo"})

	// the agent itself: never project-wide, never an admin, never manages members or agents
	var pmRows int
	_ = w.e.DB.QueryRow(`SELECT COUNT(*) FROM project_managers WHERE user_id=?`, a.ID).Scan(&pmRows)
	if pmRows != 0 {
		t.Fatal("an agent must never become a project manager")
	}
	ag := reqEnv(w.e, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+a.Key.Key) })
	if ag.User == nil || !ag.User.IsAgent || ag.User.Role != "boardUser" {
		t.Fatalf("agent principal: %+v", ag.User)
	}
	wantErr(t, ag, "agent.create", AgentCreateIn{Name: "child", BoardIDs: []string{"b1"}}, 403)
	wantErr(t, ag, "agent.list", AgentListIn{}, 403)
	if _, err := ops.CallMap(ag, "board.member.add", map[string]any{"boardId": "b1", "userId": w.pmID, "role": "editor"}); err == nil {
		t.Fatal("an agent must not manage members")
	}
	if _, err := ops.CallMap(ag, "project.create", map[string]any{"name": "Agent project"}); err == nil {
		t.Fatal("an agent must not create projects")
	}
	if _, err := ops.CallMap(w.owner, "project.manager.add", map[string]any{"id": "p1", "userId": a.ID}); err == nil {
		t.Fatal("an agent must not become a project manager")
	}
	if _, err := ops.CallMap(w.owner, "board.member.update", map[string]any{"memberId": a.Boards[0].MemberID, "role": "owner"}); err == nil {
		t.Fatal("an agent's board membership must stay editor/viewer/commenter")
	}

	// listing
	if l := listOf(t, w.owner); len(l) != 1 || boardsOf(l[a.ID]) != "b1" {
		t.Fatalf("owner list: %+v", l)
	}
	for name, env := range map[string]*ops.Env{"editor": w.editor, "viewer": w.viewer, "commenter": w.commen, "outsider": w.outsider, "pm of another project": w.pm} {
		if l := listOf(t, env); len(l) != 0 {
			t.Fatalf("%s sees agents: %+v", name, l)
		}
	}
	if l := listOf(t, w.admin); len(l) != 2 {
		t.Fatalf("admin sees every agent: %+v", l)
	}
	wantErr(t, w.editor, "agent.list", AgentListIn{BoardID: "b1"}, 403)
	if l := call[[]AgentOut](t, w.owner, "agent.list", AgentListIn{BoardID: "b1"}); len(l) != 1 {
		t.Fatalf("board filter: %+v", l)
	}

	// grants need Manage on the target board
	call[AgentOut](t, w.owner, "agent.access", AgentAccessIn{ID: a.ID, BoardID: "b2", Role: "commenter"})
	wantErr(t, w.owner, "agent.access", AgentAccessIn{ID: a.ID, BoardID: "b3"}, 404)
	wantErr(t, w.editor, "agent.access", AgentAccessIn{ID: a.ID, BoardID: "b1", Role: "viewer"}, 404)
	// the manager of b3 may invite the agent onto their own board; each side sees only the boards it manages
	call[AgentOut](t, w.pm, "agent.access", AgentAccessIn{ID: a.ID, BoardID: "b3", Role: "viewer"})
	if got := boardsOf(listOf(t, w.owner)[a.ID]); got != "b1,b2" {
		t.Fatalf("owner sees boards %q", got)
	}
	pmView := listOf(t, w.pm)[a.ID]
	if boardsOf(pmView) != "b3" || pmView.CanManage {
		t.Fatalf("pm view: %+v", pmView)
	}
	// the pm does not manage every board the agent touches: no account management
	wantErr(t, w.pm, "agent.rotateKey", AgentRotateIn{ID: a.ID}, 403)
	wantErr(t, w.pm, "agent.delete", AgentIDIn{ID: a.ID}, 403)
	// the creator revokes access to a board they do not manage
	call[AgentOut](t, w.owner, "agent.access", AgentAccessIn{ID: a.ID, BoardID: "b3", Revoke: true})
	var n int
	_ = w.e.DB.QueryRow(`SELECT COUNT(*) FROM board_memberships WHERE user_id=? AND board_id='b3'`, a.ID).Scan(&n)
	if n != 0 {
		t.Fatal("creator revoke on b3 did not remove the membership")
	}
	// outsiders and members cannot manage the account
	for name, env := range map[string]*ops.Env{"editor": w.editor, "viewer": w.viewer, "outsider": w.outsider} {
		if _, err := ops.Call(env, "agent.rotateKey", &AgentRotateIn{ID: a.ID}); err == nil {
			t.Fatalf("%s rotated the key", name)
		}
		if _, err := ops.Call(env, "agent.update", &AgentUpdateIn{ID: a.ID, Name: ptr("pwn")}); err == nil {
			t.Fatalf("%s renamed the agent", name)
		}
	}
	// the creator: rename, deactivate, rotate (old key dies), but not the role
	wantErr(t, w.owner, "agent.update", AgentUpdateIn{ID: a.ID, Role: ptr("worker")}, 403)
	u := call[AgentOut](t, w.owner, "agent.update", AgentUpdateIn{ID: a.ID, Name: ptr("Triage bot"), IsDeactivated: ptrB(true)})
	if u.Name != "Triage bot" || !u.IsDeactivated {
		t.Fatalf("update: %+v", u)
	}
	call[AgentOut](t, w.owner, "agent.update", AgentUpdateIn{ID: a.ID, IsDeactivated: ptrB(false)})
	r := call[AgentOut](t, w.owner, "agent.rotateKey", AgentRotateIn{ID: a.ID})
	if r.Key == nil || reqEnv(w.e, func(q *http.Request) { q.Header.Set("Authorization", "Bearer "+a.Key.Key) }).User != nil {
		t.Fatal("rotation must kill the old key")
	}
	// keys never reach the activity log
	var leaks int
	_ = w.e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE data LIKE ? OR COALESCE(envelope,'') LIKE ?`, "%"+r.Key.Key+"%", "%"+r.Key.Key+"%").Scan(&leaks)
	if leaks != 0 {
		t.Fatal("agent key in the activity log")
	}

	// a manager of every board the agent touches may manage it (b1 and b2 are in the owner's project; a second manager)
	if _, err := w.e.DB.Exec(`INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES(?,?,?,?)`, db.NewID(), "p1", w.pmID, db.Now()); err != nil {
		t.Fatal(err)
	}
	if !listOf(t, w.pm)[a.ID].CanManage {
		t.Fatal("a manager of all the agent's boards may manage it")
	}

	// delete releases claims
	if _, err := w.e.DB.Exec(`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('l1','b1','active',1,'Todo',?)`, db.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.DB.Exec(`INSERT INTO cards(id,board_id,list_id,position,name,claimed_by,claimed_until,list_changed_at,created_at) VALUES('c1','b1','l1',1,'Work',?,?,?,?)`, a.ID, "2099-01-01T00:00:00Z", db.Now(), db.Now()); err != nil {
		t.Fatal(err)
	}
	call[AgentOut](t, w.owner, "agent.delete", AgentIDIn{ID: a.ID})
	var claimed *string
	_ = w.e.DB.QueryRow(`SELECT claimed_by FROM cards WHERE id='c1'`).Scan(&claimed)
	if claimed != nil {
		t.Fatal("claims must be released when the agent is deleted")
	}
	if l := listOf(t, w.owner); len(l) != 0 {
		t.Fatalf("deleted agent still listed: %+v", l)
	}
}

func setAgentsCap(t *testing.T, v string) {
	t.Helper()
	agentsCapEnv = func() string { return v }
	agentsCapOnce, agentsCapVal = sync.Once{}, defaultAgentsPerUser
	t.Cleanup(func() {
		agentsCapEnv = func() string { return os.Getenv("WEAVE_AGENTS_PER_USER") }
		agentsCapOnce, agentsCapVal = sync.Once{}, defaultAgentsPerUser
	})
}

func TestAgentCapPerCreator(t *testing.T) {
	setAgentsCap(t, "1")
	w := newAgentWorld(t)
	call[AgentOut](t, w.owner, "agent.create", AgentCreateIn{Name: "one", BoardIDs: []string{"b1"}})
	wantErr(t, w.owner, "agent.create", AgentCreateIn{Name: "two", BoardIDs: []string{"b1"}}, 422)
	call[AgentOut](t, w.admin, "agent.create", AgentCreateIn{Name: "admin's"}) // admins are not capped
	setAgentsCap(t, "0")
	call[AgentOut](t, w.owner, "agent.create", AgentCreateIn{Name: "two", BoardIDs: []string{"b1"}})
}

func ptrB(b bool) *bool { return &b }
