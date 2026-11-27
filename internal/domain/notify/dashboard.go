package notify

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// DashboardPanels are the available dashboard panels, in default order.
var DashboardPanels = []struct{ ID, Title string }{
	{"myCards", "My cards"},
	{"dueSoon", "Due soon (next 7 days)"},
	{"overdue", "Overdue"},
	{"recentActivity", "Recent activity"},
}

type PanelCfg struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Enabled bool   `json:"enabled"`
}
type DashCard struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BoardID   string `json:"boardId"`
	BoardName string `json:"boardName"`
	Project   string `json:"projectName"`
	List      string `json:"listName"`
	DueDate   string `json:"dueDate,omitempty"`
	Completed bool   `json:"isDueCompleted"`
}
type DashActivity struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	User      string `json:"userName"`
	CardID    string `json:"cardId,omitempty"`
	CardName  string `json:"cardName,omitempty"`
	BoardID   string `json:"boardId"`
	BoardName string `json:"boardName"`
	Summary   string `json:"summary"`
	CreatedAt string `json:"createdAt"`
}
type DashboardOut struct {
	Panels   []PanelCfg     `json:"panels"` // display order; Enabled=false panels are listed last
	Limit    int            `json:"limit"`
	MyCards  []DashCard     `json:"myCards"`
	DueSoon  []DashCard     `json:"dueSoon"`
	Overdue  []DashCard     `json:"overdue"`
	Activity []DashActivity `json:"recentActivity"`
	Unread   int            `json:"unreadNotifications"`
}
type DashboardUpdateIn struct {
	Panels string `json:"panels" desc:"comma separated panel ids to show, in order: myCards,dueSoon,overdue,recentActivity"`
	Limit  int    `json:"limit" desc:"rows per panel (1-50, default 10)"`
}

type dashCfg struct {
	Order []string `json:"panels"`
	Limit int      `json:"limit"`
}

func loadDashCfg(e *ops.Env) dashCfg {
	var raw string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT dashboard FROM users WHERE id=?`, e.User.UserID).Scan(&raw)
	c := dashCfg{}
	if json.Unmarshal([]byte(raw), &c) != nil || c.Order == nil {
		c.Order = nil
		for _, p := range DashboardPanels {
			c.Order = append(c.Order, p.ID)
		}
		if c.Limit == 0 {
			c.Limit = 10
		}
	}
	if c.Limit <= 0 || c.Limit > 50 {
		c.Limit = 10
	}
	return c
}

func panelCfgs(c dashCfg) []PanelCfg {
	title := map[string]string{}
	for _, p := range DashboardPanels {
		title[p.ID] = p.Title
	}
	var out []PanelCfg
	seen := map[string]bool{}
	for _, id := range c.Order {
		if t, ok := title[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, PanelCfg{id, t, true})
		}
	}
	for _, p := range DashboardPanels {
		if !seen[p.ID] {
			out = append(out, PanelCfg{p.ID, p.Title, false})
		}
	}
	return out
}

// dashCards lists the caller's cards for a panel: one row per card (never once per board it is on), shown on its
// context board (authz.CardAccesses: the home when viewable, else the board with the most access), with that
// board's column. Only placements in an active column count; hidden placements are never named. Constant queries:
// one candidates query, one placements query and one BoardLevels call.
func dashCards(e *ops.Env, where string, order string, args []any, limit int) []DashCard {
	q := `SELECT c.id,c.name,p.board_id,b.name,pr.name,COALESCE(l.name,l.type),COALESCE(c.due_date,''),COALESCE(c.is_due_completed,0)
	 FROM cards c JOIN card_placements p ON p.card_id=c.id JOIN lists l ON l.id=p.list_id JOIN boards b ON b.id=p.board_id JOIN projects pr ON pr.id=b.project_id
	 WHERE c.is_closed=0 AND c.is_draft=0 AND c.is_template=0 AND l.type='active' AND (` + where + `) ORDER BY ` + order + `, p.is_home DESC, p.placed_at, p.board_id LIMIT ?`
	rows, err := e.DB.QueryContext(e.Ctx, q, append(args, limit*10)...)
	out := []DashCard{}
	if err != nil {
		return out
	}
	var ids []string
	cands := map[string][]DashCard{}
	for rows.Next() {
		var c DashCard
		if rows.Scan(&c.ID, &c.Name, &c.BoardID, &c.BoardName, &c.Project, &c.List, &c.DueDate, &c.Completed) != nil {
			continue
		}
		if _, ok := cands[c.ID]; !ok {
			ids = append(ids, c.ID)
		}
		cands[c.ID] = append(cands[c.ID], c)
	}
	rows.Close()
	access := authz.CardAccesses(e, ids, "")
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		a, ok := access[id]
		if !ok {
			continue
		}
		var pick *DashCard
		for i, c := range cands[id] { // the context board's row, else the first viewable one (home first)
			if _, ok := a.Boards[c.BoardID]; ok && (pick == nil || c.BoardID == a.BoardID) {
				pick = &cands[id][i]
				if c.BoardID == a.BoardID {
					break
				}
			}
		}
		if pick != nil {
			out = append(out, *pick)
		}
	}
	return out
}

var camelRe = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func humanAction(t string) string {
	switch t {
	case "cardCreate":
		return "created"
	case "cardUpdate":
		return "updated"
	case "cardMove":
		return "moved"
	case "cardDelete":
		return "deleted"
	case "commentCreate":
		return "commented on"
	case "taskCreate":
		return "added a task to"
	}
	return strings.ToLower(camelRe.ReplaceAllString(t, "$1 $2"))
}

func dashActivity(e *ops.Env, limit int) []DashActivity {
	out := []DashActivity{}
	cursor := "zzzz" // ids are hex: sorts after every id
	level := map[string]bool{}
	shown := map[string]bool{} // logical events already listed: an event recorded on several boards shows once
	type actRow struct {
		a      DashActivity
		origin string
	}
	for page := 0; page < 6 && len(out) < limit; page++ {
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT a.id,a.type,COALESCE(u.name,''),COALESCE(a.card_id,''),COALESCE(c.name,''),a.board_id,COALESCE(b.name,''),a.created_at,COALESCE(a.origin_id,a.id)
		 FROM actions a LEFT JOIN users u ON u.id=a.user_id LEFT JOIN cards c ON c.id=a.card_id LEFT JOIN boards b ON b.id=a.board_id
		 WHERE a.id<? ORDER BY a.id DESC LIMIT 100`, cursor)
		if err != nil {
			return out
		}
		var batch []actRow
		for rows.Next() {
			var r actRow
			if rows.Scan(&r.a.ID, &r.a.Type, &r.a.User, &r.a.CardID, &r.a.CardName, &r.a.BoardID, &r.a.BoardName, &r.a.CreatedAt, &r.origin) == nil {
				cursor = r.a.ID
				batch = append(batch, r)
			}
		}
		rows.Close()
		var unseen []string
		unseenSet := map[string]bool{}
		for _, r := range batch {
			if _, seen := level[r.a.BoardID]; !seen && !unseenSet[r.a.BoardID] {
				unseenSet[r.a.BoardID] = true
				unseen = append(unseen, r.a.BoardID)
			}
		}
		if len(unseen) > 0 {
			lv := authz.BoardLevels(e, unseen)
			for _, b := range unseen {
				level[b] = lv[b] >= authz.View
			}
		}
		for _, r := range batch {
			// the access filter first, then the dedupe: a copy on a hidden board never hides the event (nor names it)
			if !level[r.a.BoardID] || len(out) >= limit || shown[r.origin] {
				continue
			}
			shown[r.origin] = true
			who := r.a.User
			if who == "" {
				who = "Someone"
			}
			r.a.Summary = who + " " + humanAction(r.a.Type)
			if r.a.CardName != "" {
				r.a.Summary += " " + r.a.CardName
			}
			out = append(out, r.a)
		}
		if len(batch) < 100 {
			break
		}
	}
	return out
}

func init() {
	ops.Register(ops.Op{Name: "dashboard.get", Summary: "Personal dashboard: my cards, due soon, overdue, recent activity (panels per my settings)", Method: "GET", Path: "/dashboard", UI: "GET /dashboard"},
		func(e *ops.Env, _ NoInput) (DashboardOut, error) {
			cfg := loadDashCfg(e)
			out := DashboardOut{Panels: panelCfgs(cfg), Limit: cfg.Limit, MyCards: []DashCard{}, DueSoon: []DashCard{}, Overdue: []DashCard{}, Activity: []DashActivity{}, Unread: unreadCount(e)}
			uid := e.User.UserID
			mine := `(EXISTS(SELECT 1 FROM card_memberships m WHERE m.card_id=c.id AND m.user_id=?)`
			mineSub := mine + ` OR EXISTS(SELECT 1 FROM card_subscriptions s WHERE s.card_id=c.id AND s.user_id=? AND s.muted=0))` // a muted card is not followed
			now := time.Now().UTC()
			for _, p := range out.Panels {
				if !p.Enabled {
					continue
				}
				switch p.ID {
				case "myCards":
					out.MyCards = dashCards(e, mine+`)`, `(c.due_date IS NULL), c.due_date, c.id DESC`, []any{uid}, cfg.Limit)
				case "dueSoon":
					out.DueSoon = dashCards(e, mineSub+` AND c.due_date>=? AND c.due_date<=? AND COALESCE(c.is_due_completed,0)=0`, `c.due_date`,
						[]any{uid, uid, now.Format(time.RFC3339), now.AddDate(0, 0, 7).Format(time.RFC3339)}, cfg.Limit)
				case "overdue":
					out.Overdue = dashCards(e, mineSub+` AND c.due_date<? AND COALESCE(c.is_due_completed,0)=0`, `c.due_date`,
						[]any{uid, uid, now.Format(time.RFC3339)}, cfg.Limit)
				case "recentActivity":
					out.Activity = dashActivity(e, cfg.Limit)
				}
			}
			return out, nil
		})
	ops.Register(ops.Op{Name: "dashboard.update", Summary: "Choose and order my dashboard panels and row limit", Method: "PATCH", Path: "/dashboard", UI: "POST /ui/dashboard"},
		func(e *ops.Env, in DashboardUpdateIn) (DashboardOut, error) {
			valid := map[string]bool{}
			for _, p := range DashboardPanels {
				valid[p.ID] = true
			}
			cfg := dashCfg{Order: []string{}, Limit: in.Limit}
			for _, id := range strings.FieldsFunc(in.Panels, func(r rune) bool { return r == ',' || r == ' ' }) {
				if !valid[id] {
					return DashboardOut{}, ops.Invalid("unknown panel " + id)
				}
				if !slices.Contains(cfg.Order, id) {
					cfg.Order = append(cfg.Order, id)
				}
			}
			if cfg.Limit == 0 {
				cfg.Limit = 10
			}
			if cfg.Limit < 1 || cfg.Limit > 50 {
				return DashboardOut{}, ops.Invalid("limit must be between 1 and 50")
			}
			b, _ := json.Marshal(cfg)
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE users SET dashboard=?,updated_at=? WHERE id=?`, string(b), db.Now(), e.User.UserID); err != nil {
				return DashboardOut{}, err
			}
			o, err := ops.Typed[DashboardOut](e, "dashboard.get", NoInput{})
			return o, err
		})
}
