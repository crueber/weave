package boards

import (
	"context"
	"database/sql"
	"strings"

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
	return placeBetween(
		func(id string) (float64, bool) {
			if id == "" {
				return 0, false
			}
			var p sql.NullFloat64
			if q.QueryRowContext(ctx, `SELECT position FROM card_placements WHERE card_id=? AND list_id=?`, id, listID).Scan(&p) != nil || !p.Valid {
				return 0, false
			}
			return p.Float64, true
		},
		func() float64 {
			return appendPos(ctx, q, "card_placements", "list_id=? AND card_id<>?", listID, cardID)
		},
		func() error {
			return rebalanceColumn(ctx, q, listID, cardID)
		},
		afterID, beforeID,
	)
}

// rebalanceColumn renumbers the placements of listID (except skipCardID) with PositionGap spacing, keeping order.
func rebalanceColumn(ctx context.Context, q querier, listID, skipCardID string) error {
	run := func(w querier) error {
		rows, err := w.QueryContext(ctx, `SELECT card_id FROM card_placements WHERE list_id=? AND card_id<>? ORDER BY position, card_id`, listID, skipCardID)
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
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := 0; i < len(ids); i += archiveChunk {
			chunk := ids[i:min(i+archiveChunk, len(ids))]
			in := "(?" + strings.Repeat(",?", len(chunk)-1) + ")"
			uargs := make([]any, 0, 1+3*len(chunk))
			for k, id := range chunk {
				uargs = append(uargs, id, float64(i+k+1)*db.PositionGap)
			}
			uargs = append(uargs, listID)
			for _, id := range chunk {
				uargs = append(uargs, id)
			}
			if _, err := w.ExecContext(ctx, `UPDATE card_placements SET position=CASE card_id`+strings.Repeat(" WHEN ? THEN ?", len(chunk))+` END WHERE list_id=? AND card_id IN `+in, uargs...); err != nil {
				return err
			}
		}
		return nil
	}
	if d, ok := q.(*db.DB); ok {
		return d.Tx(ctx, func(tx *sql.Tx) error { return run(tx) })
	}
	return run(q)
}

// placementMove describes one card placement being moved into a target list on a board.
type placementMove struct {
	CardID     string
	FromListID string
	Position   float64
}

// movePlacements moves each placement on boardID to targetListID with its new prev_list_id, position and
// list_changed_at, and stamps cards.updated_at, in chunks of archiveChunk.
func movePlacements(ctx context.Context, tx *sql.Tx, boardID, targetListID, now string, moves []placementMove) error {
	for i := 0; i < len(moves); i += archiveChunk {
		chunk := moves[i:min(i+archiveChunk, len(moves))]
		in := "(?" + strings.Repeat(",?", len(chunk)-1) + ")"
		args := make([]any, 0, 3+5*len(chunk))
		args = append(args, targetListID, now)
		for _, m := range chunk {
			args = append(args, m.CardID, m.FromListID)
		}
		for _, m := range chunk {
			args = append(args, m.CardID, m.Position)
		}
		args = append(args, boardID)
		ids := make([]any, 0, len(chunk))
		for _, m := range chunk {
			ids = append(ids, m.CardID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE card_placements SET list_id=?,list_changed_at=?,prev_list_id=CASE card_id`+
			strings.Repeat(" WHEN ? THEN ?", len(chunk))+` END,position=CASE card_id`+
			strings.Repeat(" WHEN ? THEN ?", len(chunk))+` END WHERE board_id=? AND card_id IN `+in, append(args, ids...)...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET updated_at=? WHERE id IN `+in, append([]any{now}, ids...)...); err != nil {
			return err
		}
	}
	return nil
}
