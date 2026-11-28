package boards

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// This file holds the "quick" board endpoints: composite fragments used by the
// add-card / add-list composers and the tile quick-edit popover. Every mutation
// still goes through the owning op (card.create, list.create, card.update ...).

// Minus returns the filter query with one criterion removed: kind is q, label,
// member or due (val is the label/member id; ignored for q and due).
func (f Filter) Minus(kind, val string) string {
	g := Filter{Q: f.Q, Due: f.Due}
	switch kind {
	case "q":
		g.Q = ""
	case "due":
		g.Due = ""
	}
	for _, l := range f.Labels {
		if kind != "label" || l != val {
			g.Labels = append(g.Labels, l)
		}
	}
	for _, m := range f.Members {
		if kind != "member" || m != val {
			g.Members = append(g.Members, m)
		}
	}
	return g.Query()
}

// DueLabel is the human wording of the due filter.
func (f Filter) DueLabel() string {
	switch f.Due {
	case "overdue":
		return "Overdue"
	case "today":
		return "Due today"
	case "week":
		return "Due this week"
	case "done":
		return "Completed"
	case "none":
		return "No due date"
	}
	return ""
}

// ColsData feeds the fragment that carries freshly created columns.
type ColsData struct {
	P    *PageData
	Cols []Column
}

// TilesData feeds the fragment that carries freshly created tiles.
type TilesData struct {
	P     *PageData
	Tiles []Tile
}

// QuickLabel is one row of the tile quick-edit popover.
type QuickLabel struct {
	Label
	On bool
}

// QuickData feeds the tile quick-edit popover.
type QuickData struct {
	P      *PageData
	T      Tile
	Labels []QuickLabel
	// CanRemove: "Remove from this board" (authz.CardCaps.CanRemoveFromBoard on this board: card.unplace's rule, and
	// the card stays on another board the caller can see). The archive wording uses T.AlsoOn (the other boards).
	CanRemove bool
}

// shell builds the page data a fragment needs (permissions, board, labels) and returns the board's finite lists
// without loading any cards, reusing d.Labels and d.Lists from board.get.
func shell(c *web.Ctx, boardID string) (*PageData, []List, error) {
	d, err := ops.Typed[BoardDetail](c.Env, "board.get", BoardGetIn{ID: boardID})
	if err != nil {
		return nil, nil, err
	}
	caps := authz.Board(c.Env, boardID)
	pd := &PageData{Board: d.Board, Level: levelName(caps.Level), Caps: caps, CanEdit: caps.CanEdit, CanManage: caps.CanManage, Labels: d.Labels, PageURL: "/boards/" + boardID, View: "kanban"}
	lists := make([]List, 0, len(d.Lists))
	for _, l := range d.Lists {
		if isFinite(l.Type) {
			lists = append(lists, l)
		}
	}
	return pd, lists, nil
}

// splitNames turns form values (possibly multi-line pastes) into trimmed, non-empty titles.
func splitNames(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, line := range strings.Split(strings.ReplaceAll(v, "\r", "\n"), "\n") {
			line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*•"))
			if line != "" {
				if r := []rune(line); len(r) > 1024 {
					line = string(r[:1024])
				}
				out = append(out, line)
			}
		}
	}
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

func idOf(v any) string {
	b, _ := json.Marshal(v)
	var x struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(b, &x)
	return x.ID
}

func trigger(c *web.Ctx, name string, detail any) {
	b, _ := json.Marshal(map[string]any{name: detail})
	c.W.Header().Set("HX-Trigger", string(b))
}

// boardOfCard is the board a quick edit renders the card's tile for: the request's context board (?board= or the
// form field board) when the card is placed there and the caller can view it, else the default context (the home
// when viewable). A card on several boards must re-render on the board the user is looking at.
func boardOfCard(c *web.Ctx, cardID string) (string, error) {
	a, err := authz.CardAccessFor(c.Env, cardID, c.BoardHint())
	return a.BoardID, err
}

// tilesFor loads the tiles of the given cards (in board order), reusing lists from shell.
func tilesFor(c *web.Ctx, pd *PageData, lists []List, ids []string) ([]Tile, error) {
	idx := map[string]int{}
	for i, l := range lists {
		idx[l.ID] = i
	}
	return loadTilesOnly(c.Env, pd.Board, pd.Caps.Level, idx, lists, ids)
}

// backToBoard is the progressive-enhancement answer to a plain (non-htmx) form post.
func backToBoard(c *web.Ctx, boardID string) error {
	http.Redirect(c.W, c.R, "/boards/"+boardID, http.StatusSeeOther)
	return nil
}

func registerQuick() {
	// Add one or several cards (one per line). Responds with just the new tile(s);
	// never opens the card. name may repeat. top=1 inserts above the first card.
	web.Route("POST /ui/lists/{listId}/cards/quick", func(c *web.Ctx) error {
		if err := c.R.ParseForm(); err != nil {
			return ops.Invalid("bad form")
		}
		listID := c.R.PathValue("listId")
		l, err := GetList(c.R.Context(), c.Env.DB, listID)
		if err != nil {
			return ops.NotFound("list")
		}
		if err := authz.RequireBoard(c.Env, l.BoardID, authz.Edit); err != nil {
			return err
		}
		names := splitNames(c.R.Form["name"])
		if len(names) == 0 {
			if !c.IsHTMX() {
				return backToBoard(c, l.BoardID)
			}
			c.W.WriteHeader(http.StatusNoContent)
			return nil
		}
		top := c.R.Form.Get("top") == "1"
		first, prev := "", ""
		if top {
			_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT c.id FROM card_placements p JOIN cards c ON c.id=p.card_id WHERE p.list_id=? AND c.is_draft=0 AND c.is_template=0 ORDER BY p.position, c.id LIMIT 1`, listID).Scan(&first)
		}
		var ids []string
		var failure error
		for _, n := range names {
			form := url.Values{"listId": {listID}, "name": {n}}
			if top && first != "" {
				pos, err := PlaceCardBetween(c.R.Context(), c.Env.DB, listID, "", prev, first)
				if err != nil {
					failure = err
					break
				}
				form.Set("position", fmt.Sprint(pos))
			}
			out, err := CallByForm(c.Env, "card.create", form)
			if err != nil {
				failure = err
				break
			}
			prev = idOf(out)
			ids = append(ids, prev)
		}
		if len(ids) == 0 && failure != nil {
			return failure
		}
		if !c.IsHTMX() {
			return backToBoard(c, l.BoardID)
		}
		pd, lists, err := shell(c, l.BoardID)
		if err != nil {
			return err
		}
		tiles, err := tilesFor(c, pd, lists, ids)
		if err != nil {
			return err
		}
		detail := map[string]any{"listId": listID, "ids": ids, "count": len(ids), "top": top}
		if failure != nil {
			detail["error"] = "Some cards could not be added"
		}
		trigger(c, "boards:added", detail)
		return c.RenderBlock("boards_board.html", "boards_tiles", TilesData{P: pd, Tiles: tiles})
	})

	// Add one or several lists (one per line); responds with the new column(s).
	web.Route("POST /ui/boards/{boardId}/lists/quick", func(c *web.Ctx) error {
		if err := c.R.ParseForm(); err != nil {
			return ops.Invalid("bad form")
		}
		boardID := c.R.PathValue("boardId")
		if err := authz.RequireBoard(c.Env, boardID, authz.Edit); err != nil {
			return err
		}
		names := splitNames(c.R.Form["name"])
		if len(names) == 0 {
			if !c.IsHTMX() {
				return backToBoard(c, boardID)
			}
			c.W.WriteHeader(http.StatusNoContent)
			return nil
		}
		for i := range names {
			if r := []rune(names[i]); len(r) > 128 {
				names[i] = string(r[:128])
			}
		}
		var ids []string
		var failure error
		for _, n := range names {
			out, err := CallByForm(c.Env, "list.create", url.Values{"boardId": {boardID}, "name": {n}})
			if err != nil {
				failure = err
				break
			}
			ids = append(ids, idOf(out))
		}
		if len(ids) == 0 && failure != nil {
			return failure
		}
		if !c.IsHTMX() {
			return backToBoard(c, boardID)
		}
		pd, lists, err := shell(c, boardID)
		if err != nil {
			return err
		}
		want := map[string]bool{}
		for _, id := range ids {
			want[id] = true
		}
		pattern := addCardPattern()
		var cols []Column
		for _, l := range lists {
			if !want[l.ID] {
				continue
			}
			col := Column{List: l}
			if pd.CanEdit && pattern != "" {
				col.AddCardURL = replaceParams(pattern, l.ID)
			}
			cols = append(cols, col)
		}
		detail := map[string]any{"ids": ids, "count": len(ids)}
		if failure != nil {
			detail["error"] = "Some lists could not be added"
		}
		trigger(c, "boards:added", map[string]any{"lists": detail})
		return c.RenderBlock("boards_board.html", "boards_cols", ColsData{P: pd, Cols: cols})
	})

	// One tile, re-rendered (after the card detail closed, for example).
	web.Route("GET /ui/cards/{id}/tile", func(c *web.Ctx) error {
		return renderTile(c, c.R.PathValue("id"), "boards_tile_frag")
	})

	// Quick-edit popover: rename + label toggles + archive for one tile.
	web.Route("GET /ui/cards/{id}/quick", func(c *web.Ctx) error { return renderQuick(c, c.R.PathValue("id"), false) })
	web.Route("POST /ui/cards/{id}/quick", func(c *web.Ctx) error {
		id := c.R.PathValue("id")
		if err := c.R.ParseForm(); err != nil {
			return ops.Invalid("bad form")
		}
		name := strings.TrimSpace(c.R.Form.Get("name"))
		if name == "" {
			return ops.Invalid("name is required")
		}
		if _, err := CallByForm(c.Env, "card.update", url.Values{"id": {id}, "name": {name}}); err != nil {
			return err
		}
		trigger(c, "boards:closepop", true)
		return renderTile(c, id, "boards_tile_oob")
	})
	web.Route("POST /ui/cards/{id}/quick/label", func(c *web.Ctx) error {
		id := c.R.PathValue("id")
		if err := c.R.ParseForm(); err != nil {
			return ops.Invalid("bad form")
		}
		op := "card.label.add"
		if c.R.Form.Get("on") == "0" {
			op = "card.label.remove"
		}
		board, err := boardOfCard(c, id) // labels belong to the tile's board (the card may be on several)
		if err != nil {
			return err
		}
		if _, err := CallByForm(c.Env, op, url.Values{"id": {id}, "cardId": {id}, "labelId": {c.R.Form.Get("labelId")}, "boardId": {board}}); err != nil {
			return err
		}
		if c.R.Form.Get("_part") == "labels" { // the keyboard label picker (shortcut L) re-renders itself
			return renderPicker(c, id, "labels")
		}
		return renderQuick(c, id, true)
	})
	web.Route("POST /ui/cards/{id}/quick/member", func(c *web.Ctx) error {
		id := c.R.PathValue("id")
		if err := c.R.ParseForm(); err != nil {
			return ops.Invalid("bad form")
		}
		op := "card.member.add"
		if c.R.Form.Get("on") == "0" {
			op = "card.member.remove"
		}
		if _, err := CallByForm(c.Env, op, url.Values{"id": {id}, "userId": {c.R.Form.Get("userId")}}); err != nil {
			return err
		}
		return renderPicker(c, id, "members")
	})
	// trash=1 moves the card to the trash list instead (the board's Delete shortcut); both offer Undo.
	web.Route("POST /ui/cards/{id}/quick/archive", func(c *web.Ctx) error {
		id := c.R.PathValue("id")
		if _, err := boardOfCard(c, id); err != nil {
			return err
		}
		_ = c.R.ParseForm()
		trash := c.R.Form.Get("trash") == "1" || c.R.Form.Get("trash") == "true"
		if _, err := CallByForm(c.Env, "card.archive", url.Values{"id": {id}, "trash": {fmt.Sprint(trash)}}); err != nil {
			return err
		}
		msg := "Card archived"
		if trash {
			msg = "Card moved to trash"
		}
		trigger(c, "boards:undo", map[string]any{"msg": msg, "url": "/ui/cards/" + id + "/quick/restore", "id": id})
		c.W.WriteHeader(http.StatusNoContent)
		return nil
	})
	// Keyboard pickers for the card shortcuts (boards_cardkeys.js): L labels, m members, d due date, p location. Each is a
	// fragment for #boards-pop; the pickers post to the existing routes (quick/label, quick/member, due, location).
	for _, kind := range []string{"labels", "members", "due", "place"} {
		kind := kind
		web.Route("GET /ui/cards/{id}/quick/"+kind, func(c *web.Ctx) error { return renderPicker(c, c.R.PathValue("id"), kind) })
	}
	web.Route("POST /ui/cards/{id}/quick/restore", func(c *web.Ctx) error {
		id := c.R.PathValue("id")
		if _, err := CallByForm(c.Env, "card.restore", url.Values{"id": {id}}); err != nil {
			return err
		}
		trigger(c, "boards:refresh", true)
		c.W.WriteHeader(http.StatusNoContent)
		return nil
	})
}

func renderTile(c *web.Ctx, id, block string) error {
	boardID, err := boardOfCard(c, id)
	if err != nil {
		return err
	}
	pd, lists, err := shell(c, boardID)
	if err != nil {
		return err
	}
	tiles, err := tilesFor(c, pd, lists, []string{id})
	if err != nil {
		return err
	}
	if len(tiles) == 0 {
		return ops.NotFound("card")
	}
	return c.RenderBlock("boards_board.html", block, TilesData{P: pd, Tiles: tiles})
}

// QuickMember is one assignable user row of the keyboard member picker.
type QuickMember struct {
	ID       string
	Name     string
	Username string
	IsAgent  bool
	On       bool
}

// PickerData feeds the keyboard pickers (labels / members / due date / location) of the card shortcuts.
type PickerData struct {
	QuickData
	Kind     string
	Members  []QuickMember
	DueValue string // the due date as a datetime-local value (UTC wall time, like the card sheet), "" when none
	Lat, Lng string // the location, "" when none
	Place    string // the location's name
}

func quickData(c *web.Ctx, id string) (QuickData, error) {
	boardID, err := boardOfCard(c, id)
	if err != nil {
		return QuickData{}, err
	}
	pd, lists, err := shell(c, boardID)
	if err != nil {
		return QuickData{}, err
	}
	cc := authz.Card(c.Env, id)
	if !pd.CanEdit && !cc.CanEdit { // a worker edits the cards they are a member of
		return QuickData{}, ops.Forbidden()
	}
	tiles, err := tilesFor(c, pd, lists, []string{id})
	if err != nil {
		return QuickData{}, err
	}
	if len(tiles) == 0 {
		return QuickData{}, ops.NotFound("card")
	}
	qd := QuickData{P: pd, T: tiles[0], CanRemove: cc.CanRemoveFromBoard(boardID)}
	for _, l := range pd.Labels {
		on := false
		for _, tl := range tiles[0].Labels {
			on = on || tl.ID == l.ID
		}
		qd.Labels = append(qd.Labels, QuickLabel{Label: l, On: on})
	}
	return qd, nil
}

func renderPicker(c *web.Ctx, id, kind string) error {
	qd, err := quickData(c, id)
	if err != nil {
		return err
	}
	d := PickerData{QuickData: qd, Kind: kind}
	if t, ok := parseTime(qd.T.DueRaw); ok {
		d.DueValue = t.UTC().Format("2006-01-02T15:04")
	}
	if kind == "members" {
		assigned := map[string]bool{}
		if rows, err := c.Env.DB.QueryContext(c.R.Context(), `SELECT user_id FROM card_memberships WHERE card_id=?`, id); err == nil {
			for rows.Next() {
				var uid string
				if rows.Scan(&uid) == nil {
					assigned[uid] = true
				}
			}
			rows.Close()
		}
		if rows, err := c.Env.DB.QueryContext(c.R.Context(), `SELECT u.id,u.name,COALESCE(u.username,''),u.is_agent FROM users u
			WHERE u.deleted_at IS NULL AND u.is_deactivated=0 AND (
				u.id IN (SELECT user_id FROM board_memberships WHERE board_id=?1 AND user_id IS NOT NULL) OR
				u.id IN (SELECT gm.user_id FROM user_group_members gm JOIN board_memberships bm ON bm.group_id=gm.group_id WHERE bm.board_id=?1) OR
				u.id IN (SELECT pm.user_id FROM project_managers pm JOIN boards b ON b.project_id=pm.project_id WHERE b.id=?1))
			ORDER BY u.name, u.id`, qd.P.Board.ID); err == nil {
			for rows.Next() {
				var m QuickMember
				if rows.Scan(&m.ID, &m.Name, &m.Username, &m.IsAgent) == nil {
					m.On = assigned[m.ID]
					d.Members = append(d.Members, m)
				}
			}
			rows.Close()
		}
	}
	if kind == "place" { // read-only over the cards table, like the tile loader
		var lat, lng *float64
		var name *string
		_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT lat,lng,location_name FROM cards WHERE id=?`, id).Scan(&lat, &lng, &name)
		if lat != nil && lng != nil {
			d.Lat, d.Lng = fmt.Sprint(*lat), fmt.Sprint(*lng)
		}
		if name != nil {
			d.Place = *name
		}
	}
	return c.RenderBlock("boards_board.html", "boards_kpick", d)
}

func renderQuick(c *web.Ctx, id string, oob bool) error {
	qd, err := quickData(c, id)
	if err != nil {
		return err
	}
	block := "boards_quick"
	if oob {
		block = "boards_quick_oob"
	}
	return c.RenderBlock("boards_board.html", block, qd)
}
