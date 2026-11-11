package projects_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/projects"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
	"github.com/crueber/weave/internal/web"
)

func addUser(t *testing.T, e *ops.Env, id, role string) *ops.Principal {
	t.Helper()
	_, err := e.DB.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,?,?)`, id, id+"@x.io", "User "+id, role, db.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &ops.Principal{UserID: id, Role: role, Name: "User " + id}
}

func setup(t *testing.T) (*ops.Env, *ops.Principal, *ops.Principal, *ops.Principal) {
	e := testutil.Env(t, nil)
	e.Svc["config"] = config.Config{DataDir: t.TempDir()}
	admin := addUser(t, e, "admin1", "admin")
	owner := addUser(t, e, "owner1", "projectOwner")
	plain := addUser(t, e, "plain1", "boardUser")
	return testutil.As(e, owner), admin, owner, plain
}

func call[T any](t *testing.T, e *ops.Env, op string, in any) T {
	t.Helper()
	out, err := ops.Typed[T](e, op, in)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return out
}

func wantErr(t *testing.T, e *ops.Env, op string, in any, status int) {
	t.Helper()
	pv := reflect.New(reflect.TypeOf(in))
	pv.Elem().Set(reflect.ValueOf(in))
	_, err := ops.Call(e, op, pv.Interface())
	oe, ok := err.(*ops.Error)
	if !ok || oe.Status != status {
		t.Fatalf("%s: want status %d, got %v", op, status, err)
	}
}

func TestProjectLifecycle(t *testing.T) {
	e, admin, owner, plain := setup(t)
	wantErr(t, testutil.As(e, plain), "project.create", projects.CreateIn{Name: "x"}, 403)
	wantErr(t, e, "project.create", projects.CreateIn{Name: "  "}, 422)

	p := call[projects.Project](t, e, "project.create", projects.CreateIn{Name: "Alpha", Description: "d"})
	if !p.CanManage || len(p.ManagerUserIDs) != 1 || p.ManagerUserIDs[0] != owner.UserID || p.OwnerManagerID == "" {
		t.Fatalf("creator should be manager/owner: %+v", p)
	}
	// plain user cannot see or modify
	wantErr(t, testutil.As(e, plain), "project.get", projects.IDIn{ID: p.ID}, 404)
	// update
	name, hidden, labels := "Beta", true, true
	u := call[projects.Project](t, e, "project.update", projects.UpdateIn{ID: p.ID, Name: &name, IsHidden: &hidden, UseProjectLabels: &labels})
	if u.Name != "Beta" || !u.IsHidden || !u.UseProjectLabels {
		t.Fatalf("update: %+v", u)
	}
	empty := ""
	u = call[projects.Project](t, e, "project.update", projects.UpdateIn{ID: p.ID, Description: &empty})
	if u.Description != "" {
		t.Fatal("description should clear")
	}
	// hidden excluded unless asked
	l := call[[]projects.Project](t, e, "project.list", projects.ListIn{})
	if len(l) != 0 {
		t.Fatalf("hidden project listed: %v", l)
	}
	l = call[[]projects.Project](t, e, "project.list", projects.ListIn{Hidden: true})
	if len(l) != 1 {
		t.Fatal("hidden project missing with hidden=true")
	}
	// admin sees all
	if l = call[[]projects.Project](t, testutil.As(e, admin), "project.list", projects.ListIn{Hidden: true}); len(l) != 1 {
		t.Fatal("admin should see all projects")
	}
	// favorites
	f := call[projects.Project](t, e, "project.favorite", projects.IDIn{ID: p.ID})
	if !f.IsFavorite {
		t.Fatal("favorite not set")
	}
	f = call[projects.Project](t, e, "project.unfavorite", projects.IDIn{ID: p.ID})
	if f.IsFavorite {
		t.Fatal("favorite not cleared")
	}
	// search
	if l = call[[]projects.Project](t, e, "project.list", projects.ListIn{Hidden: true, Search: "zzz"}); len(l) != 0 {
		t.Fatal("search should filter")
	}
	// delete
	wantErr(t, testutil.As(e, plain), "project.delete", projects.IDIn{ID: p.ID}, 404)
	call[projects.DeleteOut](t, e, "project.delete", projects.IDIn{ID: p.ID})
	wantErr(t, e, "project.get", projects.IDIn{ID: p.ID}, 404)
}

func TestBackgroundValidation(t *testing.T) {
	e, _, _, _ := setup(t)
	p := call[projects.Project](t, e, "project.create", projects.CreateIn{Name: "P"})
	s := func(v string) *string { return &v }
	wantErr(t, e, "project.update", projects.UpdateIn{ID: p.ID, BackgroundType: s("color")}, 422)
	wantErr(t, e, "project.update", projects.UpdateIn{ID: p.ID, BackgroundType: s("color"), BackgroundColor: s("red;x")}, 422)
	wantErr(t, e, "project.update", projects.UpdateIn{ID: p.ID, BackgroundType: s("gradient"), BackgroundGradient: s("nope")}, 422)
	wantErr(t, e, "project.update", projects.UpdateIn{ID: p.ID, BackgroundType: s("image"), BackgroundImageID: s("zzz")}, 404)
	u := call[projects.Project](t, e, "project.update", projects.UpdateIn{ID: p.ID, BackgroundType: s("color"), BackgroundColor: s("#1d6fa5")})
	if u.BackgroundType != "color" || !strings.Contains(string(projects.BackgroundCSS(u)), "#1d6fa5") {
		t.Fatalf("%+v", u)
	}
	u = call[projects.Project](t, e, "project.update", projects.UpdateIn{ID: p.ID, BackgroundType: s("gradient"), BackgroundGradient: s("ocean-dive")})
	if !strings.Contains(string(projects.BackgroundCSS(u)), "background:#") || strings.Contains(string(projects.BackgroundCSS(u)), "gradient") {
		t.Fatal("gradient css")
	}
	u = call[projects.Project](t, e, "project.update", projects.UpdateIn{ID: p.ID, BackgroundType: s("")})
	if u.BackgroundType != "" {
		t.Fatal("clear background")
	}
}

func pngBytes(w, h int) []byte {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		im.Set(x, 0, color.RGBA{200, 10, 10, 255})
	}
	var b bytes.Buffer
	_ = png.Encode(&b, im)
	return b.Bytes()
}

func TestBackgroundImages(t *testing.T) {
	e, _, _, plain := setup(t)
	p := call[projects.Project](t, e, "project.create", projects.CreateIn{Name: "P"})
	wantErr(t, e, "project.background.upload", projects.BackgroundUploadIn{ID: p.ID, File: &ops.File{Name: "a.txt", Data: []byte("hello")}}, 422)
	img := call[projects.BackgroundImage](t, e, "project.background.upload", projects.BackgroundUploadIn{
		ID: p.ID, Apply: true, File: &ops.File{Name: "bg.png", Data: pngBytes(1200, 600)}})
	if img.Width != 1200 || img.Height != 600 {
		t.Fatalf("dims %+v", img)
	}
	root, _ := projects.FilesDir(e)
	var found int
	_ = filepath.WalkDir(filepath.Join(root, "backgrounds"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found++
		}
		return nil
	})
	if found != 2 {
		t.Fatalf("expected original+thumbnail on disk, found %d", found)
	}
	pp := call[projects.Project](t, e, "project.get", projects.IDIn{ID: p.ID})
	if pp.BackgroundType != "image" || pp.BackgroundImageID != img.ID {
		t.Fatalf("apply failed: %+v", pp)
	}
	th := call[*ops.Stream](t, e, "project.background.file", projects.BackgroundFileIn{ID: p.ID, ImageID: img.ID, Variant: "thumbnail"})
	cfg, _, err := image.DecodeConfig(bytes.NewReader(th.Data))
	if err != nil || cfg.Width != 480 || cfg.Height != 240 {
		t.Fatalf("thumbnail %v %v", cfg, err)
	}
	wantErr(t, testutil.As(e, plain), "project.background.file", projects.BackgroundFileIn{ID: p.ID, ImageID: img.ID, Variant: "original"}, 404)
	wantErr(t, e, "project.background.file", projects.BackgroundFileIn{ID: p.ID, ImageID: img.ID, Variant: "huge"}, 422)

	call[projects.DeleteOut](t, e, "project.background.delete", projects.BackgroundIn{ID: p.ID, ImageID: img.ID})
	pp = call[projects.Project](t, e, "project.get", projects.IDIn{ID: p.ID})
	if pp.BackgroundImageID != "" || pp.BackgroundType != "" {
		t.Fatalf("background not cleared on delete: %+v", pp)
	}
	found = 0
	_ = filepath.WalkDir(filepath.Join(root, "backgrounds"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found++
		}
		return nil
	})
	if found != 0 {
		t.Fatal("files should be removed")
	}
}

func TestManagers(t *testing.T) {
	e, admin, _, plain := setup(t)
	owner2 := addUser(t, e, "owner2", "projectOwner")
	p := call[projects.Project](t, e, "project.create", projects.CreateIn{Name: "P"})

	wantErr(t, e, "project.manager.add", projects.ManagerAddIn{ID: p.ID, UserID: plain.UserID}, 422) // role not allowed
	wantErr(t, e, "project.manager.add", projects.ManagerAddIn{ID: p.ID, UserID: "ghost"}, 404)
	m := call[projects.Manager](t, e, "project.manager.add", projects.ManagerAddIn{ID: p.ID, UserID: owner2.UserID})
	wantErr(t, e, "project.manager.add", projects.ManagerAddIn{ID: p.ID, UserID: owner2.UserID}, 409)
	// new manager now has manage rights
	name := "Renamed"
	call[projects.Project](t, testutil.As(e, owner2), "project.update", projects.UpdateIn{ID: p.ID, Name: &name})
	ms := call[[]projects.Manager](t, e, "project.manager.list", projects.IDIn{ID: p.ID})
	if len(ms) != 2 || !ms[0].IsOwner {
		t.Fatalf("%+v", ms)
	}
	// non-owner manager cannot remove the owning manager
	wantErr(t, testutil.As(e, owner2), "project.manager.remove", projects.ManagerRemoveIn{ID: p.ID, ManagerID: ms[0].ID}, 403)
	call[projects.DeleteOut](t, e, "project.manager.remove", projects.ManagerRemoveIn{ID: p.ID, ManagerID: m.ID})
	// last manager cannot be removed
	wantErr(t, testutil.As(e, admin), "project.manager.remove", projects.ManagerRemoveIn{ID: p.ID, ManagerID: ms[0].ID}, 409)
	// removed manager loses access
	wantErr(t, testutil.As(e, owner2), "project.get", projects.IDIn{ID: p.ID}, 404)
}

func TestBaseCustomFields(t *testing.T) {
	e, _, _, plain := setup(t)
	p := call[projects.Project](t, e, "project.create", projects.CreateIn{Name: "P"})
	wantErr(t, testutil.As(e, plain), "project.fieldgroup.create", projects.GroupCreateIn{ID: p.ID, Name: "G"}, 404)
	g := call[projects.BaseGroup](t, e, "project.fieldgroup.create", projects.GroupCreateIn{ID: p.ID, Name: "Specs"})
	wantErr(t, e, "project.field.create", projects.FieldCreateIn{ID: g.ID, Name: "F", Type: "bogus"}, 422)
	wantErr(t, e, "project.field.create", projects.FieldCreateIn{ID: g.ID, Name: "F", Type: "select"}, 422)
	f1 := call[projects.BaseField](t, e, "project.field.create", projects.FieldCreateIn{ID: g.ID, Name: "Size", Type: "select", Options: []string{"S", " M ", ""}})
	f2 := call[projects.BaseField](t, e, "project.field.create", projects.FieldCreateIn{ID: g.ID, Name: "Note"})
	if f2.Type != "text" || f2.Position <= f1.Position || len(f1.Options) != 2 || f1.Options[1] != "M" {
		t.Fatalf("%+v %+v", f1, f2)
	}
	nm := "Shirt size"
	typ := "text"
	f1 = call[projects.BaseField](t, e, "project.field.update", projects.FieldUpdateIn{ID: f1.ID, Name: &nm, Type: &typ})
	if f1.Name != nm || f1.Type != "text" {
		t.Fatalf("%+v", f1)
	}
	gs := call[[]projects.BaseGroup](t, e, "project.fieldgroup.list", projects.IDIn{ID: p.ID})
	if len(gs) != 1 || len(gs[0].Fields) != 2 {
		t.Fatalf("%+v", gs)
	}
	wantErr(t, testutil.As(e, plain), "project.field.delete", projects.FieldIn{ID: f1.ID}, 404)
	call[projects.DeleteOut](t, e, "project.field.delete", projects.FieldIn{ID: f1.ID})
	call[projects.BaseGroup](t, e, "project.fieldgroup.update", projects.GroupUpdateIn{ID: g.ID, Name: "Renamed"})
	call[projects.DeleteOut](t, e, "project.fieldgroup.delete", projects.GroupIn{ID: g.ID})
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM custom_fields WHERE base_group_id=?`, g.ID).Scan(&n)
	if n != 0 {
		t.Fatal("fields should cascade")
	}
}

// TestUI drives the real HTMX routes through web.Mount.
func TestUI(t *testing.T) {
	e, _, owner, _ := setup(t)
	mux := http.NewServeMux()
	web.Mount(mux, func(r *http.Request) *ops.Env { c := *e; c.User = owner; c.Req = r; c.Ctx = r.Context(); return &c })
	do := func(method, path string, body url.Values, hx bool) *httptest.ResponseRecorder {
		var rd *strings.Reader
		if body != nil {
			rd = strings.NewReader(body.Encode())
		} else {
			rd = strings.NewReader("")
		}
		r := httptest.NewRequest(method, path, rd)
		if body != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if hx {
			r.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	ok := func(w *httptest.ResponseRecorder, want string) {
		t.Helper()
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("code %d, want %q in %s", w.Code, want, w.Body.String())
		}
	}
	ok(do("GET", "/", nil, false), "You have no projects yet")
	ok(do("GET", "/ui/projects/new", nil, true), "Create project")
	w := do("POST", "/ui/projects", url.Values{"name": {"UI Proj"}}, true)
	loc := w.Header().Get("HX-Redirect")
	if !strings.HasPrefix(loc, "/projects/") {
		t.Fatalf("redirect %q (%d %s)", loc, w.Code, w.Body.String())
	}
	id := strings.TrimPrefix(loc, "/projects/")
	ok(do("GET", "/", nil, false), "UI Proj")
	ok(do("GET", loc, nil, false), "Project settings")
	ok(do("GET", "/ui/projects/"+id+"/settings", nil, true), "Project settings")
	ok(do("POST", "/ui/projects/"+id+"/favorite?ctx=home", nil, true), "Favorites")
	ok(do("DELETE", "/ui/projects/"+id+"/favorite?ctx=page", nil, true), "Add to favorites")
	ok(do("POST", "/ui/projects/"+id, url.Values{"tab": {"general"}, "useProjectLabels": {"true"}}, true), "Project-wide labels: <b>on")
	ok(do("POST", "/ui/projects/"+id, url.Values{"tab": {"background"}, "backgroundType": {"gradient"}, "backgroundGradient": {"ocean-dive"}}, true), "projects-swatch on")
	ok(do("GET", "/ui/projects/"+id+"/settings/managers", nil, true), "Add manager")
	ok(do("POST", "/ui/projects/"+id+"/fieldgroups", url.Values{"name": {"G1"}}, true), "G1")
	gs := call[[]projects.BaseGroup](t, e, "project.fieldgroup.list", projects.IDIn{ID: id})
	ok(do("POST", "/ui/fieldgroups/"+gs[0].ID+"/fields?pid="+id, url.Values{"name": {"Pick"}, "type": {"select"}, "options": {"a, b"}}, true), "select: a, b")

	// multipart upload through the UI
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("apply", "true")
	fw, _ := mw.CreateFormFile("file", "x.png")
	_, _ = fw.Write(pngBytes(64, 64))
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/ui/projects/"+id+"/background-images", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/thumbnail") {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	imgs := call[[]projects.BackgroundImage](t, e, "project.background.list", projects.IDIn{ID: id})
	rec = do("GET", "/projects/"+id+"/background-images/"+imgs[0].ID+"/thumbnail", nil, false)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb route %d", rec.Code)
	}
	w = do("DELETE", "/ui/projects/"+id, nil, true)
	if w.Header().Get("HX-Redirect") != "/" {
		t.Fatalf("delete redirect: %d", w.Code)
	}
}
