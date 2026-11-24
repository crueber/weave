package boards

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
)

// Filter is the board filter bar state (also the saved filter query format).
type Filter struct {
	Q       string
	Labels  []string
	Members []string // user ids, "me" or "none"
	Due     string   // overdue|today|week|none|done
}

func parseFilter(v url.Values) Filter {
	f := Filter{Q: strings.TrimSpace(v.Get("q")), Due: v.Get("due")}
	for _, l := range v["label"] {
		if l != "" {
			f.Labels = append(f.Labels, l)
		}
	}
	for _, m := range v["member"] {
		if m != "" {
			f.Members = append(f.Members, m)
		}
	}
	return f
}

// Active reports whether any filter criterion is set.
func (f Filter) Active() bool {
	return f.Q != "" || len(f.Labels) > 0 || len(f.Members) > 0 || f.Due != ""
}

// Query encodes the filter as a URL query string (without the view).
func (f Filter) Query() string {
	v := url.Values{}
	if f.Q != "" {
		v.Set("q", f.Q)
	}
	for _, l := range f.Labels {
		v.Add("label", l)
	}
	for _, m := range f.Members {
		v.Add("member", m)
	}
	if f.Due != "" {
		v.Set("due", f.Due)
	}
	return v.Encode()
}

func (f Filter) HasLabel(id string) bool  { return contains(f.Labels, id) }
func (f Filter) HasMember(id string) bool { return contains(f.Members, id) }

func contains(s []string, x string) bool {
	for _, y := range s {
		if y == x {
			return true
		}
	}
	return false
}

// Tile is the compact card representation rendered on board views.
type Tile struct {
	ID, ListID, ListName, Name string
	HasDescription             bool
	Labels                     []TileLabel
	Members                    []TileMember
	Due                        string // display
	DueRaw                     string
	DueState                   string // overdue|soon|done|""
	TasksDone, TasksTotal      int
	Comments, Attachments      int
	AgeDays                    int
	Closed                     bool
	CoverID                    string
	ClaimedBy                  string // name of the holder of an unexpired claim
	ClaimedUntil               string // HH:MM
	ClaimedUntilRaw            string
	Running                    bool   // the card's stopwatch (time tracking) is running
	Place                      string // the card's location (its name, or "lat, lng"); "" when none
	AlsoOn                     int    // how many OTHER boards the viewer can see this card on ("also on" marker data; the marker itself comes later)
	// CanEdit: the caller may edit this card although the board gives them no Edit (authz.CardCaps.CanEdit: a worker
	// who is a member of the card). Board editors use PageData.CanEdit; the tile offers quick edit for either.
	CanEdit bool
	desc    string
}
type TileLabel struct{ ID, Name, Color string }
type TileMember struct {
	ID, Name, Avatar string
	IsAgent          bool
}

// Column is a list with its (filtered) cards.
type Column struct {
	List       List
	Cards      []Tile
	Total      int // unfiltered card count
	AddCardURL string
	Listeners  []ListenerOut // open event streams on this list (agents, integrations)
	Watching   bool          // the viewer watches this column (header bell; menu item state)
}

// Crumb is a board plus its project's name: what the top bar's breadcrumbs and view switcher need (templates _boards_topbar.html).
type Crumb struct {
	Board       Board
	ProjectName string
}

// BoardCrumb loads a board and its project name for pages other domains render under a board (calendar, map).
// No permission check: the caller has already authorized the board through an op.
func BoardCrumb(ctx context.Context, q querier, id string) (Crumb, error) {
	b, err := GetBoard(ctx, q, id)
	if err != nil {
		return Crumb{}, err
	}
	c := Crumb{Board: b}
	_ = q.QueryRowContext(ctx, `SELECT name FROM projects WHERE id=?`, b.ProjectID).Scan(&c.ProjectName)
	return c, nil
}

// PageData feeds the board page, the body fragment and the public page.
type PageData struct {
	OpenList    string // list id whose sheet opens on load (GET /lists/{id})
	Board       Board
	ProjectName string // empty on the public page (the project is not disclosed)
	Level       string
	Caps        authz.BoardCaps // what the caller may do here (authz.Board; on the read-only public page: view only)
	CanEdit     bool            // Caps.CanEdit (kept for the column, tile and composer templates)
	CanManage   bool            // Caps.CanManage
	ReadOnly    bool
	Public      bool
	View        string
	Columns     []Column
	Filter      Filter
	Labels      []Label
	Members     []Member // user members for the member filter
	Saved       []SavedFilter
	Query       string // raw filter query for links
	CardURL     string // "/ui/cards/" prefix for tiles
	BodyURL     string
	PageURL     string
	LiveBoardID string
	NoCards     int
	Watching    bool    // the viewer watches the whole board (header bell); never set on public pages
	HasInbox    bool    // the caller sees this board's Inbox (inbox.go: showInbox)
	Drafts      []Draft // the Inbox, oldest first (never filtered, never counted in NoCards)
}

func addCardPattern() string {
	o := ops.Get("card.create")
	if o == nil || !strings.HasPrefix(o.UI, "POST ") {
		return ""
	}
	return strings.TrimPrefix(o.UI, "POST ")
}

// loadPage assembles a board's page data using the caller's permissions.
func loadPage(e *ops.Env, b Board, v url.Values, readOnly bool) (*PageData, error) {
	caps := authz.Board(e, b.ID)
	if readOnly {
		// The /public/{token} page is a read-only rendering whoever looks at it: it offers no writes, no settings,
		// no watching and no Inbox, only "Connect an agent" for a signed-in viewer (agent.prompt needs View, which
		// the public board gives them).
		caps = authz.BoardCaps{BoardID: caps.BoardID, ProjectID: caps.ProjectID, Level: caps.Level, Public: caps.Public, CanView: caps.CanView,
			CanConnectAgent: caps.CanConnectAgent}
	}
	pd := &PageData{Board: b, Level: levelName(caps.Level), Caps: caps, CanEdit: caps.CanEdit, CanManage: caps.CanManage, ReadOnly: readOnly,
		Public: readOnly && e.User == nil, Filter: parseFilter(v)}
	pd.Query = pd.Filter.Query()
	pd.PageURL = "/boards/" + b.ID
	pd.View = v.Get("view")
	if pd.View != "list" && pd.View != "grid" && pd.View != "kanban" {
		pd.View = "kanban"
		if b.DefaultView == "list" || b.DefaultView == "grid" {
			pd.View = b.DefaultView
		}
	}
	if !readOnly {
		pd.LiveBoardID = b.ID
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT name FROM projects WHERE id=?`, b.ProjectID).Scan(&pd.ProjectName)
	}
	lists, err := listsOf(e, b.ID, true)
	if err != nil {
		return nil, err
	}
	labs, err := labelsOf(e, b.ID)
	if err != nil {
		return nil, err
	}
	pd.Labels = labs
	if pd.Members, err = membersOf(e, b.ID); err != nil {
		return nil, err
	}
	if pd.Saved, err = filtersOf(e, b.ID); err != nil {
		return nil, err
	}
	pattern := addCardPattern()
	watched := map[string]bool{}
	if !readOnly && e.User != nil {
		pd.Watching = boardWatched(e, b.ID)
		watched = watchedLists(e, b.ID)
	}
	cols := make([]Column, 0, len(lists))
	idx := map[string]int{}
	for _, l := range lists {
		idx[l.ID] = len(cols)
		c := Column{List: l, Watching: watched[l.ID]}
		for _, x := range e.Hub.Listeners.ForList(l.ID) {
			c.Listeners = append(c.Listeners, ListenerOut{UserID: x.UserID, Name: x.Name, IsAgent: x.IsAgent, Label: x.Label, Since: x.Since})
		}
		if pd.CanEdit && pattern != "" {
			c.AddCardURL = replaceParams(pattern, l.ID)
		}
		cols = append(cols, c)
	}
	tiles, err := loadTiles(e, b, idx, lists)
	if err != nil {
		return nil, err
	}
	me := ""
	if e.User != nil {
		me = e.User.UserID
	}
	now := time.Now()
	for _, t := range tiles {
		i := idx[t.ListID]
		cols[i].Total++
		if pd.Filter.match(t, me, now) {
			cols[i].Cards = append(cols[i].Cards, t)
			pd.NoCards++
		}
	}
	pd.Columns = cols
	if err := loadInbox(e, pd, readOnly); err != nil {
		return nil, err
	}
	return pd, nil
}

func replaceParams(pattern, id string) string {
	var out strings.Builder
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '{' {
			j := strings.IndexByte(pattern[i:], '}')
			if j > 0 {
				out.WriteString(id)
				i += j
				continue
			}
		}
		out.WriteByte(pattern[i])
	}
	return out.String()
}

func parseTime(s string) (time.Time, bool) {
	for _, l := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (f Filter) match(t Tile, me string, now time.Time) bool {
	if f.Q != "" {
		q := strings.ToLower(f.Q)
		hay := strings.ToLower(t.Name)
		for _, l := range t.Labels {
			hay += " " + strings.ToLower(l.Name)
		}
		for _, m := range t.Members {
			hay += " " + strings.ToLower(m.Name)
		}
		if !strings.Contains(hay, q) && !strings.Contains(strings.ToLower(t.desc), q) {
			return false
		}
	}
	if len(f.Labels) > 0 {
		ok := false
		for _, l := range t.Labels {
			if f.HasLabel(l.ID) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if len(f.Members) > 0 {
		ok := false
		for _, want := range f.Members {
			switch want {
			case "none":
				ok = ok || len(t.Members) == 0
			case "me":
				for _, m := range t.Members {
					ok = ok || m.ID == me
				}
			default:
				for _, m := range t.Members {
					ok = ok || m.ID == want
				}
			}
		}
		if !ok {
			return false
		}
	}
	switch f.Due {
	case "":
	case "none":
		return t.DueRaw == ""
	case "done":
		return t.DueState == "done"
	case "overdue":
		return t.DueState == "overdue"
	case "today", "week":
		d, ok := parseTime(t.DueRaw)
		if !ok || t.DueState == "done" {
			return false
		}
		days := 1
		if f.Due == "week" {
			days = 7
		}
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		return !d.Before(start) && d.Before(start.AddDate(0, 0, days))
	}
	return true
}
