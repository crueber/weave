package projects_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/domain/projects"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
	"github.com/crueber/weave/internal/web"
)

func callErr(e *ops.Env, op string, in any) error {
	switch v := in.(type) {
	case projects.CreateIn:
		_, err := ops.Typed[projects.Project](e, op, v)
		return err
	case projects.ManagerAddIn:
		_, err := ops.Typed[projects.Manager](e, op, v)
		return err
	case projects.UpdateIn:
		_, err := ops.Typed[projects.Project](e, op, v)
		return err
	case boards.BoardCreateIn:
		_, err := ops.Typed[boards.Board](e, op, v)
		return err
	case boards.InstantiateIn:
		_, err := ops.Typed[boards.Board](e, op, v)
		return err
	case boards.ImportIn:
		_, err := ops.Typed[boards.Board](e, op, v)
		return err
	case boards.MemberAddIn:
		_, err := ops.Typed[boards.Member](e, op, v)
		return err
	case boards.MemberUpdateIn:
		_, err := ops.Typed[boards.Member](e, op, v)
		return err
	case boards.MemberRefIn:
		_, err := ops.Typed[boards.Member](e, op, v)
		return err
	case boards.BoardUpdateIn:
		_, err := ops.Typed[boards.Board](e, op, v)
		return err
	case boards.BoardMoveIn:
		_, err := ops.Typed[boards.Board](e, op, v)
		return err
	}
	panic("unknown input type")
}

type persona struct {
	name string
	p    *ops.Principal
}

// ownershipWorld: a project P created by "creator" (its manager) with one board B1 that every member persona is on, a
// second project manager "pm", and the personas of the ownership table.
type ownershipWorld struct {
	e        *ops.Env
	project  string
	board    string
	ps       map[string]*ops.Principal
	order    []string
	memberOf map[string]string // persona -> membership id on B1
}

func newOwnershipWorld(t *testing.T) *ownershipWorld {
	e := testutil.Env(t, nil)
	e.Svc["config"] = config.Config{DataDir: t.TempDir()}
	w := &ownershipWorld{e: e, ps: map[string]*ops.Principal{}, memberOf: map[string]string{}}
	add := func(name, role string, agent bool) {
		p := addUser(t, e, name, role)
		if agent {
			if _, err := e.DB.Exec(`UPDATE users SET is_agent=1 WHERE id=?`, name); err != nil {
				t.Fatal(err)
			}
			p.IsAgent = true
		}
		w.ps[name] = p
		w.order = append(w.order, name)
	}
	add("admin", "admin", false)
	add("creator", "boardUser", false)
	add("pm", "boardUser", false)
	add("editor", "boardUser", false)
	add("viewer", "boardUser", false)
	add("commenter", "boardUser", false)
	add("worker", "worker", false)
	add("guest", "guest", false)
	add("agent", "boardUser", true)
	add("outsider", "boardUser", false)
	w.ps["anonymous"] = nil
	w.order = append(w.order, "anonymous")

	ce := testutil.As(e, w.ps["creator"])
	w.project = call[projects.Project](t, ce, "project.create", projects.CreateIn{Name: "Owned"}).ID
	w.board = call[boards.Board](t, ce, "board.create", boards.BoardCreateIn{ProjectID: w.project, Name: "B1"}).ID
	call[projects.Manager](t, ce, "project.manager.add", projects.ManagerAddIn{ID: w.project, UserID: "pm"})
	yes := true
	for name, role := range map[string]string{"editor": "editor", "viewer": "viewer", "commenter": "viewer", "worker": "editor", "guest": "editor", "agent": "editor"} {
		in := boards.MemberAddIn{BoardID: w.board, UserID: name, Role: role}
		if name == "commenter" {
			in.CanComment = &yes
		}
		w.memberOf[name] = call[boards.Member](t, ce, "board.member.add", in).ID
	}
	return w
}

func (w *ownershipWorld) as(name string) *ops.Env { return testutil.As(w.e, w.ps[name]) }

func (w *ownershipWorld) setPolicy(t *testing.T, policy string) {
	t.Helper()
	call[projects.Project](t, w.as("creator"), "project.update", projects.UpdateIn{ID: w.project, BoardCreation: &policy})
}

// page renders a GET for a persona through the real web routes.
func (w *ownershipWorld) page(t *testing.T, name, path string) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	web.Mount(mux, func(r *http.Request) *ops.Env { c := *w.as(name); c.Req = r; c.Ctx = r.Context(); return &c })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code, rec.Body.String()
}

func TestOwnershipPersonaTable(t *testing.T) {
	w := newOwnershipWorld(t)
	// who may create a project, and the home page shows the button exactly then
	canProject := map[string]bool{"admin": true, "creator": true, "pm": true, "editor": true, "viewer": true, "commenter": true, "outsider": true}
	for _, n := range w.order {
		err := callErr(w.as(n), "project.create", projects.CreateIn{Name: "Mine " + n})
		if (err == nil) != canProject[n] {
			t.Errorf("project.create as %s: err=%v, want allowed=%v", n, err, canProject[n])
		}
		if n == "anonymous" {
			continue
		}
		code, body := w.page(t, n, "/")
		has := strings.Contains(body, `hx-get="/ui/projects/new"`)
		if code != 200 || has != canProject[n] {
			t.Errorf("home page as %s: code %d, create button %v, want %v", n, code, has, canProject[n])
		}
		if code, _ := w.page(t, n, "/ui/projects/new"); (code == 200) != canProject[n] {
			t.Errorf("new-project dialog as %s: %d", n, code)
		}
	}

	// adding a project manager: project managers only (an admin who is not one is an outsider here)
	canManage := map[string]bool{"creator": true, "pm": true}
	for _, n := range w.order {
		err := callErr(w.as(n), "project.manager.add", projects.ManagerAddIn{ID: w.project, UserID: "outsider"})
		if (err == nil) != canManage[n] {
			t.Errorf("project.manager.add as %s: err=%v, want allowed=%v", n, err, canManage[n])
		}
		if err == nil {
			ms := call[[]projects.Manager](t, w.as("creator"), "project.manager.list", projects.IDIn{ID: w.project})
			for _, m := range ms {
				if m.UserID == "outsider" {
					call[projects.DeleteOut](t, w.as("creator"), "project.manager.remove", projects.ManagerRemoveIn{ID: w.project, ManagerID: m.ID})
				}
			}
		}
	}

	// board creation under both policies; the project page button follows the same rule
	members := map[string]bool{"creator": true, "pm": true, "editor": true, "viewer": true, "commenter": true}
	for _, policy := range []string{authz.BoardCreationOwners, authz.BoardCreationMembers} {
		w.setPolicy(t, policy)
		for _, n := range w.order {
			want := canManage[n] || (policy == authz.BoardCreationMembers && members[n])
			if got := n != "anonymous" && authz.CanCreateBoard(w.as(n), w.project); got != want {
				t.Errorf("%s CanCreateBoard as %s = %v, want %v", policy, n, got, want)
			}
			for _, op := range []struct {
				name string
				in   any
			}{
				{"board.create", boards.BoardCreateIn{ProjectID: w.project, Name: policy + " " + n}},
				{"board.instantiate", boards.InstantiateIn{ProjectID: w.project, TemplateID: "builtin:bug-tracker", Name: "T " + n}},
				{"board.import", boards.ImportIn{ProjectID: w.project, Format: "csv", Data: "list,name\nTodo,One\n", Name: "I " + n}},
			} {
				if err := callErr(w.as(n), op.name, op.in); (err == nil) != want {
					t.Errorf("%s %s as %s: err=%v, want allowed=%v", policy, op.name, n, err, want)
				}
			}
			if n == "anonymous" {
				continue
			}
			code, body := w.page(t, n, "/projects/"+w.project)
			has := strings.Contains(body, "/boards/new")
			if code == 200 && has != want {
				t.Errorf("%s project page as %s: create entry %v, want %v", policy, n, has, want)
			}
			if code != 200 && want {
				t.Errorf("%s project page as %s: %d", policy, n, code)
			}
			if code, _ := w.page(t, n, "/ui/projects/"+w.project+"/boards/new"); (code == 200) != want {
				t.Errorf("%s template gallery as %s: %d, want allowed=%v", policy, n, code, want)
			}
		}
	}
	// project.get reports the policy and the per-caller flag
	p := call[projects.Project](t, w.as("editor"), "project.get", projects.IDIn{ID: w.project})
	if p.BoardCreation != "members" || !p.CanCreateBoard || p.CanManage {
		t.Fatalf("project.get as editor: %+v", p)
	}
	bad := "everyone"
	wantErr(t, w.as("creator"), "project.update", projects.UpdateIn{ID: w.project, BoardCreation: &bad}, 422)
}

func roleOf(t *testing.T, e *ops.Env, boardID, userID string) string {
	t.Helper()
	var r string
	_ = e.DB.QueryRow(`SELECT role FROM board_memberships WHERE board_id=? AND user_id=?`, boardID, userID).Scan(&r)
	return r
}

func memberID(t *testing.T, e *ops.Env, boardID, userID string) string {
	t.Helper()
	var id string
	_ = e.DB.QueryRow(`SELECT id FROM board_memberships WHERE board_id=? AND user_id=?`, boardID, userID).Scan(&id)
	return id
}

func TestBoardOwners(t *testing.T) {
	w := newOwnershipWorld(t)
	w.setPolicy(t, authz.BoardCreationMembers)
	// a project manager who creates a board stays an editor member; a member who creates one becomes its owner
	pmBoard := call[boards.Board](t, w.as("pm"), "board.create", boards.BoardCreateIn{ProjectID: w.project, Name: "PM board"})
	if r := roleOf(t, w.e, pmBoard.ID, "pm"); r != "editor" {
		t.Fatalf("manager's board role %q", r)
	}
	own := call[boards.Board](t, w.as("editor"), "board.create", boards.BoardCreateIn{ProjectID: w.project, Name: "Editor's own"})
	if r := roleOf(t, w.e, own.ID, "editor"); r != "owner" {
		t.Fatalf("creator's board role %q, want owner", r)
	}
	tpl := call[boards.Board](t, w.as("viewer"), "board.instantiate", boards.InstantiateIn{ProjectID: w.project, TemplateID: "builtin:bug-tracker", Name: "From template"})
	if r := roleOf(t, w.e, tpl.ID, "viewer"); r != "owner" {
		t.Fatalf("template creator's role %q, want owner", r)
	}
	oe := w.as("editor")
	if l := authz.BoardLevel(oe, own.ID); l != authz.Manage {
		t.Fatalf("owner level %v", l)
	}
	if l := authz.BoardLevel(oe, w.board); l != authz.Edit {
		t.Fatalf("an owner of one board is only an editor on another: %v", l)
	}
	if l := authz.ProjectLevel(oe, w.project); l != authz.View {
		t.Fatalf("an owner gets project View only: %v", l)
	}
	if !authz.IsBoardMember(oe, own.ID) || !authz.ViewersAmong(w.e, own.ID, []string{"editor"})["editor"] {
		t.Fatal("owner must count as a member and a viewer")
	}
	if lv := authz.BoardLevels(oe, []string{own.ID, w.board}); lv[own.ID] != authz.Manage || lv[w.board] != authz.Edit {
		t.Fatalf("BoardLevels %v", lv)
	}
	// manages their own board: settings, members, sharing
	name := "Renamed own"
	prompt := "Our board"
	call[boards.Board](t, oe, "board.update", boards.BoardUpdateIn{ID: own.ID, Name: &name, AgentPrompt: &prompt})
	m := call[boards.Member](t, oe, "board.member.add", boards.MemberAddIn{BoardID: own.ID, UserID: "outsider", Role: "viewer"})
	call[boards.Board](t, oe, "board.setPublic", boards.BoardPublicIn{ID: own.ID, IsPublic: true})
	// ...but not other boards, the project, or the project's board order
	if err := callErr(oe, "board.member.add", boards.MemberAddIn{BoardID: w.board, UserID: "outsider"}); err == nil {
		t.Fatal("owner managed another board")
	}
	if err := callErr(oe, "board.member.add", boards.MemberAddIn{BoardID: pmBoard.ID, UserID: "outsider"}); err == nil {
		t.Fatal("owner managed a board they are not on")
	}
	wantErr(t, oe, "project.update", projects.UpdateIn{ID: w.project, Name: &name}, 403)
	wantErr(t, oe, "project.manager.add", projects.ManagerAddIn{ID: w.project, UserID: "outsider"}, 403)
	wantErr(t, oe, "board.move", boards.BoardMoveIn{ID: own.ID, Position: new(float64)}, 403)
	pos := 5.0
	wantErr(t, oe, "board.update", boards.BoardUpdateIn{ID: own.ID, Position: &pos}, 403)

	// the owner role is for active people only
	ownerRole := boards.MemberAddIn{BoardID: own.ID, Role: "owner"}
	for _, who := range []string{"guest", "agent"} {
		in := ownerRole
		in.UserID = who
		wantErr(t, oe, "board.member.add", in, 422)
	}
	if _, err := w.e.DB.Exec(`INSERT INTO user_groups(id,name,created_at) VALUES('g1','Team',?)`, db.Now()); err != nil {
		t.Fatal(err)
	}
	wantErr(t, oe, "board.member.add", boards.MemberAddIn{BoardID: own.ID, GroupID: "g1", Role: "owner"}, 422)
	wantErr(t, oe, "board.member.add", boards.MemberAddIn{BoardID: own.ID, UserID: "commenter", Role: "boss"}, 422)
	if _, err := ops.Typed[boards.Member](w.as("guest"), "board.member.update", boards.MemberUpdateIn{ID: w.memberOf["guest"], Role: "owner"}); err == nil {
		t.Fatal("a guest promoted themself")
	}
	wantErr(t, w.as("creator"), "board.member.update", boards.MemberUpdateIn{ID: w.memberOf["guest"], Role: "owner"}, 422)
	wantErr(t, w.as("creator"), "board.member.update", boards.MemberUpdateIn{ID: w.memberOf["agent"], Role: "owner"}, 422)

	// last-owner rule: the only owner cannot leave or step down; with a second owner they can
	ownID := memberID(t, w.e, own.ID, "editor")
	wantErr(t, oe, "board.member.remove", boards.MemberRefIn{ID: ownID}, 409)
	wantErr(t, oe, "board.member.update", boards.MemberUpdateIn{ID: ownID, Role: "editor"}, 409)
	call[boards.Member](t, oe, "board.member.update", boards.MemberUpdateIn{ID: m.ID, Role: "owner"})
	if l := authz.BoardLevel(w.as("outsider"), own.ID); l != authz.Manage {
		t.Fatalf("promoted owner level %v", l)
	}
	call[boards.Member](t, oe, "board.member.update", boards.MemberUpdateIn{ID: ownID, Role: "editor"})
	if l := authz.BoardLevel(oe, own.ID); l != authz.Edit {
		t.Fatalf("stepped-down owner level %v", l)
	}
	// a project manager may remove the last owner (the project managers still manage the board)
	call[boards.Member](t, w.as("pm"), "board.member.remove", boards.MemberRefIn{ID: m.ID})
	if l := authz.BoardLevel(w.as("pm"), own.ID); l != authz.Manage {
		t.Fatalf("project manager level %v", l)
	}
	// owner rows never travel in an export
	out, err := ops.Call(w.as("pm"), "board.export", &boards.ExportIn{ID: tpl.ID})
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := out.(*ops.Stream); !ok || strings.Contains(string(st.Data), `"owner"`) || strings.Contains(string(st.Data), "viewer@x.io") {
		t.Fatalf("export leaks memberships: %T", out)
	}

	// the board settings members section offers the Owner role for people (not groups or agents)
	code, body := w.page(t, "creator", "/boards/"+w.board+"/settings?section=members")
	if code != 200 || !strings.Contains(body, `<option value="owner"`) {
		t.Fatalf("settings members: %d owner option missing", code)
	}
	agentSel := `id="bs-role-` + w.memberOf["agent"] + `" name="role"><option value="owner"`
	if strings.Contains(body, agentSel) {
		t.Fatal("agents must not be offered the owner role")
	}
}

// A brand-new user with no projects creates a project, a board and a card, end to end through the ops.
func TestNewUserCreatesProjectBoardCard(t *testing.T) {
	e := testutil.Env(t, nil)
	e.Svc["config"] = config.Config{DataDir: t.TempDir()}
	u := testutil.As(e, addUser(t, e, "newbie", "boardUser"))
	mux := http.NewServeMux()
	web.Mount(mux, func(r *http.Request) *ops.Env { c := *u; c.Req = r; c.Ctx = r.Context(); return &c })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), "Create your first project") {
		t.Fatalf("empty state for a new user:\n%s", rec.Body.String())
	}
	p := call[projects.Project](t, u, "project.create", projects.CreateIn{Name: "First"})
	if !p.CanManage || !p.CanCreateBoard {
		t.Fatalf("%+v", p)
	}
	b := call[boards.Board](t, u, "board.create", boards.BoardCreateIn{ProjectID: p.ID, Name: "Main"})
	l := call[boards.List](t, u, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Todo"})
	if _, err := ops.CallMap(u, "card.create", map[string]any{"listId": l.ID, "name": "Hello"}); err != nil {
		t.Fatalf("card.create: %v", err)
	}
}
