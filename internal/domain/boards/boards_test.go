package boards_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
	"github.com/crueber/weave/internal/web"
)

var (
	admin  = &ops.Principal{UserID: "u-admin", Role: "admin", Name: "Ada Admin"}
	editor = &ops.Principal{UserID: "u-ed", Role: "boardUser", Name: "Ed Itor"}
	viewer = &ops.Principal{UserID: "u-view", Role: "boardUser", Name: "Vi Ewer"}
	nobody = &ops.Principal{UserID: "u-none", Role: "boardUser", Name: "No Body"}
)

// fixture creates users + a project and returns the env acting as admin.
func fixture(t *testing.T) *ops.Env {
	t.Helper()
	e := testutil.Env(t, admin)
	for _, u := range []*ops.Principal{admin, editor, viewer, nobody} {
		if _, err := e.DB.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,?,'2024-01-01T00:00:00Z')`, u.UserID, u.UserID+"@x.io", u.Name, u.Role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.DB.Exec(`INSERT INTO projects(id,name,created_at) VALUES('p1','Proj','2024-01-01T00:00:00Z'),('p2','Proj2','2024-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return e
}

func call[T any](t *testing.T, e *ops.Env, op string, in any) T {
	t.Helper()
	out, err := ops.Typed[T](e, op, in)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return out
}

func mustFail(t *testing.T, e *ops.Env, op string, in any, code string) {
	t.Helper()
	_, err := ops.Call(e, op, ptr(in))
	oe, ok := err.(*ops.Error)
	if !ok || oe.Code != code {
		t.Fatalf("%s: want %s error, got %v", op, code, err)
	}
}

func ptr(in any) any {
	rv := reflect.ValueOf(in)
	if rv.Kind() == reflect.Ptr {
		return in
	}
	p := reflect.New(rv.Type())
	p.Elem().Set(rv)
	return p.Interface()
}

func newBoard(t *testing.T, e *ops.Env, name string) boards.Board {
	return call[boards.Board](t, e, "board.create", boards.BoardCreateIn{ProjectID: "p1", Name: name})
}

func addList(t *testing.T, e *ops.Env, boardID, name string) boards.List {
	return call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: boardID, Name: name})
}

func addCard(t *testing.T, e *ops.Env, boardID, listID, id, name string, pos float64, due string) {
	t.Helper()
	var dueV any
	if due != "" {
		dueV = due
	}
	if _, err := e.DB.Exec(`INSERT INTO cards(id,board_id,list_id,type,position,name,due_date,list_changed_at,created_at) VALUES(?,?,?,'project',?,?,?,'2024-01-01T00:00:00Z','2024-01-0'||?||'T00:00:00Z')`,
		id, boardID, listID, pos, name, dueV, int(pos)%9+1); err != nil {
		t.Fatal(err)
	}
}

func cardNames(t *testing.T, e *ops.Env, listID string) []string {
	rows, err := e.DB.Query(`SELECT name FROM cards WHERE list_id=? ORDER BY position`, listID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

func TestBoardLifecycle(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Roadmap")
	if b.DefaultView != "kanban" || b.ProjectID != "p1" {
		t.Fatalf("bad board %+v", b)
	}
	lists := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b.ID})
	types := map[string]bool{}
	for _, l := range lists.Lists {
		types[l.Type] = true
	}
	if !types["archive"] || !types["trash"] || len(lists.Members) != 1 || lists.Members[0].Role != "editor" {
		t.Fatalf("system lists / creator membership missing: %+v", lists)
	}
	yes := true
	view := "grid"
	name := "Roadmap 2"
	b = call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, Name: &name, DefaultView: &view, ShowCardAges: &yes})
	if b.Name != "Roadmap 2" || b.DefaultView != "grid" || !b.ShowCardAges {
		t.Fatalf("update failed %+v", b)
	}
	bad := "nope"
	mustFail(t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, DefaultView: &bad}, "validation")

	b2 := newBoard(t, e, "Second")
	moved := call[boards.Board](t, e, "board.move", boards.BoardMoveIn{ID: b2.ID, BeforeID: b.ID})
	if moved.Position >= b.Position {
		t.Fatalf("board not moved before: %v vs %v", moved.Position, b.Position)
	}
	got := call[[]boards.Board](t, e, "board.list", boards.BoardListIn{ProjectID: "p1"})
	if len(got) != 2 || got[0].ID != b2.ID {
		t.Fatalf("board.list order wrong: %+v", got)
	}

	// public
	anon := testutil.As(e, nil)
	if authz.BoardLevel(anon, b.ID) != authz.None {
		t.Fatal("anon should not see private board")
	}
	mustFail(t, anon, "board.public.get", boards.PublicGetIn{Token: "zzz"}, "not_found")
	pb := call[boards.Board](t, e, "board.setPublic", boards.BoardPublicIn{ID: b.ID, IsPublic: true})
	if !pb.IsPublic || pb.PublicToken == "" {
		t.Fatalf("not public: %+v", pb)
	}
	d := call[boards.BoardDetail](t, anon, "board.public.get", boards.PublicGetIn{Token: pb.PublicToken})
	if d.Board.ID != b.ID || d.Level != "view" || d.Board.PublicToken != "" {
		t.Fatalf("public detail %+v", d)
	}
	rot := call[boards.Board](t, e, "board.setPublic", boards.BoardPublicIn{ID: b.ID, IsPublic: true, Rotate: true})
	if rot.PublicToken == pb.PublicToken {
		t.Fatal("token not rotated")
	}
	mustFail(t, anon, "board.public.get", boards.PublicGetIn{Token: pb.PublicToken}, "not_found")
	call[boards.Board](t, e, "board.setPublic", boards.BoardPublicIn{ID: b.ID, IsPublic: false})
	mustFail(t, anon, "board.public.get", boards.PublicGetIn{Token: rot.PublicToken}, "not_found")

	// delete
	call[boards.Deleted](t, e, "board.delete", boards.BoardGetIn{ID: b2.ID})
	mustFail(t, e, "board.get", boards.BoardGetIn{ID: b2.ID}, "not_found")
}

func TestTemplates(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Tpl")
	l := addList(t, e, b.ID, "Todo")
	lab := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "Bug", Color: "berry-red"})
	addCard(t, e, b.ID, l.ID, "c1", "Card one", 65536, "")
	if _, err := e.DB.Exec(`INSERT INTO card_labels VALUES('c1',?)`, lab.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO task_lists(id,card_id,position,name,created_at) VALUES('tl1','c1',1,'Steps','x');
		INSERT INTO tasks(id,task_list_id,position,name,is_completed,created_at) VALUES('t1','tl1',1,'one',1,'x')`); err != nil {
		t.Fatal(err)
	}
	mustFail(t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: b.ID, Name: "X"}, "validation")
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: b.ID, IsTemplate: true})
	ts := call[[]boards.Board](t, e, "board.template.list", boards.TemplateListIn{})
	if len(ts) != 1 {
		t.Fatalf("templates: %+v", ts)
	}
	nb := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: b.ID, Name: "From tpl"})
	if nb.ProjectID != "p2" || nb.IsTemplate {
		t.Fatalf("bad instance %+v", nb)
	}
	d := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: nb.ID})
	var n, done int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE board_id=?`, nb.ID).Scan(&n)
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM tasks t JOIN task_lists tl ON tl.id=t.task_list_id JOIN cards c ON c.id=tl.card_id WHERE c.board_id=? AND t.is_completed=1`, nb.ID).Scan(&done)
	var cl int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_labels cl JOIN cards c ON c.id=cl.card_id WHERE c.board_id=?`, nb.ID).Scan(&cl)
	if len(d.Lists) != 3 || len(d.Labels) != 1 || n != 1 || done != 0 || cl != 1 {
		t.Fatalf("copy wrong: lists=%d labels=%d cards=%d done=%d cardlabels=%d", len(d.Lists), len(d.Labels), n, done, cl)
	}
}

func TestMembersAndAuthz(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Team")
	ed := testutil.As(e, editor)
	vw := testutil.As(e, viewer)
	nb := testutil.As(e, nobody)
	mustFail(t, ed, "board.get", boards.BoardGetIn{ID: b.ID}, "not_found")

	m := call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID})
	if m.Role != "editor" || m.Name != editor.Name {
		t.Fatalf("member %+v", m)
	}
	mustFail(t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID}, "conflict")
	mustFail(t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID}, "validation")
	cc := true
	vm := call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer", CanComment: &cc})
	if authz.BoardLevel(vw, b.ID) != authz.Comment || authz.BoardLevel(ed, b.ID) != authz.Edit {
		t.Fatal("levels wrong")
	}
	// group membership
	if _, err := e.DB.Exec(`INSERT INTO user_groups(id,name,created_at) VALUES('g1','Devs','x'); INSERT INTO user_group_members VALUES('g1','u-none')`); err != nil {
		t.Fatal(err)
	}
	gm := call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, GroupID: "g1", Role: "viewer"})
	if authz.BoardLevel(nb, b.ID) != authz.View || gm.GroupID == nil {
		t.Fatal("group member should view")
	}

	// editor can create lists, viewer cannot, neither can manage members
	addList(t, ed, b.ID, "Col")
	mustFail(t, vw, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "x"}, "forbidden")
	mustFail(t, ed, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: nobody.UserID}, "forbidden")
	call[boards.Member](t, e, "board.member.update", boards.MemberUpdateIn{ID: vm.ID, Role: "editor"})
	if authz.BoardLevel(vw, b.ID) != authz.Edit {
		t.Fatal("update role failed")
	}
	// removing a member drops their card memberships
	l := call[[]boards.List](t, e, "list.list", boards.ListListIn{BoardID: b.ID})
	addCard(t, e, b.ID, l[0].ID, "cc1", "C", 1, "")
	_, _ = e.DB.Exec(`INSERT INTO card_memberships VALUES('cc1',?)`, editor.UserID)
	call[boards.Member](t, e, "board.member.remove", boards.MemberRefIn{ID: m.ID})
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_memberships`).Scan(&n)
	if n != 0 {
		t.Fatal("card membership not cleaned")
	}
	// self removal allowed for a non-manager
	call[boards.Member](t, vw, "board.member.remove", boards.MemberRefIn{ID: vm.ID})
	mustFail(t, vw, "board.get", boards.BoardGetIn{ID: b.ID}, "not_found")
}

func TestLabelsAndGroups(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "L")
	g := call[boards.LabelGroup](t, e, "labelgroup.create", boards.GroupCreateIn{BoardID: b.ID, Name: "Priority", SingleSelect: true})
	l1 := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "High", Color: "berry-red", GroupID: g.ID})
	l2 := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "Low", Color: "#00ff00", GroupID: g.ID})
	other := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Color: "pink-tulip"})
	mustFail(t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Color: "chartreuse"}, "validation")
	if !(l1.Position < l2.Position && l2.Position < other.Position) {
		t.Fatal("positions should ascend")
	}
	call[boards.Label](t, e, "label.move", boards.LabelMoveIn{ID: other.ID, BeforeID: l1.ID})
	ls := call[[]boards.Label](t, e, "label.list", boards.LabelListIn{BoardID: b.ID})
	if ls[0].ID != other.ID || len(ls) != 3 {
		t.Fatalf("label order %+v", ls)
	}
	nm := "Higher"
	u := call[boards.Label](t, e, "label.update", boards.LabelUpdateIn{ID: l1.ID, Name: &nm})
	if *u.Name != "Higher" {
		t.Fatal("rename failed")
	}

	// single select conflicts
	l := addList(t, e, b.ID, "A")
	addCard(t, e, b.ID, l.ID, "k1", "K", 1, "")
	_, _ = e.DB.Exec(`INSERT INTO card_labels VALUES('k1',?),('k1',?)`, l1.ID, other.ID)
	c, err := boards.SingleSelectConflicts(e.Ctx, e.DB, "k1", l2.ID)
	if err != nil || len(c) != 1 || c[0] != l1.ID {
		t.Fatalf("conflicts %v %v", c, err)
	}
	c, _ = boards.SingleSelectConflicts(e.Ctx, e.DB, "k1", other.ID)
	if len(c) != 0 {
		t.Fatalf("ungrouped label must not conflict: %v", c)
	}
	off := false
	call[boards.LabelGroup](t, e, "labelgroup.update", boards.GroupUpdateIn{ID: g.ID, SingleSelect: &off})
	if c, _ = boards.SingleSelectConflicts(e.Ctx, e.DB, "k1", l2.ID); len(c) != 0 {
		t.Fatal("multi-select group must not conflict")
	}
	call[boards.Deleted](t, e, "labelgroup.delete", boards.LabelRefIn{ID: g.ID})
	var grp *string
	_ = e.DB.QueryRow(`SELECT group_id FROM labels WHERE id=?`, l1.ID).Scan(&grp)
	if grp != nil {
		t.Fatal("label should be ungrouped after group delete")
	}
	call[boards.Deleted](t, e, "label.delete", boards.LabelRefIn{ID: l1.ID})
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_labels WHERE label_id=?`, l1.ID).Scan(&n)
	if n != 0 {
		t.Fatal("card label link should cascade")
	}

	// project-wide labels
	_, _ = e.DB.Exec(`UPDATE projects SET use_project_labels=1 WHERE id='p1'`)
	pl := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "Shared", Color: "lagoon-blue"})
	if pl.ProjectID == nil || pl.BoardID != nil {
		t.Fatalf("expected project label %+v", pl)
	}
	b2 := newBoard(t, e, "L2")
	got := call[[]boards.Label](t, e, "label.list", boards.LabelListIn{BoardID: b2.ID})
	if len(got) != 1 || got[0].ID != pl.ID {
		t.Fatalf("project label should be visible on other boards: %+v", got)
	}
	ed := testutil.As(e, editor)
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID})
	mustFail(t, ed, "label.create", boards.LabelCreateIn{BoardID: b.ID, Color: "pink-tulip"}, "forbidden")
}

func TestLists(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Lists")
	a := addList(t, e, b.ID, "A")
	c := addList(t, e, b.ID, "C")
	if *a.Position >= *c.Position {
		t.Fatal("list positions")
	}
	mv := call[boards.List](t, e, "list.move", boards.ListMoveIn{ID: c.ID, BeforeID: a.ID})
	if *mv.Position >= *a.Position {
		t.Fatal("list.move failed")
	}
	col := "berry-red"
	nm := "Cee"
	up := call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: c.ID, Name: &nm, Color: &col})
	if *up.Name != "Cee" || *up.Color != "#c8324a" { // names resolve to hex on write
		t.Fatal("update")
	}
	mustFail(t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "x", Type: "trash"}, "validation")

	addCard(t, e, b.ID, a.ID, "x1", "banana", 1, "2025-03-01T00:00:00Z")
	addCard(t, e, b.ID, a.ID, "x2", "Apple", 2, "")
	addCard(t, e, b.ID, a.ID, "x3", "cherry", 3, "2025-01-01T00:00:00Z")
	call[boards.List](t, e, "list.sort", boards.ListSortIn{ID: a.ID, Field: "name"})
	if got := strings.Join(cardNames(t, e, a.ID), ","); got != "Apple,banana,cherry" {
		t.Fatalf("name sort %s", got)
	}
	call[boards.List](t, e, "list.sort", boards.ListSortIn{ID: a.ID, Field: "dueDate"})
	if got := strings.Join(cardNames(t, e, a.ID), ","); got != "cherry,banana,Apple" {
		t.Fatalf("due sort (nulls last) %s", got)
	}
	call[boards.List](t, e, "list.sort", boards.ListSortIn{ID: a.ID, Field: "name", Order: "desc"})
	if got := strings.Join(cardNames(t, e, a.ID), ","); got != "cherry,banana,Apple" {
		t.Fatalf("desc sort %s", got)
	}
	mustFail(t, e, "list.sort", boards.ListSortIn{ID: a.ID, Field: "bogus"}, "validation")

	addCard(t, e, b.ID, c.ID, "y1", "existing", 1, "")
	r := call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: a.ID, ListID: c.ID})
	if r.Moved != 3 || strings.Join(cardNames(t, e, c.ID), ",") != "existing,cherry,banana,Apple" {
		t.Fatalf("moveCards: %+v %v", r, cardNames(t, e, c.ID))
	}
	mustFail(t, e, "list.moveCards", boards.ListMoveCardsIn{ID: a.ID, ListID: a.ID}, "validation")

	// clear only works on trash
	mustFail(t, e, "list.clear", boards.ListRefIn{ID: a.ID}, "forbidden")
	// delete moves cards to trash, then clear empties it
	call[boards.Deleted](t, e, "list.delete", boards.ListRefIn{ID: c.ID})
	trash, _ := boards.SystemList(e.Ctx, e.DB, b.ID, "trash")
	if len(cardNames(t, e, trash)) != 4 {
		t.Fatalf("trash should hold 4 cards: %v", cardNames(t, e, trash))
	}
	call[boards.List](t, e, "list.clear", boards.ListRefIn{ID: trash})
	if len(cardNames(t, e, trash)) != 0 {
		t.Fatal("trash not cleared")
	}
	// archive
	d := addList(t, e, b.ID, "D")
	addCard(t, e, b.ID, d.ID, "z1", "zed", 1, "")
	call[boards.Deleted](t, e, "list.archive", boards.ListRefIn{ID: d.ID})
	arch, _ := boards.SystemList(e.Ctx, e.DB, b.ID, "archive")
	if strings.Join(cardNames(t, e, arch), ",") != "zed" {
		t.Fatal("archive failed")
	}
	// system lists cannot be deleted through list.delete
	mustFail(t, e, "list.delete", boards.ListRefIn{ID: arch}, "forbidden")
}

func TestSavedFilters(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "F")
	f := call[boards.SavedFilter](t, e, "filter.create", boards.FilterCreateIn{BoardID: b.ID, Name: "Mine", Query: "member=me"})
	if got := call[[]boards.SavedFilter](t, e, "filter.list", boards.FilterListIn{BoardID: b.ID}); len(got) != 1 {
		t.Fatal("list")
	}
	// private per user
	other := testutil.As(e, editor)
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID})
	if got := call[[]boards.SavedFilter](t, other, "filter.list", boards.FilterListIn{BoardID: b.ID}); len(got) != 0 {
		t.Fatal("filters must be per user")
	}
	mustFail(t, other, "filter.delete", boards.FilterRefIn{ID: f.ID}, "not_found")
	q := "q=bug"
	u := call[boards.SavedFilter](t, e, "filter.update", boards.FilterUpdateIn{ID: f.ID, Query: &q})
	if u.Query != q {
		t.Fatal("update")
	}
	call[boards.Deleted](t, e, "filter.delete", boards.FilterRefIn{ID: f.ID})
}

const trelloJSON = `{"name":"Trello Board","lists":[{"id":"l1","name":"Doing","closed":false,"pos":2},{"id":"l0","name":"Todo","closed":false,"pos":1},{"id":"l9","name":"Old","closed":true,"pos":3}],
"labels":[{"id":"b1","name":"Bug","color":"red"}],
"cards":[{"id":"c1","idList":"l0","name":"First","desc":"hello","closed":false,"due":"2025-02-01T10:00:00.000Z","dueComplete":true,"pos":1,"idLabels":["b1"]},
{"id":"c2","idList":"l1","name":"Second","desc":"","closed":false,"pos":1,"idLabels":[]},
{"id":"c3","idList":"l9","name":"Ancient","closed":false,"pos":1,"idLabels":[]}],
"checklists":[{"idCard":"c1","name":"QA","pos":1,"checkItems":[{"name":"a","state":"complete","pos":1},{"name":"b","state":"incomplete","pos":2}]}],
"actions":[{"type":"commentCard","date":"2025-01-02T00:00:00.000Z","data":{"text":"nice","card":{"id":"c1"}}}]}`

func TestImportExport(t *testing.T) {
	e := fixture(t)
	b := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p1", Format: "trello", Data: trelloJSON})
	if b.Name != "Trello Board" {
		t.Fatalf("name %q", b.Name)
	}
	d := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b.ID})
	var names []string
	for _, l := range d.Lists {
		if l.Type == "active" {
			names = append(names, *l.Name)
		}
	}
	if strings.Join(names, ",") != "Todo,Doing" || len(d.Labels) != 1 || d.Labels[0].Color != "berry-red" {
		t.Fatalf("lists=%v labels=%+v", names, d.Labels)
	}
	var tasks, done, comments, archived int
	_ = e.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(is_completed),0) FROM tasks`).Scan(&tasks, &done)
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM comments`).Scan(&comments)
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM cards c JOIN lists l ON l.id=c.list_id WHERE l.type='archive' AND c.board_id=?`, b.ID).Scan(&archived)
	var dueDone int
	_ = e.DB.QueryRow(`SELECT is_due_completed FROM cards WHERE name='First'`).Scan(&dueDone)
	if tasks != 2 || done != 1 || comments != 1 || archived != 1 || dueDone != 1 {
		t.Fatalf("tasks=%d done=%d comments=%d archived=%d dueDone=%d", tasks, done, comments, archived, dueDone)
	}

	// JSON export round-trips through the planka/weave importer
	st := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: b.ID})
	var doc map[string]any
	if err := json.Unmarshal(st.Data, &doc); err != nil || doc["format"] != "weave-board" {
		t.Fatalf("export json: %v", err)
	}
	b2 := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: string(st.Data), Name: "Copy"})
	var n2, t2, c2 int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE board_id=?`, b2.ID).Scan(&n2)
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM tasks t JOIN task_lists tl ON tl.id=t.task_list_id JOIN cards c ON c.id=tl.card_id WHERE c.board_id=?`, b2.ID).Scan(&t2)
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_labels cl JOIN cards c ON c.id=cl.card_id WHERE c.board_id=?`, b2.ID).Scan(&c2)
	if n2 != 3 || t2 != 2 || c2 != 1 || b2.Name != "Copy" {
		t.Fatalf("round trip: cards=%d tasks=%d labels=%d name=%s", n2, t2, c2, b2.Name)
	}
	if got := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b2.ID}); len(got.Lists) != 4 {
		t.Fatalf("archive list should map onto the system list: %d lists", len(got.Lists))
	}

	// CSV export + import
	csvOut := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: b.ID, Format: "csv"})
	if !strings.HasPrefix(string(csvOut.Data), "list,name,description,due,completed,labels,members") || !strings.Contains(string(csvOut.Data), "Todo,First,hello") {
		t.Fatalf("csv:\n%s", csvOut.Data)
	}
	csvIn := "List,Title,Due,Labels\nBacklog,One,2025-05-01,\"bug;ui\"\nBacklog,Two,,\nDone,Three,01/02/2025,bug\n"
	b3 := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p1", Format: "csv", Name: "CSV", Data: csvIn})
	d3 := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b3.ID})
	var n3 int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE board_id=?`, b3.ID).Scan(&n3)
	if n3 != 3 || len(d3.Labels) != 2 || len(d3.Lists) != 4 {
		t.Fatalf("csv import: cards=%d labels=%d lists=%d", n3, len(d3.Labels), len(d3.Lists))
	}
	mustFail(t, e, "board.import", &boards.ImportIn{ProjectID: "p1", Format: "trello", Data: "{}"}, "validation")
	mustFail(t, e, "board.import", &boards.ImportIn{ProjectID: "p1", Format: "csv", Data: "nope"}, "validation")
	mustFail(t, e, "board.import", &boards.ImportIn{ProjectID: "p1", Format: "xml", Data: "<a/>"}, "validation")
	mustFail(t, testutil.As(e, editor), "board.import", &boards.ImportIn{ProjectID: "p1", Format: "csv", Data: csvIn}, "not_found")
}

// ---- pages ----

func mux(e *ops.Env) *http.ServeMux {
	m := http.NewServeMux()
	web.Mount(m, func(r *http.Request) *ops.Env { c := *e; c.Req = r; c.Ctx = r.Context(); return &c })
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(web.Static())))
	return m
}

func get(t *testing.T, m http.Handler, path string, htmx bool) (int, string) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if htmx {
		r.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func TestBoardPages(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Kanban <b>")
	l1 := addList(t, e, b.ID, "Todo")
	l2 := addList(t, e, b.ID, "Done")
	lab := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "Urgent", Color: "berry-red"})
	addCard(t, e, b.ID, l1.ID, "k1", "Fix login", 1, "2020-01-01T00:00:00Z")
	addCard(t, e, b.ID, l1.ID, "k2", "Write docs", 2, "")
	addCard(t, e, b.ID, l2.ID, "k3", "Ship it", 1, "")
	_, _ = e.DB.Exec(`INSERT INTO card_labels VALUES('k1',?)`, lab.ID)
	_, _ = e.DB.Exec(`INSERT INTO card_memberships VALUES('k2',?)`, editor.UserID)
	_, _ = e.DB.Exec(`INSERT INTO task_lists(id,card_id,position,name,created_at) VALUES('tl','k1',1,'T','x'); INSERT INTO tasks(id,task_list_id,position,name,is_completed,created_at) VALUES('ta','tl',1,'a',1,'x'),('tb','tl',2,'b',0,'x')`)
	m := mux(e)

	code, body := get(t, m, "/boards/"+b.ID, false)
	for _, want := range []string{"<!doctype html>", `id="board-body"`, "Fix login", "Write docs", "Ship it", `data-card-drop="/ui/boards/` + b.ID + `/cards/drop"`,
		`data-list="` + l1.ID + `"`, `href="/cards/k1"`, "Urgent", "1/2", "boards-due-overdue", "/boards/" + b.ID + "/calendar", "/boards/" + b.ID + "/map",
		`data-drag-handle="list"`, "Kanban &lt;b&gt;", "Add list", "EI"} {
		if !strings.Contains(body, want) {
			t.Errorf("kanban page missing %q (status %d)", want, code)
		}
	}
	if strings.Contains(body, "Kanban <b>") {
		t.Error("board name not escaped")
	}
	// filters
	_, body = get(t, m, "/ui/boards/"+b.ID+"/body?q=docs", true)
	if !strings.Contains(body, "Write docs") || strings.Contains(body, "Fix login") {
		t.Errorf("text filter failed")
	}
	_, body = get(t, m, "/ui/boards/"+b.ID+"/body?label="+lab.ID, true)
	if !strings.Contains(body, "Fix login") || strings.Contains(body, "Write docs") {
		t.Errorf("label filter failed")
	}
	_, body = get(t, m, "/ui/boards/"+b.ID+"/body?member="+editor.UserID, true)
	if !strings.Contains(body, "Write docs") || strings.Contains(body, "Fix login") {
		t.Errorf("member filter failed")
	}
	_, body = get(t, m, "/ui/boards/"+b.ID+"/body?member=none", true)
	if strings.Contains(body, "Write docs") || !strings.Contains(body, "Fix login") {
		t.Errorf("unassigned filter failed")
	}
	_, body = get(t, m, "/ui/boards/"+b.ID+"/body?due=overdue", true)
	if !strings.Contains(body, "Fix login") || strings.Contains(body, "Ship it") {
		t.Errorf("due filter failed")
	}
	// list + grid views
	_, body = get(t, m, "/ui/boards/"+b.ID+"/body?view=list", true)
	if !strings.Contains(body, "<table") || !strings.Contains(body, "Ship it") || strings.Contains(body, "boards-lists") {
		t.Errorf("list view wrong")
	}
	_, body = get(t, m, "/ui/boards/"+b.ID+"/body?view=grid", true)
	if !strings.Contains(body, "boards-grid") || !strings.Contains(body, "Ship it") {
		t.Errorf("grid view wrong")
	}
	// calendar default view redirects to the cards domain page
	cal := "calendar"
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, DefaultView: &cal})
	r := httptest.NewRecorder()
	m.ServeHTTP(r, httptest.NewRequest("GET", "/boards/"+b.ID, nil))
	if r.Code != 303 || r.Header().Get("Location") != "/boards/"+b.ID+"/calendar" {
		t.Errorf("calendar redirect: %d %s", r.Code, r.Header().Get("Location"))
	}

	// settings page + sections
	code, body = get(t, m, "/boards/"+b.ID+"/settings", false)
	for _, want := range []string{"set-general", "set-members", "set-labels", "set-sharing", "Urgent", "Ed Itor", "export?format=csv"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings missing %q (%d)", want, code)
		}
	}
	if code, body = get(t, m, "/ui/boards/"+b.ID+"/members", true); code != 200 || !strings.Contains(body, `id="set-members"`) {
		t.Errorf("members fragment: %d", code)
	}
	if code, body = get(t, m, "/ui/projects/p1/boards", true); code != 200 || !strings.Contains(body, "Kanban &lt;b&gt;") || !strings.Contains(body, "New board") {
		t.Errorf("project boards fragment: %d", code)
	}
	if code, body = get(t, m, "/ui/projects/p1/boards/new", true); code != 200 || !strings.Contains(body, "multipart/form-data") {
		t.Errorf("new board dialog: %d", code)
	}
	if code, _ = get(t, m, "/ui/boards/"+b.ID+"/export?format=csv", false); code != 200 {
		t.Errorf("export route: %d", code)
	}

	// public board: anonymous, read-only, no editing affordances, no live room
	pb := call[boards.Board](t, e, "board.setPublic", boards.BoardPublicIn{ID: b.ID, IsPublic: true})
	anon := testutil.As(e, nil)
	pm := mux(anon)
	code, body = get(t, pm, "/public/"+pb.PublicToken, false)
	if code != 200 || !strings.Contains(body, "Fix login") || !strings.Contains(body, "Read-only public view") {
		t.Fatalf("public page: %d", code)
	}
	for _, bad := range []string{"data-sortable", "hx-get=\"/ui/cards/", "board-live", "boards-addlist", "u-ed@x.io", "/settings", "saved-filters"} {
		if strings.Contains(body, bad) {
			t.Errorf("public page leaks %q", bad)
		}
	}
	if code, _ = get(t, pm, "/public/nope", false); code != 404 {
		t.Errorf("unknown token: %d", code)
	}
	if code, _ = get(t, pm, "/boards/"+b.ID, false); code != 303 {
		t.Errorf("anon board page should redirect to login: %d", code)
	}
}

// fake card ops so the drop + add-card plumbing can be exercised without the cards domain.
type moveIn struct {
	ID       string  `path:"id"`
	ListID   string  `json:"listId" required:"true"`
	Position float64 `json:"position"`
}
type moveOut struct{ OK bool }
type createIn struct {
	ListID string `path:"listId"`
	Name   string `json:"name"`
}

func TestCardDropAndAddCardForm(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Drop")
	l1 := addList(t, e, b.ID, "A")
	l2 := addList(t, e, b.ID, "B")
	addCard(t, e, b.ID, l1.ID, "d1", "one", 65536, "")
	addCard(t, e, b.ID, l2.ID, "d2", "two", 65536, "")
	addCard(t, e, b.ID, l2.ID, "d3", "three", 131072, "")
	m := mux(e)

	_, body := get(t, m, "/boards/"+b.ID, false)
	if !strings.Contains(body, `action="/ui/lists/`+l1.ID+`/cards/quick"`) {
		t.Error("add-card form should use card.create's UI route")
	}
	drop := func(vals url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/ui/boards/"+b.ID+"/cards/drop", strings.NewReader(vals.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		return w
	}
	// drop d1 between d2 and d3 in list B
	w := drop(url.Values{"id": {"d1"}, "container": {l2.ID}, "afterId": {"d2"}, "beforeId": {"d3"}, "index": {"1"}})
	if w.Code != 204 || !strings.Contains(w.Header().Get("HX-Trigger"), "boards:refresh") {
		t.Fatalf("drop: %d %s", w.Code, w.Body.String())
	}
	if got := strings.Join(cardNames(t, e, l2.ID), ","); got != "two,one,three" {
		t.Fatalf("order after drop: %s", got)
	}
	// drop at the start of A (empty now), no neighbours
	w = drop(url.Values{"id": {"d2"}, "container": {l1.ID}})
	if w.Code != 204 || strings.Join(cardNames(t, e, l1.ID), ",") != "two" {
		t.Fatalf("drop into empty list: %d", w.Code)
	}
	// foreign list is rejected
	if w = drop(url.Values{"id": {"d2"}, "container": {"nope"}}); w.Code != 404 {
		t.Errorf("bad list: %d", w.Code)
	}
	// viewers cannot drop
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	vm := mux(testutil.As(e, viewer))
	r := httptest.NewRequest("POST", "/ui/boards/"+b.ID+"/cards/drop", strings.NewReader("id=d2&container="+l2.ID))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	vm.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Errorf("viewer drop: %d", rec.Code)
	}
	_, body = get(t, vm, "/boards/"+b.ID, false)
	if strings.Contains(body, "data-sortable") || strings.Contains(body, "boards-addcard") {
		t.Error("viewer must not get drag-drop or add-card affordances")
	}
}

func TestListUIRoutes(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "UI")
	m := mux(e)
	post := func(method, path string, vals url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(vals.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		return w
	}
	if w := post("POST", "/ui/boards/"+b.ID+"/lists", url.Values{"name": {"First"}}); w.Code != 204 {
		t.Fatalf("list.create ui: %d %s", w.Code, w.Body.String())
	}
	post("POST", "/ui/boards/"+b.ID+"/lists", url.Values{"name": {"Second"}})
	ls := call[[]boards.List](t, e, "list.list", boards.ListListIn{BoardID: b.ID})
	var first, second string
	for _, l := range ls {
		if l.Name != nil && *l.Name == "First" {
			first = l.ID
		}
		if l.Name != nil && *l.Name == "Second" {
			second = l.ID
		}
	}
	// list drag-drop route: second before first
	if w := post("POST", "/ui/boards/"+b.ID+"/lists/drop", url.Values{"id": {second}, "beforeId": {first}}); w.Code != 204 {
		t.Fatalf("lists/drop: %d", w.Code)
	}
	ls = call[[]boards.List](t, e, "list.list", boards.ListListIn{BoardID: b.ID})
	if *ls[0].Name != "Second" {
		t.Fatalf("list order: %v", *ls[0].Name)
	}
	// settings mutation re-renders the section with a bool toggle
	w := post("PATCH", "/ui/boards/"+b.ID, url.Values{"name": {"Renamed"}, "showCardCounter": {"true", "false"}, "showCardAges": {"false"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `id="set-general"`) || !strings.Contains(w.Body.String(), "Renamed") {
		t.Fatalf("board.update ui: %d %s", w.Code, w.Body.String())
	}
	got := call[boards.Board](t, e, "board.get", boards.BoardGetIn{ID: b.ID})
	_ = got
	bb, _ := boards.GetBoard(e.Ctx, e.DB, b.ID)
	if !bb.ShowCardCounter || bb.ShowCardAges {
		t.Errorf("checkbox semantic wrong: %+v", bb)
	}
	// labels through UI
	w = post("POST", "/ui/boards/"+b.ID+"/labels", url.Values{"name": {"X"}, "color": {"pink-tulip"}, "_board": {b.ID}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `id="set-labels"`) {
		t.Fatalf("label.create ui: %d %s", w.Code, w.Body.String())
	}
	// delete board redirects to project
	if w = post("DELETE", "/ui/boards/"+b.ID, nil); w.Header().Get("HX-Redirect") != "/projects/p1" {
		t.Errorf("delete redirect: %q (%d)", w.Header().Get("HX-Redirect"), w.Code)
	}
}

// Every boards op must declare a UI route that is actually served.
func TestOpsUIRoutesResolve(t *testing.T) {
	m := mux(testutil.Env(t, admin))
	mine := 0
	for _, o := range ops.All() {
		switch o.Tag {
		case "board", "label", "labelgroup", "list", "filter":
		default:
			continue
		}
		mine++
		if strings.HasPrefix(o.UI, "exempt:") {
			continue
		}
		method, path, ok := strings.Cut(o.UI, " ")
		if !ok {
			t.Errorf("%s: malformed UI %q", o.Name, o.UI)
			continue
		}
		concrete := path
		for strings.Contains(concrete, "{") {
			i := strings.Index(concrete, "{")
			j := strings.Index(concrete, "}")
			concrete = concrete[:i] + "x1" + concrete[j+1:]
		}
		req := httptest.NewRequest(method, concrete, nil)
		_, pattern := m.Handler(req)
		if pattern != o.UI {
			t.Errorf("%s: UI %q resolves to %q", o.Name, o.UI, pattern)
		}
	}
	if mine < 40 {
		t.Errorf("expected >=40 boards ops, saw %d", mine)
	}
}
