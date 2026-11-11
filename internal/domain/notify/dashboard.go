package notify

import (
	"encoding/json"
	"regexp"
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

func dashCards(e *ops.Env, where string, order string, args []any, limit int) []DashCard {
	q := `SELECT c.id,c.name,c.board_id,b.name,p.name,COALESCE(l.name,l.type),COALESCE(c.due_date,''),COALESCE(c.is_due_completed,0)
	 FROM cards c JOIN lists l ON l.id=c.list_id JOIN boards b ON b.id=c.board_id JOIN projects p ON p.id=b.project_id
	 WHERE c.is_closed=0 AND c.is_draft=0 AND c.is_template=0 AND l.type='active' AND (` + where + `) ORDER BY ` + order + ` LIMIT ?`
	rows, err := e.DB.QueryContext(e.Ctx, q, append(args, limit*5)...)
	out := []DashCard{}
	if err != nil {
		return out
	}
	defer rows.Close()
	level := map[string]bool{}
	for rows.Next() && len(out) < limit {
		var c DashCard
		_ = rows.Scan(&c.ID, &c.Name, &c.BoardID, &c.BoardName, &c.Project, &c.List, &c.DueDate, &c.Completed)
		ok, seen := level[c.BoardID]
		if !seen {
			ok = authz.BoardLevel(e, c.BoardID) >= authz.View
			level[c.BoardID] = ok
		}
		if ok {
			out = append(out, c)
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
	for page := 0; page < 6 && len(out) < limit; page++ {
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT a.id,a.type,COALESCE(u.name,''),COALESCE(a.card_id,''),COALESCE(c.name,''),a.board_id,COALESCE(b.name,''),a.created_at
		 FROM actions a LEFT JOIN users u ON u.id=a.user_id LEFT JOIN cards c ON c.id=a.card_id LEFT JOIN boards b ON b.id=a.board_id
		 WHERE a.id<? ORDER BY a.id DESC LIMIT 100`, cursor)
		if err != nil {
			return out
		}
		n := 0
		for rows.Next() {
			var a DashActivity
			_ = rows.Scan(&a.ID, &a.Type, &a.User, &a.CardID, &a.CardName, &a.BoardID, &a.BoardName, &a.CreatedAt)
			cursor = a.ID
			n++
			ok, seen := level[a.BoardID]
			if !seen {
				ok = authz.BoardLevel(e, a.BoardID) >= authz.View
				level[a.BoardID] = ok
			}
			if !ok || len(out) >= limit {
				continue
			}
			who := a.User
			if who == "" {
				who = "Someone"
			}
			a.Summary = who + " " + humanAction(a.Type)
			if a.CardName != "" {
				a.Summary += " " + a.CardName
			}
			out = append(out, a)
		}
		rows.Close()
		if n < 100 {
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
			mineSub := mine + ` OR EXISTS(SELECT 1 FROM card_subscriptions s WHERE s.card_id=c.id AND s.user_id=?))`
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
				if !contains(cfg.Order, id) {
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
