package boards_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/domain/cardextras"
	"github.com/crueber/weave/internal/domain/cards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
	"github.com/crueber/weave/internal/testutil"
)

// applyBoard is a populated board: Todo (t1, t2, a draft and a card template), Doing (d1), Done (closing: x1, x2,
// closed), an archived card and a trashed card; labels "high" (berry-red, not the template's colour) and "Extra"; a
// colour and an agent prompt of its own.
func applyBoard(t *testing.T, e *ops.Env, name string) (boards.Board, map[string]string) {
	t.Helper()
	b := newBoard(t, e, name)
	todo, doing, done := addList(t, e, b.ID, "Todo"), addList(t, e, b.ID, "Doing"), addList(t, e, b.ID, "Done")
	closed := "closed"
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: done.ID, Type: &closed})
	addCard(t, e, b.ID, todo.ID, name+"t1", "T1", 1, "")
	addCard(t, e, b.ID, todo.ID, name+"t2", "T2", 2, "")
	addCard(t, e, b.ID, doing.ID, name+"d1", "D1", 1, "")
	addCard(t, e, b.ID, done.ID, name+"x1", "X1", 1, "")
	addCard(t, e, b.ID, done.ID, name+"x2", "X2", 2, "")
	addCard(t, e, b.ID, todo.ID, name+"dr", "Draft", 3, "")
	addCard(t, e, b.ID, todo.ID, name+"ct", "Card template", 4, "")
	arch, _ := boards.SystemList(e.Ctx, e.DB, b.ID, "archive")
	trash, _ := boards.SystemList(e.Ctx, e.DB, b.ID, "trash")
	addCard(t, e, b.ID, arch, name+"ar", "Archived", 1, "")
	addCard(t, e, b.ID, trash, name+"tr", "Trashed", 1, "")
	mustExec(t, e, `UPDATE cards SET is_closed=1 WHERE id IN (?,?)`, name+"x1", name+"x2")
	mustExec(t, e, `UPDATE cards SET is_draft=1 WHERE id=?`, name+"dr")
	mustExec(t, e, `UPDATE cards SET is_template=1 WHERE id=?`, name+"ct")
	call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "high", Color: "berry-red"})
	call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "Extra", Color: "lagoon-blue"})
	color, prompt := "#123456", "Our own board prompt for {board_name}."
	b = call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, Color: &color, AgentPrompt: &prompt})
	return b, map[string]string{"todo": todo.ID, "doing": doing.ID, "done": done.ID, "archive": arch, "trash": trash}
}

func mustExec(t *testing.T, e *ops.Env, q string, args ...any) {
	t.Helper()
	if _, err := e.DB.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func columnNames(t *testing.T, e *ops.Env, boardID string) []string {
	t.Helper()
	rows, err := e.DB.Query(`SELECT name FROM lists WHERE board_id=? AND type IN ('active','closed') ORDER BY position`, boardID)
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

// snapshot is everything an apply may touch, as text, so "a dry run writes nothing" is a string comparison.
func snapshot(t *testing.T, e *ops.Env) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id||'|'||COALESCE(list_id,'')||'|'||position||'|'||is_closed||'|'||COALESCE(prev_list_id,'') FROM cards ORDER BY id`,
		`SELECT id||'|'||COALESCE(name,'')||'|'||type||'|'||COALESCE(position,'')||'|'||COALESCE(archive_after,'') FROM lists ORDER BY id`,
		`SELECT id||'|'||COALESCE(name,'')||'|'||color||'|'||COALESCE(group_id,'') FROM labels ORDER BY id`,
		`SELECT id||'|'||name FROM label_groups ORDER BY id`,
		`SELECT id||'|'||COALESCE(name,'')||'|'||position FROM custom_field_groups ORDER BY id`,
		`SELECT id||'|'||COALESCE(color,'')||'|'||card_color_mode||'|'||default_view||'|'||show_card_ages||'|'||COALESCE(agent_prompt,'') FROM boards ORDER BY id`,
		`SELECT COUNT(*) FROM actions`, `SELECT COUNT(*) FROM card_labels`,
		`SELECT list_id||'|'||user_id FROM list_subscriptions ORDER BY list_id, user_id`,
		`SELECT id||'|'||is_draft||'|'||is_template FROM cards ORDER BY id`,
	} {
		rows, err := e.DB.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			b.WriteString(s + "\n")
		}
		rows.Close()
	}
	return b.String()
}

// liveNames is the column's cards as the board shows them (no drafts, no card templates), top to bottom.
func liveNames(t *testing.T, e *ops.Env, listID string) []string {
	t.Helper()
	rows, err := e.DB.Query(`SELECT name FROM cards WHERE list_id=? AND is_draft=0 AND is_template=0 ORDER BY position`, listID)
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

func apply(t *testing.T, e *ops.Env, in boards.TemplateApplyIn) boards.TemplateApplyOut {
	t.Helper()
	return call[boards.TemplateApplyOut](t, e, "board.template.apply", in)
}

// sameNumbers drops what only a real run has (ids, the dry-run flag) so a preview and the real apply compare equal.
func sameNumbers(o boards.TemplateApplyOut) boards.TemplateApplyOut {
	o.DryRun = false
	o.TargetColumn.ID = ""
	cols := append([]boards.ApplyColumn{}, o.ColumnsAdded...)
	for i := range cols {
		cols[i].ID = ""
	}
	o.ColumnsAdded = cols
	return o
}

func TestTemplateApplyBuiltin(t *testing.T) {
	e := fixture(t)
	b, l := applyBoard(t, e, "ops")
	before := snapshot(t, e)
	pre := apply(t, e, boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", DryRun: true})
	if after := snapshot(t, e); after != before {
		t.Fatalf("the dry run wrote something:\n%s\nvs\n%s", before, after)
	}
	if !pre.DryRun || pre.CardsMoved != 5 || pre.ClosedCardsMoved != 2 || pre.TargetColumn.Name != "Backlog" || pre.TargetColumn.ID != "" {
		t.Fatalf("preview: %+v", pre)
	}
	var names []string
	for _, c := range pre.ColumnsAdded {
		names = append(names, c.Name)
	}
	// the default removes the old columns: none kept, so no duplicate names either
	if !reflect.DeepEqual(names, []string{"Backlog", "To do", "Doing", "Done"}) || !reflect.DeepEqual(pre.ColumnsRemoved, []string{"Todo", "Doing", "Done"}) ||
		len(pre.ColumnsKept) != 0 || len(pre.DuplicateColumnNames) != 0 || !pre.RemoveOldColumns {
		t.Fatalf("columns: %v removed %v kept %v dup %v", names, pre.ColumnsRemoved, pre.ColumnsKept, pre.DuplicateColumnNames)
	}
	if !reflect.DeepEqual(pre.LabelsReused, []string{"High"}) || !reflect.DeepEqual(pre.LabelsAdded, []string{"Low", "Medium", "Blocked"}) {
		t.Fatalf("labels: reused %v added %v", pre.LabelsReused, pre.LabelsAdded)
	}
	if len(pre.SettingsChanged) != 0 || len(pre.SettingsAvailable) == 0 || pre.SampleCardsAdded != 0 {
		t.Fatalf("settings/samples: %+v", pre)
	}
	// the real run needs confirm
	mustFail(t, e, "board.template.apply", boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics"}, "validation")
	if snapshot(t, e) != before {
		t.Fatal("an unconfirmed apply wrote something")
	}
	got := apply(t, e, boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", Confirm: true})
	if !reflect.DeepEqual(sameNumbers(got), sameNumbers(pre)) {
		t.Fatalf("preview and apply differ:\n%+v\n%+v", pre, got)
	}
	if got.TargetColumn.ID == "" || got.DryRun {
		t.Fatalf("real run: %+v", got)
	}
	if cols := columnNames(t, e, b.ID); !reflect.DeepEqual(cols, []string{"Backlog", "To do", "Doing", "Done"}) {
		t.Fatalf("column order %v", cols)
	}
	if n := count(t, e, `SELECT COUNT(*) FROM lists WHERE id IN (?,?,?)`, l["todo"], l["doing"], l["done"]); n != 0 {
		t.Fatalf("%d old columns left", n)
	}
	// every live card in the target, in board order; closed ones still closed; prev_list_id = where they came from
	if n := liveNames(t, e, got.TargetColumn.ID); !reflect.DeepEqual(n, []string{"T1", "T2", "D1", "X1", "X2"}) {
		t.Fatalf("target holds %v", n)
	}
	var closedN, prevDone int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE list_id=? AND is_closed=1`, got.TargetColumn.ID).Scan(&closedN)
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE list_id=? AND prev_list_id=?`, got.TargetColumn.ID, l["done"]).Scan(&prevDone)
	if closedN != 2 || prevDone != 2 {
		t.Fatalf("closed %d (want 2), prev=Done %d (want 2)", closedN, prevDone)
	}
	// archive and trash untouched; the draft and the card template survive the removal of their column: re-pointed
	// to the target, still a draft and a template, still hidden from the column
	for id, list := range map[string]string{"opsdr": got.TargetColumn.ID, "opsct": got.TargetColumn.ID, "opsar": l["archive"], "opstr": l["trash"]} {
		var at string
		_ = e.DB.QueryRow(`SELECT list_id FROM cards WHERE id=?`, id).Scan(&at)
		if at != list {
			t.Fatalf("%s is in %s, want %s", id, at, list)
		}
	}
	if n := count(t, e, `SELECT COUNT(*) FROM cards WHERE (id='opsdr' AND is_draft=1) OR (id='opsct' AND is_template=1)`); n != 2 {
		t.Fatal("the draft or the card template lost its flag")
	}
	// labels: the board's "high" kept exactly (name, colour), new ones added after it
	var hName, hColor string
	_ = e.DB.QueryRow(`SELECT name,color FROM labels WHERE board_id=? AND lower(name)='high'`, b.ID).Scan(&hName, &hColor)
	if hName != "high" || hColor != "berry-red" || count(t, e, `SELECT COUNT(*) FROM labels WHERE board_id=?`, b.ID) != 5 {
		t.Fatalf("labels changed: %s %s, %d labels", hName, hColor, count(t, e, `SELECT COUNT(*) FROM labels WHERE board_id=?`, b.ID))
	}
	// settings untouched (not asked), agent prompt untouched
	nb, _ := boards.GetBoard(e.Ctx, e.DB, b.ID)
	if nb.Color == nil || *nb.Color != "#123456" || nb.AgentPrompt != b.AgentPrompt {
		t.Fatalf("settings changed without applySettings: %+v", nb)
	}
	// events: quiet per-card moves, list creations, one summary
	if n := count(t, e, `SELECT COUNT(*) FROM actions WHERE board_id=? AND type='cardMove' AND data LIKE '%"auto":"applyTemplate"%' AND data LIKE '%"quiet":true%'`, b.ID); n != 5 {
		t.Fatalf("%d quiet move events, want 5", n)
	}
	if n := count(t, e, `SELECT COUNT(*) FROM actions WHERE board_id=? AND type='listCreate' AND data LIKE '%applyTemplate%'`, b.ID); n != 4 {
		t.Fatalf("%d list events, want 4", n)
	}
	if n := count(t, e, `SELECT COUNT(*) FROM actions WHERE board_id=? AND type='boardUpdate' AND data LIKE '%"appliedTemplate":"Kanban basics"%' AND data LIKE '%"cardsMoved":5%' AND data LIKE '%"columnsRemoved":3%'`, b.ID); n != 1 {
		t.Fatalf("%d summary events, want 1", n)
	}
	// one quiet list.deleted per removed column, after the moves (seq order), with the column as listId for listeners
	if n := count(t, e, `SELECT COUNT(*) FROM actions WHERE board_id=? AND type='listDelete' AND data LIKE '%"auto":"applyTemplate"%' AND data LIKE '%"quiet":true%' AND list_id IN (?,?,?)`, b.ID, l["todo"], l["doing"], l["done"]); n != 3 {
		t.Fatalf("%d list.deleted events, want 3", n)
	}
	if n := count(t, e, `SELECT COUNT(*) FROM actions d JOIN actions m ON m.board_id=d.board_id WHERE d.board_id=? AND d.type='listDelete' AND m.type='cardMove' AND m.seq>d.seq`, b.ID); n != 0 {
		t.Fatalf("%d card moves announced after a column deletion", n)
	}

	// applying it again replaces the four template columns and reuses every label
	again := apply(t, e, boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", Confirm: true})
	if len(again.LabelsAdded) != 0 || len(again.LabelsReused) != 4 || again.CardsMoved != 5 || len(again.DuplicateColumnNames) != 0 || len(again.ColumnsRemoved) != 4 {
		t.Fatalf("second apply: %+v", again)
	}
	if n := len(columnNames(t, e, b.ID)); n != 4 {
		t.Fatalf("%d columns after two applies, want 4", n)
	}
	if n := liveNames(t, e, again.TargetColumn.ID); !reflect.DeepEqual(n, []string{"T1", "T2", "D1", "X1", "X2"}) {
		t.Fatalf("second target holds %v", n)
	}
}

func TestTemplateApplySettingsAndSamples(t *testing.T) {
	e := fixture(t)
	b, _ := applyBoard(t, e, "set")
	out := apply(t, e, boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", Confirm: true, ApplySettings: true, IncludeSampleCards: true})
	if !contains(out.SettingsChanged, "board colour") || out.SampleCardsAdded != 3 {
		t.Fatalf("settings %v samples %d", out.SettingsChanged, out.SampleCardsAdded)
	}
	nb, _ := boards.GetBoard(e.Ctx, e.DB, b.ID)
	if nb.Color == nil || *nb.Color == "#123456" || nb.AgentPrompt != b.AgentPrompt {
		t.Fatalf("colour should change, the agent prompt never: %+v", nb)
	}
	// samples land in their own template columns, after the moved cards in the target
	if n := count(t, e, `SELECT COUNT(*) FROM cards WHERE board_id=? AND list_id IN (SELECT id FROM lists WHERE board_id=? AND type IN ('active','closed')) AND is_draft=0 AND is_template=0`, b.ID, b.ID); n != 8 {
		t.Fatalf("%d live cards, want 5 moved + 3 samples", n)
	}
	names := liveNames(t, e, out.TargetColumn.ID)
	if len(names) < 5 || !reflect.DeepEqual(names[:5], []string{"T1", "T2", "D1", "X1", "X2"}) {
		t.Fatalf("moved cards must come first in the target: %v", names)
	}
	if n := count(t, e, `SELECT COUNT(*) FROM card_labels cl JOIN cards c ON c.id=cl.card_id WHERE c.board_id=?`, b.ID); n == 0 {
		t.Fatal("sample cards lost their labels")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// publishTpl makes a published template board: Shipped (closing) first, then Work (archive after 2), label "Bug"
// in group "Kind", and a custom-field group "Vendor".
func publishTpl(t *testing.T, e *ops.Env, allClosing bool) boards.Board {
	t.Helper()
	b := newBoard(t, e, "Tpl")
	closed := "closed"
	shipped := addList(t, e, b.ID, "Shipped")
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: shipped.ID, Type: &closed})
	work := addList(t, e, b.ID, "Work")
	if allClosing {
		call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: work.ID, Type: &closed})
	} else {
		two := 2
		call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: work.ID, ArchiveAfter: &two})
	}
	g := call[boards.LabelGroup](t, e, "labelgroup.create", boards.GroupCreateIn{BoardID: b.ID, Name: "Kind"})
	call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "Bug", Color: "berry-red", GroupID: g.ID})
	mustExec(t, e, `INSERT INTO custom_field_groups(id,board_id,name,position,created_at) VALUES(?,?,?,?,'2024-01-01T00:00:00Z')`, "cfg-"+b.ID, b.ID, "Vendor", 65536)
	mustExec(t, e, `INSERT INTO custom_fields(id,group_id,name,type,position) VALUES(?,?,?,?,?)`, "cf-"+b.ID, "cfg-"+b.ID, "Contact", "text", 65536)
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: b.ID, IsTemplate: true})
	return b
}

// A template whose first column closes cards: the cards go to the first open column instead; its archive-after cap is
// left off because more cards land there than it keeps, and nothing is archived, now or on the next landing.
func TestTemplateApplyClosingFirstColumnAndCap(t *testing.T) {
	e := fixture(t)
	tpl := publishTpl(t, e, false)
	b, _ := applyBoard(t, e, "cap")
	mustExec(t, e, `INSERT INTO custom_field_groups(id,board_id,name,position,created_at) VALUES('own-vendor',?,'vendor ',65536,'2024-01-01T00:00:00Z')`, b.ID)
	out := apply(t, e, boards.TemplateApplyIn{ID: b.ID, TemplateID: tpl.ID, Confirm: true})
	if out.TargetColumn.Name != "Work" || out.TargetColumn.ArchiveAfter != 0 || out.CardsAutoArchived != 0 {
		t.Fatalf("target %+v archived %d", out.TargetColumn, out.CardsAutoArchived)
	}
	if !strings.Contains(strings.Join(out.Warnings, " "), "Shipped, closes cards") || !strings.Contains(strings.Join(out.Warnings, " "), "auto-archive is left off") {
		t.Fatalf("warnings: %v", out.Warnings)
	}
	if !reflect.DeepEqual(out.FieldGroupsSkipped, []string{"Vendor"}) || len(out.FieldGroupsAdded) != 0 {
		t.Fatalf("field groups: added %v skipped %v", out.FieldGroupsAdded, out.FieldGroupsSkipped)
	}
	if !reflect.DeepEqual(out.LabelGroupsAdded, []string{"Kind"}) || !reflect.DeepEqual(out.LabelsAdded, []string{"Bug"}) {
		t.Fatalf("label groups %v labels %v", out.LabelGroupsAdded, out.LabelsAdded)
	}
	if n := count(t, e, `SELECT COUNT(*) FROM lists WHERE id=? AND archive_after IS NULL`, out.TargetColumn.ID); n != 1 {
		t.Fatal("the target kept its cap")
	}
	if n := countIn(t, e, out.TargetColumn.ID); n != 5 {
		t.Fatalf("target holds %d, want 5", n)
	}
	// the next ordinary landing archives nothing
	if _, err := ops.Typed[cards.Card](e, "card.create", cards.CreateIn{ListID: out.TargetColumn.ID, Name: "Later"}); err != nil {
		t.Fatal(err)
	}
	if n := countIn(t, e, out.TargetColumn.ID); n != 6 {
		t.Fatalf("a later card archived something: %d cards", n)
	}

	// under the cap: the column keeps the template's setting
	small := newBoard(t, e, "Small")
	one := addList(t, e, small.ID, "One")
	addCard(t, e, small.ID, one.ID, "s1", "S1", 1, "")
	o2 := apply(t, e, boards.TemplateApplyIn{ID: small.ID, TemplateID: tpl.ID, Confirm: true})
	if o2.TargetColumn.ArchiveAfter != 2 || count(t, e, `SELECT COUNT(*) FROM lists WHERE id=? AND archive_after=2`, o2.TargetColumn.ID) != 1 {
		t.Fatalf("cap dropped although 1 card fits: %+v", o2.TargetColumn)
	}
	if !reflect.DeepEqual(o2.FieldGroupsAdded, []string{"Vendor"}) {
		t.Fatalf("field group not added: %+v", o2)
	}
	// every column closes cards: refused
	all := publishTpl(t, e, true)
	mustFail(t, e, "board.template.apply", boards.TemplateApplyIn{ID: small.ID, TemplateID: all.ID, DryRun: true}, "validation")
	// a board cannot be its own template
	mustFail(t, e, "board.template.apply", boards.TemplateApplyIn{ID: tpl.ID, TemplateID: tpl.ID, DryRun: true}, "validation")
}

func TestTemplateApplyFieldGroupsAppend(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Fields")
	addList(t, e, b.ID, "One")
	mustExec(t, e, `INSERT INTO custom_field_groups(id,board_id,name,position,created_at) VALUES('own',?,'Own',65536,'2024-01-01T00:00:00Z')`, b.ID)
	out := apply(t, e, boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:event-planning", Confirm: true})
	if !reflect.DeepEqual(out.FieldGroupsAdded, []string{"Vendor"}) {
		t.Fatalf("field groups %+v", out)
	}
	var pos float64
	_ = e.DB.QueryRow(`SELECT position FROM custom_field_groups WHERE board_id=? AND name='Vendor'`, b.ID).Scan(&pos)
	if pos <= 65536 {
		t.Fatalf("the added group sits at %v, before the board's own", pos)
	}
}

func TestTemplateApplyProjectLabels(t *testing.T) {
	e := fixture(t)
	mustExec(t, e, `UPDATE projects SET use_project_labels=1 WHERE id='p2'`)
	b := call[boards.Board](t, e, "board.create", boards.BoardCreateIn{ProjectID: "p2", Name: "PL"})
	mustExec(t, e, `INSERT INTO labels(id,project_id,name,color,position,created_at) VALUES('pl-low','p2','LOW','sky',1,'2024-01-01T00:00:00Z')`)
	out := apply(t, e, boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", Confirm: true})
	if !reflect.DeepEqual(out.LabelsReused, []string{"Low"}) || len(out.LabelsAdded) != 3 || len(out.LabelGroupsAdded) != 0 {
		t.Fatalf("project labels: %+v", out)
	}
	if n := count(t, e, `SELECT COUNT(*) FROM labels WHERE project_id='p2' AND board_id IS NULL`); n != 4 {
		t.Fatalf("%d project labels, want 4", n)
	}
	var c string
	_ = e.DB.QueryRow(`SELECT color FROM labels WHERE id='pl-low'`).Scan(&c)
	if c != "sky" {
		t.Fatal("the reused project label was recoloured")
	}
}

// Failure midway leaves nothing changed.
func TestTemplateApplyAtomic(t *testing.T) {
	e := fixture(t)
	b, _ := applyBoard(t, e, "atom")
	for _, stage := range []string{"columns", "cards", "removed", "labels", "done"} {
		before := snapshot(t, e)
		boards.SetApplyHook(func(s string) error {
			if s == stage {
				return errors.New("injected at " + s)
			}
			return nil
		})
		_, err := ops.Call(e, "board.template.apply", &boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:bug-tracker", Confirm: true, ApplySettings: true, IncludeSampleCards: true})
		boards.SetApplyHook(nil)
		if err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("%s: want the injected error, got %v", stage, err)
		}
		if after := snapshot(t, e); after != before {
			t.Fatalf("%s: a failed apply left changes", stage)
		}
	}
}

func TestTemplateApplyPermissions(t *testing.T) {
	e := fixture(t)
	b, _ := applyBoard(t, e, "perm")
	mustExec(t, e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m-e',?,?,'editor','2024-01-01T00:00:00Z'),('m-v',?,?,'viewer','2024-01-01T00:00:00Z')`,
		b.ID, editor.UserID, b.ID, viewer.UserID)
	in := boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", DryRun: true}
	mustFail(t, testutil.As(e, editor), "board.template.apply", in, "forbidden")
	mustFail(t, testutil.As(e, viewer), "board.template.apply", in, "forbidden")
	mustFail(t, testutil.As(e, nobody), "board.template.apply", in, "not_found")
	// a private published template is only usable by people who can see it
	tpl := publishTpl(t, e, false)
	mustExec(t, e, `INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES('pm','p2',?,'2024-01-01T00:00:00Z')`, editor.UserID)
	mine := call[boards.Board](t, testutil.As(e, editor), "board.create", boards.BoardCreateIn{ProjectID: "p2", Name: "Mine"})
	mustFail(t, testutil.As(e, editor), "board.template.apply", boards.TemplateApplyIn{ID: mine.ID, TemplateID: tpl.ID, DryRun: true}, "not_found")
	mustFail(t, testutil.As(e, editor), "board.template.apply", boards.TemplateApplyIn{ID: mine.ID, TemplateID: "builtin:nope", DryRun: true}, "not_found")
	public := "public"
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: tpl.ID, IsTemplate: true, Visibility: public})
	apply(t, testutil.As(e, editor), boards.TemplateApplyIn{ID: mine.ID, TemplateID: tpl.ID, DryRun: true})
}

// The bulk move is quiet against the real notification planner: a board watcher gets nothing, the events are stored,
// and the board activity feed shows the summary instead of one row per card.
func TestTemplateApplyQuietForWatchers(t *testing.T) {
	e := fixture(t)
	if ops.EmitHook == nil {
		t.Skip("notify domain not linked")
	}
	b, l := applyBoard(t, e, "quiet")
	mustExec(t, e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m-q',?,?,'editor','2024-01-01T00:00:00Z')`, b.ID, editor.UserID)
	mustExec(t, e, `INSERT INTO board_subscriptions(board_id,user_id) VALUES(?,?)`, b.ID, editor.UserID)
	// a column watcher of an old column (removed by the apply) gets nothing either
	mustExec(t, e, `INSERT INTO list_subscriptions(list_id,user_id,created_at) VALUES(?,?,'2024-01-01T00:00:00Z')`, l["doing"], editor.UserID)
	apply(t, e, boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", Confirm: true, IncludeSampleCards: true})
	if n := count(t, e, `SELECT COUNT(*) FROM notifications WHERE user_id=?`, editor.UserID); n != 0 {
		t.Fatalf("applying a template notified the board watcher %d times", n)
	}
	if n := count(t, e, `SELECT COUNT(*) FROM actions WHERE board_id=? AND type='cardMove' AND seq IS NOT NULL`, b.ID); n != 5 {
		t.Fatalf("%d stored move events, want 5", n)
	}
	feed := call[[]cardextras.ActionItem](t, e, "activity.board", cardextras.ActionsIn{BoardID: b.ID, Limit: 200})
	var moves, summary int
	for _, a := range feed {
		switch {
		case a.Type == "cardMove":
			moves++
		case a.Type == "boardUpdate" && strings.Contains(cardextras.ActionText(a), "applied the template “Kanban basics”: added 4 columns, moved 5 cards, removed 3 old columns"):
			summary++
		}
	}
	if moves != 0 || summary != 1 {
		t.Fatalf("board feed: %d per-card moves (want 0), %d summaries (want 1)", moves, summary)
	}
}

// LandedKeepClosed never re-opens (a closing column still closes) and never runs the archive-after rule.
func TestLandedKeepClosed(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Keep")
	open, shut := addList(t, e, b.ID, "Open"), addList(t, e, b.ID, "Shut")
	closed, one := "closed", 1
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: shut.ID, Type: &closed})
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: open.ID, ArchiveAfter: &one})
	addCard(t, e, b.ID, open.ID, "k1", "K1", 1, "")
	addCard(t, e, b.ID, open.ID, "k2", "K2", 2, "")
	addCard(t, e, b.ID, open.ID, "k3", "K3", 3, "")
	mustExec(t, e, `UPDATE cards SET is_closed=1 WHERE id='k1'`)
	tx, err := e.DB.BeginTx(e.Ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := boards.LandedKeepClosed(e.Ctx, tx, boards.Landing{CardID: "k1", ListID: open.ID}, boards.Landing{CardID: "k2", ListID: open.ID})
	if err != nil || len(res.Reopened) != 0 || res.ArchivedCount() != 0 {
		t.Fatalf("keepClosed: %+v %v", res, err)
	}
	mustTx := func(q string) {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	mustTx(`UPDATE card_placements SET list_id='` + shut.ID + `' WHERE card_id='k3' AND is_home=1`)
	res, err = boards.LandedKeepClosed(e.Ctx, tx, boards.Landing{CardID: "k3", ListID: shut.ID})
	if err != nil || len(res.Closed) != 1 {
		t.Fatalf("a closing column must still close: %+v %v", res, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, e, `SELECT COUNT(*) FROM cards WHERE id='k1' AND is_closed=1`); n != 1 {
		t.Fatal("k1 was re-opened")
	}
	if n := countIn(t, e, open.ID); n != 2 {
		t.Fatalf("the archive-after rule ran: %d cards left", n)
	}
}

// applySettings only applies what the template states (built-ins name colour, card colouring and view, not card ages,
// the card creator or the inbox), never turns the inbox off, and the preview equals the apply with both options on.
func TestTemplateApplySettingsOnlyStated(t *testing.T) {
	e := fixture(t)
	b, _ := applyBoard(t, e, "stated")
	on := true
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, HasInbox: &on, ShowCardAges: &on, AlwaysDisplayCardCreator: &on, ExpandTaskListsByDefault: &on})
	in := boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", DryRun: true, ApplySettings: true, IncludeSampleCards: true}
	pre := apply(t, e, in)
	for _, s := range pre.SettingsChanged {
		if strings.Contains(s, "ages") || strings.Contains(s, "inbox") || strings.Contains(s, "creator") || strings.Contains(s, "task lists") {
			t.Fatalf("a setting the template does not state would change: %v", pre.SettingsChanged)
		}
	}
	in.DryRun, in.Confirm = false, true
	got := apply(t, e, in)
	if !reflect.DeepEqual(sameNumbers(got), sameNumbers(pre)) {
		t.Fatalf("preview and apply differ with both options:\n%+v\n%+v", pre, got)
	}
	nb, _ := boards.GetBoard(e.Ctx, e.DB, b.ID)
	if !nb.HasInbox || !nb.ShowCardAges || !nb.AlwaysDisplayCardCreator || !nb.ExpandTaskListsByDefault {
		t.Fatalf("unstated settings were switched off: %+v", nb)
	}
	// sample cards announce their column (column listeners see them arrive)
	if n := count(t, e, `SELECT COUNT(*) FROM actions WHERE board_id=? AND type='cardCreate' AND data LIKE '%applyTemplate%' AND COALESCE(list_id,'')<>''`, b.ID); n != 3 {
		t.Fatalf("%d sample card events with a column, want 3", n)
	}
	// published template with both options: same equality
	tpl := publishTpl(t, e, false)
	b2, _ := applyBoard(t, e, "pub")
	in2 := boards.TemplateApplyIn{ID: b2.ID, TemplateID: tpl.ID, DryRun: true, ApplySettings: true, IncludeSampleCards: true}
	p2 := apply(t, e, in2)
	in2.DryRun, in2.Confirm = false, true
	if g2 := apply(t, e, in2); !reflect.DeepEqual(sameNumbers(g2), sameNumbers(p2)) {
		t.Fatalf("published: preview and apply differ:\n%+v\n%+v", p2, g2)
	}
}

// Removing the old columns (the default) is safe: inbox drafts and card templates parked on them survive (re-pointed
// to the target, still listed by the inbox and the project's card templates), archived and trashed cards from them
// restore into the target, a claimed card moves with its claim, column watches end, and the preview says what goes:
// the old columns' settings, the watchers, a listener, the archived cards.
func TestTemplateApplyRemovalSafety(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Safe")
	todo, done := addList(t, e, b.ID, "Todo"), addList(t, e, b.ID, "Done")
	closed, three := "closed", 3
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: done.ID, Type: &closed, WIPLimit: &three})
	call[cards.InboxOut](t, e, "card.inbox.set", cards.InboxSetIn{BoardID: b.ID, Enabled: true})
	draft := call[cards.Card](t, e, "card.draft.create", cards.DraftCreateIn{BoardID: b.ID, Name: "Idea"})
	src := call[cards.Card](t, e, "card.create", cards.CreateIn{ListID: todo.ID, Name: "Source"})
	tpl := call[cards.Card](t, e, "card.template.save", cards.TemplateSaveIn{ID: src.ID, Name: "Bug report"})
	if draft.ListID != todo.ID || tpl.ListID != todo.ID {
		t.Fatalf("setup: draft in %s, template in %s (want the old column %s)", draft.ListID, tpl.ListID, todo.ID)
	}
	gone := call[cards.Card](t, e, "card.create", cards.CreateIn{ListID: todo.ID, Name: "Archived one"})
	call[cards.Card](t, e, "card.archive", cards.ArchiveIn{ID: gone.ID})
	binned := call[cards.Card](t, e, "card.create", cards.CreateIn{ListID: done.ID, Name: "Trashed one"})
	call[cards.Card](t, e, "card.archive", cards.ArchiveIn{ID: binned.ID, Trash: true})
	claimed := call[cards.Card](t, e, "card.create", cards.CreateIn{ListID: todo.ID, Name: "Claimed"})
	call[cards.Card](t, e, "card.claim", cards.ClaimIn{ID: claimed.ID, Note: "working"})
	mustExec(t, e, `INSERT INTO list_subscriptions(list_id,user_id,created_at) VALUES(?,?,'2024-01-01T00:00:00Z')`, done.ID, editor.UserID)
	unreg := e.Hub.Listeners.Register(realtime.Listener{UserID: admin.UserID, Name: "Admin", Label: "triage-bot", Lists: []string{todo.ID}, Boards: []string{b.ID}})
	defer unreg()

	pre := apply(t, e, boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", DryRun: true})
	w := strings.Join(pre.Warnings, "\n")
	for _, want := range []string{"settings go with them: Done (closes cards, WIP limit 3)", "1 person watches the old columns", "triage-bot on Todo",
		"2 cards in the archive or trash came from the old columns; restored, they go into Backlog"} {
		if !strings.Contains(w, want) {
			t.Fatalf("preview warnings lack %q:\n%s", want, w)
		}
	}
	got := apply(t, e, boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", Confirm: true})
	if !reflect.DeepEqual(sameNumbers(got), sameNumbers(pre)) {
		t.Fatalf("preview and apply differ:\n%+v\n%+v", pre, got)
	}
	target := got.TargetColumn.ID
	if n := count(t, e, `SELECT COUNT(*) FROM lists WHERE id IN (?,?)`, todo.ID, done.ID); n != 0 {
		t.Fatal("the old columns are still there")
	}
	// the draft is still in the inbox, the template still in the project's card templates
	inbox := call[cards.InboxOut](t, e, "card.inbox.list", cards.InboxIn{BoardID: b.ID})
	if len(inbox.Items) != 1 || inbox.Items[0].ID != draft.ID || inbox.Items[0].ListID != target {
		t.Fatalf("inbox after the apply: %+v", inbox.Items)
	}
	tpls := call[cards.TemplatesOut](t, e, "card.template.list", cards.TemplatesIn{ProjectID: "p1"})
	if len(tpls.Items) != 1 || tpls.Items[0].ID != tpl.ID || !tpls.Items[0].IsTemplate {
		t.Fatalf("card templates after the apply: %+v", tpls.Items)
	}
	if n := liveNames(t, e, target); !reflect.DeepEqual(n, []string{"Source", "Claimed"}) {
		t.Fatalf("target shows %v (drafts and templates must stay hidden)", n)
	}
	// the claim moved with its card
	if n := count(t, e, `SELECT COUNT(*) FROM cards WHERE id=? AND list_id=? AND claimed_by IS NOT NULL AND claim_note='working'`, claimed.ID, target); n != 1 {
		t.Fatal("the claimed card lost its claim or did not move")
	}
	// the draft publishes, the archived and the trashed card restore (prev_list_id names a column that is gone)
	call[cards.Card](t, e, "card.draft.publish", cards.PublishIn{ID: draft.ID, ListID: target})
	for _, id := range []string{gone.ID, binned.ID} {
		r := call[cards.Card](t, e, "card.restore", cards.IDIn{ID: id})
		if r.ListID != target {
			t.Fatalf("restored %s into %s, want the first open column %s", id, r.ListID, target)
		}
	}
	// the column watch went with its column
	if n := count(t, e, `SELECT COUNT(*) FROM list_subscriptions WHERE list_id=?`, done.ID); n != 0 {
		t.Fatal("a watch on a removed column is left")
	}
}

// removeOldColumns=false keeps the old columns, emptied, to the right (the earlier behaviour); the preview matches.
func TestTemplateApplyKeepOldColumns(t *testing.T) {
	e := fixture(t)
	b, l := applyBoard(t, e, "keep")
	no := false
	in := boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", DryRun: true, RemoveOldColumns: &no}
	pre := apply(t, e, in)
	if pre.RemoveOldColumns || len(pre.ColumnsRemoved) != 0 || !reflect.DeepEqual(pre.ColumnsKept, []string{"Todo", "Doing", "Done"}) ||
		!reflect.DeepEqual(pre.DuplicateColumnNames, []string{"Doing", "Done"}) || !strings.Contains(strings.Join(pre.Warnings, " "), "added again") {
		t.Fatalf("keep preview: %+v", pre)
	}
	in.DryRun, in.Confirm = false, true
	got := apply(t, e, in)
	if !reflect.DeepEqual(sameNumbers(got), sameNumbers(pre)) {
		t.Fatalf("preview and apply differ:\n%+v\n%+v", pre, got)
	}
	if cols := columnNames(t, e, b.ID); !reflect.DeepEqual(cols, []string{"Backlog", "To do", "Doing", "Done", "Todo", "Doing", "Done"}) {
		t.Fatalf("column order %v", cols)
	}
	for id, list := range map[string]string{"keepdr": l["todo"], "keepct": l["todo"]} {
		var at string
		_ = e.DB.QueryRow(`SELECT list_id FROM cards WHERE id=?`, id).Scan(&at)
		if at != list {
			t.Fatalf("%s moved to %s although the columns stay", id, at)
		}
	}
	if n := count(t, e, `SELECT COUNT(*) FROM actions WHERE board_id=? AND type='listDelete'`, b.ID); n != 0 {
		t.Fatalf("%d list.deleted events with the columns kept", n)
	}
}

// A card row that is neither moved nor parked (it cannot happen through the ops; forced here) makes the apply fail
// instead of letting the column delete cascade it away. Nothing changes.
func TestTemplateApplyRefusesToCascade(t *testing.T) {
	e := fixture(t)
	b, l := applyBoard(t, e, "casc")
	// a card of ANOTHER board pointing at this board's column: the move (board-scoped) skips it, the cascade would not
	other := newBoard(t, e, "Other")
	ol := addList(t, e, other.ID, "Elsewhere")
	addCard(t, e, other.ID, ol.ID, "stray", "Stray", 1, "")
	mustExec(t, e, `UPDATE card_placements SET list_id=? WHERE card_id='stray' AND is_home=1`, l["doing"])
	before := snapshot(t, e)
	if _, err := ops.Call(e, "board.template.apply", &boards.TemplateApplyIn{ID: b.ID, TemplateID: "builtin:kanban-basics", Confirm: true}); err == nil || !strings.Contains(err.Error(), "still on the old columns") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if snapshot(t, e) != before {
		t.Fatal("a refused apply left changes")
	}
	// put the forced row back on its own board (CheckPlacements runs when the test ends)
	mustExec(t, e, `UPDATE card_placements SET list_id=? WHERE card_id='stray' AND is_home=1`, ol.ID)
}
