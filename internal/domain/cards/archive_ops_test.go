package cards

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// Archive view backend: placement-scoped listing, filtering/sorting/cursors, and quiet card-level bulk restore/delete.
// Every fixture moves card_placements (or lets the home mirror triggers do it), never the mirrored columns on cards.

func archiveViewList(t *testing.T, e *ops.Env, in ArchiveListIn) ArchiveListOut {
	t.Helper()
	return call[ArchiveListOut](t, e, "card.archive.list", in)
}

func archiveViewIDs(items []ArchiveListItem) []string {
	out := make([]string, len(items))
	for i := range items {
		out[i] = items[i].ID
	}
	return out
}

func archiveViewIDSet(items []ArchiveListItem) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		out[item.ID] = true
	}
	return out
}

func archiveViewEqualIDs(t *testing.T, got []ArchiveListItem, want ...string) {
	t.Helper()
	g, w := archiveViewIDs(got), append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if strings.Join(g, ",") != strings.Join(w, ",") {
		t.Fatalf("card ids = %v, want %v", g, w)
	}
}

func archiveViewItem(t *testing.T, items []ArchiveListItem, id string) ArchiveListItem {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("card %s not in archive result %v", id, archiveViewIDs(items))
	return ArchiveListItem{}
}

func archiveViewInt(n int) *int { return &n }

func archiveViewCard(t *testing.T, f *fx, from, name, changedAt string) Card {
	t.Helper()
	c := f.card(t, from, name)
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: c.ID})
	mustExec(t, f.e, `UPDATE card_placements SET list_changed_at=? WHERE card_id=? AND board_id=?`, changedAt, c.ID, f.board)
	return c
}

func archiveViewListOf(t *testing.T, e *ops.Env, cardID, boardID string) string {
	t.Helper()
	var id string
	if err := e.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id=? AND board_id=?`, cardID, boardID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func archiveViewTrashList(t *testing.T, e *ops.Env, boardID string) string {
	t.Helper()
	id, err := boards.SystemList(e.Ctx, e.DB, boardID, "trash")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func archiveViewAddList(t *testing.T, f *fx, id, name string, pos float64) {
	t.Helper()
	mustExec(t, f.e, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES(?,?,'active',?,?,?)`, id, f.board, pos, name, db.Now())
}

// archiveViewBatch creates archived placements without going through card.create/card.archive: one transaction, no
// actions or notifications. Cards are first inserted into their real source column (cp_home_ai creates the home
// placement), then that placement is moved to the archive (cp_home_mirror maintains the cards mirror).
func archiveViewBatch(t *testing.T, e *ops.Env, boardID, fromID, archiveID, prefix, namePrefix, changedAt string, n int) []string {
	t.Helper()
	if n <= 0 {
		return nil
	}
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(e.Ctx, `WITH RECURSIVE seq(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM seq WHERE n+1 < ?1)
			INSERT INTO cards(id,board_id,list_id,creator_user_id,position,name,list_changed_at,created_at)
			SELECT ?2||'-'||printf('%05d',n),?3,?4,?5,(n+1)*?6,?7||' '||printf('%05d',n),?8,?8 FROM seq`,
			n, prefix, boardID, fromID, e.User.UserID, db.PositionGap, namePrefix, changedAt)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(e.Ctx, `UPDATE card_placements SET list_id=?1,prev_list_id=?2,list_changed_at=?3
			WHERE board_id=?4 AND list_id=?5 AND card_id GLOB ?6`, archiveID, fromID, changedAt, boardID, fromID, prefix+"-*")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%05d", prefix, i)
	}
	return ids
}

func TestArchiveListFieldsAndFilters(t *testing.T) {
	f := seed(t)
	mustExec(t, f.e, `UPDATE lists SET name='Todo' WHERE id=?`, f.todo)
	mustExec(t, f.e, `UPDATE lists SET name='Doing' WHERE id=?`, f.doing)
	mustExec(t, f.e, `UPDATE lists SET name='Done' WHERE id=?`, f.closed)

	a := archiveViewCard(t, f, f.todo, "Alpha release", "2026-01-02T00:00:00Z")
	b := archiveViewCard(t, f, f.doing, "Beta fix", "2026-01-03T00:00:00Z")
	c := archiveViewCard(t, f, f.todo, "Gamma Alpha", "2026-01-04T00:00:00Z")
	d := archiveViewCard(t, f, f.closed, "Delta", "2026-01-05T00:00:00Z")
	trashed := f.card(t, f.todo, "Trashed")
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashed.ID, Trash: true})

	call[Card](t, f.e, "card.label.add", LabelIn{ID: a.ID, LabelID: f.labelA})
	call[Card](t, f.e, "card.label.add", LabelIn{ID: c.ID, LabelID: f.labelA})
	call[Card](t, f.e, "card.label.add", LabelIn{ID: b.ID, LabelID: f.labelB})
	call[Card](t, f.e, "card.member.add", MemberIn{ID: a.ID, UserID: f.vw})
	call[Card](t, f.e, "card.member.add", MemberIn{ID: c.ID, UserID: f.vw})
	desc := "Contains Needle details"
	call[Card](t, f.e, "card.update", UpdateIn{ID: b.ID, Description: &desc})

	all := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Limit: 200})
	if all.Total != 4 || len(all.Items) != 4 || all.NextCursor != "" {
		t.Fatalf("default archived list = %+v, want four archived cards and no trash", all)
	}
	archiveViewEqualIDs(t, all.Items, a.ID, b.ID, c.ID, d.ID)
	gotA := archiveViewItem(t, all.Items, a.ID)
	if gotA.Name != a.Name || gotA.ArchivedAt != "2026-01-02T00:00:00Z" || gotA.FromListID != f.todo || gotA.FromListName != "Todo" || gotA.Closed || !gotA.CanRestore {
		t.Fatalf("archive row fields = %+v", gotA)
	}
	if len(gotA.Labels) != 1 || gotA.Labels[0].ID != f.labelA || gotA.Labels[0].Name != "High" {
		t.Fatalf("archive row labels = %+v", gotA.Labels)
	}
	if len(gotA.Members) != 1 || gotA.Members[0].ID != f.vw || gotA.Members[0].Name != "Vi Ewer" || gotA.Members[0].IsAgent {
		t.Fatalf("archive row members = %+v", gotA.Members)
	}
	if !archiveViewItem(t, all.Items, d.ID).Closed {
		t.Fatal("archiving must preserve the card-level closed mark")
	}
	if trash := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Kind: "trash", Limit: 200}); trash.Total != 1 || archiveViewIDs(trash.Items)[0] != trashed.ID {
		t.Fatalf("trash list = %+v", trash)
	}

	for _, tc := range []struct {
		name string
		in   ArchiveListIn
		want []string
	}{
		{"text in name", ArchiveListIn{Q: "alpha"}, []string{a.ID, c.ID}},
		{"text in description", ArchiveListIn{Q: "needle"}, []string{b.ID}},
		{"text does not match label names", ArchiveListIn{Q: "High"}, nil},
		{"text does not match member names", ArchiveListIn{Q: "Vi Ewer"}, nil},
		{"label", ArchiveListIn{LabelID: f.labelA}, []string{a.ID, c.ID}},
		{"member", ArchiveListIn{MemberID: f.vw}, []string{a.ID, c.ID}},
		{"source column", ArchiveListIn{FromListID: f.todo}, []string{a.ID, c.ID}},
		{"since inclusive", ArchiveListIn{Since: "2026-01-03T00:00:00Z"}, []string{b.ID, c.ID, d.ID}},
		{"until inclusive", ArchiveListIn{Until: "2026-01-03T00:00:00Z"}, []string{a.ID, b.ID}},
		{"date window", ArchiveListIn{Since: "2026-01-03T00:00:00Z", Until: "2026-01-04T00:00:00Z"}, []string{b.ID, c.ID}},
		{"combined", ArchiveListIn{Q: "alpha", LabelID: f.labelA, MemberID: f.vw, FromListID: f.todo, Since: "2026-01-02T00:00:00Z", Until: "2026-01-03T00:00:00Z"}, []string{a.ID}},
		{"unknown label", ArchiveListIn{LabelID: "missing-label"}, nil},
		{"unknown source column", ArchiveListIn{FromListID: "missing-list"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.BoardID, in.Limit = f.board, 200
			out := archiveViewList(t, f.e, in)
			if out.Total != len(tc.want) || len(out.Items) != len(tc.want) {
				t.Fatalf("filter total=%d rows=%d, want %d", out.Total, len(out.Items), len(tc.want))
			}
			archiveViewEqualIDs(t, out.Items, tc.want...)
		})
	}
}

func TestArchiveListMemberAliasesAndCursor(t *testing.T) {
	f := seed(t)
	mineA := archiveViewCard(t, f, f.todo, "A mine", "2026-01-06T00:00:00Z")
	mineB := archiveViewCard(t, f, f.todo, "B mine", "2026-01-07T00:00:00Z")
	theirs := archiveViewCard(t, f, f.todo, "C theirs", "2026-01-08T00:00:00Z")
	unassigned := archiveViewCard(t, f, f.todo, "D unassigned", "2026-01-09T00:00:00Z")
	call[Card](t, f.e, "card.member.add", MemberIn{ID: mineA.ID, UserID: f.ed})
	call[Card](t, f.e, "card.member.add", MemberIn{ID: mineB.ID, UserID: f.ed})
	call[Card](t, f.e, "card.member.add", MemberIn{ID: theirs.ID, UserID: f.vw})

	for _, tc := range []struct {
		name, memberID string
		want           []string
	}{
		{"me", "me", []string{mineA.ID, mineB.ID}},
		{"none", "none", []string{unassigned.ID}},
		{"concrete id", f.vw, []string{theirs.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, MemberID: tc.memberID, Limit: 200})
			if out.Total != len(tc.want) {
				t.Fatalf("memberId=%q total=%d, want %d", tc.memberID, out.Total, len(tc.want))
			}
			archiveViewEqualIDs(t, out.Items, tc.want...)
		})
	}

	// "me" is resolved before fingerprinting, so repeated requests and the equivalent concrete id
	// produce the same cursor and can page through the same set.
	firstMe := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, MemberID: "me", Sort: "name", Dir: "asc", Limit: 1})
	repeatedMe := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, MemberID: "me", Sort: "name", Dir: "asc", Limit: 1})
	firstConcrete := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, MemberID: f.ed, Sort: "name", Dir: "asc", Limit: 1})
	if firstMe.Total != 2 || len(firstMe.Items) != 1 || firstMe.NextCursor == "" {
		t.Fatalf("first me page = %+v", firstMe)
	}
	if repeatedMe.NextCursor != firstMe.NextCursor || firstConcrete.NextCursor != firstMe.NextCursor {
		t.Fatalf("resolved member cursors differ: me=%q repeated=%q concrete=%q", firstMe.NextCursor, repeatedMe.NextCursor, firstConcrete.NextCursor)
	}
	secondMe := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, MemberID: "me", Sort: "name", Dir: "asc", Limit: 1, Cursor: firstMe.NextCursor})
	if secondMe.Total != 2 || secondMe.NextCursor != "" {
		t.Fatalf("second me page = %+v", secondMe)
	}
	archiveViewEqualIDs(t, secondMe.Items, mineB.ID)

	// A public board is viewable without a principal, but "me" has no user id to resolve.
	mustExec(t, f.e, `UPDATE boards SET is_public=1 WHERE id=?`, f.board)
	anonymous := *f.e
	anonymous.User = nil
	emptyPrincipal := *f.e
	emptyPrincipal.User = &ops.Principal{}
	for _, e := range []*ops.Env{&anonymous, &emptyPrincipal} {
		_, err := ops.Typed[ArchiveListOut](e, "card.archive.list", ArchiveListIn{BoardID: f.board, MemberID: "me"})
		wantCode(t, err, "unauthorized")
	}
}

func TestArchiveListQueryEscapingVisibilityAndDateBounds(t *testing.T) {
	m := seedMB(t)
	percent := archiveViewCard(t, m.fx, m.todo, "100% ready", "2026-01-02T00:00:00Z")
	underscore := archiveViewCard(t, m.fx, m.todo, "under_score", "2026-01-02T01:00:00Z")
	plain := archiveViewCard(t, m.fx, m.todo, "plain", "2026-01-02T02:00:00Z")
	descHit := archiveViewCard(t, m.fx, m.todo, "plain two", "2026-01-02T03:00:00Z")
	desc := "literal a_b%c text"
	call[Card](t, m.e, "card.update", UpdateIn{ID: descHit.ID, Description: &desc})

	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"%", []string{percent.ID, descHit.ID}},
		{"_", []string{underscore.ID, descHit.ID}},
		{"a_b%c", []string{descHit.ID}},
		{"100%", []string{percent.ID}},
	} {
		out := archiveViewList(t, m.e, ArchiveListIn{BoardID: m.board, Q: tc.q, Limit: 200})
		if out.Total != len(tc.want) {
			t.Fatalf("q=%q total=%d, want %d", tc.q, out.Total, len(tc.want))
		}
		archiveViewEqualIDs(t, out.Items, tc.want...)
	}

	// Drafts and templates can be placed in an archive by fixture SQL, but are never archive-view rows or bulk matches.
	call[InboxOut](t, m.admin, "card.inbox.set", InboxSetIn{BoardID: m.board, Enabled: true})
	draft := call[Card](t, m.e, "card.draft.create", DraftCreateIn{BoardID: m.board, Name: "hidden draft"})
	tpl := call[Card](t, m.e, "card.template.save", TemplateSaveIn{ID: plain.ID})
	mustExec(t, m.e, `UPDATE card_placements SET list_id=?,prev_list_id=?,list_changed_at=? WHERE card_id IN (?,?)`,
		m.arch, m.todo, "2026-01-02T04:00:00Z", draft.ID, tpl.ID)
	if out := archiveViewList(t, m.e, ArchiveListIn{BoardID: m.board, Limit: 200}); out.Total != 4 {
		t.Fatalf("drafts/templates leaked into archive: %+v", out)
	}

	// A label belonging only to b2 matches there, never on b1, even when the same card is archived on both.
	shared := m.card(t, m.todo, "shared vocabulary")
	m.place(t, m.e, shared.ID, m.list2)
	call[Card](t, m.e, "card.label.add", LabelIn{ID: shared.ID, LabelID: m.labelOther, BoardID: m.board2})
	mustExec(t, m.e, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('l-b2-arch','b2','archive',10,NULL,?)`, db.Now())
	call[Card](t, m.e, "card.move", MoveIn{ID: shared.ID, BoardID: m.board, ListID: m.arch})
	call[Card](t, m.e, "card.move", MoveIn{ID: shared.ID, BoardID: m.board2, ListID: "l-b2-arch"})
	if out := archiveViewList(t, m.e, ArchiveListIn{BoardID: m.board, LabelID: m.labelOther, Limit: 200}); out.Total != 0 {
		t.Fatalf("b1 matched b2's label: %+v", out)
	}
	if out := archiveViewList(t, m.e, ArchiveListIn{BoardID: m.board2, LabelID: m.labelOther, Limit: 200}); out.Total != 1 || out.Items[0].ID != shared.ID {
		t.Fatalf("b2 did not match its own label: %+v", out)
	}

	start := archiveViewCard(t, m.fx, m.todo, "day start", "2026-02-02T00:00:00Z")
	end := archiveViewCard(t, m.fx, m.todo, "day end", "2026-02-02T23:59:59Z")
	next := archiveViewCard(t, m.fx, m.todo, "next day", "2026-02-03T00:00:00Z")
	for _, tc := range []struct {
		name string
		in   ArchiveListIn
		want []string
	}{
		{"since exact inclusive", ArchiveListIn{Since: "2026-02-02T00:00:00Z"}, []string{start.ID, end.ID, next.ID}},
		{"until timestamp inclusive", ArchiveListIn{Until: "2026-02-02T23:59:59Z"}, []string{start.ID, end.ID}},
		{"until date includes whole day", ArchiveListIn{Until: "2026-02-02"}, []string{start.ID, end.ID}},
		{"since normalizes UTC", ArchiveListIn{Since: "2026-02-02T02:00:00+02:00"}, []string{start.ID, end.ID, next.ID}},
		{"until normalizes UTC", ArchiveListIn{Until: "2026-02-03T02:00:00+02:00"}, []string{start.ID, end.ID, next.ID}},
		{"next midnight excluded by date", ArchiveListIn{Since: "2026-02-03"}, []string{next.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.BoardID, in.Q, in.Limit = m.board, "day", 200
			out := archiveViewList(t, m.e, in)
			if out.Total != len(tc.want) {
				t.Fatalf("date filter total=%d, want %d", out.Total, len(tc.want))
			}
			archiveViewEqualIDs(t, out.Items, tc.want...)
		})
	}
	for _, in := range []ArchiveListIn{
		{BoardID: m.board, Since: "not-a-date"},
		{BoardID: m.board, Until: "2026-02-30"},
		{BoardID: m.board, Since: "2026-02-03T00:00:00Z", Until: "2026-02-02T23:59:59Z"},
	} {
		_, err := ops.Typed[ArchiveListOut](m.e, "card.archive.list", in)
		wantCode(t, err, "validation")
	}
}

func TestArchiveListSortsBothDirections(t *testing.T) {
	f := seed(t)
	mustExec(t, f.e, `UPDATE lists SET name='Zulu' WHERE id=?`, f.todo)
	mustExec(t, f.e, `UPDATE lists SET name='Alpha' WHERE id=?`, f.doing)
	mustExec(t, f.e, `UPDATE lists SET name='Middle' WHERE id=?`, f.closed)
	archiveViewAddList(t, f, "l-beta", "Beta", 0)
	z := archiveViewCard(t, f, f.todo, "beta", "2026-03-01T00:00:00Z")
	a := archiveViewCard(t, f, f.doing, "alpha", "2026-03-02T00:00:00Z")
	m := archiveViewCard(t, f, f.closed, "gamma", "2026-03-03T00:00:00Z")
	b := archiveViewCard(t, f, "l-beta", "delta", "2026-03-04T00:00:00Z")

	for _, tc := range []struct {
		name, sort, dir string
		want            []string
	}{
		{"default newest first", "", "", []string{b.ID, m.ID, a.ID, z.ID}},
		{"archived asc", "archived", "asc", []string{z.ID, a.ID, m.ID, b.ID}},
		{"archived desc", "archived", "desc", []string{b.ID, m.ID, a.ID, z.ID}},
		{"name asc", "name", "asc", []string{a.ID, z.ID, b.ID, m.ID}},
		{"name desc", "name", "desc", []string{m.ID, b.ID, z.ID, a.ID}},
		{"from asc", "from", "asc", []string{a.ID, b.ID, m.ID, z.ID}},
		{"from desc", "from", "desc", []string{z.ID, m.ID, b.ID, a.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Sort: tc.sort, Dir: tc.dir, Limit: 200})
			if got := strings.Join(archiveViewIDs(out.Items), ","); got != strings.Join(tc.want, ",") {
				t.Fatalf("order = %v, want %v", archiveViewIDs(out.Items), tc.want)
			}
		})
	}
	for _, in := range []ArchiveListIn{
		{BoardID: f.board, Sort: "updated"},
		{BoardID: f.board, Dir: "sideways"},
		{BoardID: f.board, Kind: "everything"},
	} {
		_, err := ops.Typed[ArchiveListOut](f.e, "card.archive.list", in)
		wantCode(t, err, "validation")
	}
}

func TestArchiveListCursorTiesAndOpaque(t *testing.T) {
	f := seed(t)
	// Same source-list name, different source-list ids: from ordering must use name, then list id, then card id.
	archiveViewAddList(t, f, "l-queue-z", "Queue", 0)
	archiveViewAddList(t, f, "l-queue-a", "Queue", 1)
	var allIDs []string
	byList := map[string][]string{}
	for _, list := range []string{"l-queue-z", "l-queue-a"} {
		for _, name := range []string{"Same", "Other", "Third"} {
			c := archiveViewCard(t, f, list, name, "2026-04-01T00:00:00Z")
			allIDs = append(allIDs, c.ID)
			byList[list] = append(byList[list], c.ID)
		}
	}
	for _, ids := range byList {
		sort.Strings(ids)
	}
	wantFrom := append(append([]string{}, byList["l-queue-a"]...), byList["l-queue-z"]...)

	for _, tc := range []struct {
		sort, dir string
	}{
		{"archived", "asc"}, {"archived", "desc"}, {"name", "asc"}, {"name", "desc"}, {"from", "asc"}, {"from", "desc"},
	} {
		t.Run(tc.sort+"/"+tc.dir, func(t *testing.T) {
			one := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Sort: tc.sort, Dir: tc.dir, Limit: 200})
			var paged []string
			cursor := ""
			for page := 0; page < 10; page++ {
				out := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Sort: tc.sort, Dir: tc.dir, Limit: 2, Cursor: cursor})
				if out.Total != len(allIDs) || len(out.Items) > 2 {
					t.Fatalf("page %d total=%d rows=%d", page, out.Total, len(out.Items))
				}
				paged = append(paged, archiveViewIDs(out.Items)...)
				if out.NextCursor == "" {
					break
				}
				if out.NextCursor == cursor {
					t.Fatalf("page %d repeated cursor %q", page, cursor)
				}
				for _, id := range allIDs {
					if out.NextCursor == id || strings.Contains(out.NextCursor, id) {
						t.Fatalf("cursor exposes a raw card id: %q", out.NextCursor)
					}
				}
				cursor = out.NextCursor
			}
			if len(paged) != len(allIDs) {
				t.Fatalf("paged %d rows, want %d", len(paged), len(allIDs))
			}
			seen := map[string]bool{}
			for _, id := range paged {
				if seen[id] {
					t.Fatalf("duplicate card across pages: %s", id)
				}
				seen[id] = true
			}
			if strings.Join(paged, ",") != strings.Join(archiveViewIDs(one.Items), ",") {
				t.Fatalf("paged order %v differs from one-page order %v", paged, archiveViewIDs(one.Items))
			}
			if tc.sort == "from" {
				want := wantFrom
				if tc.dir == "desc" {
					want = append([]string(nil), wantFrom...)
					for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
						want[i], want[j] = want[j], want[i]
					}
				}
				if strings.Join(paged, ",") != strings.Join(want, ",") {
					t.Fatalf("from order = %v, want name/list-id/card-id order %v", paged, want)
				}
			}
		})
	}
	_, err := ops.Typed[ArchiveListOut](f.e, "card.archive.list", ArchiveListIn{BoardID: f.board, Cursor: "not a cursor!"})
	wantCode(t, err, "validation")
	page := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Sort: "name", Dir: "asc", Limit: 2})
	_, err = ops.Typed[ArchiveListOut](f.e, "card.archive.list", ArchiveListIn{BoardID: f.board, Sort: "from", Dir: "asc", Limit: 2, Cursor: page.NextCursor})
	wantCode(t, err, "validation")
	_, err = ops.Typed[ArchiveListOut](f.e, "card.archive.list", ArchiveListIn{BoardID: f.board, Sort: "name", Dir: "asc", Limit: 2, Q: "Same", Cursor: page.NextCursor})
	wantCode(t, err, "validation")
}

func TestArchiveListCursorLongCJKValues(t *testing.T) {
	f := seed(t)
	archiveViewAddList(t, f, "l-cjk", strings.Repeat("欄", 255), 0)
	short := archiveViewCard(t, f, f.todo, "A short", "2026-04-02T00:00:00Z")
	long := archiveViewCard(t, f, "l-cjk", strings.Repeat("名", 1000), "2026-04-03T00:00:00Z")
	first := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Sort: "name", Dir: "desc", Limit: 1})
	if first.Total != 2 || len(first.Items) != 1 || first.Items[0].ID != long.ID || len(first.NextCursor) <= 4096 {
		t.Fatalf("first long-value page = %+v (cursor length %d)", first, len(first.NextCursor))
	}
	second := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Sort: "name", Dir: "desc", Limit: 1, Cursor: first.NextCursor})
	if second.Total != 2 || len(second.Items) != 1 || second.Items[0].ID != short.ID || second.NextCursor != "" {
		t.Fatalf("second long-value page = %+v", second)
	}
}

func TestArchiveListLimitsAndRemovedSource(t *testing.T) {
	f := seed(t)
	ids := archiveViewBatch(t, f.e, f.board, f.todo, f.arch, "limit", "Archived", "2026-05-01T00:00:00Z", 205)
	for _, tc := range []struct {
		name  string
		limit int
		rows  int
		more  bool
	}{
		{"default", 0, 50, true},
		{"maximum", 200, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Limit: tc.limit})
			if out.Total != len(ids) || len(out.Items) != tc.rows || (out.NextCursor != "") != tc.more {
				t.Fatalf("total=%d rows=%d next=%q", out.Total, len(out.Items), out.NextCursor)
			}
		})
	}
	first := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Limit: 200})
	last := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Limit: 200, Cursor: first.NextCursor})
	if last.Total != len(ids) || len(last.Items) != 5 || last.NextCursor != "" {
		t.Fatalf("last page total=%d rows=%d next=%q", last.Total, len(last.Items), last.NextCursor)
	}
	for _, limit := range []int{-1, 201} {
		_, err := ops.Typed[ArchiveListOut](f.e, "card.archive.list", ArchiveListIn{BoardID: f.board, Limit: limit})
		wantCode(t, err, "validation")
	}

	removed := archiveViewCard(t, f, f.doing, "removed source", "2026-05-02T00:00:00Z")
	mustExec(t, f.e, `DELETE FROM lists WHERE id=?`, f.doing)
	item := archiveViewItem(t, archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Q: "removed source"}).Items, removed.ID)
	if item.FromListID != f.doing || item.FromListName != "(removed)" {
		t.Fatalf("removed source row = %+v", item)
	}

	// A corrupt/cross-board prev_list_id is treated as removed: the source-name join must require the same board.
	foreign := archiveViewCard(t, f, f.todo, "foreign previous column", "2026-05-03T00:00:00Z")
	mustExec(t, f.e, `UPDATE lists SET name='Foreign secret column' WHERE id=?`, f.list2)
	mustExec(t, f.e, `UPDATE card_placements SET prev_list_id=? WHERE card_id=? AND board_id=?`, f.list2, foreign.ID, f.board)
	foreignOut := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Q: "foreign previous"})
	foreignItem := archiveViewItem(t, foreignOut.Items, foreign.ID)
	if foreignItem.FromListID != f.list2 || foreignItem.FromListName != "" {
		t.Fatalf("cross-board previous column row = %+v", foreignItem)
	}
	foreignJSON, _ := json.Marshal(foreignOut)
	if strings.Contains(string(foreignJSON), "Foreign secret column") {
		t.Fatalf("another board's column name leaked: %s", foreignJSON)
	}
}

func TestArchiveListAuthzAndHiddenPlacements(t *testing.T) {
	m := seedMB(t)
	mustExec(t, m.e, `UPDATE boards SET name='Secret board' WHERE id='b3'`)
	viewerBoth := m.user(t, "u-archive-view", map[string]string{"b1": "viewer", "b2": "viewer"})
	mustExec(t, m.e, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('l-b2-arch','b2','archive',10,NULL,?)`, db.Now())

	shared := m.card(t, m.todo, "shared visible and hidden")
	m.place(t, m.e, shared.ID, m.list2)
	m.place(t, m.e, shared.ID, m.b3Todo)
	call[Card](t, m.e, "card.move", MoveIn{ID: shared.ID, BoardID: m.board, ListID: m.arch})

	notActive := m.card(t, m.todo, "other placements not active")
	m.place(t, m.e, notActive.ID, m.list2)
	m.place(t, m.e, notActive.ID, m.b3Todo)
	call[Card](t, m.e, "card.move", MoveIn{ID: notActive.ID, BoardID: m.board, ListID: m.arch})
	call[Card](t, m.e, "card.move", MoveIn{ID: notActive.ID, BoardID: m.board2, ListID: "l-b2-arch"})

	hiddenHome := m.card(t, m.b3Todo, "hidden home")
	m.place(t, m.e, hiddenHome.ID, m.doing)
	call[Card](t, m.e, "card.move", MoveIn{ID: hiddenHome.ID, BoardID: m.board, ListID: m.arch})

	out := archiveViewList(t, viewerBoth, ArchiveListIn{BoardID: m.board, Limit: 200})
	if out.Total != 3 {
		t.Fatalf("viewer total=%d, want 3", out.Total)
	}
	also := archiveViewItem(t, out.Items, shared.ID).AlsoActiveOn
	if len(also) != 1 || also[0].BoardID != m.board2 || also[0].BoardName != "b2" {
		t.Fatalf("visible alsoActiveOn = %+v, want only b2", also)
	}
	if len(archiveViewItem(t, out.Items, notActive.ID).AlsoActiveOn) != 0 || len(archiveViewItem(t, out.Items, hiddenHome.ID).AlsoActiveOn) != 0 {
		t.Fatal("archived/trashed or hidden placements were named as also active")
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), "Secret board") || strings.Contains(string(encoded), `"boardId":"b3"`) {
		t.Fatalf("hidden board leaked: %s", encoded)
	}
	for _, item := range out.Items {
		if item.CanRestore {
			t.Fatalf("viewer row says canRestore: %+v", item)
		}
	}
	if editor := archiveViewList(t, m.e, ArchiveListIn{BoardID: m.board, Limit: 200}); !editor.Items[0].CanRestore {
		t.Fatal("editor row must say canRestore")
	}

	_, err := ops.Typed[ArchiveListOut](m.stranger, "card.archive.list", ArchiveListIn{BoardID: m.board})
	wantCode(t, err, "not_found")
	_, err = ops.Typed[ArchiveListOut](m.admin, "card.archive.list", ArchiveListIn{BoardID: "b3"})
	wantCode(t, err, "not_found") // system admin alone grants no content access
	_, err = ops.Typed[RestoreBulkOut](m.viewer, "card.restore.bulk", RestoreBulkIn{BoardID: m.board, CardIDs: []string{shared.ID}})
	wantCode(t, err, "forbidden")
	_, err = ops.Typed[DeleteBulkOut](m.viewer, "card.delete.bulk", DeleteBulkIn{BoardID: m.board, CardIDs: []string{shared.ID}})
	wantCode(t, err, "forbidden")
	_, err = ops.Typed[RestoreBulkOut](m.stranger, "card.restore.bulk", RestoreBulkIn{BoardID: m.board, CardIDs: []string{shared.ID}})
	wantCode(t, err, "not_found")
	_, err = ops.Typed[DeleteBulkOut](m.stranger, "card.delete.bulk", DeleteBulkIn{BoardID: m.board, CardIDs: []string{shared.ID}})
	wantCode(t, err, "not_found")
}

func TestArchiveListCanDelete(t *testing.T) {
	m := seedMB(t)
	mustExec(t, m.e, `UPDATE boards SET name='Secret board' WHERE id='b3'`)
	b1Only := m.user(t, "u-archive-delete", map[string]string{"b1": "editor"})

	full := m.card(t, m.todo, "fully trashed")
	m.place(t, m.e, full.ID, m.b3Todo)
	call[Card](t, m.e, "card.archive", ArchiveIn{ID: full.ID, Trash: true})

	visibleActive := m.card(t, m.todo, "another visible board active")
	m.place(t, m.e, visibleActive.ID, m.list2)
	call[Card](t, m.e, "card.move", MoveIn{ID: visibleActive.ID, BoardID: m.board, ListID: m.trash})

	hiddenActive := m.card(t, m.todo, "another hidden board active")
	m.place(t, m.e, hiddenActive.ID, m.b3Todo)
	call[Card](t, m.e, "card.move", MoveIn{ID: hiddenActive.ID, BoardID: m.board, ListID: m.trash})

	archivedElsewhere := m.card(t, m.todo, "another board archived")
	m.place(t, m.e, archivedElsewhere.ID, m.b3Todo)
	call[Card](t, m.e, "card.move", MoveIn{ID: archivedElsewhere.ID, BoardID: m.board, ListID: m.trash})
	call[Card](t, m.e, "card.move", MoveIn{ID: archivedElsewhere.ID, BoardID: "b3", ListID: m.b3Arch})

	out := archiveViewList(t, m.e, ArchiveListIn{BoardID: m.board, Kind: "trash", Limit: 200})
	archiveViewEqualIDs(t, out.Items, full.ID, visibleActive.ID, hiddenActive.ID, archivedElsewhere.ID)
	if !archiveViewItem(t, out.Items, full.ID).CanDelete {
		t.Fatal("card in trash on every placement must say canDelete")
	}
	for _, id := range []string{visibleActive.ID, hiddenActive.ID, archivedElsewhere.ID} {
		if archiveViewItem(t, out.Items, id).CanDelete {
			t.Fatalf("card %s with a non-trash placement says canDelete", id)
		}
	}

	// A b1-only editor cannot see b2/b3, but their placements still participate in the safety check.
	only := archiveViewList(t, b1Only, ArchiveListIn{BoardID: m.board, Kind: "trash", Limit: 200})
	if !archiveViewItem(t, only.Items, full.ID).CanDelete {
		t.Fatal("hidden trash placements must not block canDelete")
	}
	for _, id := range []string{visibleActive.ID, hiddenActive.ID, archivedElsewhere.ID} {
		if archiveViewItem(t, only.Items, id).CanDelete {
			t.Fatalf("hidden non-trash placement did not block canDelete for %s", id)
		}
	}
	for _, item := range only.Items {
		if len(item.AlsoActiveOn) != 0 {
			t.Fatalf("hidden board was named or counted: %+v", item)
		}
	}
	encoded, _ := json.Marshal(only)
	if strings.Contains(string(encoded), "Secret board") || strings.Contains(string(encoded), `"boardId":"b3"`) {
		t.Fatalf("hidden board leaked: %s", encoded)
	}

	viewer := archiveViewList(t, m.viewer, ArchiveListIn{BoardID: m.board, Kind: "trash", Limit: 200})
	for _, item := range viewer.Items {
		if item.CanDelete || item.CanRestore {
			t.Fatalf("viewer row exposes mutation capability: %+v", item)
		}
	}

	archived := archiveViewCard(t, m.fx, m.todo, "archived row", "2026-06-15T00:00:00Z")
	archivedOut := archiveViewList(t, m.e, ArchiveListIn{BoardID: m.board, Limit: 200})
	if archiveViewItem(t, archivedOut.Items, archived.ID).CanDelete {
		t.Fatal("archived-mode row must not say canDelete")
	}
}

func TestArchiveBulkValidation(t *testing.T) {
	f := seed(t)
	a := archiveViewCard(t, f, f.todo, "archived", "2026-06-01T00:00:00Z")
	trashed := f.card(t, f.todo, "trashed")
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashed.ID, Trash: true})
	tooMany := make([]string, 501)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("missing-%03d", i)
	}

	for _, tc := range []struct {
		name string
		in   RestoreBulkIn
		code string
	}{
		{"neither ids nor all", RestoreBulkIn{BoardID: f.board, ConfirmCount: archiveViewInt(0)}, "validation"},
		{"both ids and all", RestoreBulkIn{BoardID: f.board, CardIDs: []string{a.ID}, All: true, ConfirmCount: archiveViewInt(1)}, "validation"},
		{"more than 500 ids", RestoreBulkIn{BoardID: f.board, CardIDs: tooMany, ConfirmCount: archiveViewInt(501)}, "validation"},
		{"me filter with explicit ids", RestoreBulkIn{BoardID: f.board, CardIDs: []string{a.ID}, MemberID: "me"}, "validation"},
		{"none filter with explicit ids", RestoreBulkIn{BoardID: f.board, CardIDs: []string{a.ID}, MemberID: "none"}, "validation"},
		{"all missing confirmation", RestoreBulkIn{BoardID: f.board, All: true}, "validation"},
		{"all mismatched confirmation", RestoreBulkIn{BoardID: f.board, All: true, ConfirmCount: archiveViewInt(999)}, "conflict"},
		{"invalid kind", RestoreBulkIn{BoardID: f.board, All: true, Kind: "everything", ConfirmCount: archiveViewInt(2)}, "validation"},
		{"invalid kind with ids", RestoreBulkIn{BoardID: f.board, CardIDs: []string{a.ID}, Kind: "everything"}, "validation"},
	} {
		t.Run("restore/"+tc.name, func(t *testing.T) {
			_, err := ops.Typed[RestoreBulkOut](f.e, "card.restore.bulk", tc.in)
			wantCode(t, err, tc.code)
		})
	}
	for _, tc := range []struct {
		name string
		in   DeleteBulkIn
		code string
	}{
		{"neither ids nor all", DeleteBulkIn{BoardID: f.board, ConfirmCount: archiveViewInt(0)}, "validation"},
		{"both ids and all", DeleteBulkIn{BoardID: f.board, CardIDs: []string{trashed.ID}, All: true, ConfirmCount: archiveViewInt(1)}, "validation"},
		{"more than 500 ids", DeleteBulkIn{BoardID: f.board, CardIDs: tooMany, ConfirmCount: archiveViewInt(501)}, "validation"},
		{"me filter with explicit ids", DeleteBulkIn{BoardID: f.board, CardIDs: []string{trashed.ID}, MemberID: "me"}, "validation"},
		{"none filter with explicit ids", DeleteBulkIn{BoardID: f.board, CardIDs: []string{trashed.ID}, MemberID: "none"}, "validation"},
		{"all missing confirmation", DeleteBulkIn{BoardID: f.board, All: true}, "validation"},
		{"all mismatched confirmation", DeleteBulkIn{BoardID: f.board, All: true, ConfirmCount: archiveViewInt(2)}, "conflict"},
	} {
		t.Run("delete/"+tc.name, func(t *testing.T) {
			_, err := ops.Typed[DeleteBulkOut](f.e, "card.delete.bulk", tc.in)
			wantCode(t, err, tc.code)
		})
	}
	if got := archiveViewListOf(t, f.e, a.ID, f.board); got != f.arch {
		t.Fatalf("failed confirmations changed the card: %s", got)
	}
	t.Run("restore ids need no confirmation", func(t *testing.T) {
		out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{a.ID}})
		if out.Restored != 1 || out.Skipped != 0 {
			t.Fatalf("restore ids without confirmCount = %+v", out)
		}
	})
	t.Run("delete ids need no confirmation", func(t *testing.T) {
		out := call[DeleteBulkOut](t, f.e, "card.delete.bulk", DeleteBulkIn{BoardID: f.board, CardIDs: []string{trashed.ID}})
		if out.Deleted != 1 || out.Skipped != 0 {
			t.Fatalf("delete ids without confirmCount = %+v", out)
		}
	})
}

func TestArchiveBulkAllMemberAliases(t *testing.T) {
	for _, op := range []string{"restore", "delete"} {
		for _, memberID := range []string{"me", "none"} {
			t.Run(op+"/"+memberID, func(t *testing.T) {
				f := seed(t)
				trash := op == "delete"
				kind := "archived"
				if trash {
					kind = "trash"
				}
				archive := func(name string) Card {
					c := f.card(t, f.todo, name)
					call[Card](t, f.e, "card.archive", ArchiveIn{ID: c.ID, Trash: trash})
					return c
				}
				mine := archive("mine")
				theirs := archive("theirs")
				unassigned := archive("unassigned")
				call[Card](t, f.e, "card.member.add", MemberIn{ID: mine.ID, UserID: f.ed})
				call[Card](t, f.e, "card.member.add", MemberIn{ID: theirs.ID, UserID: f.vw})
				want := mine.ID
				if memberID == "none" {
					want = unassigned.ID
				}

				// The exact count comes from the resolved filter, not from cards belonging to a user
				// literally named "me" or "none".
				if op == "restore" {
					out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{
						BoardID: f.board, All: true, Kind: kind, MemberID: memberID, ConfirmCount: archiveViewInt(1),
					})
					if out.Restored != 1 || out.Skipped != 0 || archiveViewListOf(t, f.e, want, f.board) != f.todo {
						t.Fatalf("restore all memberId=%q = %+v", memberID, out)
					}
				} else {
					out := call[DeleteBulkOut](t, f.e, "card.delete.bulk", DeleteBulkIn{
						BoardID: f.board, All: true, MemberID: memberID, ConfirmCount: archiveViewInt(1),
					})
					if out.Deleted != 1 || out.Skipped != 0 {
						t.Fatalf("delete all memberId=%q = %+v", memberID, out)
					}
					var exists int
					if err := f.e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE id=?`, want).Scan(&exists); err != nil {
						t.Fatal(err)
					}
					if exists != 0 {
						t.Fatalf("matching trashed card %s survived", want)
					}
				}
				remaining := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Kind: kind, Limit: 200})
				wantRemaining := []string{theirs.ID, unassigned.ID}
				if memberID == "none" {
					wantRemaining = []string{mine.ID, theirs.ID}
				}
				archiveViewEqualIDs(t, remaining.Items, wantRemaining...)
			})
		}
	}
}

func TestArchiveRestoreBulkAllBeyond500(t *testing.T) {
	f := seed(t)
	archiveViewBatch(t, f.e, f.board, f.todo, f.arch, "chunk", "Chunked", "2026-06-02T00:00:00Z", 501)
	out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, All: true, ConfirmCount: archiveViewInt(501)})
	if out.Restored != 501 || out.Skipped != 0 {
		t.Fatalf("restore all 501 = %+v", out)
	}
	if got := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Limit: 1}); got.Total != 0 || len(got.Items) != 0 {
		t.Fatalf("archive after restoring 501 = %+v", got)
	}
}

func TestArchiveRestoreBulkByIDsAndAllFilters(t *testing.T) {
	f := seed(t)
	a := archiveViewCard(t, f, f.todo, "selected one", "2026-07-01T00:00:00Z")
	b := archiveViewCard(t, f, f.doing, "selected two", "2026-07-02T00:00:00Z")
	c := archiveViewCard(t, f, f.todo, "selected three", "2026-07-03T00:00:00Z")
	call[Card](t, f.e, "card.label.add", LabelIn{ID: a.ID, LabelID: f.labelA})
	call[Card](t, f.e, "card.label.add", LabelIn{ID: c.ID, LabelID: f.labelB})

	out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{a.ID, c.ID}})
	if out.Restored != 2 || out.Skipped != 0 {
		t.Fatalf("restore by ids = %+v", out)
	}
	if archiveViewListOf(t, f.e, a.ID, f.board) != f.todo || archiveViewListOf(t, f.e, c.ID, f.board) != f.todo || archiveViewListOf(t, f.e, b.ID, f.board) != f.arch {
		t.Fatal("restore by ids touched the wrong cards")
	}

	d := archiveViewCard(t, f, f.todo, "bulk alpha", "2026-07-10T00:00:00Z")
	e := archiveViewCard(t, f, f.doing, "bulk beta", "2026-07-11T00:00:00Z")
	g := archiveViewCard(t, f, f.todo, "other alpha", "2026-07-12T00:00:00Z")
	for _, id := range []string{d.ID, g.ID} {
		call[Card](t, f.e, "card.label.add", LabelIn{ID: id, LabelID: f.labelA})
	}
	call[Card](t, f.e, "card.label.add", LabelIn{ID: e.ID, LabelID: f.labelB})
	call[Card](t, f.e, "card.member.add", MemberIn{ID: d.ID, UserID: f.vw})

	all := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{
		BoardID: f.board, All: true, ConfirmCount: archiveViewInt(1), Q: "bulk", LabelID: f.labelA, MemberID: f.vw,
		FromListID: f.todo, Since: "2026-07-10T00:00:00Z", Until: "2026-07-10T23:59:59Z",
	})
	if all.Restored != 1 || all.Skipped != 0 || archiveViewListOf(t, f.e, d.ID, f.board) != f.todo {
		t.Fatalf("restore all+filters = %+v", all)
	}
	for _, id := range []string{b.ID, e.ID, g.ID} {
		if archiveViewListOf(t, f.e, id, f.board) != f.arch {
			t.Fatalf("nonmatching card %s was restored", id)
		}
	}

	active := f.card(t, f.todo, "still active")
	skipped := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{b.ID, active.ID}})
	if skipped.Restored != 1 || skipped.Skipped != 1 || archiveViewListOf(t, f.e, active.ID, f.board) != f.todo {
		t.Fatalf("non-archived selection = %+v", skipped)
	}
}

func TestArchiveRestoreBulkAllIncludesArchiveAndTrash(t *testing.T) {
	f := seed(t)
	archived := archiveViewCard(t, f, f.todo, "all archived", "2026-07-20T00:00:00Z")
	trashed := f.card(t, f.doing, "all trashed")
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashed.ID, Trash: true})
	out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, All: true, ConfirmCount: archiveViewInt(2)})
	if out.Restored != 2 || out.Skipped != 0 || archiveViewListOf(t, f.e, archived.ID, f.board) != f.todo || archiveViewListOf(t, f.e, trashed.ID, f.board) != f.doing {
		t.Fatalf("restore all archive+trash = %+v", out)
	}
}

func TestArchiveRestoreBulkAllKind(t *testing.T) {
	for _, kind := range []string{"archived", "trash"} {
		t.Run(kind, func(t *testing.T) {
			f := seed(t)
			archived := archiveViewCard(t, f, f.todo, "kind archived", "2026-07-21T00:00:00Z")
			trashed := f.card(t, f.doing, "kind trashed")
			call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashed.ID, Trash: true})

			restored, waiting := archived, trashed
			restoredList, waitingKind := f.todo, "trash"
			if kind == "trash" {
				restored, waiting = trashed, archived
				restoredList, waitingKind = f.doing, "archived"
			}

			// confirmCount is the count for this kind alone, not the combined archive+trash total.
			_, err := ops.Typed[RestoreBulkOut](f.e, "card.restore.bulk", RestoreBulkIn{
				BoardID: f.board, All: true, Kind: kind, ConfirmCount: archiveViewInt(2),
			})
			wantCode(t, err, "conflict")
			if archiveViewListOf(t, f.e, archived.ID, f.board) != f.arch || archiveViewListOf(t, f.e, trashed.ID, f.board) != f.trash {
				t.Fatal("mismatched kind confirmation changed a card")
			}

			out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{
				BoardID: f.board, All: true, Kind: kind, ConfirmCount: archiveViewInt(1),
			})
			if out.Restored != 1 || out.Skipped != 0 {
				t.Fatalf("restore all kind=%s = %+v", kind, out)
			}
			if got := archiveViewListOf(t, f.e, restored.ID, f.board); got != restoredList {
				t.Fatalf("kind=%s restored card to %s, want %s", kind, got, restoredList)
			}
			waitingOut := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Kind: waitingKind})
			archiveViewEqualIDs(t, waitingOut.Items, waiting.ID)
			restoredOut := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Kind: kind})
			if restoredOut.Total != 0 || len(restoredOut.Items) != 0 {
				t.Fatalf("kind=%s still has restored cards: %+v", kind, restoredOut)
			}
		})
	}

	t.Run("explicit ids ignore kind", func(t *testing.T) {
		f := seed(t)
		archived := archiveViewCard(t, f, f.todo, "explicit archived", "2026-07-22T00:00:00Z")
		trashed := f.card(t, f.doing, "explicit trashed")
		call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashed.ID, Trash: true})
		out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{
			BoardID: f.board, CardIDs: []string{archived.ID, trashed.ID}, Kind: "archived",
		})
		if out.Restored != 2 || out.Skipped != 0 {
			t.Fatalf("restore explicit ids with kind = %+v", out)
		}
		if archiveViewListOf(t, f.e, archived.ID, f.board) != f.todo || archiveViewListOf(t, f.e, trashed.ID, f.board) != f.doing {
			t.Fatal("kind filtered an explicit cardIds restore")
		}
	})
}

// archiveKindChangeDriver changes a placement immediately before cardRestoreBulk's transactional candidate query.
// This makes the snapshot-to-write race deterministic without adding a production test hook.
type archiveKindChangeDriver struct {
	driver.Driver
	cardID, boardID, fromListID, toListID string
}

type archiveKindChangeConn struct {
	driver.Conn
	cardID, boardID, fromListID, toListID string
	once                                  sync.Once
	changeErr                             error
}

func (d *archiveKindChangeDriver) Open(name string) (driver.Conn, error) {
	c, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return &archiveKindChangeConn{Conn: c, cardID: d.cardID, boardID: d.boardID, fromListID: d.fromListID, toListID: d.toListID}, nil
}

func (c *archiveKindChangeConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if strings.Contains(query, "SELECT COUNT(*) FROM card_placements p JOIN lists l") &&
		strings.Contains(query, "p.card_id=? AND p.board_id=?") {
		c.once.Do(func() { c.changeErr = c.changeKind() })
		if c.changeErr != nil {
			return nil, c.changeErr
		}
	}
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *archiveKindChangeConn) changeKind() error {
	stmt, err := c.Conn.Prepare(`UPDATE card_placements SET list_id=?,prev_list_id=?,list_changed_at=? WHERE card_id=? AND board_id=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	_, err = stmt.Exec([]driver.Value{c.toListID, c.fromListID, db.Now(), c.cardID, c.boardID})
	return err
}

func TestArchiveRestoreBulkAllKindRechecksCandidate(t *testing.T) {
	for _, kind := range []string{"archived", "trash"} {
		t.Run(kind, func(t *testing.T) {
			f := seed(t)
			c := f.card(t, f.todo, "kind changed after snapshot")
			call[Card](t, f.e, "card.archive", ArchiveIn{ID: c.ID, Trash: kind == "trash"})
			fromList, toList := f.arch, f.trash
			if kind == "trash" {
				fromList, toList = f.trash, f.arch
			}

			var seq int
			var dbName, path string
			if err := f.e.DB.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &path); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", "")
			if err != nil {
				t.Fatal(err)
			}
			rawDriver := raw.Driver()
			raw.Close()
			driverName := "sqlite-archive-kind-change-" + db.NewID()
			sql.Register(driverName, &archiveKindChangeDriver{Driver: rawDriver, cardID: c.ID, boardID: f.board, fromListID: fromList, toListID: toList})
			cdb, err := sql.Open(driverName, "file:"+path+"?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_txlock=immediate")
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()
			cdb.SetMaxOpenConns(1)
			originalDB := f.e.DB
			f.e.DB = &db.DB{DB: cdb}
			defer func() { f.e.DB = originalDB }()

			out, err := ops.Typed[RestoreBulkOut](f.e, "card.restore.bulk", RestoreBulkIn{
				BoardID: f.board, All: true, Kind: kind, ConfirmCount: archiveViewInt(1),
			})
			if err != nil {
				t.Fatal(err)
			}
			if out.Restored != 0 || out.Skipped != 1 {
				t.Fatalf("kind changed after snapshot: %+v, want one skipped card", out)
			}
			if got := archiveViewListOf(t, f.e, c.ID, f.board); got != toList {
				t.Fatalf("kind changed after snapshot: card is in %s, want unchanged %s", got, toList)
			}
		})
	}
}

func TestArchiveDeleteBulkSkipsPartialTrashAndHonorsFilters(t *testing.T) {
	m := seedMB(t)
	full := m.card(t, m.todo, "fully trashed")
	m.place(t, m.e, full.ID, m.b3Todo)
	call[Card](t, m.e, "card.archive", ArchiveIn{ID: full.ID, Trash: true})
	partial := m.card(t, m.todo, "partly trashed")
	m.place(t, m.e, partial.ID, m.b3Todo)
	call[Card](t, m.e, "card.move", MoveIn{ID: partial.ID, BoardID: m.board, ListID: m.trash})

	out := call[DeleteBulkOut](t, m.e, "card.delete.bulk", DeleteBulkIn{BoardID: m.board, CardIDs: []string{full.ID, partial.ID}})
	if out.Deleted != 1 || out.Skipped != 1 {
		t.Fatalf("delete mixed trash = %+v", out)
	}
	var exists int
	_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE id=?`, full.ID).Scan(&exists)
	if exists != 0 {
		t.Fatal("fully trashed card survived")
	}
	partialLists := placementListsOf(t, m, partial.ID)
	if len(partialLists) != 2 || partialLists[m.board] != m.trash || partialLists["b3"] != m.b3Todo {
		t.Fatalf("partial trash placements = %v, want b1 trash and b3 active", partialLists)
	}
	var partialExists int
	_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE id=?`, partial.ID).Scan(&partialExists)
	if partialExists != 1 {
		t.Fatal("partially trashed card was deleted")
	}

	f := seed(t)
	trash := func(from, name, at string) Card {
		c := f.card(t, from, name)
		call[Card](t, f.e, "card.archive", ArchiveIn{ID: c.ID, Trash: true})
		mustExec(t, f.e, `UPDATE card_placements SET list_changed_at=? WHERE card_id=? AND board_id=?`, at, c.ID, f.board)
		return c
	}
	a := trash(f.todo, "bulk remove", "2026-08-01T00:00:00Z")
	b := trash(f.doing, "bulk keep", "2026-08-02T00:00:00Z")
	c := trash(f.todo, "other remove", "2026-08-03T00:00:00Z")
	call[Card](t, f.e, "card.label.add", LabelIn{ID: a.ID, LabelID: f.labelA})
	call[Card](t, f.e, "card.label.add", LabelIn{ID: c.ID, LabelID: f.labelA})
	call[Card](t, f.e, "card.member.add", MemberIn{ID: a.ID, UserID: f.vw})
	filtered := call[DeleteBulkOut](t, f.e, "card.delete.bulk", DeleteBulkIn{
		BoardID: f.board, All: true, ConfirmCount: archiveViewInt(1), Q: "remove", LabelID: f.labelA, MemberID: f.vw,
		FromListID: f.todo, Since: "2026-08-01T00:00:00Z", Until: "2026-08-01",
	})
	if filtered.Deleted != 1 || filtered.Skipped != 0 {
		t.Fatalf("filtered delete = %+v", filtered)
	}
	_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE id=?`, a.ID).Scan(&exists)
	if exists != 0 {
		t.Fatal("matching trashed card survived")
	}
	for _, id := range []string{b.ID, c.ID} {
		_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE id=?`, id).Scan(&exists)
		if exists != 1 {
			t.Fatalf("nonmatching trashed card %s was deleted", id)
		}
	}
}

func TestArchiveRestoreBulkDestinationsAndLandingRules(t *testing.T) {
	t.Run("previous column", func(t *testing.T) {
		f := seed(t)
		c := archiveViewCard(t, f, f.doing, "restore previous", "2026-09-01T00:00:00Z")
		out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{c.ID}})
		if out.Restored != 1 || archiveViewListOf(t, f.e, c.ID, f.board) != f.doing || closedNow(t, f, c.ID) {
			t.Fatalf("restore previous = %+v", out)
		}
	})

	t.Run("removed column falls back", func(t *testing.T) {
		f := seed(t)
		c := archiveViewCard(t, f, f.doing, "restore fallback", "2026-09-02T00:00:00Z")
		mustExec(t, f.e, `DELETE FROM lists WHERE id=?`, f.doing)
		call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{c.ID}})
		if archiveViewListOf(t, f.e, c.ID, f.board) != f.todo {
			t.Fatal("removed previous column did not fall back to the first open column")
		}
	})

	t.Run("first open preferred over closing column", func(t *testing.T) {
		f := seed(t)
		mustExec(t, f.e, `UPDATE lists SET position=0 WHERE id=?`, f.closed)
		c := archiveViewCard(t, f, f.doing, "prefer open", "2026-09-03T00:00:00Z")
		mustExec(t, f.e, `DELETE FROM lists WHERE id=?`, f.doing)
		call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{c.ID}})
		if archiveViewListOf(t, f.e, c.ID, f.board) != f.todo || closedNow(t, f, c.ID) {
			t.Fatal("fallback chose a closing column or closed the card")
		}
	})

	t.Run("closing column landing", func(t *testing.T) {
		f := seed(t)
		c := archiveViewCard(t, f, f.closed, "restore closed", "2026-09-04T00:00:00Z")
		item := archiveViewItem(t, archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board}).Items, c.ID)
		if !item.Closed {
			t.Fatal("archive row lost the closed mark")
		}
		call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{c.ID}})
		if archiveViewListOf(t, f.e, c.ID, f.board) != f.closed || !closedNow(t, f, c.ID) {
			t.Fatal("restore into a closing column did not close the card")
		}
	})

	t.Run("archive after trimming", func(t *testing.T) {
		f := seed(t)
		old := f.card(t, f.doing, "oldest")
		mustExec(t, f.e, `UPDATE card_placements SET list_changed_at='2020-01-01T00:00:00Z' WHERE card_id=? AND board_id=?`, old.ID, f.board)
		victim := archiveViewCard(t, f, f.doing, "restored arrival", "2026-09-05T00:00:00Z")
		mustExec(t, f.e, `UPDATE lists SET archive_after=1 WHERE id=?`, f.doing)
		call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{victim.ID}})
		if archiveViewListOf(t, f.e, victim.ID, f.board) != f.doing || archiveViewListOf(t, f.e, old.ID, f.board) != f.arch {
			t.Fatal("restore landing did not trim the oldest other card")
		}
		var quiet int
		_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE card_id=? AND etype='card.archived' AND data LIKE '%"quiet":true%' AND data LIKE '%"auto":"archiveAfter"%'`, old.ID).Scan(&quiet)
		if quiet != 1 {
			t.Fatalf("auto-archive side effect rows = %d, want 1", quiet)
		}
	})

	t.Run("lands each card sequentially", func(t *testing.T) {
		f := seed(t)
		old1 := f.card(t, f.doing, "old one")
		old2 := f.card(t, f.doing, "old two")
		mustExec(t, f.e, `UPDATE card_placements SET list_changed_at='2020-01-01T00:00:00Z' WHERE card_id=? AND board_id=?`, old1.ID, f.board)
		mustExec(t, f.e, `UPDATE card_placements SET list_changed_at='2020-01-02T00:00:00Z' WHERE card_id=? AND board_id=?`, old2.ID, f.board)
		a := archiveViewCard(t, f, f.doing, "arrival one", "2026-09-06T00:00:00Z")
		b := archiveViewCard(t, f, f.doing, "arrival two", "2026-09-07T00:00:00Z")
		mustExec(t, f.e, `UPDATE lists SET archive_after=1 WHERE id=?`, f.doing)
		mark := events.LastSeq(f.e.Ctx, f.e.DB)
		out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{a.ID, b.ID}})
		if out.Restored != 2 || archiveViewListOf(t, f.e, b.ID, f.board) != f.doing {
			t.Fatalf("sequential restore = %+v", out)
		}
		for _, id := range []string{old1.ID, old2.ID, a.ID} {
			if archiveViewListOf(t, f.e, id, f.board) != f.arch {
				t.Fatalf("card %s was not trimmed by a sequential landing", id)
			}
		}
		var passes, cards int
		rows, err := f.e.DB.Query(`SELECT data FROM actions WHERE seq>? AND etype='list.updated' AND data LIKE '%"autoArchived"%'`, mark)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var data map[string]any
			if err := json.Unmarshal([]byte(raw), &data); err != nil {
				t.Fatal(err)
			}
			passes++
			cards += int(data["autoArchived"].(float64))
		}
		rows.Close()
		if passes != 2 || cards != 3 {
			t.Fatalf("archive-after passes=%d cards=%d, want two sequential passes trimming three cards", passes, cards)
		}
	})

	t.Run("closing landing propagates to other boards", func(t *testing.T) {
		m := seedMB(t)
		c := m.card(t, m.todo, "mixed close restore")
		m.place(t, m.e, c.ID, m.b3Closed) // closes the card everywhere
		call[Card](t, m.e, "card.move", MoveIn{ID: c.ID, BoardID: m.board, ListID: m.doing})
		if closedNow(t, m.fx, c.ID) {
			t.Fatal("fixture should start open while b3 still holds it in a closing column")
		}
		call[Card](t, m.e, "card.archive", ArchiveIn{ID: c.ID})
		mark := events.LastSeq(m.e.Ctx, m.e.DB)
		call[RestoreBulkOut](t, m.e, "card.restore.bulk", RestoreBulkIn{BoardID: m.board, CardIDs: []string{c.ID}})
		if !closedNow(t, m.fx, c.ID) || archiveViewListOf(t, m.e, c.ID, "b3") != m.b3Closed {
			t.Fatal("the closing landing did not win on the other board")
		}
		var propagated int
		_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE seq>? AND card_id=? AND etype='card.updated' AND data LIKE '%"auto":"landing"%' AND data LIKE '%"quiet":true%'`, mark, c.ID).Scan(&propagated)
		if propagated != 2 {
			t.Fatalf("closed-state fan-out rows = %d, want one on each board", propagated)
		}
	})
}

func TestArchiveRestoreBulkPreflightDoesNotPartiallyWrite(t *testing.T) {
	m := seedMB(t)
	good := archiveViewCard(t, m.fx, m.todo, "good archived", "2026-07-30T00:00:00Z")
	bad := m.card(t, m.todo, "bad archived")
	m.place(t, m.e, bad.ID, m.b3Todo)
	call[Card](t, m.e, "card.archive", ArchiveIn{ID: bad.ID})
	mustExec(t, m.e, `DELETE FROM lists WHERE board_id='b3' AND type='active'`)
	_, err := ops.Typed[RestoreBulkOut](m.e, "card.restore.bulk", RestoreBulkIn{BoardID: m.board, CardIDs: []string{good.ID, bad.ID}})
	wantCode(t, err, "validation")
	if got := archiveViewListOf(t, m.e, good.ID, m.board); got != m.arch {
		t.Fatalf("preflight failure partially restored the good card to %s", got)
	}
	if got := archiveViewListOf(t, m.e, bad.ID, m.board); got != m.arch {
		t.Fatalf("preflight failure changed the bad card to %s", got)
	}
}

func TestArchiveRestoreBulkMixedArchiveAndTrashPlacements(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "mixed archive trash")
	m.place(t, m.e, c.ID, m.list2)
	m.place(t, m.e, c.ID, m.b3Todo)
	call[Card](t, m.e, "card.move", MoveIn{ID: c.ID, BoardID: m.board, ListID: m.arch})
	call[Card](t, m.e, "card.move", MoveIn{ID: c.ID, BoardID: m.board2, ListID: archiveViewTrashList(t, m.e, m.board2)})
	out := call[RestoreBulkOut](t, m.e, "card.restore.bulk", RestoreBulkIn{BoardID: m.board, CardIDs: []string{c.ID}})
	if out.Restored != 1 || out.Skipped != 0 {
		t.Fatalf("mixed restore = %+v", out)
	}
	got := placementListsOf(t, m, c.ID)
	if got[m.board] != m.todo || got[m.board2] != m.list2 || got["b3"] != m.b3Todo {
		t.Fatalf("mixed placements after restore = %v", got)
	}
}

func TestArchiveBulkQuietEventsUseRealNotifyPlanner(t *testing.T) {
	m := seedMB(t)
	t.Cleanup(notify.Wait)
	watcher := m.user(t, "u-archive-watch", map[string]string{"b1": "viewer", "b2": "viewer", "b3": "viewer"})
	var hub []realtime.Event
	m.e.Hub.Tap = func(ev realtime.Event) { hub = append(hub, ev) }

	mixed := m.card(t, m.todo, "quiet mixed restore")
	m.place(t, m.e, mixed.ID, m.list2)
	m.place(t, m.e, mixed.ID, m.b3Todo)
	call[Card](t, m.e, "card.move", MoveIn{ID: mixed.ID, BoardID: m.board, ListID: m.arch})
	call[Card](t, m.e, "card.move", MoveIn{ID: mixed.ID, BoardID: m.board2, ListID: archiveViewTrashList(t, m.e, m.board2)})
	solo := archiveViewCard(t, m.fx, m.todo, "quiet solo restore", "2026-10-01T00:00:00Z")

	for _, id := range []string{mixed.ID, solo.ID} {
		call[SubscribeOut](t, watcher, "card.subscribe", IDIn{ID: id})
	}
	call[any](t, watcher, "list.subscribe", boards.ListRefIn{ID: m.todo})
	call[any](t, watcher, "list.subscribe", boards.ListRefIn{ID: m.doing})
	call[any](t, watcher, "board.subscribe", boards.BoardGetIn{ID: m.board})
	call[any](t, watcher, "board.subscribe", boards.BoardGetIn{ID: m.board2})
	call[any](t, watcher, "board.subscribe", boards.BoardGetIn{ID: "b3"})

	// Control: these same watches and the real notify planner do notify for an ordinary move.
	control := m.card(t, m.todo, "notification control")
	call[Card](t, m.e, "card.move", MoveIn{ID: control.ID, ListID: m.doing})
	notify.Wait()
	var controlNotifications int
	_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM notifications WHERE user_id=?`, watcher.User.UserID).Scan(&controlNotifications)
	if controlNotifications == 0 {
		t.Fatal("control move did not notify the watcher; the quiet assertion would be vacuous")
	}
	mustExec(t, m.e, `DELETE FROM notifications`)
	hub = nil

	mark := events.LastSeq(m.e.Ctx, m.e.DB)
	restored := call[RestoreBulkOut](t, m.e, "card.restore.bulk", RestoreBulkIn{BoardID: m.board, CardIDs: []string{mixed.ID, solo.ID}})
	if restored.Restored != 2 || restored.Skipped != 0 {
		t.Fatalf("quiet restore = %+v", restored)
	}
	notify.Wait()
	assertArchiveViewBulkEvents(t, m.e, mark, "bulkRestore", "card.restored", 4, 2)
	assertArchiveViewNoNotifications(t, m.e, watcher.User.UserID)
	if !archiveViewHubQuiet(hub, "card.moved", "bulkRestore") {
		t.Fatal("quiet restore was not published to the realtime hub")
	}

	delSingle := m.card(t, m.todo, "quiet delete single")
	call[Card](t, m.e, "card.archive", ArchiveIn{ID: delSingle.ID, Trash: true})
	delMulti := m.card(t, m.todo, "quiet delete multi")
	m.place(t, m.e, delMulti.ID, m.list2)
	call[Card](t, m.e, "card.archive", ArchiveIn{ID: delMulti.ID, Trash: true})
	for _, id := range []string{delSingle.ID, delMulti.ID} {
		call[SubscribeOut](t, watcher, "card.subscribe", IDIn{ID: id})
	}
	mustExec(t, m.e, `DELETE FROM notifications`)
	hub = nil
	mark = events.LastSeq(m.e.Ctx, m.e.DB)
	deleted := call[DeleteBulkOut](t, m.e, "card.delete.bulk", DeleteBulkIn{BoardID: m.board, CardIDs: []string{delSingle.ID, delMulti.ID}})
	if deleted.Deleted != 2 || deleted.Skipped != 0 {
		t.Fatalf("quiet delete = %+v", deleted)
	}
	notify.Wait()
	assertArchiveViewBulkEvents(t, m.e, mark, "bulkDelete", "card.deleted", 3, 2)
	assertArchiveViewNoNotifications(t, m.e, watcher.User.UserID)
	if !archiveViewHubQuiet(hub, "card.deleted", "bulkDelete") {
		t.Fatal("quiet delete was not published to the realtime hub")
	}
}

type archiveViewBulkEventCounts struct {
	cardRows, summaries int
	origins             map[string]bool
}

func assertArchiveViewBulkEvents(t *testing.T, e *ops.Env, since int64, auto, etype string, wantCards, wantEvents int) {
	t.Helper()
	var counts archiveViewBulkEventCounts
	counts.origins = map[string]bool{}
	rows, err := e.DB.Query(`SELECT COALESCE(card_id,''),board_id,COALESCE(origin_id,id),etype,data FROM actions WHERE seq>? ORDER BY seq`, since)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cardID, boardID, origin, gotType, raw string
		if err := rows.Scan(&cardID, &boardID, &origin, &gotType, &raw); err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			t.Fatal(err)
		}
		if data["auto"] != auto {
			continue
		}
		if data["quiet"] != true {
			t.Errorf("%s event is not quiet: %s", auto, raw)
		}
		if cardID == "" {
			counts.summaries++
			if boardID != "b1" {
				t.Errorf("%s summary recorded on %s, want b1", auto, boardID)
			}
			if data["id"] != "b1" || data["boardId"] != "b1" || data["cardId"] != "" {
				t.Errorf("%s summary identity = %v, want board b1 and empty cardId", auto, data)
			}
			continue
		}
		counts.cardRows++
		counts.origins[origin] = true
		if gotType != etype {
			t.Errorf("%s card event type = %s, want %s", auto, gotType, etype)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if counts.cardRows != wantCards || counts.summaries != 1 || len(counts.origins) != wantEvents {
		t.Fatalf("%s events: card rows=%d (want %d), summaries=%d (want 1), logical events=%d (want %d)",
			auto, counts.cardRows, wantCards, counts.summaries, len(counts.origins), wantEvents)
	}
	for origin := range counts.origins {
		var originBoard string
		if err := e.DB.QueryRow(`SELECT board_id FROM actions WHERE id=?`, origin).Scan(&originBoard); err != nil {
			t.Fatalf("%s origin %s: %v", auto, origin, err)
		}
		if originBoard != "b1" {
			t.Errorf("%s event origin is on %s, want acting board b1", auto, originBoard)
		}
	}
}

func assertArchiveViewNoNotifications(t *testing.T, e *ops.Env, userID string) {
	t.Helper()
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM notifications WHERE user_id=?`, userID).Scan(&n)
	if n != 0 {
		t.Fatalf("quiet bulk operation created %d watcher notifications", n)
	}
}

func archiveViewHubQuiet(hub []realtime.Event, name, auto string) bool {
	for _, ev := range hub {
		if ev.Name != name {
			continue
		}
		if data, ok := ev.Data.(map[string]any); ok && data["quiet"] == true && data["auto"] == auto {
			return true
		}
	}
	return false
}

// bulkSummaryRows counts the board activity summaries a bulk op wrote for one auto value after seq mark.
func bulkSummaryRows(t *testing.T, e *ops.Env, mark int64, auto string) int {
	t.Helper()
	var n int
	if err := e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE seq>? AND etype='board.updated' AND data LIKE ?`,
		mark, `%"auto":"`+auto+`"%`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestArchiveBulkSummaryOnlyWhenSomethingChanged(t *testing.T) {
	mark := func(t *testing.T, e *ops.Env) int64 {
		t.Helper()
		var seq int64
		if err := e.DB.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM actions`).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		return seq
	}

	t.Run("restore all with nothing matching writes no summary", func(t *testing.T) {
		f := seed(t)
		seq := mark(t, f.e)
		out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, All: true, ConfirmCount: archiveViewInt(0)})
		if out.Restored != 0 || out.Skipped != 0 {
			t.Fatalf("restore empty all = %+v", out)
		}
		if n := bulkSummaryRows(t, f.e, seq, "bulkRestore"); n != 0 {
			t.Fatalf("summary rows for zero restored = %d", n)
		}
	})
	t.Run("restore of only skipped ids writes no summary", func(t *testing.T) {
		f := seed(t)
		active := f.card(t, f.todo, "already active")
		seq := mark(t, f.e)
		out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{active.ID}})
		if out.Restored != 0 || out.Skipped != 1 {
			t.Fatalf("restore skipped ids = %+v", out)
		}
		if n := bulkSummaryRows(t, f.e, seq, "bulkRestore"); n != 0 {
			t.Fatalf("summary rows for all-skipped restore = %d", n)
		}
	})
	t.Run("restore with a change writes one summary", func(t *testing.T) {
		f := seed(t)
		a := archiveViewCard(t, f, f.todo, "restorable", "2026-06-01T00:00:00Z")
		seq := mark(t, f.e)
		out := call[RestoreBulkOut](t, f.e, "card.restore.bulk", RestoreBulkIn{BoardID: f.board, CardIDs: []string{a.ID}})
		if out.Restored != 1 {
			t.Fatalf("restore = %+v", out)
		}
		if n := bulkSummaryRows(t, f.e, seq, "bulkRestore"); n != 1 {
			t.Fatalf("summary rows for one restored = %d, want 1", n)
		}
	})
	t.Run("delete with nothing matching writes no summary", func(t *testing.T) {
		f := seed(t)
		seq := mark(t, f.e)
		out := call[DeleteBulkOut](t, f.e, "card.delete.bulk", DeleteBulkIn{BoardID: f.board, All: true, ConfirmCount: archiveViewInt(0)})
		if out.Deleted != 0 || out.Skipped != 0 {
			t.Fatalf("delete empty all = %+v", out)
		}
		if n := bulkSummaryRows(t, f.e, seq, "bulkDelete"); n != 0 {
			t.Fatalf("summary rows for zero deleted = %d", n)
		}
	})
	t.Run("delete of only skipped ids writes no summary", func(t *testing.T) {
		f := seed(t)
		active := f.card(t, f.todo, "not trashed")
		seq := mark(t, f.e)
		out := call[DeleteBulkOut](t, f.e, "card.delete.bulk", DeleteBulkIn{BoardID: f.board, CardIDs: []string{active.ID}})
		if out.Deleted != 0 || out.Skipped != 1 {
			t.Fatalf("delete skipped ids = %+v", out)
		}
		if n := bulkSummaryRows(t, f.e, seq, "bulkDelete"); n != 0 {
			t.Fatalf("summary rows for all-skipped delete = %d", n)
		}
	})
	t.Run("delete with a change writes one summary", func(t *testing.T) {
		f := seed(t)
		trashed := f.card(t, f.todo, "to delete")
		call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashed.ID, Trash: true})
		seq := mark(t, f.e)
		out := call[DeleteBulkOut](t, f.e, "card.delete.bulk", DeleteBulkIn{BoardID: f.board, CardIDs: []string{trashed.ID}})
		if out.Deleted != 1 {
			t.Fatalf("delete = %+v", out)
		}
		if n := bulkSummaryRows(t, f.e, seq, "bulkDelete"); n != 1 {
			t.Fatalf("summary rows for one deleted = %d, want 1", n)
		}
	})
}
