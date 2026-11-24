package boards

import (
	"sort"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
)

// BoardStats is the at-a-glance state of one board: what the project page tiles show and what board.stats returns.
// Counts only; never card names. Cards are counted the way the board view shows them: lists of type active and
// closed, no drafts (Inbox cards are drafts), no card templates, nothing in archive or trash.
type BoardStats struct {
	BoardID        string       `json:"boardId"`
	Name           string       `json:"name"`
	Open           int          `json:"open" desc:"cards not closed"`
	Done           int          `json:"done" desc:"closed cards (moved into a closed column or closed by hand)"`
	Overdue        int          `json:"overdue" desc:"open cards past their due date and not marked complete"`
	DueSoon        int          `json:"dueSoon" desc:"open cards due within 7 days, not marked complete"`
	Claimed        int          `json:"claimed" desc:"cards with a live claim (lease not expired)"`
	Listening      int          `json:"listening" desc:"listeners on the board's columns right now (open /events streams with lists=); 0 after a restart until they reconnect"`
	Members        int          `json:"members" desc:"people with a board membership, direct or via a group (agents not counted)"`
	MemberSample   []StatMember `json:"memberSample" desc:"up to 3 of those members, by name"`
	LastActivityAt string       `json:"lastActivityAt,omitempty" desc:"time of the newest activity-log entry (RFC3339); absent when there is none"`
	Activity7d     []int        `json:"activity7d" desc:"activity-log entries per 24 hours over the last 7 days, oldest first (last = past 24 hours)"`
	PerColumn      []ColumnStat `json:"perColumn" desc:"every active and closed column in board order with its card count"`
	position       float64
}

// ColumnStat is one active or closed column of a board, in board order.
type ColumnStat struct {
	ListID string `json:"listId"`
	Name   string `json:"name"`
	Type   string `json:"type" desc:"active | closed"`
	Cards  int    `json:"cards" desc:"cards in the column (open and closed)"`
	Open   int    `json:"open"`
}

// StatMember is a board member shown on a tile (id for the avatar, name for its tooltip).
type StatMember struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Cards returns the number of counted cards (open + done).
func (s BoardStats) Cards() int { return s.Open + s.Done }

// statsWindow is the look-ahead for dueSoon and the look-back of Activity7d.
const statsWindow = 7 * 24 * time.Hour

// Stats computes BoardStats for the given boards in a constant number of queries, whatever the number of boards
// (access via authz.BoardLevels, one boards+last-activity query, one grouped lists x cards query, one bounded
// 7-day activity query on the actions(board_id,created_at) index, one members query; listeners are in memory).
// Boards the caller cannot view, unknown ids and duplicates are simply absent from the result.
//
// Rules: open = is_closed=0, done = is_closed=1 (moved into a closed column or closed by hand); overdue and
// dueSoon (due within 7 days) count open cards whose due date is not marked complete; claimed = a live lease
// (claimed_until >= now); listening = distinct listeners on the board's columns right now; members = active,
// non-agent people with a direct or group membership; Activity7d = activity-log rows (any recorded action) per
// rolling 24-hour window, oldest first, the last entry being the past 24 hours.
func Stats(e *ops.Env, boardIDs []string) (map[string]BoardStats, error) {
	out := map[string]BoardStats{}
	lv := authz.BoardLevels(e, boardIDs)
	var ids []string
	seen := map[string]bool{}
	for _, id := range boardIDs {
		if !seen[id] && lv[id] >= authz.View {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	in := "?" + strings.Repeat(",?", len(ids)-1)
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	with := func(pre ...any) []any { return append(append([]any{}, pre...), args...) }
	now := time.Now().UTC()
	nowS := now.Format(time.RFC3339)

	// 1. the boards, with the newest activity (MAX on the actions_board index: one index probe per board).
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT b.id,b.name,b.position,COALESCE((SELECT MAX(a.created_at) FROM actions a WHERE a.board_id=b.id),'')
		FROM boards b WHERE b.id IN (`+in+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		s := BoardStats{MemberSample: []StatMember{}, Activity7d: make([]int, 7), PerColumn: []ColumnStat{}}
		if err := rows.Scan(&s.BoardID, &s.Name, &s.position, &s.LastActivityAt); err != nil {
			rows.Close()
			return nil, err
		}
		out[s.BoardID] = s
	}
	rows.Close()

	// 2. every active/closed column with its card counts (one grouped pass over all the boards).
	rows, err = e.DB.QueryContext(e.Ctx, `SELECT l.board_id,l.id,COALESCE(l.name,''),l.type,
		COUNT(c.id),
		COALESCE(SUM(c.is_closed=0),0),
		COALESCE(SUM(c.is_closed=0 AND COALESCE(c.due_date,'')<>'' AND c.due_date<? AND COALESCE(c.is_due_completed,0)=0),0),
		COALESCE(SUM(c.is_closed=0 AND c.due_date>=? AND c.due_date<? AND COALESCE(c.is_due_completed,0)=0),0),
		COALESCE(SUM(c.claimed_by IS NOT NULL AND c.claimed_until>=?),0)
		FROM lists l LEFT JOIN card_placements p ON p.list_id=l.id LEFT JOIN cards c ON c.id=p.card_id AND c.is_draft=0 AND c.is_template=0
		WHERE l.board_id IN (`+in+`) AND l.type IN ('active','closed')
		GROUP BY l.id ORDER BY l.board_id, l.position, l.id`, with(nowS, nowS, now.Add(statsWindow).Format(time.RFC3339), nowS)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var bid string
		var c ColumnStat
		var overdue, soon, claimed int
		if err := rows.Scan(&bid, &c.ListID, &c.Name, &c.Type, &c.Cards, &c.Open, &overdue, &soon, &claimed); err != nil {
			rows.Close()
			return nil, err
		}
		s, ok := out[bid]
		if !ok {
			continue
		}
		s.PerColumn = append(s.PerColumn, c)
		s.Open += c.Open
		s.Done += c.Cards - c.Open
		s.Overdue += overdue
		s.DueSoon += soon
		s.Claimed += claimed
		out[bid] = s
	}
	rows.Close()

	// 3. activity per rolling 24-hour window over the last 7 days (range scan on actions(board_id,created_at)).
	rows, err = e.DB.QueryContext(e.Ctx, `SELECT board_id, CAST(julianday(?)-julianday(created_at) AS INTEGER), COUNT(*)
		FROM actions WHERE board_id IN (`+in+`) AND created_at>=? AND created_at<=? GROUP BY 1,2`,
		append(with(nowS), now.Add(-statsWindow).Format(time.RFC3339), nowS)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var bid string
		var ago, n int
		if err := rows.Scan(&bid, &ago, &n); err != nil {
			rows.Close()
			return nil, err
		}
		if s, ok := out[bid]; ok {
			ago = min(max(ago, 0), 6)
			s.Activity7d[6-ago] += n
		}
	}
	rows.Close()

	// 4. members: direct and through user groups, people only (agents show up as claimed/listening instead).
	person := `u.is_agent=0 AND u.is_deactivated=0 AND u.deleted_at IS NULL`
	rows, err = e.DB.QueryContext(e.Ctx, `SELECT m.board_id,u.id,u.name FROM board_memberships m JOIN users u ON u.id=m.user_id
		WHERE m.board_id IN (`+in+`) AND `+person+`
		UNION SELECT m.board_id,u.id,u.name FROM board_memberships m JOIN user_group_members g ON g.group_id=m.group_id JOIN users u ON u.id=g.user_id
		WHERE m.board_id IN (`+in+`) AND `+person+`
		ORDER BY 1, 3 COLLATE NOCASE, 2`, append(with(), args...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var bid string
		var m StatMember
		if err := rows.Scan(&bid, &m.ID, &m.Name); err != nil {
			rows.Close()
			return nil, err
		}
		if s, ok := out[bid]; ok {
			s.Members++
			if len(s.MemberSample) < 3 {
				s.MemberSample = append(s.MemberSample, m)
			}
			out[bid] = s
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// 5. listeners (in memory, not stored: 0 after a restart until agents reconnect).
	if e.Hub != nil {
		for id, s := range out {
			s.Listening = len(e.Hub.Listeners.ForBoard(id))
			out[id] = s
		}
	}
	return out, nil
}

// ActivityLess orders boards most recently active first; boards without any activity come after all the others in
// their board position order; ties fall back to position, then id, so the order is stable.
func ActivityLess(a, b BoardStats) bool {
	if (a.LastActivityAt != "") != (b.LastActivityAt != "") {
		return a.LastActivityAt != ""
	}
	if a.LastActivityAt != b.LastActivityAt {
		return a.LastActivityAt > b.LastActivityAt
	}
	if a.position != b.position {
		return a.position < b.position
	}
	return a.BoardID < b.BoardID
}

// StatsIn is board.stats's input.
type StatsIn struct {
	ProjectID string `path:"projectId" desc:"project id"`
	BoardID   string `query:"boardId" desc:"only this board of the project (default: every board of the project you can open)"`
}

func init() {
	ops.Register(ops.Op{Name: "board.stats", Summary: "At-a-glance numbers for the boards of a project (or one board), most recently active first, as on the project page tiles. Cards count as on the board view (no drafts or Inbox cards, templates, archive or trash). Counts only, never card names; boards you cannot open are left out",
		Method: "GET", Path: "/projects/{projectId}/boards/stats",
		UI: "GET /projects/{id}"}, boardStats)
}

func boardStats(e *ops.Env, in StatsIn) ([]BoardStats, error) {
	var n int
	if err := e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM projects WHERE id=?`, in.ProjectID).Scan(&n); err != nil || n == 0 {
		return nil, ops.NotFound("project")
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT id FROM boards WHERE project_id=? AND is_template=0 ORDER BY position, id`, in.ProjectID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		if in.BoardID == "" || id == in.BoardID {
			ids = append(ids, id)
		}
	}
	rows.Close()
	st, err := Stats(e, ids)
	if err != nil {
		return nil, err
	}
	if len(st) == 0 {
		if in.BoardID != "" {
			return nil, ops.NotFound("board")
		}
		if authz.ProjectLevel(e, in.ProjectID) == authz.None {
			return nil, ops.NotFound("project")
		}
	}
	out := make([]BoardStats, 0, len(st))
	for _, id := range ids {
		if s, ok := st[id]; ok {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return ActivityLess(out[i], out[j]) })
	return out, nil
}
