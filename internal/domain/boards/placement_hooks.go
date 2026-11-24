package boards

import (
	"database/sql"

	"github.com/crueber/weave/internal/ops"
)

// Placement removal hooks, installed by the cards domain (it owns card_placements and the rules of taking a card off
// a board: re-homing, dropping the labels and members no remaining board allows; docs/DESIGN.md "Placements").
// cards imports boards, so the boards writers that destroy placements reach those rules through these variables.

// RemoveFromBoards takes cardID off boardIDs inside tx: when one of them is the card's home, the oldest placement on
// another board (placed_at, then board id) becomes the home first; then the labels and members that no remaining
// board allows are dropped (as a cross-board card.move does). The caller makes sure the card keeps at least one
// placement outside boardIDs. newHome is the new home board, "" when the home did not change. No authz, no events.
var RemoveFromBoards func(e *ops.Env, tx *sql.Tx, cardID string, boardIDs []string) (newHome string, err error)

// BeforeBoardsDelete runs inside the deleting transaction, BEFORE boardIDs are deleted (board.delete,
// project.delete). A card placed both on a doomed board and on a board that survives is taken off the doomed boards
// (RemoveFromBoards: a doomed home moves to its oldest surviving placement) instead of being cascade-deleted with
// its home; a card on doomed boards only is deleted by the cascade as before. after (never nil when err is nil)
// emits the events once the transaction has committed.
var BeforeBoardsDelete func(e *ops.Env, tx *sql.Tx, boardIDs []string) (after func(), err error)

// beforeBoardsDelete calls the hook when it is installed (a binary without the cards domain has no placements to
// keep). Call it inside the transaction that deletes the boards.
func beforeBoardsDelete(e *ops.Env, tx *sql.Tx, boardIDs []string) (func(), error) {
	if BeforeBoardsDelete == nil || len(boardIDs) == 0 {
		return func() {}, nil
	}
	return BeforeBoardsDelete(e, tx, boardIDs)
}

// DeleteBoards deletes boardIDs (and everything on them, by cascade) in one transaction after BeforeBoardsDelete has
// taken the cards that live on other boards off them. del, when not nil, is the delete itself (project.delete deletes
// the project row: its boards cascade from it); nil deletes the boards. It returns the hook's after function, to call
// once the caller's own events are out. No authz: callers check Manage first.
func DeleteBoards(e *ops.Env, boardIDs []string, del func(tx *sql.Tx) error) (after func(), err error) {
	after = func() {}
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		a, err := beforeBoardsDelete(e, tx, boardIDs)
		if err != nil {
			return err
		}
		after = a
		if del != nil {
			return del(tx)
		}
		for _, id := range boardIDs {
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM boards WHERE id=?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return func() {}, err
	}
	return after, nil
}
