package boards

import (
	"strconv"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// ListSheetData feeds the column sheet: the configuration view of one column (not a card list).
type ListSheetData struct {
	Board       Board
	ProjectName string
	List        List
	Name        string
	Total       int // unfiltered card count (what the WIP limit is compared with)
	WIP         int // 0 = no limit
	Over        bool
	CanEdit     bool
	Targets     []List // other columns of the board (move-all targets)
	Columns     []List // every column of the board in board order (the breadcrumb switcher)
	PrevID      string // neighbouring columns (prev/next buttons); "" at the ends
	NextID      string
	Listeners   []ListenerOut // open event streams on this column (agents, integrations)
	Watching    bool          // the viewer watches this column
}

// ListActivityView feeds the lazily loaded activity section of the column sheet.
type ListActivityView struct {
	ListID    string
	Items     []ListActivityItem
	HasMore   bool
	NextLimit int
}

const (
	activityFirst = 4  // rows shown when the section first loads (keeps the sheet free of a scrollbar)
	activityStep  = 20 // "Show more" adds this many
)

// loadListSheet reads the column through the list.get op (View access; archive/trash are not_found).
func loadListSheet(c *web.Ctx, id string) (*ListSheetData, error) {
	l, err := ops.Typed[List](c.Env, "list.get", ListRefIn{ID: id})
	if err != nil {
		return nil, err
	}
	b, err := GetBoard(c.R.Context(), c.Env.DB, l.BoardID)
	if err != nil {
		return nil, err
	}
	lists, err := listsOf(c.Env, l.BoardID, true)
	if err != nil {
		return nil, err
	}
	d := &ListSheetData{Board: b, List: l, CanEdit: authz.BoardLevel(c.Env, l.BoardID) >= authz.Edit, Watching: l.IsSubscribed != nil && *l.IsSubscribed}
	if l.Name != nil {
		d.Name = *l.Name
	}
	if l.WIPLimit != nil {
		d.WIP = *l.WIPLimit
	}
	_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT name FROM projects WHERE id=?`, b.ProjectID).Scan(&d.ProjectName)
	_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT COUNT(*) FROM cards WHERE list_id=? AND is_draft=0 AND is_template=0`, l.ID).Scan(&d.Total)
	d.Over = d.WIP > 0 && d.Total > d.WIP
	d.Columns = lists
	for i, x := range lists {
		if x.ID == l.ID {
			if i > 0 {
				d.PrevID = lists[i-1].ID
			}
			if i+1 < len(lists) {
				d.NextID = lists[i+1].ID
			}
		} else {
			d.Targets = append(d.Targets, x)
		}
	}
	if c.Env.Hub != nil {
		for _, x := range c.Env.Hub.Listeners.ForList(l.ID) {
			d.Listeners = append(d.Listeners, ListenerOut{UserID: x.UserID, Name: x.Name, IsAgent: x.IsAgent, Label: x.Label, Since: x.Since})
		}
	}
	return d, nil
}

func registerListSheetUI() {
	// fragment loaded into the shared <dialog id="modal">
	web.Route("GET /ui/lists/{id}", func(c *web.Ctx) error {
		d, err := loadListSheet(c, c.R.PathValue("id"))
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_listsheet.html", "boards_list_sheet", d)
	})
	// the rendered description (after an inline save) and the lazily loaded activity feed
	web.Route("GET /ui/lists/{id}/description", func(c *web.Ctx) error {
		d, err := loadListSheet(c, c.R.PathValue("id"))
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_listsheet.html", "boards_list_desc_view", d)
	})
	web.Route("GET /ui/lists/{id}/activity", func(c *web.Ctx) error {
		limit := activityFirst
		if n, err := strconv.Atoi(c.R.URL.Query().Get("limit")); err == nil && n > 0 {
			limit = min(n, 99) // the op caps a page at 100 (one extra row tells whether there is more)
		}
		id := c.R.PathValue("id")
		items, err := ops.Typed[[]ListActivityItem](c.Env, "list.activity", ListActivityIn{ID: id, Limit: limit + 1})
		if err != nil {
			return err
		}
		v := ListActivityView{ListID: id, NextLimit: limit + activityStep}
		if len(items) > limit {
			items, v.HasMore = items[:limit], true
		}
		v.Items = items
		return c.RenderBlock("boards_listsheet.html", "boards_list_activity", v)
	})
	// shareable URL: the board page with the sheet opened on load (static/boards_listsheet.js)
	web.Route(ops.Get("list.get").UI, func(c *web.Ctx) error {
		l, err := ops.Typed[List](c.Env, "list.get", ListRefIn{ID: c.R.PathValue("id")})
		if err != nil {
			return err
		}
		d, err := ops.Typed[BoardDetail](c.Env, "board.get", BoardGetIn{ID: l.BoardID})
		if err != nil {
			return err
		}
		pd, err := loadPage(c.Env, d.Board, c.R.URL.Query(), false)
		if err != nil {
			return err
		}
		pd.OpenList = l.ID
		return c.Render("boards_board.html", pd)
	})
}
