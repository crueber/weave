package projects_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/projects"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
	"github.com/crueber/weave/internal/web"
)

// orphanWorld: projects in every manager state, created directly in SQL (the ops refuse most of these states).
type orphanWorld struct {
	e     *ops.Env
	admin *ops.Principal
	ids   map[string]string // project label -> id
}

func addManagerRow(t *testing.T, e *ops.Env, projectID, userID string) string {
	t.Helper()
	id := db.NewID()
	if _, err := e.DB.Exec(`INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES(?,?,?,?)`, id, projectID, userID, db.Now()); err != nil {
		t.Fatal(err)
	}
	return id
}

func addProjectRow(t *testing.T, e *ops.Env, name string) string {
	t.Helper()
	id := db.NewID()
	if _, err := e.DB.Exec(`INSERT INTO projects(id,name,created_at) VALUES(?,?,?)`, id, name, db.Now()); err != nil {
		t.Fatal(err)
	}
	return id
}

func addBoardRow(t *testing.T, e *ops.Env, projectID, name string) string {
	t.Helper()
	id := db.NewID()
	if _, err := e.DB.Exec(`INSERT INTO boards(id,project_id,position,name,created_at) VALUES(?,?,?,?,?)`, id, projectID, 65536, name, db.Now()); err != nil {
		t.Fatal(err)
	}
	return id
}

func addMembershipRow(t *testing.T, e *ops.Env, boardID, userID, groupID, role string) {
	t.Helper()
	var u, g any
	if userID != "" {
		u = userID
	}
	if groupID != "" {
		g = groupID
	}
	if _, err := e.DB.Exec(`INSERT INTO board_memberships(id,board_id,user_id,group_id,role,created_at) VALUES(?,?,?,?,?,?)`, db.NewID(), boardID, u, g, role, db.Now()); err != nil {
		t.Fatal(err)
	}
}

func newOrphanWorld(t *testing.T) *orphanWorld {
	e, admin, _, _ := setup(t)
	w := &orphanWorld{e: testutil.As(e, admin), admin: admin, ids: map[string]string{}}
	addUser(t, e, "deact1", "boardUser")
	addUser(t, e, "deact2", "boardUser")
	addUser(t, e, "softdel", "boardUser")
	addUser(t, e, "harddel", "boardUser")
	addUser(t, e, "active1", "boardUser")
	addUser(t, e, "guestm", "guest")
	addUser(t, e, "agentm", "boardUser")
	addUser(t, e, "owner2", "boardUser")
	_, _ = e.DB.Exec(`UPDATE users SET is_deactivated=1 WHERE id IN ('deact1','deact2')`)
	_, _ = e.DB.Exec(`UPDATE users SET deleted_at=? WHERE id='softdel'`, db.Now())
	_, _ = e.DB.Exec(`UPDATE users SET is_agent=1 WHERE id='agentm'`)

	w.ids["none"] = addProjectRow(t, e, "No managers")
	w.ids["deactivated"] = addProjectRow(t, e, "All deactivated")
	addManagerRow(t, e, w.ids["deactivated"], "deact1")
	addManagerRow(t, e, w.ids["deactivated"], "deact2")
	w.ids["softdeleted"] = addProjectRow(t, e, "Soft deleted")
	addManagerRow(t, e, w.ids["softdeleted"], "softdel")
	w.ids["harddeleted"] = addProjectRow(t, e, "Hard deleted")
	addManagerRow(t, e, w.ids["harddeleted"], "harddel")
	if _, err := e.DB.Exec(`DELETE FROM users WHERE id='harddel'`); err != nil { // cascades the manager row
		t.Fatal(err)
	}
	w.ids["mix"] = addProjectRow(t, e, "Mixed")
	addManagerRow(t, e, w.ids["mix"], "deact1")
	addManagerRow(t, e, w.ids["mix"], "active1")
	w.ids["guestagent"] = addProjectRow(t, e, "Guest and agent")
	addManagerRow(t, e, w.ids["guestagent"], "guestm")
	addManagerRow(t, e, w.ids["guestagent"], "agentm")
	// boards: the deactivated project has one board with an active owner and one without
	b1 := addBoardRow(t, e, w.ids["deactivated"], "Owned board")
	addMembershipRow(t, e, b1, "owner2", "", "owner")
	b2 := addBoardRow(t, e, w.ids["deactivated"], "Lost board")
	addMembershipRow(t, e, b2, "deact1", "", "owner") // an inactive owner does not count
	addMembershipRow(t, e, b2, "active1", "", "editor")
	return w
}

func TestOwnerlessDetection(t *testing.T) {
	w := newOrphanWorld(t)
	got := call[[]projects.Orphan](t, w.e, "project.orphans", struct{}{})
	by := map[string]projects.Orphan{}
	for _, o := range got {
		by[o.ID] = o
	}
	for _, k := range []string{"none", "deactivated", "softdeleted", "harddeleted", "guestagent"} {
		if _, ok := by[w.ids[k]]; !ok {
			t.Errorf("%s: want ownerless, got %+v", k, got)
		}
	}
	if _, ok := by[w.ids["mix"]]; ok {
		t.Error("a project with one active manager is not ownerless")
	}
	if len(got) != 5 {
		t.Errorf("want exactly 5 orphans, got %d", len(got))
	}
	d := by[w.ids["deactivated"]]
	if d.Boards != 2 || d.BoardsWithoutOwner != 1 || len(d.FormerManagers) != 2 || d.FormerManagers[0].State != "deactivated" || d.FormerManagers[0].Email != "deact1@x.io" {
		t.Errorf("deactivated project: %+v", d)
	}
	if s := by[w.ids["softdeleted"]]; len(s.FormerManagers) != 1 || s.FormerManagers[0].State != "deleted" {
		t.Errorf("soft-deleted: %+v", s)
	}
	if h := by[w.ids["harddeleted"]]; len(h.FormerManagers) != 0 {
		t.Errorf("hard-deleted users leave no rows: %+v", h)
	}
	states := map[string]bool{}
	for _, m := range by[w.ids["guestagent"]].FormerManagers {
		states[m.State] = true
	}
	if !states["guest"] || !states["agent"] {
		t.Errorf("guest/agent managers do not count: %+v", by[w.ids["guestagent"]])
	}
	// the listing carries no content
	for _, no := range []string{"Owned board", "Lost board"} {
		for _, o := range got {
			if strings.Contains(o.Name, no) {
				t.Errorf("board name %q leaked", no)
			}
		}
	}
	if ok, _ := projects.IsOwnerless(context.Background(), w.e.DB, w.ids["mix"]); ok {
		t.Error("IsOwnerless(mix)")
	}
	if ok, _ := projects.IsOwnerless(context.Background(), w.e.DB, w.ids["none"]); !ok {
		t.Error("IsOwnerless(none)")
	}
	if ok, _ := projects.IsOwnerless(context.Background(), w.e.DB, "nope"); ok {
		t.Error("an unknown project is not ownerless")
	}
}

func TestReassignRules(t *testing.T) {
	w := newOrphanWorld(t)
	e := w.e
	// callers: only human admins
	for _, p := range []*ops.Principal{{UserID: "active1", Role: "boardUser"}, {UserID: "guestm", Role: "guest"}, {UserID: "agentadm", Role: "admin", IsAgent: true}} {
		wantErr(t, testutil.As(e, p), "project.reassign", projects.ReassignIn{ID: w.ids["none"], UserID: "active1"}, 403)
		wantErr(t, testutil.As(e, p), "project.orphans", struct{}{}, 403)
		wantErr(t, testutil.As(e, p), "project.recovery.list", projects.RecoveryListIn{}, 403)
	}
	wantErr(t, testutil.As(e, nil), "project.orphans", struct{}{}, 401)
	// targets: deactivated, guest, agent, deleted, unknown
	wantErr(t, e, "project.reassign", projects.ReassignIn{ID: w.ids["none"], UserID: "deact1"}, 422)
	wantErr(t, e, "project.reassign", projects.ReassignIn{ID: w.ids["none"], UserID: "guestm"}, 422)
	wantErr(t, e, "project.reassign", projects.ReassignIn{ID: w.ids["none"], UserID: "agentm"}, 422)
	wantErr(t, e, "project.reassign", projects.ReassignIn{ID: w.ids["none"], UserID: "softdel"}, 404)
	wantErr(t, e, "project.reassign", projects.ReassignIn{ID: w.ids["none"], UserID: "nobody"}, 404)
	wantErr(t, e, "project.reassign", projects.ReassignIn{ID: "nope", UserID: "active1"}, 404)
	// a project with an active manager is not an orphan: refusing it is the point (no admin takeover)
	wantErr(t, e, "project.reassign", projects.ReassignIn{ID: w.ids["mix"], UserID: "owner2"}, 409)
	if n := len(call[[]projects.Recovery](t, e, "project.recovery.list", projects.RecoveryListIn{})); n != 0 {
		t.Fatalf("refusals must not write audit rows, got %d", n)
	}

	// success: realtime to the new manager, a manager row, the owning manager moves, an audit row
	sub, stop := e.Hub.Subscribe(func(room string) bool { return room == "user:owner2" || room == "global" })
	defer stop()
	r := call[projects.Recovery](t, e, "project.reassign", projects.ReassignIn{ID: w.ids["deactivated"], UserID: "owner2"})
	if r.ProjectName != "All deactivated" || r.UserID != "owner2" || r.ActorID != w.admin.UserID || r.ActorEmail != "admin1@x.io" || r.Source != "admin" || len(r.Previous) != 2 {
		t.Fatalf("recovery: %+v", r)
	}
	var owner, ownerUser string
	_ = e.DB.QueryRow(`SELECT owner_manager_id FROM projects WHERE id=?`, w.ids["deactivated"]).Scan(&owner)
	_ = e.DB.QueryRow(`SELECT user_id FROM project_managers WHERE id=?`, owner).Scan(&ownerUser)
	if ownerUser != "owner2" {
		t.Errorf("owning manager should move to the new manager, got %q", ownerUser)
	}
	if ok, _ := projects.IsOwnerless(context.Background(), e.DB, w.ids["deactivated"]); ok {
		t.Error("still ownerless after reassign")
	}
	// the new manager can now manage it through the normal path
	if err := callErr(testutil.As(e, &ops.Principal{UserID: "owner2", Role: "boardUser"}), "project.manager.add", projects.ManagerAddIn{ID: w.ids["deactivated"], UserID: "active1"}); err != nil {
		t.Fatalf("new manager cannot add a manager: %v", err)
	}
	// a second reassign is refused now that it has an active manager
	wantErr(t, e, "project.reassign", projects.ReassignIn{ID: w.ids["deactivated"], UserID: "active1"}, 409)
	names := map[string]bool{}
	for done := false; !done; {
		select {
		case ev := <-sub:
			names[ev.Room+" "+ev.Name] = true
		default:
			done = true
		}
	}
	if !names["user:owner2 projectManager.created"] || !names["global project.reassigned"] {
		t.Errorf("events: %v", names)
	}

	// the audit trail
	rs := call[[]projects.Recovery](t, e, "project.recovery.list", projects.RecoveryListIn{})
	if len(rs) != 1 || rs[0].ID != r.ID || rs[0].UserEmail != "owner2@x.io" || len(rs[0].Previous) != 2 || rs[0].Previous[0].State != "deactivated" {
		t.Fatalf("audit: %+v", rs)
	}
	// it survives the project and the people
	_, _ = e.DB.Exec(`DELETE FROM projects WHERE id=?`, w.ids["deactivated"])
	_, _ = e.DB.Exec(`DELETE FROM users WHERE id='owner2'`)
	if rs := call[[]projects.Recovery](t, e, "project.recovery.list", projects.RecoveryListIn{ProjectID: w.ids["deactivated"]}); len(rs) != 1 || rs[0].ProjectName != "All deactivated" {
		t.Fatalf("audit must outlive the project: %+v", rs)
	}
}

func TestAdoptHostCommand(t *testing.T) {
	w := newOrphanWorld(t)
	ctx := context.Background()
	// works on a project that still has an active manager (the host operator's break-glass)
	r, err := projects.Adopt(ctx, w.e.DB, w.ids["mix"], "OWNER2@x.io")
	if err != nil || r.Source != "host" || r.ActorID != "" || r.UserID != "owner2" {
		t.Fatalf("adopt: %+v %v", r, err)
	}
	var owner, ownerUser string
	_ = w.e.DB.QueryRow(`SELECT COALESCE(owner_manager_id,'') FROM projects WHERE id=?`, w.ids["mix"]).Scan(&owner)
	_ = w.e.DB.QueryRow(`SELECT user_id FROM project_managers WHERE id=?`, owner).Scan(&ownerUser)
	if ownerUser != "owner2" { // "Mixed" had no owning manager, so the mark goes to the adopter (an active owner keeps it)
		t.Errorf("owning manager: %q", ownerUser)
	}
	if _, err := projects.Adopt(ctx, w.e.DB, w.ids["mix"], "owner2@x.io"); err == nil {
		t.Fatal("adopting twice must fail")
	}
	for _, email := range []string{"guestm@x.io", "agentm@x.io", "deact1@x.io", "softdel@x.io", "nobody@x.io"} {
		if _, err := projects.Adopt(ctx, w.e.DB, w.ids["none"], email); err == nil {
			t.Errorf("adopt by %s must fail", email)
		}
	}
	if _, err := projects.Adopt(ctx, w.e.DB, "nope", "owner2@x.io"); err == nil {
		t.Error("unknown project")
	}
	rs, _ := projects.Recoveries(ctx, w.e.DB, "", 0)
	if len(rs) != 1 || rs[0].Source != "host" || rs[0].ProjectID != w.ids["mix"] {
		t.Fatalf("audit: %+v", rs)
	}
}

// An active owner keeps the owning-manager mark when an adopter joins.
func TestAdoptKeepsActiveOwner(t *testing.T) {
	e, admin, owner, _ := setup(t)
	p := call[projects.Project](t, e, "project.create", projects.CreateIn{Name: "Run"})
	if _, err := projects.Adopt(context.Background(), e.DB, p.ID, "admin1@x.io"); err != nil {
		t.Fatal(err)
	}
	got := call[projects.Project](t, testutil.As(e, admin), "project.get", projects.IDIn{ID: p.ID})
	if got.OwnerManagerID != p.OwnerManagerID {
		t.Fatalf("owner moved from %s to %s (creator %s)", p.OwnerManagerID, got.OwnerManagerID, owner.UserID)
	}
}

func TestPreflightReport(t *testing.T) {
	w := newOrphanWorld(t)
	e := w.e
	ctx := context.Background()
	addUser(t, e, "admin2", "admin")
	// admin1 manages "Mixed"; admin2 is a member of a board of "No managers" through a group; "Admin only" is managed by admins only
	addManagerRow(t, e, w.ids["mix"], "admin1")
	bNone := addBoardRow(t, e, w.ids["none"], "Board in none")
	_, _ = e.DB.Exec(`INSERT INTO user_groups(id,name,created_at) VALUES('g1','Team',?)`, db.Now())
	_, _ = e.DB.Exec(`INSERT INTO user_group_members(group_id,user_id) VALUES('g1','admin2')`)
	addMembershipRow(t, e, bNone, "", "g1", "viewer")
	ao := addProjectRow(t, e, "Admin only")
	addManagerRow(t, e, ao, "admin1")
	aob := addBoardRow(t, e, ao, "Admin board")
	addMembershipRow(t, e, aob, "admin2", "", "owner")
	addMembershipRow(t, e, aob, "active1", "", "editor") // editors do not manage

	r, err := projects.Preflight(ctx, e.DB, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Ownerless) != 5 || r.Projects != 7 || !r.NeedsAction() {
		t.Fatalf("ownerless %d projects %d", len(r.Ownerless), r.Projects)
	}
	reach := map[string]map[string]bool{}
	for _, a := range r.Admins {
		reach[a.UserID] = map[string]bool{}
		for _, p := range a.Unreachable {
			reach[a.UserID][p.ID] = true
		}
	}
	if len(reach) != 2 {
		t.Fatalf("want both admins: %+v", r.Admins)
	}
	if reach["admin1"][w.ids["mix"]] || reach["admin1"][ao] || !reach["admin1"][w.ids["none"]] || len(reach["admin1"]) != 5 {
		t.Errorf("admin1 unreachable: %v", reach["admin1"])
	}
	if reach["admin2"][w.ids["none"]] || reach["admin2"][ao] || !reach["admin2"][w.ids["mix"]] {
		t.Errorf("admin2 (group member of a board in 'none', owner of a board in 'Admin only'): %v", reach["admin2"])
	}
	if len(r.AdminOnlyBoards) != 1 || r.AdminOnlyBoards[0].ID != aob || len(r.AdminOnlyBoards[0].Managers) != 2 {
		t.Errorf("admin-only boards: %+v", r.AdminOnlyBoards)
	}
	one, err := projects.Preflight(ctx, e.DB, "ADMIN2@x.io")
	if err != nil || len(one.Admins) != 1 || one.Admins[0].UserID != "admin2" {
		t.Fatalf("--email: %+v %v", one.Admins, err)
	}
	if _, err := projects.Preflight(ctx, e.DB, "nobody@x.io"); err == nil {
		t.Error("unknown --email must fail")
	}
}

func TestOrphansAdminPage(t *testing.T) {
	w := newOrphanWorld(t)
	get := func(u *ops.Principal, method, path string, form url.Values) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		web.Mount(mux, func(r *http.Request) *ops.Env { c := *testutil.As(w.e, u); c.Req = r; c.Ctx = r.Context(); return &c })
		rec := httptest.NewRecorder()
		var req *http.Request
		if form != nil {
			req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		mux.ServeHTTP(rec, req)
		return rec
	}
	rec := get(w.admin, "GET", "/admin/orphans", nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "All deactivated") || !strings.Contains(body, "deact1@x.io") || !strings.Contains(body, "No recoveries yet") {
		t.Fatalf("admin page %d:\n%s", rec.Code, body)
	}
	for _, no := range []string{"Owned board", "Lost board", "Mixed"} {
		if strings.Contains(body, no) {
			t.Errorf("page shows %q", no)
		}
	}
	if !strings.Contains(body, `data-user="owner2"`) || strings.Contains(body, `data-user="guestm"`) || strings.Contains(body, `data-user="deact1"`) || strings.Contains(body, `data-user="agentm"`) {
		t.Error("picker must list only eligible people")
	}
	for _, p := range []*ops.Principal{{UserID: "active1", Role: "boardUser"}, nil} {
		if rec := get(p, "GET", "/admin/orphans", nil); rec.Code == 200 {
			t.Errorf("non-admin %v got the page", p)
		}
	}
	rec = get(w.admin, "POST", "/ui/admin/orphans/"+w.ids["none"]+"/reassign", url.Values{"userId": {"owner2"}})
	body = rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "User owner2 now manages No managers") || strings.Contains(body, "No recoveries yet") {
		t.Fatalf("reassign %d:\n%s", rec.Code, body)
	}
	rec = get(w.admin, "POST", "/ui/admin/orphans/"+w.ids["mix"]+"/reassign", url.Values{"userId": {"owner2"}})
	if !strings.Contains(rec.Body.String(), "still has an active manager") {
		t.Fatalf("refusal must show inline:\n%s", rec.Body.String())
	}
	if rec := get(&ops.Principal{UserID: "active1", Role: "boardUser"}, "POST", "/ui/admin/orphans/"+w.ids["none"]+"/reassign", url.Values{"userId": {"active1"}}); rec.Code == 200 {
		t.Error("non-admin reassign via UI")
	}
}

// Empty state.
func TestOrphansAdminPageEmpty(t *testing.T) {
	e, admin, _, _ := setup(t)
	call[projects.Project](t, e, "project.create", projects.CreateIn{Name: "Fine"})
	mux := http.NewServeMux()
	web.Mount(mux, func(r *http.Request) *ops.Env { c := *testutil.As(e, admin); c.Req = r; c.Ctx = r.Context(); return &c })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/orphans", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "No orphaned projects") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
