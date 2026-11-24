package boards

import (
	"encoding/json"

	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// The "also on N other boards" marker of a board tile (docs/DESIGN.md "Placements", the read half of the UI): a
// card placed on several boards shows a small neutral button on its tile (Tile.AlsoOn, which counts only the OTHER
// boards the viewer can see). The button opens this popover, which lists those boards and the card's column on each.
// It is read-only and adds no op: the data is card.get's placements (only boards the caller can view; hidden
// placements are never named or counted), on the board the tile is on (?board=, a hint card.get resolves).

// alsoPlacement is one entry of card.get's placements (the cards domain's Placement; boards cannot import cards).
type alsoPlacement struct {
	BoardID     string `json:"boardId"`
	BoardName   string `json:"boardName"`
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	ListID      string `json:"listId"`
	ListName    string `json:"listName"`
	IsHome      bool   `json:"isHome"`
	ShowProject bool   `json:"-"` // in another project than the board the tile is on (cross-project placement)
}

// AlsoData feeds the popover (block "boards_also_pop").
type AlsoData struct {
	CardID, Name string
	Others       []alsoPlacement // the visible placements other than the tile's board, home first
}

// alsoOn reads card.get on the hinted board and keeps the other placements.
func alsoOn(c *web.Ctx, cardID string) (AlsoData, error) {
	out, err := ops.CallMap(c.Env, "card.get", map[string]any{"id": cardID, "boardId": c.BoardHint()})
	if err != nil {
		return AlsoData{}, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return AlsoData{}, err
	}
	var card struct {
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		BoardID    string          `json:"boardId"`
		Placements []alsoPlacement `json:"placements"`
	}
	if err := json.Unmarshal(raw, &card); err != nil {
		return AlsoData{}, err
	}
	proj := ""
	for _, p := range card.Placements {
		if p.BoardID == card.BoardID {
			proj = p.ProjectID
		}
	}
	d := AlsoData{CardID: card.ID, Name: card.Name, Others: []alsoPlacement{}}
	for _, p := range card.Placements {
		if p.BoardID != card.BoardID {
			p.ShowProject = p.ProjectID != proj
			d.Others = append(d.Others, p)
		}
	}
	return d, nil
}

func init() {
	// A signed-in route (the public board page shows no marker: its tiles open nothing either).
	web.Route("GET /ui/cards/{id}/placements-popover", func(c *web.Ctx) error {
		d, err := alsoOn(c, c.R.PathValue("id"))
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_board.html", "boards_also_pop", d)
	})
}
