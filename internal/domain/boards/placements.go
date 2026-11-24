package boards

import (
	"context"
	"database/sql"

	"github.com/crueber/weave/internal/db"
)

// Card positions live on card_placements (migration 0404, owned by the cards domain; docs/DESIGN.md "Placements"):
// one row per board a card is on, the home included, mirrored onto cards.board_id/list_id/position/prev_list_id/
// list_changed_at by triggers. A guard trigger refuses any direct write of those cards columns, so every writer here
// (list.sort, moveAll, the archive-after pass, applying a template, the drop position below) updates the placement
// and writes only updated_at (and other card fields) on cards. The boards domain writes the table directly because
// these writers live here and cards imports boards (the same precedent as Landed writing cards.is_closed).

// PlaceCardBetween is PlaceBetween for a card dropped into listID between afterID and beforeID (either may be
// empty): the positions are the placements in that column, the dropped card itself excluded, and a collapsed gap
// rebalances the column's placements. Siblings are looked up in listID only.
func PlaceCardBetween(ctx context.Context, q querier, listID, cardID, afterID, beforeID string) (float64, error) {
	posOf := func(id string) (float64, bool) {
		if id == "" {
			return 0, false
		}
		var p sql.NullFloat64
		if q.QueryRowContext(ctx, `SELECT position FROM card_placements WHERE card_id=? AND list_id=?`, id, listID).Scan(&p) != nil || !p.Valid {
			return 0, false
		}
		return p.Float64, true
	}
	end := func() float64 {
		return appendPos(ctx, q, "card_placements", "list_id=? AND card_id<>?", listID, cardID)
	}
	for attempt := 0; attempt < 2; attempt++ {
		a, hasA := posOf(afterID)
		b, hasB := posOf(beforeID)
		switch {
		case hasA && hasB:
			if b-a >= 1 {
				return (a + b) / 2, nil
			}
		case hasA:
			return a + db.PositionGap, nil
		case hasB:
			if b >= 2 {
				return b / 2, nil
			}
			return b - db.PositionGap, nil
		default:
			return end(), nil
		}
		if err := rebalanceColumn(ctx, q, listID, cardID); err != nil {
			return 0, err
		}
	}
	return end(), nil
}

// rebalanceColumn renumbers the placements of listID (except skipCardID) with PositionGap spacing, keeping order.
func rebalanceColumn(ctx context.Context, q querier, listID, skipCardID string) error {
	rows, err := q.QueryContext(ctx, `SELECT card_id FROM card_placements WHERE list_id=? AND card_id<>? ORDER BY position, card_id`, listID, skipCardID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		if _, err := q.ExecContext(ctx, `UPDATE card_placements SET position=? WHERE card_id=? AND list_id=?`, float64(i+1)*db.PositionGap, id, listID); err != nil {
			return err
		}
	}
	return nil
}
