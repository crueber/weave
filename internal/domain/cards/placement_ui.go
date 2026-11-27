package cards

import (
	"encoding/json"
	"net/http"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// The write half of the "one card on several boards" UI (docs/DESIGN.md "Placements", phase 5b):
//
//   - the "Add to board..." picker (GET /ui/cards/{id}/place-picker): the boards the caller may put the card on
//     (authz.PlaceTargets, the rule of card.place) grouped by project, each with its open and closing columns. The
//     script (cards_place.js) hosts it over the page or the card sheet, filters it as you type and posts the choice
//     to the card.place route; it stays open for the next board.
//   - card.place / card.unplace through their UI routes: _part=modal answers with the card sheet (on the context
//     board, or the card's default board when the context board was the one removed), anything else with 204. Both
//     send HX-Trigger "cards:placement" {cardId, boardId, boardName, placed} and a toast.
//   - GET /ui/cards/{id}/placements (card.placements' UI route): the sheet's Boards row, board crumb and "..." menu
//     as out-of-band fragments, which the sheet swaps in on a live placement event (never touching what is typed).

func init() {
	web.Route("GET /ui/cards/{id}/place-picker", renderPlacePicker)
	web.Route("GET /ui/cards/{id}/placements", func(c *web.Ctx) error {
		// card.placements' UI: the parts of the open card sheet that show the placements (out-of-band swaps)
		if _, err := ops.Typed[PlacementsOut](c.Env, "card.placements", PlacementsIn{ID: c.R.PathValue("id")}); err != nil {
			return err
		}
		d, err := buildModal(c, c.R.PathValue("id"), "view", nil)
		if err != nil {
			return err
		}
		return c.RenderBlock("cards_modal.html", "cards_boards_live", d)
	})
	web.Route("POST /ui/cards/{id}/placements", func(c *web.Ctx) error {
		out, err := c.Do("card.place")
		if err != nil {
			return err
		}
		card := out.(Card)
		name := boardNameOf(card)
		placementTrigger(c, card.ID, card.BoardID, name, true, "Added to "+name)
		if c.R.Form.Get("_part") == "modal" {
			return renderModal(c, card.ID, "view", nil) // the sheet stays on its context board (hx-vals board)
		}
		c.W.WriteHeader(http.StatusNoContent)
		return nil
	})
	web.Route("POST /ui/cards/{id}/placements/{boardId}/delete", func(c *web.Ctx) error {
		id, board := c.R.PathValue("id"), c.R.PathValue("boardId")
		name := ""
		if before, err := cardGetOn(c, id); err == nil {
			for _, p := range before.Placements {
				if p.BoardID == board {
					name = p.BoardName
				}
			}
		}
		if _, err := c.Do("card.unplace"); err != nil {
			return err
		}
		if name == "" {
			name = "the board"
		}
		placementTrigger(c, id, board, name, false, "Removed from "+name)
		if c.R.Form.Get("_part") == "modal" {
			return renderModal(c, id, "view", nil) // the context board may be the one removed: card.get falls back
		}
		c.W.WriteHeader(http.StatusNoContent)
		return nil
	})
}

func boardNameOf(card Card) string {
	for _, p := range card.Placements {
		if p.BoardID == card.BoardID {
			return p.BoardName
		}
	}
	return "the board"
}

// placementTrigger tells the page what changed (cards:placement, for the board page and the sheet) and toasts it.
func placementTrigger(c *web.Ctx, cardID, boardID, boardName string, placed bool, msg string) {
	b, _ := json.Marshal(map[string]any{
		"cards:placement": map[string]any{"cardId": cardID, "boardId": boardID, "boardName": boardName, "placed": placed, "msg": msg},
		"toast":           msg,
	})
	c.W.Header().Set("HX-Trigger", string(b))
}

// ---- the "Add to board..." picker

type pickList struct {
	ID, Name, Type string
}

type pickBoard struct {
	ID, Name    string
	ProjectName string
	Lists       []pickList
	Default     string // the first open column (else the first closing one)
}

type pickProject struct {
	ID, Name string
	Boards   []pickBoard
}

type pickerData struct {
	CardID, CardName, Board string
	Projects                []pickProject
	Count                   int
}

// renderPlacePicker lists the boards the caller may place the card on (authz.PlaceTargets: the same predicates as
// card.place), grouped by project, with their active and closing columns. A constant number of queries (the
// access, the targets, one board query, one column query). Callers who may not edit the card get not_found or
// forbidden, as from the op.
func renderPlacePicker(c *web.Ctx) error {
	id := c.R.PathValue("id")
	a, err := authz.CardEditAccess(c.Env, id, c.BoardHint())
	if err != nil {
		return err
	}
	var name string
	_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT name FROM cards WHERE id=?`, id).Scan(&name)
	d := pickerData{CardID: id, CardName: name, Board: a.BoardID}
	targets := authz.PlaceTargets(c.Env, id)
	if len(targets) > 0 {
		ph := inPlaceholders(len(targets))
		args := make([]any, len(targets))
		for i, t := range targets {
			args[i] = t
		}
		byID := map[string]*pickBoard{}
		rows, err := c.Env.DB.QueryContext(c.R.Context(), `SELECT b.id,b.name,b.project_id,COALESCE(p.name,'') FROM boards b LEFT JOIN projects p ON p.id=b.project_id WHERE b.id IN (`+ph+`)`, args...)
		if err != nil {
			return err
		}
		type brow struct{ id, name, pid, pname string }
		info := map[string]brow{}
		for rows.Next() {
			var r brow
			if rows.Scan(&r.id, &r.name, &r.pid, &r.pname) == nil {
				info[r.id] = r
			}
		}
		rows.Close()
		var idx = map[string]int{}
		for _, t := range targets { // PlaceTargets' order: project name, then board position
			r, ok := info[t]
			if !ok {
				continue
			}
			i, seen := idx[r.pid]
			if !seen {
				i = len(d.Projects)
				idx[r.pid] = i
				d.Projects = append(d.Projects, pickProject{ID: r.pid, Name: r.pname})
			}
			d.Projects[i].Boards = append(d.Projects[i].Boards, pickBoard{ID: r.id, Name: r.name, ProjectName: r.pname})
		}
		for pi := range d.Projects {
			for bi := range d.Projects[pi].Boards {
				byID[d.Projects[pi].Boards[bi].ID] = &d.Projects[pi].Boards[bi]
			}
		}
		lrows, err := c.Env.DB.QueryContext(c.R.Context(), `SELECT id,board_id,COALESCE(name,type),type FROM lists WHERE board_id IN (`+ph+`) AND type IN ('active','closed') ORDER BY board_id, position`, args...)
		if err != nil {
			return err
		}
		for lrows.Next() {
			var l pickList
			var b string
			if lrows.Scan(&l.ID, &b, &l.Name, &l.Type) != nil {
				continue
			}
			if pb := byID[b]; pb != nil {
				pb.Lists = append(pb.Lists, l)
			}
		}
		lrows.Close()
		for _, pb := range byID {
			for _, l := range pb.Lists {
				if l.Type == "active" {
					pb.Default = l.ID
					break
				}
			}
			if pb.Default == "" && len(pb.Lists) > 0 {
				pb.Default = pb.Lists[0].ID
			}
			d.Count++
		}
	}
	return c.RenderBlock("cards_modal.html", "cards_place_picker", d)
}
