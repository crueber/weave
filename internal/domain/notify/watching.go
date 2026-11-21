package notify

import (
	"strconv"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// "What I watch": the read side of watching (the write ops are board.subscribe / list.subscribe / card.subscribe and
// their unsubscribe twins in the boards and cards domains). Notify reads the three watch tables like the planner does.

// WatchItem is one watched (or muted) board, column or card.
type WatchItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BoardID   string `json:"boardId"`
	BoardName string `json:"boardName"`
	Context   string `json:"context,omitempty" desc:"boards: the project; columns: the board; cards: board / column"`
	Explicit  bool   `json:"explicit,omitempty" desc:"cards: watched on purpose (card.subscribe) rather than automatically (created, commented, added)"`
}

// WatchListOut is subscription.list's output.
type WatchListOut struct {
	Boards     []WatchItem `json:"boards"`
	Lists      []WatchItem `json:"lists"`
	Cards      []WatchItem `json:"cards"`
	Muted      []WatchItem `json:"muted" desc:"cards you muted: no notifications even through a watched column or board"`
	CardsTotal int         `json:"cardsTotal"`
	MutedTotal int         `json:"mutedTotal"`
}

type WatchListIn struct {
	Limit int `query:"limit" desc:"cards and muted cards to return, most recently changed first (default 100, max 500)"`
}

func init() {
	ops.Register(ops.Op{Name: "subscription.list", Summary: "What you watch: boards, columns and cards (newest first), and the cards you muted; only boards you can still view", Method: "GET", Path: "/users/me/subscriptions",
		UI: "GET /ui/settings/watching"}, subscriptionList)
	web.Route("GET /ui/settings/watching", func(c *web.Ctx) error { return renderWatching(c) })
	web.Route("POST /ui/settings/watching/stop", uiWatchStop)
	web.Route("POST /ui/settings/watching/watch", uiWatchAgain)
}

func subscriptionList(e *ops.Env, in WatchListIn) (WatchListOut, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		return WatchListOut{}, ops.Invalid("limit must be 1..500")
	}
	out := WatchListOut{Boards: []WatchItem{}, Lists: []WatchItem{}, Cards: []WatchItem{}, Muted: []WatchItem{}}
	uid := e.User.UserID
	type row struct {
		WatchItem
		muted bool
	}
	scan := func(q string, extra bool) []row {
		var rs []row
		rows, err := e.DB.QueryContext(e.Ctx, q, uid)
		if err != nil {
			return nil
		}
		defer rows.Close()
		for rows.Next() {
			var r row
			var err error
			if extra {
				err = rows.Scan(&r.ID, &r.Name, &r.BoardID, &r.BoardName, &r.Context, &r.Explicit, &r.muted)
			} else {
				err = rows.Scan(&r.ID, &r.Name, &r.BoardID, &r.BoardName, &r.Context)
			}
			if err == nil {
				rs = append(rs, r)
			}
		}
		return rs
	}
	boards := scan(`SELECT b.id,b.name,b.id,b.name,COALESCE(p.name,'') FROM board_subscriptions s JOIN boards b ON b.id=s.board_id
		LEFT JOIN projects p ON p.id=b.project_id WHERE s.user_id=? ORDER BY b.name COLLATE NOCASE, b.id`, false)
	lists := scan(`SELECT l.id,COALESCE(l.name,''),b.id,b.name,b.name FROM list_subscriptions s JOIN lists l ON l.id=s.list_id JOIN boards b ON b.id=l.board_id
		WHERE s.user_id=? AND l.type IN ('active','closed') ORDER BY b.name COLLATE NOCASE, b.id, l.position`, false)
	cards := scan(`SELECT c.id,c.name,b.id,b.name,b.name||' / '||COALESCE(l.name,l.type),s.is_permanent,s.muted FROM card_subscriptions s JOIN cards c ON c.id=s.card_id
		JOIN boards b ON b.id=c.board_id JOIN lists l ON l.id=c.list_id WHERE s.user_id=? AND c.is_template=0
		ORDER BY COALESCE(c.updated_at,c.created_at) DESC, c.id DESC`, true)
	seen, ids := map[string]bool{}, []string{}
	for _, set := range [][]row{boards, lists, cards} {
		for _, r := range set {
			if !seen[r.BoardID] {
				seen[r.BoardID] = true
				ids = append(ids, r.BoardID)
			}
		}
	}
	lv := authz.BoardLevels(e, ids)
	ok := func(r row) bool { return lv[r.BoardID] >= authz.View }
	for _, r := range boards {
		if ok(r) {
			out.Boards = append(out.Boards, r.WatchItem)
		}
	}
	for _, r := range lists {
		if ok(r) {
			out.Lists = append(out.Lists, r.WatchItem)
		}
	}
	for _, r := range cards {
		if !ok(r) {
			continue
		}
		if r.muted {
			r.Explicit = false
			if out.MutedTotal++; out.MutedTotal <= limit {
				out.Muted = append(out.Muted, r.WatchItem)
			}
		} else if out.CardsTotal++; out.CardsTotal <= limit {
			out.Cards = append(out.Cards, r.WatchItem)
		}
	}
	return out, nil
}

// watchingView feeds the "Watching" section of the user settings page.
type watchingView struct {
	L     WatchListOut
	Limit int // cards shown (auto-watch makes the card list long: the page starts short and grows on request)
	More  int // the next limit for "Show more"
}

func renderWatching(c *web.Ctx) error {
	limit := 20
	if n, err := strconv.Atoi(c.R.FormValue("limit")); err == nil && n > 0 {
		limit = min(n, 500)
	}
	l, err := ops.Typed[WatchListOut](c.Env, "subscription.list", WatchListIn{Limit: limit})
	if err != nil {
		return err
	}
	return c.RenderBlock("notify_watching.html", "notify_watching", watchingView{L: l, Limit: limit, More: min(limit+100, 500)})
}

// uiWatchStop stops one watch from the settings list: kind board|list|card (a card is muted: see card.unsubscribe).
func uiWatchStop(c *web.Ctx) error {
	_ = c.R.ParseForm()
	op, msg := "", ""
	switch c.R.FormValue("kind") {
	case "board":
		op, msg = "board.unsubscribe", "Stopped watching the board"
	case "list":
		op, msg = "list.unsubscribe", "Stopped watching the column"
	case "card":
		op, msg = "card.unsubscribe", "Card muted"
	default:
		return ops.Invalid("kind must be board, list or card")
	}
	if _, err := ops.CallMap(c.Env, op, map[string]any{"id": c.R.FormValue("id")}); err != nil {
		return err
	}
	c.Toast(msg)
	return renderWatching(c)
}

// uiWatchAgain watches a muted card again (card.subscribe clears the mute).
func uiWatchAgain(c *web.Ctx) error {
	_ = c.R.ParseForm()
	if _, err := ops.CallMap(c.Env, "card.subscribe", map[string]any{"id": c.R.FormValue("id")}); err != nil {
		return err
	}
	c.Toast("Watching the card again")
	return renderWatching(c)
}
