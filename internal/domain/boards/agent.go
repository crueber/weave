package boards

import (
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

// Context-efficient reads and listener presence for agents.

type TreeIn struct {
	ID            string `path:"id" desc:"board id"`
	IncludeClosed bool   `query:"includeClosed" desc:"also return closed lists and closed cards (default false)"`
	ListIDs       string `query:"listIds" desc:"comma separated list ids to restrict the tree to"`
}

type TreeList struct {
	List
	Cards []events.CardSnap `json:"cards"`
}

type TreeOut struct {
	Board Board      `json:"board"`
	Lists []TreeList `json:"lists"`
}

type ListenersListIn struct {
	ID string `path:"id" desc:"list id"`
}

type ListenersBoardIn struct {
	ID string `path:"id" desc:"board id"`
}

// ListenerOut is one party currently streaming events for a list.
type ListenerOut struct {
	UserID  string   `json:"userId"`
	Name    string   `json:"name"`
	IsAgent bool     `json:"isAgent"`
	Label   string   `json:"label"`
	Since   string   `json:"since"`
	ListIDs []string `json:"listIds,omitempty" desc:"board.listeners only: the board's lists it listens to"`
}

func init() {
	ops.Register(ops.Op{Name: "board.tree", Summary: "Board, lists and compact cards (id, name, description, labels, members, claim) in one call",
		Method: "GET", Path: "/boards/{id}/tree", UI: "exempt:the board page renders the same data as HTML; this is the compact machine read"}, boardTree)
	ops.Register(ops.Op{Name: "list.listeners", Summary: "Who is listening to this list right now (open /events streams with lists=)",
		Method: "GET", Path: "/lists/{id}/listeners", UI: "exempt:rendered server-side as the robot indicator in the board list headers (updates live via SSE listener.joined/left)"}, func(e *ops.Env, in ListenersListIn) ([]ListenerOut, error) {
		l, err := requireList(e, in.ID, authz.View)
		if err != nil {
			return nil, err
		}
		out := []ListenerOut{}
		for _, x := range e.Hub.Listeners.ForList(l.ID) {
			out = append(out, ListenerOut{UserID: x.UserID, Name: x.Name, IsAgent: x.IsAgent, Label: x.Label, Since: x.Since})
		}
		return out, nil
	})
	ops.Register(ops.Op{Name: "board.listeners", Summary: "Who is listening to any list of this board right now",
		Method: "GET", Path: "/boards/{id}/listeners", UI: "exempt:rendered server-side as the robot indicator in the board list headers (updates live via SSE listener.joined/left)"}, func(e *ops.Env, in ListenersBoardIn) ([]ListenerOut, error) {
		if err := authz.RequireBoard(e, in.ID, authz.View); err != nil {
			return nil, err
		}
		out := []ListenerOut{}
		for _, x := range e.Hub.Listeners.ForBoard(in.ID) {
			var ids []string
			for i, l := range x.Lists {
				if x.Boards[i] == in.ID {
					ids = append(ids, l)
				}
			}
			out = append(out, ListenerOut{UserID: x.UserID, Name: x.Name, IsAgent: x.IsAgent, Label: x.Label, Since: x.Since, ListIDs: ids})
		}
		return out, nil
	})
}

func boardTree(e *ops.Env, in TreeIn) (TreeOut, error) {
	if err := authz.RequireBoard(e, in.ID, authz.View); err != nil {
		return TreeOut{}, err
	}
	b, err := GetBoard(e.Ctx, e.DB, in.ID)
	if err != nil {
		return TreeOut{}, err
	}
	if authz.BoardLevel(e, in.ID) < authz.Manage {
		b.PublicToken = ""
	}
	all, err := listsOf(e, in.ID, true)
	if err != nil {
		return TreeOut{}, err
	}
	only := map[string]bool{}
	for _, id := range strings.Split(in.ListIDs, ",") {
		if id = strings.TrimSpace(id); id != "" {
			only[id] = true
		}
	}
	out := TreeOut{Board: b, Lists: []TreeList{}}
	idx := map[string]int{}
	for _, l := range all {
		if (l.Type == "closed" && !in.IncludeClosed) || (len(only) > 0 && !only[l.ID]) {
			continue
		}
		idx[l.ID] = len(out.Lists)
		out.Lists = append(out.Lists, TreeList{List: l, Cards: []events.CardSnap{}})
	}
	q := `SELECT id FROM cards WHERE board_id=? AND is_draft=0 AND is_template=0`
	if !in.IncludeClosed {
		q += ` AND is_closed=0`
	}
	q += ` ORDER BY list_id, position, created_at, id`
	if events.Snapshots == nil {
		return out, nil
	}
	for _, c := range events.Snapshots(e, strs(e, q, in.ID)) {
		if i, ok := idx[c.ListID]; ok {
			out.Lists[i].Cards = append(out.Lists[i].Cards, c)
		}
	}
	return out, nil
}
