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
}

// shell builds the page data a fragment needs (permissions, board, labels) without loading any cards.
func shell(c *web.Ctx, boardID string) (*PageData, error) {
	d, err := ops.Typed[BoardDetail](c.Env, "board.get", BoardGetIn{ID: boardID})
	if err != nil {
		return nil, err
	}
	lvl := authz.BoardLevel(c.Env, boardID)
	pd := &PageData{Board: d.Board, Level: levelName(lvl), CanEdit: lvl >= authz.Edit, CanManage: lvl >= authz.Manage, PageURL: "/boards/" + boardID, View: "kanban"}
	if pd.Labels, err = labelsOf(c.Env, boardID); err != nil {
		return nil, err
	}
	return pd, nil
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

func boardOfCard(c *web.Ctx, cardID string) (string, error) {
	var b string
	if err := c.Env.DB.QueryRowContext(c.R.Context(), `SELECT board_id FROM cards WHERE id=?`, cardID).Scan(&b); err != nil {
		return "", ops.NotFound("card")
	}
	if err := authz.RequireBoard(c.Env, b, authz.View); err != nil {
		return "", err
	}
	return b, nil
}

// tilesFor loads the tiles of the given cards (in board order).
func tilesFor(c *web.Ctx, pd *PageData, ids []string) ([]Tile, error) {
	lists, err := listsOf(c.Env, pd.Board.ID, true)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, l := range lists {
		idx[l.ID] = i
	}
	return loadTilesOnly(c.Env, pd.Board, idx, lists, ids)
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
			_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT id FROM cards WHERE list_id=? AND is_draft=0 AND is_template=0 ORDER BY position, id LIMIT 1`, listID).Scan(&first)
		}
		var ids []string
		var failure error
		for _, n := range names {
			form := url.Values{"listId": {listID}, "name": {n}}
			if top && first != "" {
				pos := PlaceBetween(c.R.Context(), c.Env.DB, "cards", "list_id=?", []any{listID}, prev, first)
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
		pd, err := shell(c, l.BoardID)
		if err != nil {
			return err
		}
		tiles, err := tilesFor(c, pd, ids)
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
		d, err := ops.Typed[BoardDetail](c.Env, "board.get", BoardGetIn{ID: boardID})
		if err != nil {
			return err
		}
		pd, err := loadPage(c.Env, d.Board, url.Values{}, false)
		if err != nil {
			return err
		}
		want := map[string]bool{}
		for _, id := range ids {
			want[id] = true
		}
		var cols []Column
		for _, col := range pd.Columns {
			if want[col.List.ID] {
				cols = append(cols, col)
			}
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
		if _, err := CallByForm(c.Env, op, url.Values{"id": {id}, "cardId": {id}, "labelId": {c.R.Form.Get("labelId")}}); err != nil {
			return err
		}
		return renderQuick(c, id, true)
	})
	web.Route("POST /ui/cards/{id}/quick/archive", func(c *web.Ctx) error {
		id := c.R.PathValue("id")
		if _, err := boardOfCard(c, id); err != nil {
			return err
		}
		if _, err := CallByForm(c.Env, "card.archive", url.Values{"id": {id}}); err != nil {
			return err
		}
		trigger(c, "boards:undo", map[string]any{"msg": "Card archived", "url": "/ui/cards/" + id + "/quick/restore", "id": id})
		c.W.WriteHeader(http.StatusNoContent)
		return nil
	})
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
	pd, err := shell(c, boardID)
	if err != nil {
		return err
	}
	tiles, err := tilesFor(c, pd, []string{id})
	if err != nil {
		return err
	}
	if len(tiles) == 0 {
		return ops.NotFound("card")
	}
	return c.RenderBlock("boards_board.html", block, TilesData{P: pd, Tiles: tiles})
}

func renderQuick(c *web.Ctx, id string, oob bool) error {
	boardID, err := boardOfCard(c, id)
	if err != nil {
		return err
	}
	pd, err := shell(c, boardID)
	if err != nil {
		return err
	}
	if !pd.CanEdit {
		return ops.Forbidden()
	}
	tiles, err := tilesFor(c, pd, []string{id})
	if err != nil {
		return err
	}
	if len(tiles) == 0 {
		return ops.NotFound("card")
	}
	qd := QuickData{P: pd, T: tiles[0]}
	for _, l := range pd.Labels {
		on := false
		for _, tl := range tiles[0].Labels {
			on = on || tl.ID == l.ID
		}
		qd.Labels = append(qd.Labels, QuickLabel{Label: l, On: on})
	}
	block := "boards_quick"
	if oob {
		block = "boards_quick_oob"
	}
	return c.RenderBlock("boards_board.html", block, qd)
}
