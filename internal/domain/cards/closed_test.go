package cards

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crueber/weave/internal/domain/boards"
)

func closedNow(t *testing.T, f *fx, id string) bool {
	t.Helper()
	var v bool
	var closedAt *string
	if err := f.e.DB.QueryRow(`SELECT is_closed, closed_at FROM cards WHERE id=?`, id).Scan(&v, &closedAt); err != nil {
		t.Fatal(err)
	}
	if v && (closedAt == nil || *closedAt == "") {
		t.Fatalf("card %s has is_closed=true but empty closed_at", id)
	}
	if !v && closedAt != nil {
		t.Fatalf("card %s has is_closed=false but closed_at=%q", id, *closedAt)
	}
	return v
}

// Every single-card path that puts a card into a column follows boards.ClosedAfterLanding.
func TestClosedLandingPaths(t *testing.T) {
	f := seed(t)
	want := func(id string, v bool, what string) {
		t.Helper()
		if got := closedNow(t, f, id); got != v {
			t.Fatalf("%s: closed=%v want %v", what, got, v)
		}
	}
	// create
	open, done := f.card(t, f.todo, "open"), f.card(t, f.closed, "done")
	want(open.ID, false, "create in active")
	want(done.ID, true, "create in closed")
	// move (card.move) both ways; a reorder inside the column changes nothing
	call[Card](t, f.e, "card.move", MoveIn{ID: open.ID, ListID: f.closed})
	want(open.ID, true, "move into closed")
	call[Card](t, f.e, "card.move", MoveIn{ID: open.ID, ListID: f.closed, BeforeID: done.ID})
	want(open.ID, true, "reorder inside closed")
	call[Card](t, f.e, "card.move", MoveIn{ID: open.ID, ListID: f.doing})
	want(open.ID, false, "move out of closed")
	// drag-drop (the board's drop route calls card.move)
	mux := mount(f, f.e)
	if w := do(mux, "POST", "/ui/boards/"+f.board+"/cards/drop", url.Values{"id": {open.ID}, "container": {f.closed}}); w.Code >= 400 {
		t.Fatalf("drop %d %s", w.Code, w.Body.String())
	}
	want(open.ID, true, "drag into closed")
	// archive and trash keep the closed state; restore applies the destination's rule
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: open.ID})
	want(open.ID, true, "archive keeps closed")
	call[Card](t, f.e, "card.restore", IDIn{open.ID})
	want(open.ID, true, "restored into its closed column")
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: done.ID, Trash: true})
	want(done.ID, true, "trash keeps closed")
	call[Card](t, f.e, "card.move", MoveIn{ID: done.ID, ListID: f.todo})
	want(done.ID, false, "out of the trash into an active column")
	// closed in place (board without a closed column): reorder keeps it, archive keeps it, restore re-opens
	ip := call[Card](t, f.e, "card.create", CreateIn{ListID: f.list2, Name: "in place"})
	other := call[Card](t, f.e, "card.create", CreateIn{ListID: f.list2, Name: "other"})
	call[Card](t, f.e, "card.close", CloseIn{ID: ip.ID})
	want(ip.ID, true, "closed in place")
	call[Card](t, f.e, "card.move", MoveIn{ID: ip.ID, ListID: f.list2, AfterID: other.ID})
	want(ip.ID, true, "reorder keeps a card closed in place")
	mustExec(t, f.e, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('l-b2-arch','b2','archive',NULL,NULL,?)`, "2024-01-01T00:00:00Z")
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: ip.ID})
	want(ip.ID, true, "archiving a card closed in place keeps it closed")
	call[Card](t, f.e, "card.restore", IDIn{ip.ID})
	want(ip.ID, false, "restoring into an active column re-opens")
	// duplicate, from-template, recurrence
	dup := call[Card](t, f.e, "card.duplicate", DuplicateIn{ID: open.ID})
	want(dup.ID, true, "duplicate inside a closed column")
	tpl := call[Card](t, f.e, "card.template.save", TemplateSaveIn{ID: open.ID})
	want(tpl.ID, false, "a card template is never closed")
	ft := call[Card](t, f.e, "card.fromTemplate", FromTemplateIn{ID: tpl.ID, ListID: f.closed})
	want(ft.ID, true, "from template into closed")
	ft2 := call[Card](t, f.e, "card.fromTemplate", FromTemplateIn{ID: tpl.ID, ListID: f.todo})
	want(ft2.ID, false, "from template into active")
	rc := f.card(t, f.closed, "weekly review")
	call[Card](t, f.e, "card.due.set", DueIn{ID: rc.ID, DueDate: time.Now().UTC().AddDate(0, 0, -2).Format(time.RFC3339)})
	call[Card](t, f.e, "card.recurrence.set", RecurrenceIn{ID: rc.ID, Recurrence: `{"freq":"daily","count":2}`})
	if n, err := RunRecurrence(f.e.Ctx, f.e.DB, f.e.Hub, time.Now()); err != nil || n != 1 {
		t.Fatalf("recurrence spawned %d err %v", n, err)
	}
	var spawned string
	_ = f.e.DB.QueryRow(`SELECT id FROM cards WHERE name='weekly review' AND id<>? AND is_template=0`, rc.ID).Scan(&spawned)
	want(spawned, true, "recurrence spawn into a closed column")
	// inbox: a draft is never closed; publishing into a closed column closes it
	call[InboxOut](t, f.admin, "card.inbox.set", InboxSetIn{BoardID: f.board, Enabled: true})
	d1 := call[Card](t, f.e, "card.draft.create", DraftCreateIn{BoardID: f.board, Name: "idea"})
	want(d1.ID, false, "draft")
	call[Card](t, f.e, "card.draft.publish", PublishIn{ID: d1.ID, ListID: f.closed})
	want(d1.ID, true, "draft published into closed")
	d2 := call[Card](t, f.e, "card.draft.create", DraftCreateIn{BoardID: f.board, Name: "idea 2"})
	call[Card](t, f.e, "card.draft.publish", PublishIn{ID: d2.ID, ListID: f.todo})
	want(d2.ID, false, "draft published into active")
}

const markHTML = `class="closed-mark"`

// The check mark appears exactly on closed cards in every view, with screen-reader text and a hover title.
func TestClosedMarkRendered(t *testing.T) {
	f := seed(t)
	mux := mount(f, f.e)
	open := f.card(t, f.todo, "Open card")
	done := f.card(t, f.closed, "Done card")
	call[Card](t, f.e, "card.due.set", DueIn{ID: open.ID, DueDate: "2026-11-12"})
	call[Card](t, f.e, "card.due.set", DueIn{ID: done.ID, DueDate: "2026-11-13"})
	call[InboxOut](t, f.admin, "card.inbox.set", InboxSetIn{BoardID: f.board, Enabled: true})
	call[Card](t, f.e, "card.draft.create", DraftCreateIn{BoardID: f.board, Name: "Draft card"})

	// the mark sits right before the title of the closed card only
	chunk := func(body, name string) string {
		i := strings.Index(body, name)
		if i < 0 {
			t.Fatalf("%q not rendered", name)
		}
		lo := max(0, i-900)
		return body[lo:i]
	}
	for _, view := range []string{"kanban", "grid", "list"} {
		w := do(mux, "GET", "/boards/"+f.board+"?view="+view, nil)
		body := w.Body.String()
		if w.Code != 200 {
			t.Fatalf("%s: %d", view, w.Code)
		}
		if n := strings.Count(body, markHTML); n != 1 {
			t.Fatalf("%s: %d marks, want 1 (the closed card only)", view, n)
		}
		before := chunk(body, ">Done card<")
		if !strings.Contains(before, markHTML) || !strings.Contains(before, `title="Closed"`) || !strings.Contains(before, `Closed: </span>`) {
			t.Fatalf("%s: the closed card lacks the mark: %s", view, before)
		}
		if strings.Contains(body, "opacity:.7") {
			t.Fatal("the closed fade is gone")
		}
		// both due dates are in the past: only the open card is flagged overdue (a closed card is finished)
		if view != "list" && strings.Count(body, "boards-due-overdue") != 1 {
			t.Fatalf("%s: %d overdue chips, want 1 (the open card only)", view, strings.Count(body, "boards-due-overdue"))
		}
	}
	// the calendar (month grid plots closed cards by closed_at, open cards by due_date) and its op output
	mustExec(t, f.e, `UPDATE cards SET closed_at='2026-11-14T10:00:00Z' WHERE id=?`, done.ID)
	cal := call[CalendarOut](t, f.e, "card.calendar", CalendarIn{BoardID: f.board, Month: "2026-11"})
	got := map[string]bool{}
	calDates := map[string]string{}
	for _, c := range cal.Cards {
		got[c.Name] = c.IsClosed
		calDates[c.Name] = c.CalendarDate
	}
	if !got["Done card"] || got["Open card"] {
		t.Fatalf("calendar isClosed: %v", got)
	}
	if calDates["Done card"] != "2026-11-14T10:00:00Z" || !strings.HasPrefix(calDates["Open card"], "2026-11-12") {
		t.Fatalf("calendar dates: %v", calDates)
	}
	w := do(mux, "GET", "/boards/"+f.board+"/calendar?month=2026-11", nil)
	if body := w.Body.String(); strings.Count(body, markHTML) != 1 || !strings.Contains(chunk(body, "Done card</a>"), markHTML) {
		t.Fatalf("calendar marks: %d", strings.Count(body, markHTML))
	}
	// a card closed in place (no due date) plots on its closed date rather than sitting in Unscheduled
	nodue := call[Card](t, f.e, "card.create", CreateIn{ListID: f.todo, Name: "Closed no due"})
	mustExec(t, f.e, `UPDATE cards SET is_closed=1, closed_at='2026-11-18T12:00:00Z' WHERE id=?`, nodue.ID)
	cal = call[CalendarOut](t, f.e, "card.calendar", CalendarIn{BoardID: f.board, Month: "2026-11"})
	for _, u := range cal.Unscheduled {
		if u.ID == nodue.ID {
			t.Fatal("closed card should not appear in Unscheduled")
		}
	}
	w = do(mux, "GET", "/boards/"+f.board+"/calendar?month=2026-11", nil)
	if body := w.Body.String(); !strings.Contains(chunk(body, "Closed no due</a>"), markHTML) {
		t.Fatal("closed card without due date lacks the mark on the calendar grid")
	}
	// the card sheet: a success badge with the mark and the word, nothing on an open card
	if body := do(mux, "GET", "/ui/cards/"+done.ID, nil).Body.String(); !strings.Contains(body, `cards-badge cards-badge-closed`) || !strings.Contains(body, markHTML) {
		t.Fatal("card sheet of a closed card lacks the closed badge")
	}
	if body := do(mux, "GET", "/ui/cards/"+open.ID, nil).Body.String(); strings.Contains(body, markHTML) {
		t.Fatal("open card sheet shows the mark")
	}
	// the share page
	sh := call[ShareOut](t, f.e, "card.share.create", IDIn{done.ID})
	if v := call[SharedCard](t, f.e, "card.share.view", ShareViewIn{Token: strings.TrimPrefix(sh.URL, "/share/")}); !v.IsClosed {
		t.Fatal("share view isClosed")
	}
	if body := do(mux, "GET", sh.URL, nil, "X-Anon", "1").Body.String(); !strings.Contains(body, markHTML) {
		t.Fatalf("share page lacks the mark")
	}
	sh2 := call[ShareOut](t, f.e, "card.share.create", IDIn{open.ID})
	if body := do(mux, "GET", sh2.URL, nil, "X-Anon", "1").Body.String(); strings.Contains(body, markHTML) {
		t.Fatal("open shared card shows the mark")
	}
	// the public board (read-only tiles)
	call[boards.Board](t, f.admin, "board.setPublic", boards.BoardPublicIn{ID: f.board, IsPublic: true})
	var tok string
	_ = f.e.DB.QueryRow(`SELECT public_token FROM boards WHERE id=?`, f.board).Scan(&tok)
	w = do(mux, "GET", "/public/"+tok, nil, "X-Anon", "1")
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(chunk(body, ">Done card<"), markHTML) || strings.Contains(chunk(body, ">Open card<"), markHTML) {
		t.Fatalf("public board marks (%d)", w.Code)
	}
}

// Calendar plots open cards by due date and closed cards by closed_at (even if they had a due date in another month or none at all).
func TestCalendarClosedDate(t *testing.T) {
	f := seed(t)
	mux := mount(f, f.e)

	openDue := f.card(t, f.todo, "Open due")
	call[Card](t, f.e, "card.due.set", DueIn{ID: openDue.ID, DueDate: "2026-06-10"})
	openNone := f.card(t, f.todo, "Open unscheduled")

	closedHadDue := f.card(t, f.todo, "Closed with earlier due")
	call[Card](t, f.e, "card.due.set", DueIn{ID: closedHadDue.ID, DueDate: "2026-05-02"})
	closedHadDue = call[Card](t, f.e, "card.close", CloseIn{ID: closedHadDue.ID})
	if !closedHadDue.IsClosed || closedHadDue.ClosedAt == nil || *closedHadDue.ClosedAt == "" {
		t.Fatalf("card.close must populate closedAt: %+v", closedHadDue)
	}
	mustExec(t, f.e, `UPDATE cards SET closed_at='2026-06-15T14:30:00Z' WHERE id=?`, closedHadDue.ID)

	closedNoDue := f.card(t, f.closed, "Closed no due")
	if !closedNoDue.IsClosed || closedNoDue.ClosedAt == nil || *closedNoDue.ClosedAt == "" {
		t.Fatalf("creating in closed list must populate closedAt: %+v", closedNoDue)
	}
	mustExec(t, f.e, `UPDATE cards SET closed_at='2026-06-20T09:00:00Z' WHERE id=?`, closedNoDue.ID)

	// May should NOT contain closedHadDue (its due date was May 2, but it closed in June)
	may := call[CalendarOut](t, f.e, "card.calendar", CalendarIn{BoardID: f.board, Month: "2026-05"})
	for _, c := range may.Cards {
		if c.ID == closedHadDue.ID {
			t.Fatalf("closed card appeared in due-date month May instead of closed month June: %+v", c)
		}
	}

	// June should contain openDue (June 10), closedHadDue (June 15), and closedNoDue (June 20)
	june := call[CalendarOut](t, f.e, "card.calendar", CalendarIn{BoardID: f.board, Month: "2026-06"})
	byID := map[string]CalendarCard{}
	for _, c := range june.Cards {
		byID[c.ID] = c
	}
	if len(byID) != 3 {
		t.Fatalf("expected 3 cards in June, got %d: %+v", len(byID), june.Cards)
	}
	if !strings.HasPrefix(byID[openDue.ID].CalendarDate, "2026-06-10") {
		t.Errorf("openDue calendarDate=%q want 2026-06-10*", byID[openDue.ID].CalendarDate)
	}
	if byID[closedHadDue.ID].CalendarDate != "2026-06-15T14:30:00Z" || byID[closedHadDue.ID].ClosedAt != "2026-06-15T14:30:00Z" {
		t.Errorf("closedHadDue=%+v", byID[closedHadDue.ID])
	}
	if byID[closedNoDue.ID].CalendarDate != "2026-06-20T09:00:00Z" {
		t.Errorf("closedNoDue=%+v", byID[closedNoDue.ID])
	}
	// Unscheduled contains ONLY openNone
	if len(june.Unscheduled) != 1 || june.Unscheduled[0].ID != openNone.ID {
		t.Fatalf("unscheduled=%+v want only openNone", june.Unscheduled)
	}

	// HTML places closed chips in their closed_at day cell with draggable="false"
	w := do(mux, "GET", "/boards/"+f.board+"/calendar?month=2026-06", nil)
	if w.Code != 200 {
		t.Fatalf("calendar HTML: %d", w.Code)
	}
	body := w.Body.String()
	cell := func(date string) string {
		marker := `data-date="` + date + `"`
		i := strings.Index(body, marker)
		if i < 0 {
			t.Fatalf("day cell %s not found", date)
		}
		end := strings.Index(body[i:], "</div>")
		return body[i : i+end]
	}
	if !strings.Contains(cell("2026-06-10"), "Open due") || !strings.Contains(cell("2026-06-10"), `draggable="true"`) {
		t.Errorf("2026-06-10 cell: %s", cell("2026-06-10"))
	}
	if !strings.Contains(cell("2026-06-15"), "Closed with earlier due") || !strings.Contains(cell("2026-06-15"), `draggable="false"`) {
		t.Errorf("2026-06-15 cell: %s", cell("2026-06-15"))
	}
	if !strings.Contains(cell("2026-06-20"), "Closed no due") || !strings.Contains(cell("2026-06-20"), `draggable="false"`) {
		t.Errorf("2026-06-20 cell: %s", cell("2026-06-20"))
	}

	// Re-opening closedHadDue clears closedAt and returns it to its May 2 due date
	falseVal := false
	reopened := call[Card](t, f.e, "card.close", CloseIn{ID: closedHadDue.ID, Closed: &falseVal})
	if reopened.IsClosed || reopened.ClosedAt != nil {
		t.Fatalf("reopening must clear closedAt: %+v", reopened)
	}
	may = call[CalendarOut](t, f.e, "card.calendar", CalendarIn{BoardID: f.board, Month: "2026-05"})
	if len(may.Cards) != 1 || may.Cards[0].ID != closedHadDue.ID {
		t.Fatalf("reopened card should return to due-date month May: %+v", may.Cards)
	}
}

// Archived cards (closed or with a due date) stay on the calendar marked isArchived; trashed cards and unscheduled
// archived cards are excluded.
func TestCalendarArchivedCards(t *testing.T) {
	f := seed(t)
	mux := mount(f, f.e)

	// Closed card that is then archived keeps its closed_at and stays on the calendar
	archClosed := f.card(t, f.closed, "Archived done")
	mustExec(t, f.e, `UPDATE cards SET closed_at='2026-07-08T15:00:00Z' WHERE id=?`, archClosed.ID)
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: archClosed.ID})

	// Open card with a due date that is archived stays on the calendar on its due date
	archDue := f.card(t, f.todo, "Archived scheduled")
	call[Card](t, f.e, "card.due.set", DueIn{ID: archDue.ID, DueDate: "2026-07-12"})
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: archDue.ID})

	// Open card without a due date that is archived must NOT show up in Unscheduled
	archNone := f.card(t, f.todo, "Archived unscheduled")
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: archNone.ID})

	// Trashed card (even closed or with a due date) must NOT appear on the calendar
	trashed := f.card(t, f.closed, "Trashed done")
	mustExec(t, f.e, `UPDATE cards SET closed_at='2026-07-14T10:00:00Z' WHERE id=?`, trashed.ID)
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashed.ID, Trash: true})

	july := call[CalendarOut](t, f.e, "card.calendar", CalendarIn{BoardID: f.board, Month: "2026-07"})
	byID := map[string]CalendarCard{}
	for _, c := range july.Cards {
		byID[c.ID] = c
	}
	if len(byID) != 2 {
		t.Fatalf("expected 2 archived cards in July, got %d: %+v", len(byID), july.Cards)
	}
	if c, ok := byID[archClosed.ID]; !ok || !c.IsArchived || !c.IsClosed || c.CalendarDate != "2026-07-08T15:00:00Z" {
		t.Errorf("archClosed=%+v", c)
	}
	if c, ok := byID[archDue.ID]; !ok || !c.IsArchived || c.IsClosed || !strings.HasPrefix(c.CalendarDate, "2026-07-12") {
		t.Errorf("archDue=%+v", c)
	}
	if _, ok := byID[trashed.ID]; ok {
		t.Error("trashed card must not appear on the calendar")
	}
	for _, u := range july.Unscheduled {
		if u.ID == archNone.ID {
			t.Error("archived card without due date must not appear in Unscheduled")
		}
	}

	w := do(mux, "GET", "/boards/"+f.board+"/calendar?month=2026-07", nil)
	if w.Code != 200 {
		t.Fatalf("calendar HTML: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Archived done", "Archived scheduled", "is-archived", `draggable="false"`, "Archived · "} {
		if !strings.Contains(body, want) {
			t.Errorf("calendar HTML missing %q", want)
		}
	}
	if strings.Contains(body, "Trashed done") || strings.Contains(body, "Archived unscheduled") {
		t.Error("trashed or unscheduled-archived card leaked into calendar HTML")
	}
}
