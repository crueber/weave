package cardextras_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/cardextras"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

var (
	admin     = &ops.Principal{UserID: "uadmin", Role: "admin", Name: "Admin"}
	editor    = &ops.Principal{UserID: "ued", Role: "boardUser", Name: "Edith"}
	viewer    = &ops.Principal{UserID: "uview", Role: "boardUser", Name: "Vic"}
	commenter = &ops.Principal{UserID: "ucom", Role: "boardUser", Name: "Cora"}
	outsider  = &ops.Principal{UserID: "uout", Role: "boardUser", Name: "Otto"}
)

// fixture builds project p1 / board b1 (public=false) / list l1 / cards c1,c2 and users with
// editor, viewer and commenter memberships.
func fixture(t *testing.T) *ops.Env {
	t.Helper()
	e := testutil.Env(t, editor)
	e.Svc["dataDir"] = t.TempDir()
	now := db.Now()
	stmts := []string{
		`INSERT INTO users(id,email,name,role,created_at) VALUES('uadmin','a@x','Admin','admin','` + now + `'),('ued','e@x','Edith','boardUser','` + now + `'),('uview','v@x','Vic','boardUser','` + now + `'),('ucom','c@x','Cora','boardUser','` + now + `'),('uout','o@x','Otto','boardUser','` + now + `')`,
		`INSERT INTO projects(id,name,created_at) VALUES('p1','P','` + now + `')`,
		`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'B','` + now + `')`,
		`INSERT INTO board_memberships(id,board_id,user_id,role,can_comment,created_at) VALUES('m1','b1','ued','editor',NULL,'` + now + `'),('m2','b1','uview','viewer',0,'` + now + `'),('m3','b1','ucom','viewer',1,'` + now + `')`,
		`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('l1','b1','active',1,'L','` + now + `')`,
		`INSERT INTO cards(id,board_id,list_id,position,name,list_changed_at,created_at) VALUES('c1','b1','l1',1,'Card one','` + now + `','` + now + `'),('c2','b1','l1',2,'Card two','` + now + `','` + now + `')`,
	}
	for _, s := range stmts {
		if _, err := e.DB.Exec(s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
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

func wantErr(t *testing.T, e *ops.Env, op string, in any, code string) {
	t.Helper()
	_, err := ops.Typed[any](e, op, in)
	var oe *ops.Error
	if !errors.As(err, &oe) || oe.Code != code {
		t.Fatalf("%s: want %s error, got %v", op, code, err)
	}
}

func TestTasks(t *testing.T) {
	e := fixture(t)
	tl := call[cardextras.TaskListItem](t, e, "tasklist.create", cardextras.TaskListCreateIn{CardID: "c1", Name: "Todo", ShowOnFrontOfCard: true})
	tl2 := call[cardextras.TaskListItem](t, e, "tasklist.create", cardextras.TaskListCreateIn{CardID: "c1", Name: "Done"})
	if tl2.Position <= tl.Position {
		t.Fatal("task lists must append")
	}
	a := call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "a", AssigneeUserID: ptr("uview")})
	b := call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "b"})
	c := call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, LinkedCardID: ptr("c2")})
	if c.Name != "Card two" || c.LinkedCardID == nil || *c.LinkedCardID != "c2" {
		t.Fatalf("linked task: %+v", c)
	}
	if a.AssigneeName == nil || *a.AssigneeName != "Vic" {
		t.Fatalf("assignee: %+v", a)
	}
	wantErr(t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, LinkedCardID: ptr("c1")}, "validation")
	wantErr(t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID}, "validation")
	wantErr(t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "x", AssigneeUserID: ptr("nobody")}, "validation")

	// complete + reorder: put c between a and b, then move b to the other list.
	call[cardextras.TaskItem](t, e, "task.update", cardextras.TaskUpdateIn{ID: a.ID, IsCompleted: ptr(true)})
	call[cardextras.TaskItem](t, e, "task.update", cardextras.TaskUpdateIn{ID: c.ID, AfterID: a.ID, BeforeID: b.ID})
	call[cardextras.TaskItem](t, e, "task.update", cardextras.TaskUpdateIn{ID: b.ID, TaskListID: tl2.ID})
	call[cardextras.TaskItem](t, e, "task.update", cardextras.TaskUpdateIn{ID: a.ID, AssigneeUserID: ptr("")})
	lists := call[[]cardextras.TaskListItem](t, e, "tasklist.list", cardextras.TaskListsIn{CardID: "c1"})
	if len(lists) != 2 || len(lists[0].Tasks) != 2 || lists[0].Tasks[0].ID != a.ID || lists[0].Tasks[1].ID != c.ID {
		t.Fatalf("order: %+v", lists)
	}
	if lists[0].TasksCompleted != 1 || lists[0].TasksTotal != 2 || lists[0].Tasks[0].AssigneeUserID != nil || len(lists[1].Tasks) != 1 {
		t.Fatalf("counts: %+v", lists)
	}
	// repeated drops into a tiny gap force a rebalance and keep order.
	prev := a.ID
	for i := 0; i < 25; i++ {
		n := call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl2.ID, Name: "n"})
		call[cardextras.TaskItem](t, e, "task.update", cardextras.TaskUpdateIn{ID: n.ID, TaskListID: tl2.ID, AfterID: b.ID, BeforeID: ""})
		_ = prev
	}
	l2 := call[cardextras.TaskListItem](t, e, "tasklist.update", cardextras.TaskListUpdateIn{ID: tl2.ID, HideCompletedTasks: ptr(true), Name: ptr("Finished")})
	if !l2.HideCompletedTasks || l2.Name != "Finished" {
		t.Fatalf("update: %+v", l2)
	}
	call[cardextras.TaskListItem](t, e, "tasklist.update", cardextras.TaskListUpdateIn{ID: tl2.ID, BeforeID: tl.ID})
	lists = call[[]cardextras.TaskListItem](t, e, "tasklist.list", cardextras.TaskListsIn{CardID: "c1"})
	if lists[0].ID != tl2.ID {
		t.Fatal("task list reorder failed")
	}

	// permissions
	wantErr(t, testutil.As(e, viewer), "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "no"}, "forbidden")
	wantErr(t, testutil.As(e, outsider), "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "no"}, "not_found")
	wantErr(t, testutil.As(e, outsider), "tasklist.list", cardextras.TaskListsIn{CardID: "c1"}, "not_found")
	if len(call[[]cardextras.TaskListItem](t, testutil.As(e, viewer), "tasklist.list", cardextras.TaskListsIn{CardID: "c1"})) != 2 {
		t.Fatal("viewer should read")
	}

	// deleting the linked card keeps the task (FK SET NULL); deleting list cascades tasks.
	call[cardextras.DeletedOut](t, e, "task.delete", cardextras.IDIn{ID: a.ID})
	call[cardextras.DeletedOut](t, e, "tasklist.delete", cardextras.IDIn{ID: tl2.ID})
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE task_list_id=?`, tl2.ID).Scan(&n)
	if n != 0 {
		t.Fatal("tasks must cascade")
	}
}

func ptr[T any](v T) *T { return &v }

func TestComments(t *testing.T) {
	e := fixture(t)
	wantErr(t, testutil.As(e, viewer), "comment.create", cardextras.CommentCreateIn{CardID: "c1", Text: "hi"}, "forbidden")
	wantErr(t, testutil.As(e, outsider), "comment.create", cardextras.CommentCreateIn{CardID: "c1", Text: "hi"}, "not_found")
	ce := testutil.As(e, commenter)
	c1 := call[cardextras.CommentItem](t, ce, "comment.create", cardextras.CommentCreateIn{CardID: "c1", Text: " first "})
	if c1.Text != "first" || !c1.CanEdit || !c1.CanDelete {
		t.Fatalf("%+v", c1)
	}
	call[cardextras.CommentItem](t, e, "comment.create", cardextras.CommentCreateIn{CardID: "c1", Text: "second"})
	var total int
	_ = e.DB.QueryRow(`SELECT comments_total FROM cards WHERE id='c1'`).Scan(&total)
	if total != 2 {
		t.Fatalf("counter %d", total)
	}
	out := call[cardextras.CommentsOut](t, testutil.As(e, viewer), "comment.list", cardextras.CommentsIn{CardID: "c1"})
	if out.CanComment || out.Total != 2 || len(out.Items) != 2 || out.Items[0].Text != "second" || out.Items[0].CanEdit || out.Items[0].CanDelete {
		t.Fatalf("viewer list: %+v", out)
	}
	page := call[cardextras.CommentsOut](t, e, "comment.list", cardextras.CommentsIn{CardID: "c1", Limit: 1, Before: out.Items[0].ID})
	if len(page.Items) != 1 || page.Items[0].ID != c1.ID {
		t.Fatalf("cursor: %+v", page)
	}
	// edit: author only (even admin cannot), delete: author or manager.
	wantErr(t, e, "comment.update", cardextras.CommentUpdateIn{ID: c1.ID, Text: "x"}, "forbidden")
	wantErr(t, testutil.As(e, admin), "comment.update", cardextras.CommentUpdateIn{ID: c1.ID, Text: "x"}, "forbidden")
	up := call[cardextras.CommentItem](t, ce, "comment.update", cardextras.CommentUpdateIn{ID: c1.ID, Text: "edited"})
	if up.Text != "edited" || up.UpdatedAt == nil {
		t.Fatalf("%+v", up)
	}
	wantErr(t, e, "comment.delete", cardextras.IDIn{ID: c1.ID}, "forbidden")
	call[cardextras.DeletedOut](t, testutil.As(e, admin), "comment.delete", cardextras.IDIn{ID: c1.ID})
	_ = e.DB.QueryRow(`SELECT comments_total FROM cards WHERE id='c1'`).Scan(&total)
	if total != 1 {
		t.Fatalf("counter after delete %d", total)
	}
	mine := out.Items[0]
	call[cardextras.DeletedOut](t, e, "comment.delete", cardextras.IDIn{ID: mine.ID})
	wantErr(t, e, "comment.update", cardextras.CommentUpdateIn{ID: mine.ID, Text: "x"}, "not_found")
}

func pngBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func TestAttachments(t *testing.T) {
	e := fixture(t)
	dir := e.Svc["dataDir"].(string)
	img := call[cardextras.AttachmentItem](t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1",
		File: &ops.File{Name: "../../evil/pic.png", Data: pngBytes(800, 400)}})
	if !img.IsImage || img.Width == nil || *img.Width != 800 || img.ThumbnailURL == "" || !img.IsCover || img.Name != "pic.png" {
		t.Fatalf("image: %+v", img)
	}
	if _, err := os.Stat(filepath.Join(dir, "files", "attachments", img.ID, "thumb.jpg")); err != nil {
		t.Fatal("thumbnail missing")
	}
	f, _ := os.Open(filepath.Join(dir, "files", "attachments", img.ID, "thumb.jpg"))
	cfg, _, err := image.DecodeConfig(f)
	f.Close()
	if err != nil || cfg.Width != 360 || cfg.Height != 180 {
		t.Fatalf("thumb dims %v %v", cfg, err)
	}
	doc := call[cardextras.AttachmentItem](t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1",
		File: &ops.File{Name: "notes.txt", Data: []byte("hello world")}})
	if doc.IsImage || doc.IsCover || doc.ThumbnailURL != "" {
		t.Fatalf("doc: %+v", doc)
	}
	link := call[cardextras.AttachmentItem](t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1", URL: "https://example.com/x"})
	if link.Type != "link" || link.Name != "example.com" {
		t.Fatalf("link: %+v", link)
	}
	wantErr(t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1", URL: "javascript:alert(1)"}, "validation")
	wantErr(t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1"}, "validation")
	wantErr(t, testutil.As(e, viewer), "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1", URL: "https://a.b"}, "forbidden")

	var usage int64
	_ = e.DB.QueryRow(`SELECT bytes FROM storage_usage WHERE id=1`).Scan(&usage)
	if usage <= int64(len(pngBytes(800, 400))+11) {
		t.Fatalf("storage usage %d", usage)
	}

	// download: authorized users get the bytes, outsiders do not exist.
	st := call[*ops.Stream](t, testutil.As(e, viewer), "attachment.download", cardextras.AttachmentDownloadIn{ID: doc.ID, Name: "notes.txt"})
	if string(st.Data) != "hello world" || st.Name != "notes.txt" {
		t.Fatalf("download: %+v", st)
	}
	wantErr(t, testutil.As(e, outsider), "attachment.download", cardextras.AttachmentDownloadIn{ID: doc.ID, Name: "x"}, "not_found")
	wantErr(t, testutil.As(e, nil), "attachment.download", cardextras.AttachmentDownloadIn{ID: doc.ID, Name: "x"}, "not_found")
	th := call[*ops.Stream](t, e, "attachment.thumbnail", cardextras.ThumbnailIn{ID: img.ID})
	if th.ContentType != "image/jpeg" {
		t.Fatal("thumb type")
	}
	wantErr(t, e, "attachment.thumbnail", cardextras.ThumbnailIn{ID: doc.ID}, "not_found")
	// public board: anonymous may download.
	_, _ = e.DB.Exec(`UPDATE boards SET is_public=1 WHERE id='b1'`)
	call[*ops.Stream](t, testutil.As(e, nil), "attachment.download", cardextras.AttachmentDownloadIn{ID: doc.ID, Name: "x"})
	_, _ = e.DB.Exec(`UPDATE boards SET is_public=0 WHERE id='b1'`)

	// media filters
	count := func(typ string) int {
		return len(call[[]cardextras.AttachmentItem](t, e, "attachment.media", cardextras.MediaIn{BoardID: "b1", Type: typ}))
	}
	if count("") != 3 || count("image") != 1 || count("video") != 0 || count("doc") != 2 {
		t.Fatalf("media: all=%d image=%d video=%d doc=%d", count(""), count("image"), count("video"), count("doc"))
	}
	wantErr(t, e, "attachment.media", cardextras.MediaIn{BoardID: "b1", Type: "zip"}, "validation")
	wantErr(t, testutil.As(e, outsider), "attachment.media", cardextras.MediaIn{BoardID: "b1"}, "not_found")

	// cover handling
	wantErr(t, e, "attachment.cover", cardextras.AttachmentCoverIn{ID: doc.ID}, "validation")
	un := call[cardextras.AttachmentItem](t, e, "attachment.cover", cardextras.AttachmentCoverIn{ID: img.ID, Unset: true})
	if un.IsCover {
		t.Fatal("unset cover")
	}
	if !call[cardextras.AttachmentItem](t, e, "attachment.cover", cardextras.AttachmentCoverIn{ID: img.ID}).IsCover {
		t.Fatal("set cover")
	}
	if call[cardextras.AttachmentItem](t, e, "attachment.update", cardextras.AttachmentUpdateIn{ID: doc.ID, Name: "renamed"}).Name != "renamed" {
		t.Fatal("rename")
	}

	// delete frees storage, files and the cover pointer.
	call[cardextras.DeletedOut](t, e, "attachment.delete", cardextras.IDIn{ID: img.ID})
	if _, err := os.Stat(filepath.Join(dir, "files", "attachments", img.ID)); !os.IsNotExist(err) {
		t.Fatal("files not removed")
	}
	var cover *string
	_ = e.DB.QueryRow(`SELECT cover_attachment_id FROM cards WHERE id='c1'`).Scan(&cover)
	if cover != nil {
		t.Fatal("cover not cleared")
	}
	var after int64
	_ = e.DB.QueryRow(`SELECT bytes FROM storage_usage WHERE id=1`).Scan(&after)
	if after != 11 {
		t.Fatalf("storage after delete: %d", after)
	}
}

func TestCustomFields(t *testing.T) {
	e := fixture(t)
	now := db.Now()
	_, err := e.DB.Exec(`INSERT INTO base_custom_field_groups(id,project_id,name,created_at) VALUES('bg1','p1','Base',?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	bg := call[cardextras.FieldItem](t, admin2(e), "customfield.create", cardextras.FieldCreateIn{BaseGroupID: "bg1", Name: "Region", Type: "select", Options: []string{"EU", "US", "EU"}})
	if len(bg.Options) != 2 {
		t.Fatalf("options %v", bg.Options)
	}
	wantErr(t, e, "customfield.create", cardextras.FieldCreateIn{BaseGroupID: "bg1", Name: "X"}, "forbidden") // editor is not a project manager

	g := call[cardextras.GroupItem](t, e, "customfieldgroup.create", cardextras.GroupCreateIn{BoardID: "b1", Name: "Board info"})
	rep := call[cardextras.GroupItem](t, e, "customfieldgroup.create", cardextras.GroupCreateIn{CardID: "c1", Name: "Items", Repeatable: true})
	derived := call[cardextras.GroupItem](t, e, "customfieldgroup.create", cardextras.GroupCreateIn{CardID: "c1", BaseGroupID: "bg1"})
	if derived.Name != "Base" || len(derived.Fields) != 1 || g.Scope != "board" || rep.Scope != "card" {
		t.Fatalf("groups: %+v %+v %+v", g, rep, derived)
	}
	wantErr(t, e, "customfieldgroup.create", cardextras.GroupCreateIn{BoardID: "b1", CardID: "c1", Name: "x"}, "validation")
	wantErr(t, e, "customfieldgroup.create", cardextras.GroupCreateIn{CardID: "c1"}, "validation")

	field := func(gid, name, typ string, opts ...string) cardextras.FieldItem {
		return call[cardextras.FieldItem](t, e, "customfield.create", cardextras.FieldCreateIn{GroupID: gid, Name: name, Type: typ, Options: opts})
	}
	fText, fNum, fDate := field(g.ID, "Notes", "text"), field(g.ID, "Score", "number"), field(g.ID, "Due", "date")
	fCheck, fURL := field(g.ID, "Ok", "checkbox"), field(g.ID, "Link", "url")
	fSel := field(g.ID, "Tier", "select", "a", "b")
	wantErr(t, e, "customfield.create", cardextras.FieldCreateIn{GroupID: g.ID, Name: "bad", Type: "emoji"}, "validation")
	wantErr(t, e, "customfield.create", cardextras.FieldCreateIn{GroupID: g.ID, Name: "bad", Type: "select"}, "validation")
	wantErr(t, e, "customfield.create", cardextras.FieldCreateIn{GroupID: derived.ID, Name: "bad"}, "validation")

	set := func(card, gid, fid, row, content string) (cardextras.ValueItem, error) {
		out, err := ops.Call(e, "customfieldvalue.set", &cardextras.ValueSetIn{CardID: card, GroupID: gid, FieldID: fid, RowID: row, Content: content})
		if err != nil {
			return cardextras.ValueItem{}, err
		}
		return out.(cardextras.ValueItem), nil
	}
	ok := func(fid, in, want string) {
		t.Helper()
		v, err := set("c1", g.ID, fid, "", in)
		if err != nil || v.Content != want {
			t.Fatalf("set %q => %q, %v (want %q)", in, v.Content, err, want)
		}
	}
	bad := func(fid, in string) {
		t.Helper()
		if _, err := set("c1", g.ID, fid, "", in); err == nil {
			t.Fatalf("expected validation error for %q", in)
		}
	}
	ok(fText.ID, "hello", "hello")
	ok(fNum.ID, " 3.50 ", "3.5")
	bad(fNum.ID, "abc")
	ok(fDate.ID, "2026-03-04", "2026-03-04")
	bad(fDate.ID, "yesterday")
	ok(fCheck.ID, "on", "true")
	ok(fCheck.ID, "false", "false")
	bad(fCheck.ID, "maybe")
	ok(fURL.ID, "https://example.com/a", "https://example.com/a")
	bad(fURL.ID, "javascript:alert(1)")
	ok(fSel.ID, "b", "b")
	bad(fSel.ID, "zzz")
	if _, err := set("c1", g.ID, fText.ID, "row1", "x"); err == nil {
		t.Fatal("row id on non-repeatable group must fail")
	}

	// board-wide group applies to c2 too; values are per card.
	if _, err := set("c2", g.ID, fText.ID, "", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := set("c2", rep.ID, field(rep.ID, "q", "text").ID, "", "x"); err == nil {
		t.Fatal("card-level group must not apply to another card")
	}
	groups := call[[]cardextras.GroupItem](t, e, "customfieldgroup.list", cardextras.GroupListIn{CardID: "c1"})
	if len(groups) != 3 || groups[0].ID != g.ID || groups[0].Rows[0].Values[fText.ID] != "hello" {
		t.Fatalf("list: %+v", groups)
	}
	if call[[]cardextras.GroupItem](t, e, "customfieldgroup.list", cardextras.GroupListIn{CardID: "c2"})[0].Rows[0].Values[fText.ID] != "second" {
		t.Fatal("c2 value")
	}
	// clearing
	if _, err := set("c1", g.ID, fText.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	call[cardextras.ValueItem](t, e, "customfieldvalue.clear", cardextras.ValueClearIn{CardID: "c1", GroupID: g.ID, FieldID: fNum.ID})
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM custom_field_values WHERE card_id='c1' AND field_id IN (?,?)`, fText.ID, fNum.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("values not cleared: %d", n)
	}

	// derived group reads fields from the base group; values work through it.
	if _, err := set("c1", derived.ID, bg.ID, "", "EU"); err != nil {
		t.Fatal(err)
	}

	// repeatable group: rows, ordering, per-row values.
	cellF := field(rep.ID, "Item", "text")
	wantErr(t, e, "customfieldrow.create", cardextras.RowCreateIn{CardID: "c1", GroupID: g.ID}, "validation")
	r1 := call[cardextras.RowItem](t, e, "customfieldrow.create", cardextras.RowCreateIn{CardID: "c1", GroupID: rep.ID})
	r2 := call[cardextras.RowItem](t, e, "customfieldrow.create", cardextras.RowCreateIn{CardID: "c1", GroupID: rep.ID})
	r3 := call[cardextras.RowItem](t, e, "customfieldrow.create", cardextras.RowCreateIn{CardID: "c1", GroupID: rep.ID})
	if _, err := set("c1", rep.ID, cellF.ID, "", "x"); err == nil {
		t.Fatal("empty rowId on repeatable must fail")
	}
	for i, r := range []cardextras.RowItem{r1, r2, r3} {
		if _, err := set("c1", rep.ID, cellF.ID, r.ID, string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	call[cardextras.RowItem](t, e, "customfieldrow.update", cardextras.RowUpdateIn{ID: r3.ID, BeforeID: r1.ID})
	groups = call[[]cardextras.GroupItem](t, e, "customfieldgroup.list", cardextras.GroupListIn{CardID: "c1"})
	var repG cardextras.GroupItem
	for _, x := range groups {
		if x.ID == rep.ID {
			repG = x
		}
	}
	if len(repG.Rows) != 3 || repG.Rows[0].ID != r3.ID || repG.Rows[0].Values[cellF.ID] != "c" || repG.Rows[2].ID != r2.ID {
		t.Fatalf("rows: %+v", repG.Rows)
	}
	wantErr(t, e, "customfieldgroup.update", cardextras.GroupUpdateIn{ID: rep.ID, Repeatable: ptr(false)}, "conflict")
	call[cardextras.DeletedOut](t, e, "customfieldrow.delete", cardextras.IDIn{ID: r1.ID})
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM custom_field_values WHERE row_id=?`, r1.ID).Scan(&n)
	if n != 0 {
		t.Fatal("row values must be deleted")
	}

	// changing a field type clears its values; group reorder; delete cascades.
	call[cardextras.FieldItem](t, e, "customfield.update", cardextras.FieldUpdateIn{ID: cellF.ID, Type: ptr("number")})
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM custom_field_values WHERE field_id=?`, cellF.ID).Scan(&n)
	if n != 0 {
		t.Fatal("type change must clear values")
	}
	call[cardextras.GroupItem](t, e, "customfieldgroup.update", cardextras.GroupUpdateIn{ID: rep.ID, Name: ptr("Lines")})
	call[cardextras.FieldItem](t, e, "customfield.update", cardextras.FieldUpdateIn{ID: fURL.ID, BeforeID: fText.ID, ShowOnFrontOfCard: ptr(true)})
	call[cardextras.DeletedOut](t, e, "customfield.delete", cardextras.IDIn{ID: fSel.ID})
	call[cardextras.DeletedOut](t, e, "customfieldgroup.delete", cardextras.IDIn{ID: rep.ID})
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM custom_field_rows`).Scan(&n)
	if n != 0 {
		t.Fatal("rows must cascade")
	}

	// permissions
	wantErr(t, testutil.As(e, viewer), "customfieldvalue.set", cardextras.ValueSetIn{CardID: "c1", GroupID: g.ID, FieldID: fText.ID, Content: "z"}, "forbidden")
	wantErr(t, testutil.As(e, outsider), "customfieldgroup.list", cardextras.GroupListIn{CardID: "c1"}, "not_found")

	// face-of-card summary
	b, err := cardextras.CardBadges(e, []string{"c1", "c2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b["c1"].Fields) != 1 || b["c1"].Fields[0].Name != "Link" {
		t.Fatalf("badges: %+v", b["c1"])
	}
}

func admin2(e *ops.Env) *ops.Env { return testutil.As(e, admin) }

func TestTimeTracking(t *testing.T) {
	e := fixture(t)
	s := call[cardextras.TimeSummary](t, e, "time.start", cardextras.CardIDIn{CardID: "c1"})
	if !s.Running || s.StartedAt == nil || s.RunningUserName == nil {
		t.Fatalf("%+v", s)
	}
	wantErr(t, e, "time.start", cardextras.CardIDIn{CardID: "c1"}, "conflict")
	// pretend it ran for 90 seconds
	_, _ = e.DB.Exec(`UPDATE cards SET stopwatch_started_at=? WHERE id='c1'`, time.Now().UTC().Add(-90*time.Second).Format(time.RFC3339))
	live := call[cardextras.TimeSummary](t, e, "time.list", cardextras.CardIDIn{CardID: "c1"})
	if live.TotalSeconds < 90 || live.RecordedSeconds != 0 {
		t.Fatalf("live: %+v", live)
	}
	s = call[cardextras.TimeSummary](t, testutil.As(e, admin), "time.stop", cardextras.CardIDIn{CardID: "c1"})
	if s.Running || s.RecordedSeconds < 90 || s.RecordedSeconds > 95 || len(s.Entries) != 1 || s.Entries[0].UserID == nil || *s.Entries[0].UserID != "ued" {
		t.Fatalf("stopped: %+v", s)
	}
	wantErr(t, e, "time.stop", cardextras.CardIDIn{CardID: "c1"}, "conflict")
	var started *string
	var total int
	_ = e.DB.QueryRow(`SELECT stopwatch_started_at,stopwatch_total_seconds FROM cards WHERE id='c1'`).Scan(&started, &total)
	if started != nil || total != s.RecordedSeconds {
		t.Fatalf("cards.stopwatch_* inconsistent: %v %d", started, total)
	}

	s = call[cardextras.TimeSummary](t, e, "time.add", cardextras.TimeAddIn{CardID: "c1", Hours: 1, Minutes: 30, Note: "manual"})
	if s.RecordedSeconds != total+5400 || len(s.Entries) != 2 {
		t.Fatalf("add: %+v", s)
	}
	wantErr(t, e, "time.add", cardextras.TimeAddIn{CardID: "c1"}, "validation")
	wantErr(t, e, "time.add", cardextras.TimeAddIn{CardID: "c1", Minutes: 1, StartedAt: "nope"}, "validation")
	manual := s.Entries[0]
	for _, en := range s.Entries {
		if en.Note != nil && *en.Note == "manual" {
			manual = en
		}
	}
	s = call[cardextras.TimeSummary](t, e, "time.update", cardextras.TimeUpdateIn{ID: manual.ID, Minutes: ptr(10), Hours: ptr(0), Note: ptr("fixed")})
	if s.RecordedSeconds != total+600 {
		t.Fatalf("update total %d", s.RecordedSeconds)
	}
	call[cardextras.DeletedOut](t, e, "time.delete", cardextras.IDIn{ID: manual.ID})
	_ = e.DB.QueryRow(`SELECT stopwatch_total_seconds FROM cards WHERE id='c1'`).Scan(&total)
	if total != s.RecordedSeconds-600 {
		t.Fatalf("delete total %d", total)
	}
	call[cardextras.TimeSummary](t, e, "time.start", cardextras.CardIDIn{CardID: "c1"})
	s = call[cardextras.TimeSummary](t, e, "time.reset", cardextras.CardIDIn{CardID: "c1"})
	if s.Running || s.TotalSeconds != 0 || len(s.Entries) != 0 {
		t.Fatalf("reset: %+v", s)
	}
	wantErr(t, testutil.As(e, viewer), "time.start", cardextras.CardIDIn{CardID: "c1"}, "forbidden")
	wantErr(t, testutil.As(e, outsider), "time.list", cardextras.CardIDIn{CardID: "c1"}, "not_found")
}

func TestActivity(t *testing.T) {
	e := fixture(t)
	// standalone path (no notify hook): ops record actions themselves.
	tl := call[cardextras.TaskListItem](t, e, "tasklist.create", cardextras.TaskListCreateIn{CardID: "c1", Name: "L"})
	call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "do it"})
	call[cardextras.CommentItem](t, e, "comment.create", cardextras.CommentCreateIn{CardID: "c1", Text: "hello there"})
	call[cardextras.CommentItem](t, e, "comment.create", cardextras.CommentCreateIn{CardID: "c2", Text: "other card"})
	acts := call[[]cardextras.ActionItem](t, e, "activity.card", cardextras.CardActionsIn{CardID: "c1"})
	if len(acts) != 3 || acts[0].Type != "commentCreate" || acts[0].UserName == nil || *acts[0].UserName != "Edith" {
		t.Fatalf("card actions: %+v", acts)
	}
	if txt := cardextras.ActionText(acts[0]); !strings.Contains(txt, "commented") || !strings.Contains(txt, "hello there") {
		t.Fatalf("text %q", txt)
	}
	board := call[[]cardextras.ActionItem](t, e, "activity.board", cardextras.ActionsIn{BoardID: "b1"})
	if len(board) != 4 || board[0].CardName == nil {
		t.Fatalf("board actions: %d", len(board))
	}
	page := call[[]cardextras.ActionItem](t, e, "activity.board", cardextras.ActionsIn{BoardID: "b1", Limit: 2, Before: board[1].ID})
	if len(page) != 2 || page[0].ID != board[2].ID {
		t.Fatalf("cursor page: %+v", page)
	}
	wantErr(t, testutil.As(e, outsider), "activity.board", cardextras.ActionsIn{BoardID: "b1"}, "not_found")

	// through the notify-installed hook: exported RecordAction is what it calls.
	old := ops.EmitHook
	defer func() { ops.EmitHook = old }()
	var seen []string
	ops.EmitHook = func(e *ops.Env, ev ops.Event) {
		seen = append(seen, ev.Type)
		if _, err := cardextras.RecordAction(e, ev); err != nil {
			t.Error(err)
		}
	}
	call[cardextras.CommentItem](t, e, "comment.create", cardextras.CommentCreateIn{CardID: "c1", Text: "via hook"})
	if len(seen) != 1 || seen[0] != "commentCreate" {
		t.Fatalf("hook: %v", seen)
	}
	if got := call[[]cardextras.ActionItem](t, e, "activity.card", cardextras.CardActionsIn{CardID: "c1"}); len(got) != 4 {
		t.Fatalf("hook should not double record: %d", len(got))
	}
	var data map[string]any
	if err := json.Unmarshal(acts[0].Data, &data); err != nil || data["commentId"] == nil {
		t.Fatalf("data: %v %v", data, err)
	}
}

func TestBadges(t *testing.T) {
	e := fixture(t)
	tl := call[cardextras.TaskListItem](t, e, "tasklist.create", cardextras.TaskListCreateIn{CardID: "c1", Name: "L", ShowOnFrontOfCard: true})
	tk := call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "x"})
	call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "y"})
	call[cardextras.TaskItem](t, e, "task.update", cardextras.TaskUpdateIn{ID: tk.ID, IsCompleted: ptr(true)})
	call[cardextras.CommentItem](t, e, "comment.create", cardextras.CommentCreateIn{CardID: "c1", Text: "c"})
	call[cardextras.AttachmentItem](t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1", URL: "https://a.example"})
	b, err := cardextras.CardBadges(e, []string{"c1", "c2"})
	if err != nil {
		t.Fatal(err)
	}
	c1 := b["c1"]
	if c1.TasksTotal != 2 || c1.TasksDone != 1 || c1.Comments != 1 || c1.Attachments != 1 || len(c1.TaskLists) != 1 || c1.TaskLists[0].Done != 1 {
		t.Fatalf("%+v", c1)
	}
	if b["c2"].TasksTotal != 0 {
		t.Fatal("c2 should be empty")
	}
}
