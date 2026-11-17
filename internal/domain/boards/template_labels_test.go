package boards_test

import (
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
)

// A template whose cards carry project-wide labels must keep them when instantiated into a project that also uses
// project-wide labels: the matching label is reused (never duplicated) and the card links survive.
func TestInstantiateReusesProjectLabels(t *testing.T) {
	e := fixture(t)
	if _, err := e.DB.Exec(`UPDATE projects SET use_project_labels=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO labels(id,project_id,name,color,position,created_at) VALUES('pl1','p1','Bug','crimson',1,'x')`); err != nil {
		t.Fatal(err)
	}
	b := newBoard(t, e, "Tpl")
	l := addList(t, e, b.ID, "Todo")
	addCard(t, e, b.ID, l.ID, "c1", "Card one", 65536, "")
	if _, err := e.DB.Exec(`INSERT INTO card_labels VALUES('c1','pl1')`); err != nil {
		t.Fatal(err)
	}
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: b.ID, IsTemplate: true})
	for i := 0; i < 2; i++ {
		nb := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: b.ID, Name: "Copy"})
		var linked int
		_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_labels cl JOIN cards c ON c.id=cl.card_id JOIN labels l ON l.id=cl.label_id
			WHERE c.board_id=? AND l.project_id='p2' AND l.board_id IS NULL AND l.name='Bug'`, nb.ID).Scan(&linked)
		if linked != 1 {
			t.Fatalf("copy %d: card keeps its project label: got %d links", i, linked)
		}
	}
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM labels WHERE project_id='p2'`).Scan(&n)
	if n != 1 {
		t.Fatalf("project label duplicated: %d labels in p2", n)
	}
}
