package boards_test

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/domain/cards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// fillColumn inserts n cards into listID: card i (1..n) entered the column at minute i (so card 1 is the oldest),
// at position i. Ids are prefix+i.
func fillColumn(t *testing.T, e *ops.Env, boardID, listID, prefix string, n int) {
	t.Helper()
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		for i := 1; i <= n; i++ {
			at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
			if _, err := tx.Exec(`INSERT INTO cards(id,board_id,list_id,position,name,list_changed_at,created_at) VALUES(?,?,?,?,?,?,?)`,
				fmt.Sprintf("%s%d", prefix, i), boardID, listID, float64(i), fmt.Sprintf("%s %d", prefix, i), at, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func countIn(t *testing.T, e *ops.Env, listID string) int {
	t.Helper()
	var n int
	if err := e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE list_id=? AND is_draft=0 AND is_template=0`, listID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func idsIn(t *testing.T, e *ops.Env, listID string) map[string]bool {
	t.Helper()
	rows, err := e.DB.Query(`SELECT id FROM cards WHERE list_id=?`, listID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out[id] = true
	}
	return out
}

func setKeep(e *ops.Env, listID string, n int, confirm bool) (boards.List, error) {
	return ops.Typed[boards.List](e, "list.update", boards.ListUpdateIn{ID: listID, ArchiveAfter: &n, ConfirmArchive: confirm})
}

// The ticket's example: 94 cards, auto-archive after 30. Without the confirm the API refuses with the count; with it
// exactly the 64 oldest go to the archive (restorable to the column, closed state kept), and the column holds 30.
func TestArchiveAfterRetroactive(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Retro")
	done := closedList(t, e, b.ID, "Done")
	fillColumn(t, e, b.ID, done.ID, "c", 94)
	if _, err := e.DB.Exec(`UPDATE cards SET is_closed=1 WHERE list_id=? AND id IN ('c1','c2')`, done.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE cards SET is_closed=0 WHERE id='c3'`); err != nil { // a card kept open (T2 keep)
		t.Fatal(err)
	}
	// the preview counts without writing
	p := call[boards.ListArchivePreview](t, e, "list.archivePreview", boards.ListArchivePreviewIn{ID: done.ID, ArchiveAfter: 30})
	if p.Cards != 94 || p.Keep != 30 || p.WillArchive != 64 || p.SkippedClaimed != 0 || p.Current != 0 || p.Name != "Done" {
		t.Fatalf("preview %+v", p)
	}
	// no confirm: 409 with the count, nothing changed (not even the setting)
	_, err := setKeep(e, done.ID, 30, false)
	oe, ok := err.(*ops.Error)
	if !ok || oe.Status != 409 || !strings.Contains(oe.Msg, "would archive 64 cards") || !strings.Contains(oe.Msg, "confirmArchive=true") {
		t.Fatalf("want 409 with the count, got %v", err)
	}
	if countIn(t, e, done.ID) != 94 {
		t.Fatal("a refused update archived cards")
	}
	if l := call[boards.List](t, e, "list.get", boards.ListRefIn{ID: done.ID}); l.ArchiveAfter != nil {
		t.Fatal("a refused update stored the setting")
	}
	// confirmed: the 64 oldest (c1..c64) go, the newest 30 (c65..c94) stay
	l, err := setKeep(e, done.ID, 30, true)
	if err != nil || l.ArchiveAfter == nil || *l.ArchiveAfter != 30 {
		t.Fatalf("set: %+v %v", l, err)
	}
	arch := listOfType(t, e, b.ID, "archive")
	left, gone := idsIn(t, e, done.ID), idsIn(t, e, arch)
	if len(left) != 30 || len(gone) != 64 {
		t.Fatalf("column %d, archive %d", len(left), len(gone))
	}
	for i := 1; i <= 94; i++ {
		id := fmt.Sprintf("c%d", i)
		if (i <= 64) != gone[id] {
			t.Errorf("%s: archived=%v", id, gone[id])
		}
	}
	var prev string
	var closed1, closed3 bool
	_ = e.DB.QueryRow(`SELECT prev_list_id,is_closed FROM cards WHERE id='c1'`).Scan(&prev, &closed1)
	_ = e.DB.QueryRow(`SELECT is_closed FROM cards WHERE id='c3'`).Scan(&closed3)
	if prev != done.ID || !closed1 || closed3 {
		t.Fatalf("archived card: prev=%s closed(c1)=%v closed(c3)=%v (want the column, true, false: the state is kept)", prev, closed1, closed3)
	}
	// archive order: oldest first, appended after what the archive held
	if names := cardNames(t, e, arch); names[0] != "c 1" || names[63] != "c 64" {
		t.Fatalf("archive order %v...", names[:3])
	}
	// events: one quiet card.archived per card plus one summary on the column
	var perCard, summary int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE type='cardMove' AND etype='card.archived' AND data LIKE '%"auto":"archiveAfter"%' AND data LIKE '%"quiet":true%' AND from_list_id=?`, done.ID).Scan(&perCard)
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE type='listUpdate' AND data LIKE '%"autoArchived":64%' AND data LIKE '%"keep":30%'`).Scan(&summary)
	if perCard != 64 || summary != 1 {
		t.Fatalf("events: %d per-card, %d summaries", perCard, summary)
	}
	// the column's Activity shows the summary, not 64 card rows
	act := call[[]boards.ListActivityItem](t, e, "list.activity", boards.ListActivityIn{ID: done.ID, Limit: 5})
	if len(act) == 0 || act[0].Summary != "auto-archived 64 cards: the column keeps the newest 30" {
		t.Fatalf("activity %+v", act)
	}
	if len(act) < 2 || act[1].Summary != "set auto-archive: keep the newest 30 cards" {
		t.Fatalf("activity (setting row) %+v", act)
	}
	for _, a := range act {
		if a.Type == "card.archived" {
			t.Fatalf("per-card auto-archive rows flood the column activity: %+v", a)
		}
	}

	// idempotent: the same setting again archives nothing and needs no confirm
	if _, err := setKeep(e, done.ID, 30, false); err != nil || countIn(t, e, done.ID) != 30 {
		t.Fatalf("re-send: %v, %d cards", err, countIn(t, e, done.ID))
	}
	// raising it archives nothing
	if _, err := setKeep(e, done.ID, 50, false); err != nil || countIn(t, e, done.ID) != 30 {
		t.Fatalf("raise: %v", err)
	}
	// lowering it asks again with the new count
	if _, err := setKeep(e, done.ID, 25, false); err == nil || !strings.Contains(err.Error(), "would archive 5 cards") {
		t.Fatalf("lower: %v", err)
	}
	// other edits (a rename) never archive, even when the column is over the setting
	fillColumn(t, e, b.ID, done.ID, "late", 25)
	nm := "Shipped"
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: done.ID, Name: &nm})
	if countIn(t, e, done.ID) != 55 {
		t.Fatalf("a rename archived cards: %d", countIn(t, e, done.ID))
	}
	// turning it off archives nothing and clears the setting
	if l, err := setKeep(e, done.ID, 0, false); err != nil || l.ArchiveAfter != nil || countIn(t, e, done.ID) != 55 {
		t.Fatalf("off: %+v %v", l, err)
	}
	act = call[[]boards.ListActivityItem](t, e, "list.activity", boards.ListActivityIn{ID: done.ID, Limit: 1})
	if act[0].Summary != "turned auto-archive off" {
		t.Fatalf("activity %+v", act[0])
	}
}

// Validation and permissions: 1..10000 (0 clears), archive/trash lists cannot carry it, viewers and outsiders cannot
// set or preview it, list.create accepts it.
func TestArchiveAfterValidation(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Valid")
	l := addList(t, e, b.ID, "Todo")
	for _, n := range []int{-1, 10001} {
		v := n
		mustFail(t, e, "list.update", boards.ListUpdateIn{ID: l.ID, ArchiveAfter: &v}, "validation")
		mustFail(t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "x", ArchiveAfter: &v}, "validation")
		mustFail(t, e, "list.archivePreview", boards.ListArchivePreviewIn{ID: l.ID, ArchiveAfter: v}, "validation")
	}
	n := 10000
	c := call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Capped", ArchiveAfter: &n})
	if c.ArchiveAfter == nil || *c.ArchiveAfter != 10000 {
		t.Fatalf("create: %+v", c.ArchiveAfter)
	}
	arch := listOfType(t, e, b.ID, "archive")
	one := 1
	mustFail(t, e, "list.update", boards.ListUpdateIn{ID: arch, ArchiveAfter: &one}, "forbidden")
	mustFail(t, e, "list.archivePreview", boards.ListArchivePreviewIn{ID: arch, ArchiveAfter: 1}, "not_found")
	// a viewer can read the setting but not change or preview it; an outsider sees nothing
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	ve := testutil.As(e, viewer)
	mustFail(t, ve, "list.update", boards.ListUpdateIn{ID: l.ID, ArchiveAfter: &one}, "forbidden")
	mustFail(t, ve, "list.archivePreview", boards.ListArchivePreviewIn{ID: l.ID, ArchiveAfter: 1}, "forbidden")
	if got := call[boards.List](t, ve, "list.get", boards.ListRefIn{ID: c.ID}); got.ArchiveAfter == nil {
		t.Fatal("viewers see the setting")
	}
	ne := testutil.As(e, nobody)
	mustFail(t, ne, "list.update", boards.ListUpdateIn{ID: l.ID, ArchiveAfter: &one}, "not_found")
	mustFail(t, ne, "list.archivePreview", boards.ListArchivePreviewIn{ID: l.ID, ArchiveAfter: 1}, "not_found")
}

// The rule itself, through landings: oldest by list change, ties by position then id, the card that lands is never
// the one that goes, claimed cards are skipped, drafts and templates are not counted, closed cards are.
func TestArchiveAfterRule(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Rule")
	l := addList(t, e, b.ID, "Lane")
	src := addList(t, e, b.ID, "Source")
	arch := listOfType(t, e, b.ID, "archive")
	land := func(ids ...string) boards.LandResult {
		t.Helper()
		var res boards.LandResult
		err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			var lands []boards.Landing
			for _, id := range ids {
				if _, err := tx.Exec(`UPDATE card_placements SET list_id=?,list_changed_at=? WHERE card_id=? AND is_home=1`, l.ID, "2030-01-01T00:00:00Z", id); err != nil {
					return err
				}
				lands = append(lands, boards.Landing{CardID: id, ListID: l.ID})
			}
			var err error
			res, err = boards.Landed(e.Ctx, tx, lands...)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// four cards that entered at the same second: the top of the column counts as older; a draft and a card template
	// on top are not counted
	for i, id := range []string{"d", "b", "a", "c"} {
		addCard(t, e, b.ID, l.ID, id, strings.ToUpper(id), float64(i+1), "")
	}
	addCard(t, e, b.ID, l.ID, "draft", "Draft", 0.1, "")
	addCard(t, e, b.ID, l.ID, "tpl", "Tpl", 0.2, "")
	if _, err := e.DB.Exec(`UPDATE cards SET is_draft=1 WHERE id='draft'; UPDATE cards SET is_template=1 WHERE id='tpl'; UPDATE cards SET is_closed=1 WHERE id='d'`); err != nil {
		t.Fatal(err)
	}
	n := 3
	if _, err := setKeep(e, l.ID, n, true); err != nil {
		t.Fatal(err)
	}
	// 4 counted -> the top one (d, closed: closed cards count) goes; draft and template stay
	if got := idsIn(t, e, arch); !got["d"] || len(got) != 1 {
		t.Fatalf("retro trimmed %v", got)
	}
	// a claimed card is skipped: b (next oldest) holds a live claim, so a landing archives a instead
	if _, err := e.DB.Exec(`UPDATE cards SET claimed_by='u-ed',claimed_until=? WHERE id='b'`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	addCard(t, e, b.ID, src.ID, "n1", "New 1", 1, "")
	res := land("n1")
	if res.ArchivedCount() != 1 || res.Archived[0].CardIDs[0] != "a" || res.Archived[0].Keep != 3 || res.Archived[0].ArchiveID != arch || res.Archived[0].BoardID != b.ID {
		t.Fatalf("claim not skipped: %+v", res.Archived)
	}
	// an expired claim does not protect: b goes now
	if _, err := e.DB.Exec(`UPDATE cards SET claimed_until='2020-01-01T00:00:00Z' WHERE id='b'`); err != nil {
		t.Fatal(err)
	}
	addCard(t, e, b.ID, src.ID, "n2", "New 2", 2, "")
	if res := land("n2"); res.ArchivedCount() != 1 || res.Archived[0].CardIDs[0] != "b" {
		t.Fatalf("expired claim: %+v", res.Archived)
	}
	// the card that lands is never the one that goes, even when it is older than everything else (a restore with an
	// old timestamp) and every other card is claimed: the column then sits above N until a claim ends
	if _, err := e.DB.Exec(`UPDATE cards SET claimed_by='u-ed',claimed_until=? WHERE list_id=?`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), l.ID); err != nil {
		t.Fatal(err)
	}
	addCard(t, e, b.ID, src.ID, "n3", "New 3", 3, "")
	if res := land("n3"); res.ArchivedCount() != 0 || countIn(t, e, l.ID) != 4 {
		t.Fatalf("arrival archived or claims ignored: %+v, %d cards", res.Archived, countIn(t, e, l.ID))
	}
	// the claim ends: EnforceArchiveAfter (card.release, the lease sweeper) brings it back to N, oldest first
	if _, err := e.DB.Exec(`UPDATE cards SET claimed_by=NULL,claimed_until=NULL WHERE list_id=?`, l.ID); err != nil {
		t.Fatal(err)
	}
	var enf boards.LandResult
	_ = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		var err error
		enf, err = boards.EnforceArchiveAfter(e.Ctx, tx, l.ID)
		return err
	})
	if enf.ArchivedCount() != 1 || enf.Archived[0].CardIDs[0] != "c" || countIn(t, e, l.ID) != 3 {
		t.Fatalf("enforce: %+v", enf.Archived)
	}
	// a batch larger than N keeps its newest N (the bottom of the batch), after every older card has gone
	for i := 1; i <= 5; i++ {
		addCard(t, e, b.ID, src.ID, fmt.Sprintf("m%d", i), fmt.Sprintf("M%d", i), float64(10+i), "")
	}
	res = land("m1", "m2", "m3", "m4", "m5")
	left := idsIn(t, e, l.ID)
	if res.ArchivedCount() != 5 || !left["m3"] || !left["m4"] || !left["m5"] || !left["draft"] || !left["tpl"] || countIn(t, e, l.ID) != 3 {
		t.Fatalf("batch: archived %v, left %v", res.Archived, left)
	}
	// idempotent: enforcing again does nothing
	_ = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		var err error
		enf, err = boards.EnforceArchiveAfter(e.Ctx, tx, l.ID)
		return err
	})
	if enf.ArchivedCount() != 0 {
		t.Fatalf("second pass archived %+v", enf.Archived)
	}
	// moving a card out lowers the count; nothing is ever un-archived
	call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: l.ID, ListID: src.ID})
	if countIn(t, e, l.ID) != 0 || len(idsIn(t, e, arch)) != 9 {
		t.Fatalf("after moving out: lane %d, archive %d", countIn(t, e, l.ID), len(idsIn(t, e, arch)))
	}
}

// list.moveCards into a capped column: the column's old cards go first, then the top of the batch; one summary event.
func TestArchiveAfterMoveCards(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Bulk")
	from, to := addList(t, e, b.ID, "From"), addList(t, e, b.ID, "To")
	fillColumn(t, e, b.ID, to.ID, "old", 10)
	fillColumn(t, e, b.ID, from.ID, "new", 50)
	if _, err := setKeep(e, to.ID, 30, true); err != nil { // 10 cards: nothing to archive, no confirm needed
		t.Fatal(err)
	}
	call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: from.ID, ListID: to.ID})
	left := idsIn(t, e, to.ID)
	if len(left) != 30 {
		t.Fatalf("column holds %d", len(left))
	}
	for i := 21; i <= 50; i++ {
		if !left[fmt.Sprintf("new%d", i)] {
			t.Fatalf("new%d should stay (the newest 30 of the batch): %v", i, left)
		}
	}
	var summary int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE type='listUpdate' AND data LIKE '%"autoArchived":30%'`).Scan(&summary)
	if summary != 1 {
		t.Fatalf("summaries %d", summary)
	}
}

// Two landings at the same time serialise on SQLite's write lock: the column ends at exactly N.
func TestArchiveAfterConcurrentLandings(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Race")
	l := addList(t, e, b.ID, "Lane")
	fillColumn(t, e, b.ID, l.ID, "c", 5)
	if _, err := setKeep(e, l.ID, 5, false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := ops.Call(e, "list.create", &boards.ListCreateIn{BoardID: b.ID, Name: "noise"}) // unrelated writes in between
			if err == nil {
				_, err = ops.Typed[cards.Card](e, "card.create", cards.CreateIn{ListID: l.ID, Name: fmt.Sprintf("racer %d", i)})
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := countIn(t, e, l.ID); n != 5 {
		t.Fatalf("after 20 concurrent landings the column holds %d, want 5", n)
	}
}

// A large column: 5000 -> 30 in one confirmed update (one transaction, chunked statements) stays fast.
func TestArchiveAfterLargeColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}
	e := fixture(t)
	b := newBoard(t, e, "Big")
	l := addList(t, e, b.ID, "Done")
	fillColumn(t, e, b.ID, l.ID, "c", 5000)
	p := call[boards.ListArchivePreview](t, e, "list.archivePreview", boards.ListArchivePreviewIn{ID: l.ID, ArchiveAfter: 30})
	if p.WillArchive != 4970 {
		t.Fatalf("preview %+v", p)
	}
	start := time.Now()
	if _, err := setKeep(e, l.ID, 30, true); err != nil {
		t.Fatal(err)
	}
	t.Logf("5000 -> 30: %v (write transaction plus %d quiet events)", time.Since(start), p.WillArchive)
	if countIn(t, e, l.ID) != 30 {
		t.Fatal("not trimmed")
	}
}

// The setting travels: list.get/board.tree, export/import (an import trims a column over N), templates.
func TestArchiveAfterTravels(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Travel")
	l := addList(t, e, b.ID, "Done")
	fillColumn(t, e, b.ID, l.ID, "c", 12)
	if _, err := setKeep(e, l.ID, 8, true); err != nil {
		t.Fatal(err)
	}
	tree := call[boards.TreeOut](t, e, "board.tree", boards.TreeIn{ID: b.ID})
	found := false
	for _, x := range tree.Lists {
		if x.ID == l.ID {
			found = x.ArchiveAfter != nil && *x.ArchiveAfter == 8
		}
	}
	if !found {
		t.Fatal("board.tree lacks archiveAfter")
	}
	st := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: b.ID})
	if !strings.Contains(string(st.Data), `"archiveAfter": 8`) {
		t.Fatal("export lacks archiveAfter")
	}
	// a file whose capped column holds more cards than N (say it was edited by hand) is trimmed on import
	doc := strings.Replace(string(st.Data), `"archiveAfter": 8`, `"archiveAfter": 5`, 1)
	imp := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p1", Format: "planka", Data: doc, Name: "Imported"})
	lid := listIDOf(t, e, imp.ID, "Done")
	if got := call[boards.List](t, e, "list.get", boards.ListRefIn{ID: lid}); got.ArchiveAfter == nil || *got.ArchiveAfter != 5 {
		t.Fatalf("import setting %+v", got.ArchiveAfter)
	}
	if countIn(t, e, lid) != 5 {
		t.Fatalf("imported column holds %d, want 5", countIn(t, e, lid))
	}
	// out-of-range values in a file are dropped
	bad := strings.Replace(string(st.Data), `"archiveAfter": 8`, `"archiveAfter": 99999`, 1)
	imp2 := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p1", Format: "planka", Data: bad, Name: "Bad"})
	if got := call[boards.List](t, e, "list.get", boards.ListRefIn{ID: listIDOf(t, e, imp2.ID, "Done")}); got.ArchiveAfter != nil {
		t.Fatal("an out-of-range value was imported")
	}
	// published template: the gallery info and an instantiated board carry it
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: b.ID, IsTemplate: true})
	info := call[boards.TemplatePreview](t, e, "board.template.get", boards.TemplateGetIn{TemplateID: b.ID})
	if len(info.Columns) == 0 || info.Columns[0].ArchiveAfter != 8 {
		t.Fatalf("template columns %+v", info.Columns)
	}
	inst := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p1", TemplateID: b.ID, Name: "From template"})
	if got := call[boards.List](t, e, "list.get", boards.ListRefIn{ID: listIDOf(t, e, inst.ID, "Done")}); got.ArchiveAfter == nil || *got.ArchiveAfter != 8 {
		t.Fatal("instantiate lost archiveAfter")
	}
}

// The UI: the sheet row and popover, the preview/confirm with the exact count, the auto-apply when nothing would be
// archived, the menu row, the header indicator, the read-only sheet.
func TestArchiveAfterUI(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "UI")
	l := addList(t, e, b.ID, "Done")
	empty := addList(t, e, b.ID, "Empty")
	fillColumn(t, e, b.ID, l.ID, "c", 40)
	m := mux(e)
	mustGet := func(path string, wants ...string) string {
		t.Helper()
		code, body := get(t, m, path, true)
		if code != 200 {
			t.Fatalf("GET %s: %d %s", path, code, body)
		}
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("GET %s lacks %q", path, w)
			}
		}
		return body
	}
	mustGet("/ui/lists/"+l.ID, `id="ls-pl-archive">Auto-archive</span>`, `data-ls-keep-val>Off</span>`, "Keep the newest N cards",
		`hx-get="/ui/lists/`+l.ID+`/archive-preview"`, `name="archiveAfter"`, `id="ls-archive-confirm"`)
	conf := mustGet("/ui/lists/"+l.ID+"/archive-preview?archiveAfter=30&ctx=sheet", "Archive 10 cards now?",
		"Done holds 40 cards; it keeps the newest 30.", `name="confirmArchive" value="true"`, `data-closes-default autofocus>Archive 10 cards</button>`,
		`data-ls-form="archive" data-keep="30" data-archived="10"`, "data-closes-cancel")
	if strings.Contains(conf, `hx-trigger="load"`) {
		t.Error("a setting that archives cards must ask first")
	}
	mustGet("/ui/lists/"+empty.ID+"/archive-preview?archiveAfter=5&ctx=sheet", `hx-trigger="load"`, `name="archiveAfter" value="5"`)
	if menu := mustGet("/ui/lists/"+l.ID+"/archive-preview?archiveAfter=30&ctx=menu", `role="menuitem"`); strings.Contains(menu, "data-ls-form") {
		t.Error("the menu confirm must not carry the sheet hooks")
	}
	patch := func(vals url.Values) int {
		t.Helper()
		r := httptest.NewRequest("PATCH", "/ui/lists/"+l.ID, strings.NewReader(vals.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		return w.Code
	}
	if c := patch(url.Values{"archiveAfter": {"30"}}); c != 409 {
		t.Fatalf("PATCH without confirm: %d", c)
	}
	if c := patch(url.Values{"archiveAfter": {"30"}, "confirmArchive": {"true"}}); c >= 300 || countIn(t, e, l.ID) != 30 {
		t.Fatalf("PATCH confirmed: %d, %d cards", c, countIn(t, e, l.ID))
	}
	_, board := get(t, m, "/boards/"+b.ID, false)
	for _, want := range []string{`class="boards-col-archive" role="img" aria-label="Auto-archive: keeps the newest 30 cards"`,
		`hx-get="/ui/lists/` + l.ID + `/archive-preview" hx-target="#lac-` + l.ID + `"`, `value="30" placeholder="Off"`, "Turn auto-archive off"} {
		if !strings.Contains(board, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	mustGet("/ui/lists/"+l.ID, `Keeps newest 30</span>`, `Keep 30</span>`, `data-ls-keep-clear>`)
	// "Turn off" posts an empty value: that clears it
	if c := patch(url.Values{"archiveAfter": {""}}); c >= 300 {
		t.Fatalf("turn off: %d", c)
	}
	if got := call[boards.List](t, e, "list.get", boards.ListRefIn{ID: l.ID}); got.ArchiveAfter != nil {
		t.Fatal("empty value must clear the setting")
	}
	// a viewer sees the setting read-only
	thirty := 30
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, ArchiveAfter: &thirty})
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	vm := mux(testutil.As(e, viewer))
	code, ro := get(t, vm, "/ui/lists/"+l.ID, true)
	if code != 200 || !strings.Contains(ro, `<span class="cm-pl">Auto-archive</span>`) || strings.Contains(ro, "archive-preview") {
		t.Fatalf("read-only sheet: %d", code)
	}
}
