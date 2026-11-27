package cardextras_test

import (
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/cardextras"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

func TestCardBadgesOnAndRowAuthAndMultiBoardComments(t *testing.T) {
	e := fixture(t)
	now := "2026-01-01T00:00:00Z"
	// Second board b2 and second user u-b2 (viewer on b1, editor on b2)
	_, err := e.DB.Exec(`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b2','p1',131072,'B2',?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.DB.Exec(`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('l2','b2','active',65536,'Todo2',?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.DB.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES('u-b2','b2@x','B2Editor','boardUser',?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.DB.Exec(`INSERT INTO board_memberships(id,board_id,user_id,role,can_comment,created_at) VALUES
		('mb2-e','b2','ued','editor',1,?),
		('mb1-u','b1','u-b2','viewer',0,?),
		('mb2-u','b2','u-b2','editor',1,?)`, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	// Place c1 on b2 as well
	_, err = e.DB.Exec(`INSERT INTO card_placements(card_id,board_id,list_id,position,list_changed_at,is_home,placed_by,placed_at) VALUES('c1','b2','l2',65536,?,0,'ued',?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}

	// Board-wide front-of-card fields on b1 and b2
	g1 := call[cardextras.GroupItem](t, e, "customfieldgroup.create", cardextras.GroupCreateIn{BoardID: "b1", Name: "G1"})
	f1 := call[cardextras.FieldItem](t, e, "customfield.create", cardextras.FieldCreateIn{GroupID: g1.ID, Name: "F1", Type: "text", ShowOnFrontOfCard: true})
	call[cardextras.ValueItem](t, e, "customfieldvalue.set", cardextras.ValueSetIn{CardID: "c1", GroupID: g1.ID, FieldID: f1.ID, Content: "v1"})

	g2 := call[cardextras.GroupItem](t, e, "customfieldgroup.create", cardextras.GroupCreateIn{BoardID: "b2", Name: "G2"})
	f2 := call[cardextras.FieldItem](t, e, "customfield.create", cardextras.FieldCreateIn{GroupID: g2.ID, Name: "F2", Type: "text", ShowOnFrontOfCard: true})
	call[cardextras.ValueItem](t, e, "customfieldvalue.set", cardextras.ValueSetIn{CardID: "c1", GroupID: g2.ID, FieldID: f2.ID, Content: "v2"})

	// CardBadges filters to home board (b1)
	bHome, err := cardextras.CardBadges(e, []string{"c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bHome["c1"].Fields) != 1 || bHome["c1"].Fields[0].Name != "F1" {
		t.Fatalf("CardBadges leaked non-home field: %+v", bHome["c1"].Fields)
	}
	// CardBadgesOn filters to b2
	bTwo, err := cardextras.CardBadgesOn(e, "b2", []string{"c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bTwo["c1"].Fields) != 1 || bTwo["c1"].Fields[0].Name != "F2" {
		t.Fatalf("CardBadgesOn(b2) mismatch: %+v", bTwo["c1"].Fields)
	}

	// rowAuth enforces appliesTo: repeatable group on b1 cannot be mutated by u-b2 (only editor on b2, viewer on b1)
	rep1 := call[cardextras.GroupItem](t, e, "customfieldgroup.create", cardextras.GroupCreateIn{BoardID: "b1", Name: "Rep1", Repeatable: true})
	row1 := call[cardextras.RowItem](t, e, "customfieldrow.create", cardextras.RowCreateIn{CardID: "c1", GroupID: rep1.ID})
	b2Editor := testutil.As(e, &ops.Principal{UserID: "u-b2", Name: "B2Editor", Role: "boardUser"})
	wantErr(t, b2Editor, "customfieldrow.update", cardextras.RowUpdateIn{ID: row1.ID, Position: ptr(100.0)}, "not_found")
	wantErr(t, b2Editor, "customfieldrow.delete", cardextras.IDIn{ID: row1.ID}, "not_found")

	// Multi-board comment.list / comment.create uses max level across placements (u-b2 is viewer on home b1, editor on b2)
	cm := call[cardextras.CommentItem](t, b2Editor, "comment.create", cardextras.CommentCreateIn{CardID: "c1", Text: "from b2 editor"})
	if !cm.CanEdit || !cm.CanDelete {
		t.Fatalf("expected CanEdit/CanDelete true on comment.create: %+v", cm)
	}
	cl := call[cardextras.CommentsOut](t, b2Editor, "comment.list", cardextras.CommentsIn{CardID: "c1"})
	if !cl.CanComment || len(cl.Items) != 1 || !cl.Items[0].CanEdit {
		t.Fatalf("expected CanComment/CanEdit true on comment.list: %+v", cl)
	}

	// Cross-card Before cursor on comment.list returns empty
	other := call[cardextras.CommentItem](t, e, "comment.create", cardextras.CommentCreateIn{CardID: "c2", Text: "c2 comment"})
	scoped := call[cardextras.CommentsOut](t, e, "comment.list", cardextras.CommentsIn{CardID: "c1", Before: other.ID})
	if len(scoped.Items) != 0 {
		t.Fatalf("cross-card comment cursor leaked rows: %+v", scoped.Items)
	}
	wantErr(t, e, "comment.update", cardextras.CommentUpdateIn{ID: other.ID, Text: strings.Repeat("a", (1<<20)+1)}, "validation")
}

func TestPlaceSingleNeighborAndValidationFixes(t *testing.T) {
	e := fixture(t)
	tl := call[cardextras.TaskListItem](t, e, "tasklist.create", cardextras.TaskListCreateIn{CardID: "c1", Name: "L"})
	t1 := call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "t1"})
	t2 := call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "t2"})
	t3 := call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "t3"})

	// Move t3 after t1 (afterId-only): must land between t1 and t2, NOT after t2!
	moved := call[cardextras.TaskItem](t, e, "task.update", cardextras.TaskUpdateIn{ID: t3.ID, AfterID: t1.ID})
	if !(moved.Position > t1.Position && moved.Position < t2.Position) {
		t.Fatalf("afterId-only did not place midpoint: t1=%v t3=%v t2=%v", t1.Position, moved.Position, t2.Position)
	}
	// Self-neighbor is ignored (no error, keeps position)
	same := call[cardextras.TaskItem](t, e, "task.update", cardextras.TaskUpdateIn{ID: t3.ID, AfterID: t3.ID})
	if same.Position != moved.Position {
		t.Fatalf("self afterId changed position: %v vs %v", same.Position, moved.Position)
	}

	// Custom field NaN/Inf rejection + empty checkbox clears + name length limits
	g := call[cardextras.GroupItem](t, e, "customfieldgroup.create", cardextras.GroupCreateIn{CardID: "c1", Name: "G"})
	fNum := call[cardextras.FieldItem](t, e, "customfield.create", cardextras.FieldCreateIn{GroupID: g.ID, Name: "N", Type: "number"})
	fChk := call[cardextras.FieldItem](t, e, "customfield.create", cardextras.FieldCreateIn{GroupID: g.ID, Name: "C", Type: "checkbox"})
	wantErr(t, e, "customfieldvalue.set", cardextras.ValueSetIn{CardID: "c1", GroupID: g.ID, FieldID: fNum.ID, Content: "NaN"}, "validation")
	wantErr(t, e, "customfieldvalue.set", cardextras.ValueSetIn{CardID: "c1", GroupID: g.ID, FieldID: fNum.ID, Content: "Inf"}, "validation")
	call[cardextras.ValueItem](t, e, "customfieldvalue.set", cardextras.ValueSetIn{CardID: "c1", GroupID: g.ID, FieldID: fChk.ID, Content: "true"})
	cleared := call[cardextras.ValueItem](t, e, "customfieldvalue.set", cardextras.ValueSetIn{CardID: "c1", GroupID: g.ID, FieldID: fChk.ID, Content: ""})
	if cleared.Content != "" {
		t.Fatalf("empty checkbox value did not clear: %+v", cleared)
	}
	tooLong := strings.Repeat("x", ops.MaxNameLen+1)
	wantErr(t, e, "tasklist.create", cardextras.TaskListCreateIn{CardID: "c1", Name: tooLong}, "validation")
	wantErr(t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: tooLong}, "validation")
	wantErr(t, e, "customfieldgroup.create", cardextras.GroupCreateIn{CardID: "c1", Name: tooLong}, "validation")
	wantErr(t, e, "customfield.create", cardextras.FieldCreateIn{GroupID: g.ID, Name: tooLong}, "validation")

	// Time overflow + note length
	wantErr(t, e, "time.add", cardextras.TimeAddIn{CardID: "c1", Hours: 1 << 50}, "validation")
	wantErr(t, e, "time.add", cardextras.TimeAddIn{CardID: "c1", Minutes: 5, Note: strings.Repeat("n", 2001)}, "validation")

	// ActionText fixed keys
	if s := cardextras.ActionText(cardextras.ActionItem{Type: "cardCreate"}); s != "created the card" {
		t.Fatalf("cardCreate ActionText = %q", s)
	}
	if s := cardextras.ActionText(cardextras.ActionItem{Type: "cardMove"}); s != "moved the card" {
		t.Fatalf("cardMove ActionText = %q", s)
	}
	if s := cardextras.ActionText(cardextras.ActionItem{Type: "cardPlace"}); s != "placed the card on a board" {
		t.Fatalf("cardPlace ActionText = %q", s)
	}
}
