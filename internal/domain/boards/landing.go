package boards

import (
	"context"
	"database/sql"
	"strings"

	"github.com/crueber/weave/internal/db"
)

// Landing is one card arriving in one column. Every path that puts a card into a column (create, move, drop,
// duplicate, from-template, recurrence, inbox publish, list.moveCards, archiving or deleting a column, import,
// templates, a column type switch) reports its cards to Landed as (card, column) pairs, inside its own write
// transaction. Pairs, not "one list + ids", so the helper can later work on per-board placements unchanged.
type Landing struct {
	CardID string
	ListID string
}

// LandResult names the cards whose is_closed Landed changed and the cards it auto-archived, so callers can emit
// events after the commit (EmitAutoArchived for the archived ones).
type LandResult struct {
	Closed   []string      // now closed (landed in a closed-type column)
	Reopened []string      // now open (landed in an active column)
	Archived []AutoArchive // one entry per column the archive-after rule trimmed
}

// AutoArchive is one pass of a column's archive-after rule: CardIDs moved from ListID to the board's ArchiveID
// (oldest first) so that the column keeps Keep cards.
type AutoArchive struct {
	BoardID   string
	ListID    string
	ArchiveID string
	Keep      int
	CardIDs   []string
}

// ArchivedCount is the number of cards auto-archived in all columns.
func (r LandResult) ArchivedCount() int {
	n := 0
	for _, a := range r.Archived {
		n += len(a.CardIDs)
	}
	return n
}

// Add appends another result (several landings in one transaction).
func (r *LandResult) Add(o LandResult) {
	r.Closed = append(r.Closed, o.Closed...)
	r.Reopened = append(r.Reopened, o.Reopened...)
	r.Archived = append(r.Archived, o.Archived...)
}

// Changed is every card whose closed state Landed flipped.
func (r LandResult) Changed() []string {
	return append(append([]string{}, r.Closed...), r.Reopened...)
}

// ClosedAfterLanding is the one rule for a card's closed state when it lands in a column of type listType:
//
//	closed          -> closed (the column closes cards)
//	active          -> open   (today's behaviour: leaving a closed column, or moving between columns, re-opens)
//	archive, trash  -> unchanged (archiving or trashing a closed card keeps its check mark)
//
// Restoring from the archive or trash is a landing in the destination column, so the destination decides.
func ClosedAfterLanding(listType string, wasClosed bool) bool {
	switch listType {
	case "closed":
		return true
	case "active":
		return false
	}
	return wasClosed
}

// relandList lands every card already in a column again (after its type changed), inside tx.
func relandList(ctx context.Context, tx *sql.Tx, listID string) (LandResult, error) {
	// every placement in the column (a card placed here from another board lands here too)
	rows, err := tx.QueryContext(ctx, `SELECT c.id FROM card_placements p JOIN cards c ON c.id=p.card_id
		WHERE p.list_id=? AND c.is_draft=0 AND c.is_template=0 ORDER BY p.position, c.id`, listID)
	if err != nil {
		return LandResult{}, err
	}
	var lands []Landing
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return LandResult{}, err
		}
		lands = append(lands, Landing{CardID: id, ListID: listID})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return LandResult{}, err
	}
	return land(ctx, tx, landOpts{}, lands...) // a type switch does not change how many cards the column holds: no trim
}

// landChunk bounds the number of ids in one statement (SQLite variable limit; imports land thousands of cards).
const landChunk = 500

// Landed runs the landing rules for cards that have just been written into a column. It must run inside the
// writing transaction, after the card rows carry their new list_id. Drafts and card templates are never touched,
// and nothing is written for archive/trash columns.
//
//  1. The closed state: ClosedAfterLanding. One query per distinct column plus one UPDATE per chunk of cards whose
//     state actually changes; only rows whose value differs are written.
//  2. Archive after N (lists.archive_after): when the column now holds more than N cards, the ones that have been in
//     it longest go to the board's archive (see trimColumn: the cards that just landed go last, claimed cards are
//     skipped). Because this runs in the landing's own BEGIN IMMEDIATE transaction the count is exact and two
//     concurrent landings serialise.
//
// Callers emit the result after the commit: their own card events, plus EmitAutoArchived for result.Archived.
func Landed(ctx context.Context, tx *sql.Tx, lands ...Landing) (LandResult, error) {
	return land(ctx, tx, landOpts{trim: true}, lands...)
}

// LandedKeepClosed is the landing of a bulk re-layout (board.template.apply moves every card of a board into the
// first column of the template): a card that was closed stays closed (with its check mark) even though the column is
// an ordinary one, and the archive-after rule does not run (the apply decides about the target's cap itself, see
// docs/DESIGN.md "Board templates"). A closing column still closes the cards that land in it.
func LandedKeepClosed(ctx context.Context, tx *sql.Tx, lands ...Landing) (LandResult, error) {
	return land(ctx, tx, landOpts{keepClosed: true}, lands...)
}

// landOpts tunes land: trim runs the archive-after rule; keepClosed leaves closed cards closed in an active column.
type landOpts struct {
	trim, keepClosed bool
}

func land(ctx context.Context, tx *sql.Tx, o landOpts, lands ...Landing) (LandResult, error) {
	var res LandResult
	byList := map[string][]string{}
	var order []string
	for _, l := range lands {
		if l.CardID == "" || l.ListID == "" {
			continue
		}
		if _, ok := byList[l.ListID]; !ok {
			order = append(order, l.ListID)
		}
		byList[l.ListID] = append(byList[l.ListID], l.CardID)
	}
	for _, listID := range order {
		var typ string
		var keep sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT type,archive_after FROM lists WHERE id=?`, listID).Scan(&typ, &keep); err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return res, err
		}
		var want bool
		switch typ {
		case "closed", "active":
			want = ClosedAfterLanding(typ, false)
		default: // archive, trash: keep
			continue
		}
		if !(o.keepClosed && !want) { // keepClosed never re-opens
			if err := closeLanded(ctx, tx, listID, want, byList[listID], &res); err != nil {
				return res, err
			}
		}
		if o.trim && keep.Valid && keep.Int64 > 0 {
			a, err := trimColumn(ctx, tx, listID, int(keep.Int64), byList[listID])
			if err != nil {
				return res, err
			}
			if len(a.CardIDs) > 0 {
				res.Archived = append(res.Archived, a)
			}
		}
	}
	return res, nil
}

// closeLanded writes the closed state want on the landed cards of one column (only rows whose value differs).
func closeLanded(ctx context.Context, tx *sql.Tx, listID string, want bool, ids []string, res *LandResult) error {
	now := db.Now()
	for len(ids) > 0 {
		n := min(len(ids), landChunk)
		chunk := ids[:n]
		ids = ids[n:]
		var q string
		var args []any
		if want {
			args = make([]any, 0, n+2)
			args = append(args, now, listID)
			q = `UPDATE cards SET is_closed=1, closed_at=COALESCE(closed_at,?) WHERE id IN (SELECT card_id FROM card_placements WHERE list_id=?) AND is_draft=0 AND is_template=0 AND is_closed=0
				AND id IN (?` + strings.Repeat(",?", n-1) + `) RETURNING id`
		} else {
			args = make([]any, 0, n+1)
			args = append(args, listID)
			q = `UPDATE cards SET is_closed=0, closed_at=NULL WHERE id IN (SELECT card_id FROM card_placements WHERE list_id=?) AND is_draft=0 AND is_template=0 AND is_closed=1
				AND id IN (?` + strings.Repeat(",?", n-1) + `) RETURNING id`
		}
		for _, id := range chunk {
			args = append(args, id)
		}
		// closed is per CARD: a card placed in this column (whichever board is its home) takes the column's rule
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if want {
				res.Closed = append(res.Closed, id)
			} else {
				res.Reopened = append(res.Reopened, id)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}
