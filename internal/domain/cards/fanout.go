package cards

import (
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

// Event fan-out support (docs/DESIGN.md "Placements", events): notify.Record writes one activity row and publishes
// to one room per board an event concerns. The cards domain owns placements, so it installs the two hooks Record
// needs: the placements of a card and the card payload as seen on one board.

func init() {
	events.Placements = placementsOf
	events.CardOn = cardOn
}

// placementsOf lists a card's placements, home first, then by placed_at (one query).
func placementsOf(e *ops.Env, cardID string) []events.Placement {
	var out []events.Placement
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT board_id,list_id,is_home FROM card_placements WHERE card_id=? ORDER BY is_home DESC, placed_at, board_id`, cardID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var p events.Placement
		if rows.Scan(&p.BoardID, &p.ListID, &p.IsHome) == nil {
			out = append(out, p)
		}
	}
	return out
}

// placementBoards is the boards a card is on (home first). Writers that delete a card read it BEFORE the write and
// pass it as ops.Event.Boards, since the placements are gone when the event is recorded.
func placementBoards(e *ops.Env, q querier, cardID string) []string {
	return strs(e, q, `SELECT board_id FROM card_placements WHERE card_id=? ORDER BY is_home DESC, placed_at, board_id`, cardID)
}

// cardOn is events.CardOn: the Card payload on boardID with every per-viewer field cleared (watch state, placements,
// the share token: a share link is a capability only editors of the home board see). nil when not placed there.
func cardOn(e *ops.Env, cardID, boardID string) any {
	if boardID == "" {
		return nil
	}
	c, err := loadCardOn(e, e.DB, cardID, boardID)
	if err != nil {
		return nil
	}
	return eventCard(c)
}

// eventCard clears the per-viewer fields of a card that goes into an event payload.
func eventCard(c Card) Card {
	c.IsSubscribed = false
	c.HomeBoardID, c.Placements, c.AlsoOnBoards = "", nil, 0
	c.ShareToken = nil
	return c
}

// labelBoards is the boards among the card's placements that show labelID (its own board, or every board of its
// project for a project label: db.LabelOnBoard): a label event concerns only those, never a board whose vocabulary
// does not have the label. One query.
func labelBoards(e *ops.Env, cardID, labelID string) []string {
	return strs(e, e.DB, `SELECT p.board_id FROM card_placements p JOIN labels l ON l.id=? WHERE p.card_id=? AND `+db.LabelOnBoard("l", "p.board_id")+`
		ORDER BY p.is_home DESC, p.placed_at, p.board_id`, labelID, cardID)
}
