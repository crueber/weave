package cards

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
	"github.com/crueber/weave/internal/web"
)

type fx struct {
	e              *ops.Env // editor (ed)
	admin, viewer  *ops.Env
	stranger       *ops.Env
	proj, board    string
	board2         string
	todo, doing    string
	closed, arch   string
	trash, list2   string
	labelA, labelB string
	labelOther     string
	ed, vw, ad, st string
}

func mustExec(t *testing.T, e *ops.Env, q string, args ...any) {
	t.Helper()
	if _, err := e.DB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func seed(t *testing.T) *fx {
	t.Helper()
	f := &fx{ed: "u-ed", vw: "u-vw", ad: "u-ad", st: "u-st", proj: "p1", board: "b1", board2: "b2"}
	f.e = testutil.Env(t, &ops.Principal{UserID: f.ed, Role: "boardUser", Name: "Ed"})
	f.admin = testutil.As(f.e, &ops.Principal{UserID: f.ad, Role: "admin", Name: "Ada"})
	f.viewer = testutil.As(f.e, &ops.Principal{UserID: f.vw, Role: "boardUser", Name: "Vi"})
	f.stranger = testutil.As(f.e, &ops.Principal{UserID: f.st, Role: "boardUser", Name: "Stan"})
	now := db.Now()
	for _, u := range [][2]string{{f.ed, "Ed Itor"}, {f.vw, "Vi Ewer"}, {f.ad, "Ada Min"}, {f.st, "Stan Ger"}} {
		mustExec(t, f.e, `INSERT INTO users(id,email,name,created_at) VALUES(?,?,?,?)`, u[0], u[0]+"@x.io", u[1], now)
	}
	mustExec(t, f.e, `INSERT INTO projects(id,name,created_at) VALUES('p1','P',?)`, now)
	for i, b := range []string{"b1", "b2"} {
		mustExec(t, f.e, `INSERT INTO boards(id,project_id,position,name,created_at) VALUES(?,?,?,?,?)`, b, f.proj, float64(i+1), b, now)
		mustExec(t, f.e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,'editor',?)`, "m-ed-"+b, b, f.ed, now)
	}
	mustExec(t, f.e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m-vw','b1',?,'viewer',?)`, f.vw, now)
	mk := func(id, board, typ string, pos float64) string {
		mustExec(t, f.e, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES(?,?,?,?,?,?)`, id, board, typ, pos, id, now)
		return id
	}
	f.todo, f.doing = mk("l-todo", "b1", "active", 1), mk("l-doing", "b1", "active", 2)
	f.closed, f.arch, f.trash = mk("l-closed", "b1", "closed", 3), mk("l-arch", "b1", "archive", 4), mk("l-trash", "b1", "trash", 5)
	f.list2 = mk("l-b2", "b2", "active", 1)
	mustExec(t, f.e, `INSERT INTO label_groups(id,board_id,name,single_select,position) VALUES('g1','b1','Priority',1,1)`)
	mustExec(t, f.e, `INSERT INTO labels(id,board_id,group_id,name,color,position,created_at) VALUES('lab-a','b1','g1','High','red',1,?),('lab-b','b1','g1','Low','blue',2,?),('lab-x','b1',NULL,'Bug','green',3,?)`, now, now, now)
	mustExec(t, f.e, `INSERT INTO labels(id,board_id,name,color,position,created_at) VALUES('lab-other','b2','Other','pink-tulip',1,?)`, now)
	f.labelA, f.labelB, f.labelOther = "lab-a", "lab-b", "lab-other"
	return f
}

func (f *fx) card(t *testing.T, list, name string) Card {
	t.Helper()
	c, err := ops.Typed[Card](f.e, "card.create", CreateIn{ListID: list, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func call[T any](t *testing.T, e *ops.Env, op string, in any) T {
	t.Helper()
	out, err := ops.Typed[T](e, op, in)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return out
}

func TestCRUDAndAuthz(t *testing.T) {
	f := seed(t)
	c := f.card(t, f.todo, "  First  ")
	if c.Name != "First" || c.Type != "project" || c.Position == nil || *c.Position != db.PositionGap {
		t.Fatalf("bad card %+v", c)
	}
	c2 := f.card(t, f.todo, "Second")
	if *c2.Position != 2*db.PositionGap {
		t.Fatalf("append position %v", *c2.Position)
	}
	if _, err := ops.Typed[Card](f.e, "card.create", CreateIn{ListID: f.arch, Name: "x"}); err == nil {
		t.Fatal("create in archive list must fail")
	}
	d := "desc"
	typ := "story"
	u := call[Card](t, f.e, "card.update", UpdateIn{ID: c.ID, Description: &d, Type: &typ})
	if *u.Description != "desc" || u.Type != "story" {
		t.Fatalf("update %+v", u)
	}
	bad := "epic"
	if _, err := ops.Typed[Card](f.e, "card.update", UpdateIn{ID: c.ID, Type: &bad}); err == nil {
		t.Fatal("bad type")
	}
	// viewer can read but not write; stranger sees nothing
	if _, err := ops.Typed[Card](f.viewer, "card.get", GetIn{ID: c.ID}); err != nil {
		t.Fatal(err)
	}
	nm := "nope"
	if _, err := ops.Typed[Card](f.viewer, "card.update", UpdateIn{ID: c.ID, Name: &nm}); err == nil || err.(*ops.Error).Status != 403 {
		t.Fatalf("viewer write: %v", err)
	}
	if _, err := ops.Typed[Card](f.stranger, "card.get", GetIn{ID: c.ID}); err == nil || err.(*ops.Error).Status != 404 {
		t.Fatalf("stranger: %v", err)
	}
	lst := call[ListOut](t, f.e, "card.list", ListCardsIn{BoardID: f.board})
	if len(lst.Items) != 2 {
		t.Fatalf("list %d", len(lst.Items))
	}
	// delete requires the trash list
	if _, err := ops.Typed[Deleted](f.e, "card.delete", IDIn{c.ID}); err == nil {
		t.Fatal("delete outside trash must fail")
	}
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: c.ID, Trash: true})
	call[Deleted](t, f.e, "card.delete", IDIn{c.ID})
	if _, err := ops.Typed[Card](f.e, "card.get", GetIn{ID: c.ID}); err == nil {
		t.Fatal("deleted card still present")
	}
}

func TestMoveRules(t *testing.T) {
	f := seed(t)
	a, b, c := f.card(t, f.todo, "a"), f.card(t, f.todo, "b"), f.card(t, f.todo, "c")
	// reorder c between a and b
	m := call[Card](t, f.e, "card.move", MoveIn{ID: c.ID, ListID: f.todo, AfterID: a.ID})
	if !(*m.Position > *a.Position && *m.Position < *b.Position) {
		t.Fatalf("position %v not between %v and %v", *m.Position, *a.Position, *b.Position)
	}
	if m.ListChangedAt != c.ListChangedAt {
		t.Fatal("reorder must not touch list_changed_at")
	}
	time.Sleep(1100 * time.Millisecond)
	m = call[Card](t, f.e, "card.move", MoveIn{ID: c.ID, ListID: f.doing})
	if m.ListID != f.doing || m.ListChangedAt == c.ListChangedAt || m.PrevListID != nil {
		t.Fatalf("move to doing %+v", m)
	}
	// archive remembers prev, restore returns there
	m = call[Card](t, f.e, "card.archive", ArchiveIn{ID: c.ID})
	if m.ListID != f.arch || m.PrevListID == nil || *m.PrevListID != f.doing {
		t.Fatalf("archive %+v", m)
	}
	m = call[Card](t, f.e, "card.move", MoveIn{ID: c.ID, ListID: f.trash}) // archive -> trash keeps the original prev
	if *m.PrevListID != f.doing {
		t.Fatalf("prev after archive->trash %v", *m.PrevListID)
	}
	m = call[Card](t, f.e, "card.restore", IDIn{c.ID})
	if m.ListID != f.doing || m.PrevListID != nil {
		t.Fatalf("restore %+v", m)
	}
	// close goes to the closed list and back
	m = call[Card](t, f.e, "card.close", CloseIn{ID: c.ID})
	if m.ListID != f.closed || !m.IsClosed {
		t.Fatalf("close %+v", m)
	}
	no := false
	m = call[Card](t, f.e, "card.close", CloseIn{ID: c.ID, Closed: &no})
	if m.ListID != f.doing || m.IsClosed {
		t.Fatalf("reopen %+v", m)
	}
	// cross-board move drops foreign labels and non-member card members
	call[Card](t, f.e, "card.label.add", LabelIn{ID: a.ID, LabelID: "lab-x"})
	call[Card](t, f.e, "card.member.add", MemberIn{ID: a.ID, UserID: f.vw})
	m = call[Card](t, f.e, "card.move", MoveIn{ID: a.ID, ListID: f.list2})
	if m.BoardID != f.board2 || len(m.LabelIDs) != 0 || len(m.MemberIDs) != 0 {
		t.Fatalf("cross-board %+v", m)
	}
	// moving to a board the user cannot edit is refused
	mustExec(t, f.e, `INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b3','p1',3,'b3',?)`, db.Now())
	mustExec(t, f.e, `INSERT INTO lists(id,board_id,type,position,created_at) VALUES('l-b3','b3','active',1,?)`, db.Now())
	if _, err := ops.Typed[Card](f.e, "card.move", MoveIn{ID: b.ID, ListID: "l-b3"}); err == nil {
		t.Fatal("move to inaccessible board must fail")
	}
}

func TestRelations(t *testing.T) {
	f := seed(t)
	c := f.card(t, f.todo, "rel")
	// single-select group: second label replaces the first
	call[Card](t, f.e, "card.label.add", LabelIn{ID: c.ID, LabelID: f.labelA})
	got := call[Card](t, f.e, "card.label.add", LabelIn{ID: c.ID, LabelID: f.labelB})
	if len(got.LabelIDs) != 1 || got.LabelIDs[0] != f.labelB {
		t.Fatalf("single select %v", got.LabelIDs)
	}
	got = call[Card](t, f.e, "card.label.add", LabelIn{ID: c.ID, LabelID: "lab-x"})
	if len(got.LabelIDs) != 2 {
		t.Fatalf("free label %v", got.LabelIDs)
	}
	if _, err := ops.Typed[Card](f.e, "card.label.add", LabelIn{ID: c.ID, LabelID: f.labelOther}); err == nil {
		t.Fatal("label of another board accepted")
	}
	got = call[Card](t, f.e, "card.label.remove", LabelRemoveIn{ID: c.ID, LabelID: "lab-x"})
	if len(got.LabelIDs) != 1 {
		t.Fatal("remove label")
	}
	// members must belong to the board
	if _, err := ops.Typed[Card](f.e, "card.member.add", MemberIn{ID: c.ID, UserID: f.st}); err == nil {
		t.Fatal("non-member added")
	}
	got = call[Card](t, f.e, "card.member.add", MemberIn{ID: c.ID, UserID: f.vw})
	if len(got.MemberIDs) != 1 {
		t.Fatal("member add")
	}
	var n int
	_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM card_subscriptions WHERE card_id=? AND user_id=?`, c.ID, f.vw).Scan(&n)
	if n != 1 {
		t.Fatal("member should be auto-subscribed")
	}
	got = call[Card](t, f.e, "card.member.remove", MemberRemoveIn{ID: c.ID, UserID: f.vw})
	if len(got.MemberIDs) != 0 {
		t.Fatal("member remove")
	}
	// subscriptions + read notifications (viewer may do both)
	s := call[SubscribeOut](t, f.viewer, "card.subscribe", IDIn{c.ID})
	if !s.Subscribed {
		t.Fatal("subscribe")
	}
	mustExec(t, f.e, `INSERT INTO notifications(id,user_id,card_id,type,created_at) VALUES('n1',?,?,'x',?),('n2','u-ed',?,'x',?)`, f.vw, c.ID, db.Now(), c.ID, db.Now())
	r := call[ReadOut](t, f.viewer, "card.notifications.read", IDIn{c.ID})
	if r.Marked < 1 { // notify may add its own subscriber notifications
		t.Fatalf("marked %d", r.Marked)
	}
	call[SubscribeOut](t, f.viewer, "card.unsubscribe", IDIn{c.ID})
	// duplicate keeps labels/members; hooks run
	call[Card](t, f.e, "card.member.add", MemberIn{ID: c.ID, UserID: f.vw})
	hooked := false
	DuplicateHooks = append(DuplicateHooks, func(e *ops.Env, tx *sql.Tx, src, dst string) error { hooked = src == c.ID && dst != ""; return nil })
	defer func() { DuplicateHooks = nil }()
	d := call[Card](t, f.e, "card.duplicate", DuplicateIn{ID: c.ID})
	if d.Name != "rel (copy)" || len(d.LabelIDs) != 1 || len(d.MemberIDs) != 1 || !hooked || *d.Position <= *c.Position {
		t.Fatalf("duplicate %+v hooked=%v", d, hooked)
	}
}

func TestDueLocationCover(t *testing.T) {
	f := seed(t)
	c := f.card(t, f.todo, "due")
	got := call[Card](t, f.e, "card.due.set", DueIn{ID: c.ID, DueDate: "2026-11-12T09:30"})
	if *got.DueDate != "2026-11-12T09:30:00Z" {
		t.Fatalf("due %v", *got.DueDate)
	}
	out := call[DueOut](t, f.e, "card.due.complete", DueCompleteIn{ID: c.ID})
	if out.IsDueCompleted == nil || !*out.IsDueCompleted {
		t.Fatal("not completed")
	}
	got = call[Card](t, f.e, "card.due.set", DueIn{ID: c.ID})
	if got.DueDate != nil || got.IsDueCompleted != nil {
		t.Fatal("clearing due date must reset completion")
	}
	if _, err := ops.Typed[DueOut](f.e, "card.due.complete", DueCompleteIn{ID: c.ID}); err == nil {
		t.Fatal("complete without due date")
	}
	lat, lng := 48.85, 2.35
	got = call[Card](t, f.e, "card.location.set", LocationIn{ID: c.ID, Lat: &lat, Lng: &lng, Name: "Paris"})
	if *got.Lat != lat || *got.LocationName != "Paris" {
		t.Fatal("location")
	}
	bad := 120.0
	if _, err := ops.Typed[Card](f.e, "card.location.set", LocationIn{ID: c.ID, Lat: &bad, Lng: &lng}); err == nil {
		t.Fatal("lat range")
	}
	if _, err := ops.Typed[Card](f.e, "card.cover.set", CoverIn{ID: c.ID, AttachmentID: "nope"}); err == nil {
		t.Fatal("cover must reference an image attachment")
	}
	mustExec(t, f.e, `INSERT INTO attachments(id,card_id,type,name,is_image,created_at) VALUES('att1',?,'file','a.png',1,?)`, c.ID, db.Now())
	got = call[Card](t, f.e, "card.cover.set", CoverIn{ID: c.ID, AttachmentID: "att1"})
	if got.CoverAttachmentID == nil {
		t.Fatal("cover")
	}
	got = call[Card](t, f.e, "card.cover.set", CoverIn{ID: c.ID})
	if got.CoverAttachmentID != nil {
		t.Fatal("cover clear")
	}
}

func TestDraftsTemplatesShare(t *testing.T) {
	f := seed(t)
	if _, err := ops.Typed[Card](f.e, "card.draft.create", DraftCreateIn{BoardID: f.board, Name: "idea"}); err == nil {
		t.Fatal("inbox disabled must refuse")
	}
	call[InboxOut](t, f.admin, "card.inbox.set", InboxSetIn{BoardID: f.board, Enabled: true})
	dr := call[Card](t, f.e, "card.draft.create", DraftCreateIn{BoardID: f.board, Name: "idea"})
	if !dr.IsDraft {
		t.Fatal("draft flag")
	}
	if l := call[ListOut](t, f.e, "card.list", ListCardsIn{BoardID: f.board}); len(l.Items) != 0 {
		t.Fatal("drafts must be hidden from card.list")
	}
	if in := call[InboxOut](t, f.e, "card.inbox.list", InboxIn{f.board}); !in.Enabled || len(in.Items) != 1 {
		t.Fatalf("inbox %+v", in)
	}
	if _, err := ops.Typed[Card](f.e, "card.move", MoveIn{ID: dr.ID, ListID: f.doing}); err == nil {
		t.Fatal("moving a draft must be refused")
	}
	pub := call[Card](t, f.e, "card.draft.publish", PublishIn{ID: dr.ID, ListID: f.doing})
	if pub.IsDraft || pub.ListID != f.doing {
		t.Fatalf("publish %+v", pub)
	}

	// templates
	call[Card](t, f.e, "card.label.add", LabelIn{ID: pub.ID, LabelID: "lab-x"})
	tpl := call[Card](t, f.e, "card.template.save", TemplateSaveIn{ID: pub.ID, Name: "Bug report"})
	if !tpl.IsTemplate || tpl.TemplateProjectID == nil || *tpl.TemplateProjectID != f.proj {
		t.Fatalf("template %+v", tpl)
	}
	if l := call[ListOut](t, f.e, "card.list", ListCardsIn{BoardID: f.board}); len(l.Items) != 1 {
		t.Fatal("templates must be hidden from card.list")
	}
	tl := call[TemplatesOut](t, f.e, "card.template.list", TemplatesIn{f.proj})
	if len(tl.Items) != 1 {
		t.Fatal("template list")
	}
	nc := call[Card](t, f.e, "card.fromTemplate", FromTemplateIn{ID: tpl.ID, ListID: f.todo})
	if nc.IsTemplate || nc.Name != "Bug report" || len(nc.LabelIDs) != 1 || nc.ListID != f.todo {
		t.Fatalf("fromTemplate %+v", nc)
	}
	if _, err := ops.Typed[Card](f.e, "card.fromTemplate", FromTemplateIn{ID: nc.ID, ListID: f.todo}); err == nil {
		t.Fatal("non-template accepted")
	}
	if _, err := ops.Typed[TemplatesOut](f.stranger, "card.template.list", TemplatesIn{f.proj}); err == nil {
		t.Fatal("stranger lists templates")
	}

	// share link
	anon := testutil.As(f.e, nil)
	sh := call[ShareOut](t, f.e, "card.share.create", IDIn{nc.ID})
	again := call[ShareOut](t, f.e, "card.share.create", IDIn{nc.ID})
	if sh.Token == "" || sh.Token != again.Token {
		t.Fatal("share token must be stable")
	}
	v := call[SharedCard](t, anon, "card.share.view", ShareViewIn{sh.Token})
	if v.Name != "Bug report" || len(v.Labels) != 1 {
		t.Fatalf("shared view %+v", v)
	}
	if _, err := ops.Typed[ShareOut](f.viewer, "card.share.create", IDIn{nc.ID}); err == nil {
		t.Fatal("viewer created share link")
	}
	call[Deleted](t, f.e, "card.share.revoke", IDIn{nc.ID})
	if _, err := ops.Typed[SharedCard](anon, "card.share.view", ShareViewIn{sh.Token}); err == nil {
		t.Fatal("revoked link still works")
	}
	if _, err := ops.Typed[SharedCard](anon, "card.share.view", ShareViewIn{""}); err == nil {
		t.Fatal("empty token")
	}
}

func TestRecurrence(t *testing.T) {
	d := func(s string) time.Time { x, _ := time.Parse("2006-01-02", s); return x }
	for _, tc := range []struct {
		r    Recurrence
		from string
		want string
	}{
		{Recurrence{Freq: "daily", Interval: 3}, "2026-01-30", "2026-02-02"},
		{Recurrence{Freq: "weekly", Interval: 1}, "2026-11-10", "2026-11-17"},
		{Recurrence{Freq: "weekly", Interval: 1, Weekdays: []int{1, 3}}, "2026-11-10", "2026-11-11"}, // Tue -> Wed
		{Recurrence{Freq: "weekly", Interval: 1, Weekdays: []int{1, 3}}, "2026-11-11", "2026-11-16"}, // Wed -> next Mon
		{Recurrence{Freq: "weekly", Interval: 2, Weekdays: []int{1}}, "2026-11-09", "2026-11-23"},
		{Recurrence{Freq: "monthly", Interval: 1}, "2026-01-31", "2026-02-28"},
		{Recurrence{Freq: "yearly", Interval: 1}, "2024-02-29", "2025-02-28"},
	} {
		if got := nextDue(d(tc.from), &tc.r).Format("2006-01-02"); got != tc.want {
			t.Errorf("%+v from %s: got %s want %s", tc.r, tc.from, got, tc.want)
		}
	}

	f := seed(t)
	c := f.card(t, f.todo, "standup")
	if _, err := ops.Typed[Card](f.e, "card.recurrence.set", RecurrenceIn{ID: c.ID, Recurrence: `{"freq":"daily"}`}); err == nil {
		t.Fatal("recurrence without due date")
	}
	past := time.Now().UTC().AddDate(0, 0, -3).Format(time.RFC3339)
	call[Card](t, f.e, "card.due.set", DueIn{ID: c.ID, DueDate: past})
	if _, err := ops.Typed[Card](f.e, "card.recurrence.set", RecurrenceIn{ID: c.ID, Recurrence: `{"freq":"hourly"}`}); err == nil {
		t.Fatal("bad freq")
	}
	call[Card](t, f.e, "card.label.add", LabelIn{ID: c.ID, LabelID: "lab-x"})
	call[Card](t, f.e, "card.recurrence.set", RecurrenceIn{ID: c.ID, Recurrence: `{"freq":"daily","count":2}`})
	n, err := RunRecurrence(f.e.Ctx, f.e.DB, f.e.Hub, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("spawned %d err %v", n, err)
	}
	if n, _ = RunRecurrence(f.e.Ctx, f.e.DB, f.e.Hub, time.Now()); n != 0 {
		t.Fatalf("second sweep must be idempotent, spawned %d", n)
	}
	old := call[Card](t, f.e, "card.get", GetIn{ID: c.ID})
	if old.Recurrence != nil {
		t.Fatal("spawning card must give up the rule")
	}
	all := call[ListOut](t, f.e, "card.list", ListCardsIn{BoardID: f.board, ListID: f.todo})
	if len(all.Items) != 2 {
		t.Fatalf("want 2 cards got %d", len(all.Items))
	}
	var next Card
	for _, x := range all.Items {
		if x.ID != c.ID {
			next = x
		}
	}
	due, _ := time.Parse(time.RFC3339, *next.DueDate)
	if !due.After(time.Now()) || len(next.LabelIDs) != 1 || next.Recurrence == nil || !strings.Contains(*next.Recurrence, `"count":1`) {
		t.Fatalf("next %+v", next)
	}

	// completed trigger spawns on completion; final count ends the chain
	call[Card](t, f.e, "card.recurrence.set", RecurrenceIn{ID: next.ID, Recurrence: `{"freq":"weekly","trigger":"completed","count":1}`})
	if n, _ = RunRecurrence(f.e.Ctx, f.e.DB, f.e.Hub, time.Now().AddDate(1, 0, 0)); n != 0 {
		t.Fatal("completed trigger must not fire by date")
	}
	res := call[DueOut](t, f.e, "card.due.complete", DueCompleteIn{ID: next.ID})
	if res.NextCardID == "" {
		t.Fatal("completion should spawn")
	}
	last := call[Card](t, f.e, "card.get", GetIn{ID: res.NextCardID})
	if last.Recurrence != nil {
		t.Fatal("last occurrence must not recur")
	}
	// manual run
	call[Card](t, f.e, "card.recurrence.set", RecurrenceIn{ID: last.ID, Recurrence: `{"freq":"monthly"}`})
	if r := call[RecurrenceRunOut](t, f.e, "card.recurrence.run", IDIn{last.ID}); r.NextCardID == "" {
		t.Fatal("manual run")
	}
}

func TestEditLocks(t *testing.T) {
	f := seed(t)
	mustExec(t, f.e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m-ad2','b1',?,'editor',?)`, f.st, db.Now())
	c := f.card(t, f.todo, "locks")
	me := call[EditTouchOut](t, f.e, "card.edit.touch", EditTouchIn{ID: c.ID})
	if !me.Granted || len(me.Editors) != 1 {
		t.Fatalf("first touch %+v", me)
	}
	if again := call[EditTouchOut](t, f.e, "card.edit.touch", EditTouchIn{ID: c.ID}); !again.Granted {
		t.Fatal("heartbeat must stay granted")
	}
	other := call[EditTouchOut](t, f.stranger, "card.edit.touch", EditTouchIn{ID: c.ID, Takeover: true})
	if other.Granted || other.Holder == nil || other.Holder.Name != "Ed Itor" || other.Holder.Idle {
		t.Fatalf("fresh lock must block even with takeover: %+v", other)
	}
	if _, err := ops.Typed[EditTouchOut](f.viewer, "card.edit.touch", EditTouchIn{ID: c.ID}); err == nil {
		t.Fatal("viewers cannot edit")
	}
	// holder goes idle
	old := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	mustExec(t, f.e, `UPDATE card_editing SET touched_at=?`, old)
	blocked := call[EditTouchOut](t, f.stranger, "card.edit.touch", EditTouchIn{ID: c.ID})
	if blocked.Granted || !blocked.Holder.Idle {
		t.Fatalf("idle lock requires explicit takeover: %+v", blocked)
	}
	won := call[EditTouchOut](t, f.stranger, "card.edit.touch", EditTouchIn{ID: c.ID, Takeover: true})
	if !won.Granted || len(won.Editors) != 1 || won.Editors[0].UserID != f.st {
		t.Fatalf("takeover %+v", won)
	}
	lst := call[EditListOut](t, f.viewer, "card.edit.list", IDIn{c.ID})
	if len(lst.Editors) != 1 {
		t.Fatal("viewer sees editors")
	}
	call[EditListOut](t, f.stranger, "card.edit.release", EditReleaseIn{ID: c.ID})
	if lst = call[EditListOut](t, f.viewer, "card.edit.list", IDIn{c.ID}); len(lst.Editors) != 0 {
		t.Fatal("release")
	}
}

func TestCalendarAndMap(t *testing.T) {
	f := seed(t)
	a, b, c := f.card(t, f.todo, "a"), f.card(t, f.todo, "b"), f.card(t, f.todo, "c")
	call[Card](t, f.e, "card.due.set", DueIn{ID: a.ID, DueDate: "2026-11-12"})
	call[Card](t, f.e, "card.due.set", DueIn{ID: b.ID, DueDate: "2026-12-31"}) // visible in the Nov grid? Nov 30 is Monday; grid ends Sun Dec 6
	call[Card](t, f.e, "card.due.set", DueIn{ID: c.ID, DueDate: "2027-03-01"})
	cal := call[CalendarOut](t, f.e, "card.calendar", CalendarIn{BoardID: f.board, Month: "2026-11"})
	if cal.From != "2026-10-26" || cal.To != "2026-12-06" || len(cal.Cards) != 1 || cal.Cards[0].ID != a.ID {
		t.Fatalf("calendar %+v", cal)
	}
	if len(cal.Unscheduled) != 0 {
		t.Fatalf("unscheduled %+v", cal.Unscheduled)
	}
	if _, err := ops.Typed[CalendarOut](f.e, "card.calendar", CalendarIn{BoardID: f.board, Month: "nov"}); err == nil {
		t.Fatal("bad month")
	}
	if _, err := ops.Typed[CalendarOut](f.stranger, "card.calendar", CalendarIn{BoardID: f.board}); err == nil {
		t.Fatal("stranger calendar")
	}
	lat, lng := 1.5, -2.5
	call[Card](t, f.e, "card.location.set", LocationIn{ID: a.ID, Lat: &lat, Lng: &lng})
	mp := call[MapOut](t, f.e, "card.map", InboxIn{f.board})
	if len(mp.Points) != 1 || mp.Points[0].Lng != -2.5 {
		t.Fatalf("map %+v", mp)
	}
}

func TestRoutesDoNotConflict(t *testing.T) {
	mux := http.NewServeMux() // panics on ambiguous patterns
	n := 0
	for _, o := range ops.All() {
		if strings.HasPrefix(o.Name, "card.") {
			mux.HandleFunc(o.Method+" /api/v1"+o.Path, func(http.ResponseWriter, *http.Request) {})
			n++
		}
	}
	if n < 35 {
		t.Fatalf("only %d card ops registered", n)
	}
}

func TestMarkdown(t *testing.T) {
	got := string(RenderMarkdown("# Title\nhello **bold** and _it_ `x<y>` [l](https://a.io) [bad](javascript:alert(1))\n\n- a\n- [x] done\n\n<script>alert(1)</script>"))
	for _, want := range []string{"<h3>Title</h3>", "<strong>bold</strong>", "<em>it</em>", "<code>x&lt;y&gt;</code>", `href="https://a.io"`, "<li>a</li>", "checked", "&lt;script&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "<script") || strings.Contains(got, `href="javascript`) {
		t.Fatalf("unsafe output: %s", got)
	}
}

// ---- UI

func mount(f *fx, who *ops.Env) *http.ServeMux {
	mux := http.NewServeMux()
	web.Mount(mux, func(r *http.Request) *ops.Env {
		e := *who
		e.Req = r
		e.Ctx = r.Context()
		if r.Header.Get("X-Anon") != "" {
			e.User = nil
		}
		return &e
	})
	return mux
}

func do(mux *http.ServeMux, method, path string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("HX-Request", "true")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestUI(t *testing.T) {
	f := seed(t)
	mux := mount(f, f.e)
	c := f.card(t, f.todo, "Ship <it>")
	call[Card](t, f.e, "card.member.add", MemberIn{ID: c.ID, UserID: f.vw})

	w := do(mux, "GET", "/ui/cards/"+c.ID, nil)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `value="Ship &lt;it&gt;"`) {
		t.Fatalf("modal %d %s", w.Code, body)
	}
	for _, sec := range []string{"tasks", "comments", "attachments", "fields", "time", "activity"} {
		if !strings.Contains(body, `hx-get="/ui/cards/`+c.ID+`/`+sec+`" hx-trigger="load"`) {
			t.Errorf("missing lazy section %s", sec)
		}
	}
	// full page works too
	if w = do(mux, "GET", "/cards/"+c.ID, nil); w.Code != 200 {
		t.Fatalf("page %d", w.Code)
	}

	// description editing flow with locks
	w = do(mux, "POST", "/ui/cards/"+c.ID+"/editing/touch", url.Values{"field": {"description"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<textarea") {
		t.Fatalf("touch %d %s", w.Code, w.Body.String())
	}
	other := mount(f, f.stranger)
	mustExec(t, f.e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m-st','b1',?,'editor',?)`, f.st, db.Now())
	w = do(other, "POST", "/ui/cards/"+c.ID+"/editing/touch", url.Values{"field": {"description"}})
	if !strings.Contains(w.Body.String(), "Ed Itor</strong> is editing") || strings.Contains(w.Body.String(), "Take over") {
		t.Fatalf("locked view: %s", w.Body.String())
	}
	mustExec(t, f.e, `UPDATE card_editing SET touched_at=?`, time.Now().UTC().Add(-3*time.Minute).Format(time.RFC3339))
	w = do(other, "POST", "/ui/cards/"+c.ID+"/editing/touch", url.Values{"field": {"description"}})
	if !strings.Contains(w.Body.String(), "Take over") {
		t.Fatalf("idle holder should offer takeover: %s", w.Body.String())
	}
	w = do(other, "POST", "/ui/cards/"+c.ID+"/editing/touch", url.Values{"field": {"description"}, "takeover": {"true"}})
	if !strings.Contains(w.Body.String(), "<textarea") {
		t.Fatal("takeover should open the editor")
	}
	// the displaced editor cannot overwrite
	w = do(mux, "POST", "/ui/cards/"+c.ID+"/update", url.Values{"_part": {"desc"}, "description": {"mine"}})
	if w.Code != 409 {
		t.Fatalf("stale save should conflict, got %d", w.Code)
	}
	w = do(other, "POST", "/ui/cards/"+c.ID+"/update", url.Values{"_part": {"desc"}, "description": {"**hi**"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<strong>hi</strong>") {
		t.Fatalf("save %d %s", w.Code, w.Body.String())
	}

	// sheet extras: properties rail, actions menu, discussion tabs, composer, skeletons, prev/next, title submits on Enter
	for _, want := range []string{`data-pop="members"`, `data-pop="labels"`, `data-pop="due"`, `data-pop="menu"`, `data-open-pop="recurrence"`, `data-comment-input`, `data-tab="activity"`, `role="tablist"`, `aria-label="Properties"`, `aria-label="Breadcrumb"`, `class="cards-skel"`, `data-card-nav="next"`, `hx-trigger="change, submit"`} {
		if !strings.Contains(body, want) {
			t.Errorf("card sheet missing %q", want)
		}
	}
	// description autosave stores the draft and answers with a tiny "Saved" (editor keeps its lock)
	do(other, "POST", "/ui/cards/"+c.ID+"/editing/touch", url.Values{"field": {"description"}})
	w = do(other, "POST", "/ui/cards/"+c.ID+"/update", url.Values{"_part": {"autosave"}, "description": {"draft text"}})
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "Saved" {
		t.Fatalf("autosave %d %q", w.Code, w.Body.String())
	}
	if got := call[Card](t, f.e, "card.get", GetIn{ID: c.ID}); got.Description == nil || *got.Description != "draft text" {
		t.Fatalf("autosave did not persist: %v", got.Description)
	}

	// title, due, labels, members, recurrence, move via UI
	if w = do(mux, "POST", "/ui/cards/"+c.ID+"/update", url.Values{"_part": {"title"}, "name": {"Renamed"}}); !strings.Contains(w.Body.String(), "Renamed") {
		t.Fatal("title")
	}
	do(mux, "POST", "/ui/cards/"+c.ID+"/due", url.Values{"dueDate": {"2026-11-20T10:00"}})
	w = do(mux, "POST", "/ui/cards/"+c.ID+"/due", url.Values{"dueDate": {"2026-11-22"}, "_part": {"none"}})
	if w.Code != 204 {
		t.Fatalf("calendar drop %d", w.Code)
	}
	if got := call[Card](t, f.e, "card.get", GetIn{ID: c.ID}); *got.DueDate != "2026-11-22T10:00:00Z" {
		t.Fatalf("time of day must survive a reschedule: %s", *got.DueDate)
	}
	do(mux, "POST", "/ui/cards/"+c.ID+"/labels", url.Values{"labelId": {"lab-x"}})
	do(mux, "POST", "/ui/cards/"+c.ID+"/recurrence", url.Values{"freq": {"weekly"}, "interval": {"1"}, "weekdays": {"1", "4"}})
	got := call[Card](t, f.e, "card.get", GetIn{ID: c.ID})
	if len(got.LabelIDs) != 1 || got.Recurrence == nil || !strings.Contains(*got.Recurrence, `"weekdays":[1,4]`) {
		t.Fatalf("ui mutations %+v", got)
	}
	if w = do(mux, "POST", "/ui/cards/"+c.ID+"/move", url.Values{"container": {f.doing}, "index": {"0"}}); w.Code != 204 {
		t.Fatalf("sortable move %d %s", w.Code, w.Body.String())
	}
	if got = call[Card](t, f.e, "card.get", GetIn{ID: c.ID}); got.ListID != f.doing {
		t.Fatal("move via container")
	}
	// viewers get a modal without edit controls
	w = do(mount(f, f.viewer), "GET", "/ui/cards/"+c.ID, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "/due\"") || strings.Contains(w.Body.String(), "Archive") {
		t.Fatalf("viewer modal: %d", w.Code)
	}
	if w = do(mount(f, f.stranger), "GET", "/ui/cards/nope", nil); w.Code != 404 {
		t.Fatalf("missing card %d", w.Code)
	}

	// calendar + map pages
	w = do(mux, "GET", "/boards/"+f.board+"/calendar?month=2026-11", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-date="2026-11-22"`) || !strings.Contains(w.Body.String(), `data-card="`+c.ID+`"`) {
		t.Fatalf("calendar %d", w.Code)
	}
	lat, lng := 10.0, 20.0
	call[Card](t, f.e, "card.location.set", LocationIn{ID: c.ID, Lat: &lat, Lng: &lng, Name: "Spot"})
	w = do(mux, "GET", "/boards/"+f.board+"/map", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<circle") || !strings.Contains(w.Body.String(), "Spot") {
		t.Fatalf("map %d %s", w.Code, w.Body.String())
	}

	// share page is public
	sh := call[ShareOut](t, f.e, "card.share.create", IDIn{c.ID})
	w = do(mux, "GET", sh.URL, nil, "X-Anon", "1")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Renamed") || strings.Contains(w.Body.String(), "textarea") {
		t.Fatalf("share %d", w.Code)
	}
	if w = do(mux, "GET", "/share/bogus", nil, "X-Anon", "1"); w.Code != 404 {
		t.Fatalf("bogus share %d", w.Code)
	}

	// inbox + templates fragments
	call[InboxOut](t, f.admin, "card.inbox.set", InboxSetIn{BoardID: f.board, Enabled: true})
	w = do(mux, "POST", "/ui/boards/"+f.board+"/inbox", url.Values{"name": {"capture"}})
	if !strings.Contains(w.Body.String(), "capture") {
		t.Fatalf("inbox %s", w.Body.String())
	}
	call[Card](t, f.e, "card.template.save", TemplateSaveIn{ID: c.ID, Name: "Tpl"})
	w = do(mux, "GET", "/ui/projects/"+f.proj+"/card-templates?listId="+f.todo, nil)
	if !strings.Contains(w.Body.String(), "Tpl") || !strings.Contains(w.Body.String(), "from-template") {
		t.Fatalf("templates %s", w.Body.String())
	}

	// delete flow closes the modal
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: c.ID, Trash: true})
	w = do(mux, "POST", "/ui/cards/"+c.ID+"/delete", url.Values{})
	if w.Code != 204 || !strings.Contains(w.Header().Get("HX-Trigger"), "cardsClosed") {
		t.Fatalf("delete %d %v", w.Code, w.Header())
	}
}
