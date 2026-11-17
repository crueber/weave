package boards_test

import (
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
)

func fieldCounts(t *testing.T, e *ops.Env, board string) (groups, fields, values int) {
	t.Helper()
	groups = count(t, e, `SELECT COUNT(*) FROM custom_field_groups WHERE board_id=?`, board)
	fields = count(t, e, `SELECT COUNT(*) FROM custom_fields f JOIN custom_field_groups g ON g.id=f.group_id WHERE g.board_id=?`, board)
	values = count(t, e, `SELECT COUNT(*) FROM custom_field_values v JOIN cards c ON c.id=v.card_id WHERE c.board_id=?`, board)
	return
}

// Custom fields travel with built-in templates (definitions, and values when the sample cards come along).
func TestCustomFieldsInBuiltins(t *testing.T) {
	e := fixture(t)
	b := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:sales-pipeline", Name: "CRM"})
	if g, f, v := fieldCounts(t, e, b.ID); g != 1 || f != 2 || v != 2 {
		t.Fatalf("instantiate: groups=%d fields=%d values=%d", g, f, v)
	}
	var val, typ, opts string
	_ = e.DB.QueryRow(`SELECT v.content FROM custom_field_values v JOIN custom_fields f ON f.id=v.field_id WHERE f.name='Deal value'`).Scan(&val)
	_ = e.DB.QueryRow(`SELECT type,COALESCE(options,'') FROM custom_fields WHERE name='Source'`).Scan(&typ, &opts)
	if val != "12000" || typ != "select" || !strings.Contains(opts, "Referral") {
		t.Fatalf("field data: %q %q %q", val, typ, opts)
	}
	no := false
	b2 := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: "builtin:sales-pipeline", Name: "CRM bare", SampleCards: &no})
	if g, f, v := fieldCounts(t, e, b2.ID); g != 1 || f != 2 || v != 0 {
		t.Fatalf("no sample cards: groups=%d fields=%d values=%d", g, f, v)
	}
}

const seedFields = `
INSERT INTO base_custom_field_groups(id,project_id,name,created_at) VALUES('bg1','p1','Base','x');
INSERT INTO custom_fields(id,base_group_id,name,type,position) VALUES('bf1','bg1','Inherited','text',1);
INSERT INTO custom_field_groups(id,board_id,base_group_id,name,repeatable,position,created_at) VALUES('g-base',?,'bg1',NULL,0,1,'x');
INSERT INTO custom_field_groups(id,board_id,name,repeatable,position,created_at) VALUES('g-rep',?,'Rows',1,2,'x');
INSERT INTO custom_fields(id,group_id,name,type,position) VALUES('f-rep','g-rep','Item','text',1);
INSERT INTO custom_field_groups(id,card_id,name,repeatable,position,created_at) VALUES('g-card','k1','Mine',0,1,'x');
INSERT INTO custom_fields(id,group_id,name,type,position) VALUES('f-card','g-card','Note','text',1);
INSERT INTO custom_field_rows(id,card_id,group_id,position,created_at) VALUES('r1','k1','g-rep',1,'x');
INSERT INTO custom_field_rows(id,card_id,group_id,position,created_at) VALUES('r2','k1','g-rep',2,'x');
INSERT INTO custom_field_values(id,card_id,group_id,field_id,row_id,content) VALUES('v1','k1','g-base','bf1','','base value');
INSERT INTO custom_field_values(id,card_id,group_id,field_id,row_id,content) VALUES('v2','k1','g-rep','f-rep','r1','one');
INSERT INTO custom_field_values(id,card_id,group_id,field_id,row_id,content) VALUES('v3','k1','g-rep','f-rep','r2','two');
INSERT INTO custom_field_values(id,card_id,group_id,field_id,row_id,content) VALUES('v4','k1','g-card','f-card','','card note');
`

// Board-wide, base-derived, repeatable and card-level groups round-trip through export/import and published templates.
func TestCustomFieldsRoundTrip(t *testing.T) {
	e := fixture(t)
	src := call[boards.Board](t, e, "board.create", boards.BoardCreateIn{ProjectID: "p1", Name: "Src"})
	l := addList(t, e, src.ID, "Todo")
	addCard(t, e, src.ID, l.ID, "k1", "Card", 65536, "")
	for _, stmt := range strings.Split(strings.TrimSpace(seedFields), "\n") {
		args := []any{}
		if strings.Contains(stmt, "VALUES('g-base',?") || strings.Contains(stmt, "VALUES('g-rep',?") {
			args = []any{src.ID}
		}
		if _, err := e.DB.Exec(stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	check := func(label, board string) {
		t.Helper()
		one := func(field string) string {
			var s string
			_ = e.DB.QueryRow(`SELECT v.content FROM custom_field_values v JOIN custom_fields f ON f.id=v.field_id JOIN cards c ON c.id=v.card_id WHERE c.board_id=? AND f.name=?`, board, field).Scan(&s)
			return s
		}
		rows, err := e.DB.Query(`SELECT v.content FROM custom_field_values v JOIN custom_fields f ON f.id=v.field_id JOIN cards c ON c.id=v.card_id JOIN custom_field_rows r ON r.id=v.row_id
			WHERE c.board_id=? AND f.name='Item' ORDER BY r.position`, board)
		if err != nil {
			t.Fatal(err)
		}
		var items []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			items = append(items, s)
		}
		rows.Close()
		if one("Inherited") != "base value" || strings.Join(items, ",") != "one,two" || one("Note") != "card note" {
			t.Fatalf("%s: base=%q rows=%v card=%q", label, one("Inherited"), items, one("Note"))
		}
		if n := count(t, e, `SELECT COUNT(*) FROM custom_field_groups WHERE board_id=? AND base_group_id IS NOT NULL`, board); n != 0 {
			t.Fatalf("%s: a base group must be flattened into the board", label)
		}
	}
	st := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: src.ID})
	check("export/import", call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: string(st.Data), Name: "Imported"}).ID)

	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: src.ID, IsTemplate: true, Visibility: "public"})
	check("published template", call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: src.ID, Name: "Copy"}).ID)
	no := false
	bare := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: src.ID, Name: "Bare", SampleCards: &no})
	if g, f, v := fieldCounts(t, e, bare.ID); g != 2 || f != 2 || v != 0 {
		t.Fatalf("copy without cards: groups=%d fields=%d values=%d (card-level groups and values must go)", g, f, v)
	}
}

// A file is untrusted: invalid values and unknown types are dropped, and a file without any custom fields still imports.
func TestCustomFieldsUntrustedAndLegacyFiles(t *testing.T) {
	e := fixture(t)
	bad := `{"format":"weave-board","version":1,"board":{"name":"B"},"lists":[{"id":"l","name":"L","type":"active"}],
	 "customFields":[{"id":"g","name":"G","fields":[{"id":"f","name":"N","type":"number"},{"id":"f2","name":"X","type":"bogus"}]}],
	 "cards":[{"id":"c","listId":"l","name":"C","type":"project","customFieldValues":[
	   {"groupId":"g","fieldId":"f","content":"abc"},{"groupId":"g","fieldId":"f2","content":"free text"},{"groupId":"zz","fieldId":"f","content":"1"}]}]}`
	bb := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: bad})
	if g, f, v := fieldCounts(t, e, bb.ID); g != 1 || f != 2 || v != 1 {
		t.Fatalf("untrusted file: groups=%d fields=%d values=%d", g, f, v)
	}
	old := `{"format":"weave-board","version":1,"board":{"name":"Old"},"lists":[{"id":"l","name":"L","type":"active","position":1}],"cards":[{"id":"c","listId":"l","name":"Card","type":"project","position":1}]}`
	ob := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: old})
	if count(t, e, `SELECT COUNT(*) FROM cards WHERE board_id=?`, ob.ID) != 1 {
		t.Fatal("an export from before custom fields no longer imports")
	}
}

// Board settings and the default view travel with an export; file imports keep recording board_imports.
func TestImportHonoursViewAndSettings(t *testing.T) {
	e := fixture(t)
	src := call[boards.Board](t, e, "board.create", boards.BoardCreateIn{ProjectID: "p1", Name: "Src", DefaultView: "list"})
	yes := true
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: src.ID, ShowCardCounter: &yes, ExpandTaskListsByDefault: &yes})
	addList(t, e, src.ID, "A")
	st := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: src.ID})
	imp := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: string(st.Data), Name: "Imp"})
	if imp.DefaultView != "list" || !imp.ShowCardCounter || !imp.ExpandTaskListsByDefault {
		t.Fatalf("settings lost: %+v", imp)
	}
	if count(t, e, `SELECT COUNT(*) FROM board_imports`) != 1 {
		t.Fatal("file imports must still record a board_imports row")
	}
}
