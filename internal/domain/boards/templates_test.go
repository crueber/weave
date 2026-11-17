package boards_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

func count(t *testing.T, e *ops.Env, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func builtinIDs(t *testing.T, e *ops.Env) []string {
	t.Helper()
	var ids []string
	for _, ti := range call[[]boards.TemplateInfo](t, e, "board.template.list", boards.TemplateListIn{}) {
		if ti.Source == "builtin" {
			ids = append(ids, ti.ID)
		}
	}
	if len(ids) != 14 {
		t.Fatalf("expected 14 built-in templates, got %d", len(ids))
	}
	return ids
}

// Every built-in can be created, with and without its sample cards, in a project with board labels and in one with
// project-wide labels.
func TestInstantiateEveryBuiltin(t *testing.T) {
	e := fixture(t)
	if _, err := e.DB.Exec(`UPDATE projects SET use_project_labels=1 WHERE id='p2'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range builtinIDs(t, e) {
		prev := call[boards.TemplatePreview](t, e, "board.template.get", boards.TemplateGetIn{TemplateID: id})
		if len(prev.Columns) < 3 || prev.Name == "" || prev.Category == "" {
			t.Fatalf("%s: thin preview %+v", id, prev.TemplateInfo)
		}
		for _, project := range []string{"p1", "p2"} {
			for _, sample := range []bool{true, false} {
				s := sample
				b := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: project, TemplateID: id, Name: fmt.Sprintf("%s %v", prev.Name, sample), SampleCards: &s})
				if b.IsTemplate || b.ProjectID != project || b.DefaultView != prev.DefaultView {
					t.Fatalf("%s: bad board %+v", id, b)
				}
				d := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b.ID})
				active := 0
				for _, l := range d.Lists {
					if l.Type == "active" || l.Type == "closed" {
						active++
					}
				}
				if active != len(prev.Columns) {
					t.Fatalf("%s: %d columns, preview says %d", id, active, len(prev.Columns))
				}
				cards := count(t, e, `SELECT COUNT(*) FROM cards WHERE board_id=?`, b.ID)
				want := len(prev.Cards)
				if !sample {
					want = 0
				}
				if cards != want {
					t.Fatalf("%s sample=%v project=%s: %d cards, want %d", id, sample, project, cards, want)
				}
				linked := count(t, e, `SELECT COUNT(*) FROM card_labels cl JOIN cards c ON c.id=cl.card_id WHERE c.board_id=?`, b.ID)
				wantLinks := 0
				for _, c := range prev.Cards {
					wantLinks += len(c.Labels)
				}
				if sample && linked != wantLinks {
					t.Fatalf("%s project=%s: %d label links, want %d", id, project, linked, wantLinks)
				}
				if count(t, e, `SELECT COUNT(*) FROM board_imports`) != 0 {
					t.Fatalf("built-ins must not write board_imports rows")
				}
			}
		}
	}
	// the project-wide label set must not have been duplicated by the second and third copy
	if n := count(t, e, `SELECT COUNT(*) FROM labels WHERE project_id='p2' AND name='Bug'`); n != 1 {
		t.Fatalf("project label duplicated: %d", n)
	}
}

func TestBuiltinDetailsAndErrors(t *testing.T) {
	e := fixture(t)
	b := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:sprint", Name: "S"})
	if b.DefaultCardType != "story" && count(t, e, `SELECT COUNT(*) FROM cards WHERE board_id=? AND type='story'`, b.ID) != 1 {
		t.Fatal("sprint story card lost its type")
	}
	d := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b.ID})
	wip := map[string]int{}
	for _, l := range d.Lists {
		if l.WIPLimit != nil && l.Name != nil {
			wip[*l.Name] = *l.WIPLimit
		}
	}
	if wip["In progress"] != 4 || wip["Review"] != 3 || len(wip) != 2 {
		t.Fatalf("WIP limits: %v", wip)
	}
	if count(t, e, `SELECT COUNT(*) FROM tasks t JOIN task_lists tl ON tl.id=t.task_list_id JOIN cards c ON c.id=tl.card_id WHERE c.board_id=?`, b.ID) != 4 {
		t.Fatal("acceptance criteria tasks missing")
	}
	// view override and the default sample choice
	c := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:kanban-basics", Name: "K", DefaultView: "list"})
	if c.DefaultView != "list" {
		t.Fatalf("view override: %s", c.DefaultView)
	}
	cal := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:content-calendar", Name: "C"})
	if cal.DefaultView != "calendar" {
		t.Fatalf("content calendar view: %s", cal.DefaultView)
	}
	mustFail(t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:nope", Name: "X"}, "not_found")
	mustFail(t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:sprint", Name: "X", DefaultView: "bogus"}, "validation")
	mustFail(t, e, "board.template.get", boards.TemplateGetIn{TemplateID: "builtin:nope"}, "not_found")
	// built-ins need project Manage, not just authentication
	mustFail(t, testutil.As(e, nobody), "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:sprint", Name: "X"}, "not_found")
	call[boards.TemplatePreview](t, testutil.As(e, nobody), "board.template.get", boards.TemplateGetIn{TemplateID: "builtin:sprint"})
}

func TestTemplateSearchAndCategories(t *testing.T) {
	e := fixture(t)
	names := func(in boards.TemplateListIn) string {
		var out []string
		for _, ti := range call[[]boards.TemplateInfo](t, e, "board.template.list", in) {
			out = append(out, ti.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(boards.TemplateListIn{Category: "software"}); got != "Sprint (Scrum),Bug tracker,Software release checklist" {
		t.Fatalf("software: %s", got)
	}
	if got := names(boards.TemplateListIn{Query: "RECIPES"}); got != "Meal planning" {
		t.Fatalf("search: %s", got)
	}
	if got := names(boards.TemplateListIn{Query: "sprint agile"}); got != "Sprint (Scrum)" {
		t.Fatalf("multi word search: %s", got)
	}
	if got := names(boards.TemplateListIn{Query: "zzzz"}); got != "" {
		t.Fatalf("no match: %s", got)
	}
	mustFail(t, e, "board.template.list", boards.TemplateListIn{Category: "nope"}, "validation")
}

func TestHiddenBuiltinTemplates(t *testing.T) {
	e := fixture(t)
	if _, err := e.DB.Exec(`INSERT INTO config(key,value) VALUES('templates.hidden','["builtin:sprint","builtin:okrs"]')`); err != nil {
		t.Fatal(err)
	}
	ids := call[[]boards.TemplateInfo](t, e, "board.template.list", boards.TemplateListIn{})
	if len(ids) != 12 {
		t.Fatalf("hidden templates still listed: %d", len(ids))
	}
	for _, ti := range ids {
		if ti.ID == "builtin:sprint" || ti.ID == "builtin:okrs" {
			t.Fatalf("%s listed", ti.ID)
		}
	}
	mustFail(t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:sprint", Name: "X"}, "not_found")
	mustFail(t, e, "board.template.get", boards.TemplateGetIn{TemplateID: "builtin:okrs"}, "not_found")
	call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:bug-tracker", Name: "ok"})
}

func TestRelativeDueDatesFromBuiltin(t *testing.T) {
	e := fixture(t)
	b := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:release-checklist", Name: "R"})
	var due string
	if err := e.DB.QueryRow(`SELECT due_date FROM cards WHERE board_id=? AND name='Release 1.0'`, b.ID).Scan(&due); err != nil {
		t.Fatal(err)
	}
	got, err := time.Parse(time.RFC3339, due)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(got); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Fatalf("due %s is not about a week away (%v)", due, d)
	}
}

func publishTemplate(t *testing.T, e *ops.Env, name string, in boards.TemplatePublishIn) boards.Board {
	t.Helper()
	b := call[boards.Board](t, e, "board.create", boards.BoardCreateIn{ProjectID: "p1", Name: name})
	l := addList(t, e, b.ID, "Todo")
	addCard(t, e, b.ID, l.ID, "c-"+name, "Card of "+name, 65536, "")
	in.ID, in.IsTemplate = b.ID, true
	return call[boards.Board](t, e, "board.template.publish", in)
}

func listIDs(t *testing.T, e *ops.Env) map[string]boards.TemplateInfo {
	t.Helper()
	out := map[string]boards.TemplateInfo{}
	for _, ti := range call[[]boards.TemplateInfo](t, e, "board.template.list", boards.TemplateListIn{Category: "published"}) {
		out[ti.Name] = ti
	}
	return out
}

func TestTemplateVisibility(t *testing.T) {
	e := fixture(t)
	if _, err := e.DB.Exec(`INSERT INTO user_groups(id,name,created_at) VALUES('g1','Devs','x'),('g2','Ops','x'); INSERT INTO user_group_members VALUES('g1','u-view')`); err != nil {
		t.Fatal(err)
	}
	// the editor manages project p1 and publishes; the viewer is in group Devs; nobody is in no group
	if _, err := e.DB.Exec(`INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES('pm1','p1','u-ed','x')`); err != nil {
		t.Fatal(err)
	}
	ed, vw, nb := testutil.As(e, editor), testutil.As(e, viewer), testutil.As(e, nobody)
	priv := publishTemplate(t, ed, "Priv", boards.TemplatePublishIn{})
	if priv.TemplateVisibility != "private" {
		t.Fatalf("a new template defaults to private, got %q", priv.TemplateVisibility)
	}
	grp := publishTemplate(t, ed, "Grp", boards.TemplatePublishIn{Visibility: "group", GroupID: "g1"})
	pub := publishTemplate(t, ed, "Pub", boards.TemplatePublishIn{Visibility: "public"})
	mustFail(t, ed, "board.template.publish", boards.TemplatePublishIn{ID: priv.ID, IsTemplate: true, Visibility: "group"}, "validation")
	mustFail(t, ed, "board.template.publish", boards.TemplatePublishIn{ID: priv.ID, IsTemplate: true, Visibility: "group", GroupID: "nope"}, "validation")
	mustFail(t, ed, "board.template.publish", boards.TemplatePublishIn{ID: priv.ID, IsTemplate: true, Visibility: "everyone"}, "validation")
	mustFail(t, nb, "board.template.publish", boards.TemplatePublishIn{ID: priv.ID, IsTemplate: true, Visibility: "public"}, "not_found")

	for _, c := range []struct {
		who  string
		env  *ops.Env
		want string
	}{{"admin", e, "Grp,Priv,Pub"}, {"owner", ed, "Grp,Priv,Pub"}, {"group member", vw, "Grp,Pub"}, {"outsider", nb, "Pub"}} {
		var got []string
		for n := range listIDs(t, c.env) {
			got = append(got, n)
		}
		if g := strings.Join(sorted(got), ","); g != c.want {
			t.Fatalf("%s sees %q, want %q", c.who, g, c.want)
		}
	}
	// instantiate follows the same rule (the target project must be one the caller manages)
	if _, err := e.DB.Exec(`INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES('pm2','p2','u-none','x'),('pm3','p2','u-view','x')`); err != nil {
		t.Fatal(err)
	}
	in := func(id string) boards.InstantiateIn {
		return boards.InstantiateIn{ProjectID: "p2", TemplateID: id, Name: "Copy"}
	}
	mustFail(t, nb, "board.instantiate", in(priv.ID), "not_found")
	mustFail(t, nb, "board.instantiate", in(grp.ID), "not_found")
	mustFail(t, nb, "board.template.get", boards.TemplateGetIn{TemplateID: priv.ID}, "not_found")
	call[boards.Board](t, nb, "board.instantiate", in(pub.ID))
	call[boards.Board](t, vw, "board.instantiate", in(grp.ID))
	mustFail(t, vw, "board.instantiate", in(priv.ID), "not_found")
	call[boards.Board](t, e, "board.instantiate", in(priv.ID))
	// publisher and project label
	if ti := listIDs(t, e)["Grp"]; ti.Publisher != editor.Name || ti.Project != "Proj" || ti.Visibility != "group" || ti.Group != "Devs" {
		t.Fatalf("labels: %+v", ti)
	}
	// re-publishing without a visibility keeps it; unpublishing hides it from everyone but board members
	b := call[boards.Board](t, ed, "board.template.publish", boards.TemplatePublishIn{ID: grp.ID, IsTemplate: true})
	if b.TemplateVisibility != "group" || b.TemplateGroupID == nil || *b.TemplateGroupID != "g1" {
		t.Fatalf("re-publish changed visibility: %+v", b)
	}
	call[boards.Board](t, ed, "board.template.publish", boards.TemplatePublishIn{ID: pub.ID})
	mustFail(t, nb, "board.instantiate", in(pub.ID), "not_found")
	// an ordinary board is not a template, even for someone who can see it
	plain := call[boards.Board](t, e, "board.create", boards.BoardCreateIn{ProjectID: "p1", Name: "Plain"})
	mustFail(t, e, "board.instantiate", in(plain.ID), "validation")
}

func sorted(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

// A published template stores due dates relative to the moment it was published; copies get them relative to now.
func TestPublishedTemplateRelativeDueDates(t *testing.T) {
	e := fixture(t)
	b := call[boards.Board](t, e, "board.create", boards.BoardCreateIn{ProjectID: "p1", Name: "Tpl"})
	l := addList(t, e, b.ID, "Todo")
	due := time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339)
	addCard(t, e, b.ID, l.ID, "c1", "Ship it", 65536, due)
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: b.ID, IsTemplate: true, Visibility: "public"})
	// a year later the template's own date is long past; the copy still lands ten days from now
	if _, err := e.DB.Exec(`UPDATE boards SET template_published_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), b.ID); err != nil {
		t.Fatal(err)
	}
	nb := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: b.ID, Name: "Copy"})
	var got string
	if err := e.DB.QueryRow(`SELECT due_date FROM cards WHERE board_id=?`, nb.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	g, _ := time.Parse(time.RFC3339, got)
	if d := time.Until(g); d < 9*24*time.Hour || d > 11*24*time.Hour {
		t.Fatalf("copy due %s, expected about 10 days out", got)
	}
	prev := call[boards.TemplatePreview](t, e, "board.template.get", boards.TemplateGetIn{TemplateID: b.ID})
	if len(prev.Cards) != 1 || prev.Cards[0].DueOffsetDays == nil || *prev.Cards[0].DueOffsetDays != 10 {
		t.Fatalf("preview offset: %+v", prev.Cards)
	}
}
