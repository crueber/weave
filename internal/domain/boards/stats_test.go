package boards_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// statCard inserts a card with the columns the statistics read.
type statCard struct {
	list              string
	closed, draft     bool
	template          bool
	due               string
	dueDone           bool
	claimedBy, claimU string
}

func insertStatCard(t *testing.T, e *ops.Env, boardID string, c statCard) {
	t.Helper()
	var due, cb, cu any
	if c.due != "" {
		due = c.due
	}
	if c.claimedBy != "" {
		cb, cu = c.claimedBy, c.claimU
	}
	if _, err := e.DB.Exec(`INSERT INTO cards(id,board_id,list_id,position,name,due_date,is_due_completed,is_closed,is_draft,is_template,claimed_by,claimed_until,list_changed_at,created_at)
		VALUES(?,?,?,1,'secret card name',?,?,?,?,?,?,?,?,?)`, db.NewID(), boardID, c.list, due, c.dueDone, c.closed, c.draft, c.template, cb, cu, db.Now(), db.Now()); err != nil {
		t.Fatal(err)
	}
}

func rfc(d time.Duration) string { return time.Now().UTC().Add(d).Format(time.RFC3339) }

func systemList(t *testing.T, e *ops.Env, boardID, typ string) string {
	id, err := boards.SystemList(context.Background(), e.DB, boardID, typ)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStatsTable(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Weave")
	backlog := addList(t, e, b.ID, "Backlog")
	doing := addList(t, e, b.ID, "Doing")
	done := call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Done", Type: "closed"})
	cards := []statCard{
		{list: backlog.ID}, // open
		{list: backlog.ID, due: rfc(-48 * time.Hour)},                       // open, overdue
		{list: backlog.ID, due: rfc(-48 * time.Hour), dueDone: true},        // due marked complete: not overdue
		{list: backlog.ID, due: rfc(72 * time.Hour)},                        // due this week
		{list: backlog.ID, due: rfc(10 * 24 * time.Hour)},                   // due later: neither
		{list: doing.ID, claimedBy: editor.UserID, claimU: rfc(time.Hour)},  // live claim
		{list: doing.ID, claimedBy: editor.UserID, claimU: rfc(-time.Hour)}, // expired claim
		{list: doing.ID, closed: true, due: rfc(-time.Hour)},                // closed by hand (done; overdue ignored)
		{list: done.ID, closed: true},
		{list: done.ID, closed: true},
		{list: backlog.ID, draft: true, due: rfc(-time.Hour)}, // drafts (Inbox cards) never count
		{list: backlog.ID, template: true},                    // card templates never count
		{list: systemList(t, e, b.ID, "archive")},             // archived
		{list: systemList(t, e, b.ID, "trash")},               // trashed
	}
	for _, c := range cards {
		insertStatCard(t, e, b.ID, c)
	}
	// an Inbox board: the inbox holds drafts, which are excluded like any draft
	yes := true
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, HasInbox: &yes})
	empty := newBoard(t, e, "Empty")
	addList(t, e, empty.ID, "Todo")
	// members: the creator (admin) is a member; add the editor directly and an agent (agents are not counted)
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID, Role: "editor"})
	if _, err := e.DB.Exec(`INSERT INTO users(id,email,name,role,is_agent,created_at) VALUES('u-bot','bot@agents.invalid','Bot','boardUser',1,'2024-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: "u-bot", Role: "editor"})
	// activity: one 3 days ago, two in the past day (plus whatever the ops above recorded today)
	for _, at := range []string{rfc(-73 * time.Hour), rfc(-2 * time.Hour), rfc(-time.Hour), rfc(-30 * 24 * time.Hour)} {
		if _, err := e.DB.Exec(`INSERT INTO actions(id,board_id,type,created_at) VALUES(?,?,'x',?)`, db.NewID(), b.ID, at); err != nil {
			t.Fatal(err)
		}
	}

	st, err := boards.Stats(e, []string{b.ID, empty.ID, "nope", b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 {
		t.Fatalf("want 2 boards (unknown and duplicate ids dropped), got %d", len(st))
	}
	s := st[b.ID]
	if s.Open != 7 || s.Done != 3 || s.Overdue != 1 || s.DueSoon != 1 || s.Claimed != 1 {
		t.Fatalf("counts: open=%d done=%d overdue=%d dueSoon=%d claimed=%d", s.Open, s.Done, s.Overdue, s.DueSoon, s.Claimed)
	}
	if len(s.PerColumn) != 3 || s.PerColumn[0].Name != "Backlog" || s.PerColumn[0].Cards != 5 || s.PerColumn[1].Cards != 3 || s.PerColumn[1].Open != 2 ||
		s.PerColumn[2].Type != "closed" || s.PerColumn[2].Cards != 2 {
		t.Fatalf("per column: %+v", s.PerColumn)
	}
	if s.Members != 2 || len(s.MemberSample) != 2 || s.MemberSample[0].Name != "Ada Admin" {
		t.Fatalf("members (people only): %d %+v", s.Members, s.MemberSample)
	}
	if len(s.Activity7d) != 7 || s.Activity7d[3] != 1 || s.Activity7d[6] < 2 || s.LastActivityAt == "" {
		t.Fatalf("activity: %v last=%q", s.Activity7d, s.LastActivityAt)
	}
	sum := 0
	for _, n := range s.Activity7d {
		sum += n
	}
	if sum < 3 || s.Activity7d[0] != 0 {
		t.Fatalf("the 30-day-old action is outside the window: %v", s.Activity7d)
	}
	z := st[empty.ID]
	if z.Cards() != 0 || z.Overdue != 0 || len(z.PerColumn) != 1 || z.PerColumn[0].Cards != 0 {
		t.Fatalf("empty board: %+v", z)
	}
	// boards the caller cannot open are absent; a viewer sees only its own board
	if st, _ := boards.Stats(testutil.As(e, nobody), []string{b.ID, empty.ID}); len(st) != 0 {
		t.Fatalf("outsider must get nothing, got %v", st)
	}
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: empty.ID, UserID: viewer.UserID, Role: "viewer"})
	if st, _ := boards.Stats(testutil.As(e, viewer), []string{b.ID, empty.ID}); len(st) != 1 || st[empty.ID].BoardID != empty.ID {
		t.Fatalf("viewer: %v", st)
	}
}

// BoardLevels must agree with BoardLevel for every caller kind.
func TestBoardLevelsMatchBoardLevel(t *testing.T) {
	e := fixture(t)
	a, b, c := newBoard(t, e, "A"), newBoard(t, e, "B"), newBoard(t, e, "C")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: a.ID, UserID: viewer.UserID, Role: "viewer"})
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID, Role: "editor"})
	call[boards.Board](t, e, "board.setPublic", boards.BoardPublicIn{ID: c.ID, IsPublic: true})
	ids := []string{a.ID, b.ID, c.ID, "missing"}
	for _, p := range []*ops.Principal{admin, editor, viewer, nobody, nil} {
		pe := testutil.As(e, p)
		got := authz.BoardLevels(pe, ids)
		for _, id := range ids {
			if want := authz.BoardLevel(pe, id); got[id] != want {
				t.Errorf("%v on %s: BoardLevels=%d BoardLevel=%d", p, id, got[id], want)
			}
		}
	}
}

// countingDriver wraps the sqlite driver and counts prepared statements (database/sql prepares every query
// through a driver.Conn that implements only the basic interface).
type countingDriver struct {
	inner driver.Driver
	n     *atomic.Int64
}
type countingConn struct {
	driver.Conn
	n *atomic.Int64
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	return countingConn{c, d.n}, err
}
func (c countingConn) Prepare(q string) (driver.Stmt, error) { c.n.Add(1); return c.Conn.Prepare(q) }

var queries atomic.Int64

func init() {
	raw, _ := sql.Open("sqlite", ":memory:")
	sql.Register("sqlite-counting", countingDriver{raw.Driver(), &queries})
	raw.Close()
}

// The statistics cost a constant number of queries whatever the number of boards.
func TestStatsQueryCountIsConstant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	d, err := db.Open(path) // migrations
	if err != nil {
		t.Fatal(err)
	}
	e := fixture(t)
	e.DB = d
	for _, u := range []*ops.Principal{admin, editor} {
		_, _ = e.DB.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,?,'2024-01-01T00:00:00Z')`, u.UserID, u.UserID+"@x.io", u.Name, u.Role)
	}
	_, _ = e.DB.Exec(`INSERT INTO projects(id,name,created_at) VALUES('p1','Proj','2024-01-01T00:00:00Z')`)
	var ids []string
	for i := 0; i < 12; i++ {
		b := newBoard(t, e, "B")
		l := addList(t, e, b.ID, "L")
		insertStatCard(t, e, b.ID, statCard{list: l.ID})
		call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID, Role: "editor"})
		ids = append(ids, b.ID)
	}
	cdb, err := sql.Open("sqlite-counting", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	for _, p := range []*ops.Principal{admin, editor} {
		ce := testutil.As(e, p)
		ce.DB = &db.DB{DB: cdb}
		count := func(ids []string) int64 {
			queries.Store(0)
			st, err := boards.Stats(ce, ids)
			if err != nil || len(st) != len(ids) {
				t.Fatalf("stats: %v (%d of %d)", err, len(st), len(ids))
			}
			return queries.Load()
		}
		one, many := count(ids[:1]), count(ids)
		if one != many || many > 8 || many < 4 {
			t.Fatalf("%s: 1 board = %d queries, 12 boards = %d (want equal and small)", p.Role, one, many)
		}
	}
}

func TestBoardStatsOpOrderAndAccess(t *testing.T) {
	e := fixture(t)
	old, recent, quiet1, quiet2 := newBoard(t, e, "Old"), newBoard(t, e, "Recent"), newBoard(t, e, "Quiet 1"), newBoard(t, e, "Quiet 2")
	set := func(b boards.Board, at string) {
		if _, err := e.DB.Exec(`UPDATE actions SET created_at=? WHERE board_id=?`, at, b.ID); err != nil {
			t.Fatal(err)
		}
	}
	set(old, rfc(-48*time.Hour))
	set(recent, rfc(-time.Hour))
	for _, b := range []boards.Board{quiet1, quiet2} {
		if _, err := e.DB.Exec(`DELETE FROM actions WHERE board_id=?`, b.ID); err != nil {
			t.Fatal(err)
		}
	}
	// a template board is never listed
	tpl := newBoard(t, e, "Tpl")
	_, _ = e.DB.Exec(`UPDATE boards SET is_template=1 WHERE id=?`, tpl.ID)
	names := func(st []boards.BoardStats) (out []string) {
		for _, s := range st {
			out = append(out, s.Name)
		}
		return
	}
	got := names(call[[]boards.BoardStats](t, e, "board.stats", boards.StatsIn{ProjectID: "p1"}))
	want := []string{"Recent", "Old", "Quiet 1", "Quiet 2"} // no activity: board position order after the active ones
	if len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("order %v, want %v", got, want)
	}
	// ties on time fall back to position: move Quiet 2 before Quiet 1, give Old and Recent the same time
	call[boards.Board](t, e, "board.move", boards.BoardMoveIn{ID: quiet2.ID, BeforeID: quiet1.ID})
	_, _ = e.DB.Exec(`DELETE FROM actions WHERE board_id=?`, quiet2.ID) // the move itself is activity; keep it quiet
	set(old, rfc(-time.Hour))
	set(recent, rfc(-time.Hour))
	if _, err := e.DB.Exec(`UPDATE actions SET created_at=(SELECT MAX(created_at) FROM actions WHERE board_id=?) WHERE board_id=?`, recent.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	got = names(call[[]boards.BoardStats](t, e, "board.stats", boards.StatsIn{ProjectID: "p1"}))
	want = []string{"Old", "Recent", "Quiet 2", "Quiet 1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tie order %v, want %v", got, want)
		}
	}
	// one board; access
	one := call[[]boards.BoardStats](t, e, "board.stats", boards.StatsIn{ProjectID: "p1", BoardID: old.ID})
	if len(one) != 1 || one[0].BoardID != old.ID {
		t.Fatalf("boardId filter: %+v", one)
	}
	mustFail(t, testutil.As(e, nobody), "board.stats", boards.StatsIn{ProjectID: "p1"}, "not_found")
	mustFail(t, e, "board.stats", boards.StatsIn{ProjectID: "nope"}, "not_found")
	mustFail(t, e, "board.stats", boards.StatsIn{ProjectID: "p2", BoardID: old.ID}, "not_found")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: quiet1.ID, UserID: viewer.UserID, Role: "viewer"})
	vs := call[[]boards.BoardStats](t, testutil.As(e, viewer), "board.stats", boards.StatsIn{ProjectID: "p1"})
	if len(vs) != 1 || vs[0].BoardID != quiet1.ID {
		t.Fatalf("viewer sees only its board: %+v", vs)
	}
	mustFail(t, testutil.As(e, viewer), "board.stats", boards.StatsIn{ProjectID: "p1", BoardID: old.ID}, "not_found")
}
