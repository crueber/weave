package boards

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// Archive after N (a column setting, lists.archive_after; docs/DESIGN.md "Archive after N").
//
// A column with archive_after = N keeps at most N cards. Whenever a landing takes it over N, the cards that have been
// in the column longest go to the board's archive list, exactly as card.archive moves them (prev_list_id = the column,
// so card.restore brings them back; the closed state is kept). The rule, in one place (planArchive):
//
//   - counted: every card in the column except drafts and card templates (closed cards count: they are in the lane);
//   - oldest first: smallest list_changed_at, ties by position (top of the column first), then id (timestamps have
//     one-second precision and bulk moves stamp one time on a whole batch);
//   - the cards that just landed go last, and the newest min(N, landed) of them are never archived: a single card that
//     lands is never the one that goes; a batch larger than N (list.moveCards, an import) keeps its newest N;
//   - cards with a live claim (an agent is working on them) are skipped: the column may sit above N until the claim
//     ends (card.release and the lease sweeper enforce the rule again, as does the next landing);
//   - cards with a recurrence rule (cards.recurrence set) are skipped too, for now (maintainer decision): archiving one
//     would stop its occurrences. They still count toward the column's total. The occurrence spawn clears the old
//     card's rule in its own transaction, so the next landing (the spawned card itself, when it lands here) may
//     archive it; clearing a rule by hand archives nothing until the next landing or a re-sent setting.

// MaxArchiveAfter caps a column's archive-after setting.
const MaxArchiveAfter = 10000

// checkArchiveAfter validates a requested setting: 0 clears it (nil), 1..MaxArchiveAfter sets it.
func checkArchiveAfter(n int) (*int, error) {
	if n < 0 || n > MaxArchiveAfter {
		return nil, ops.Invalid(fmt.Sprintf("archiveAfter must be between 0 and %d (0 turns auto-archive off)", MaxArchiveAfter))
	}
	if n == 0 {
		return nil, nil
	}
	return &n, nil
}

// archivePlan is what the archive-after rule would do to a column now.
type archivePlan struct {
	Cards            int      // cards counted (not drafts, not card templates)
	Keep             int      // N
	SkippedClaimed   int      // cards over N that stay because they hold a live claim
	SkippedRecurring int      // cards over N that stay because they have a recurrence rule (and no live claim)
	IDs              []string // the cards to archive, oldest first
}

// planArchive computes the cards the rule archives in listID for keep = N, without writing. landed are the cards
// that have just landed (nil for a retroactive pass). It is the single source for the landing step, list.update's
// confirm count and list.archivePreview, so the preview and the real pass cannot drift.
func planArchive(ctx context.Context, q querier, listID string, keep int, landed []string) (archivePlan, error) {
	p := archivePlan{Keep: keep}
	if keep <= 0 {
		return p, nil
	}
	// the column's placements (this board's), oldest in the column first; claims and recurrence are the card's
	rows, err := q.QueryContext(ctx, `SELECT c.id, (c.claimed_by IS NOT NULL AND c.claimed_until IS NOT NULL AND c.claimed_until>=?), COALESCE(c.recurrence,'')<>''
		FROM card_placements p JOIN cards c ON c.id=p.card_id
		WHERE p.list_id=? AND c.is_draft=0 AND c.is_template=0 ORDER BY p.list_changed_at, p.position, c.id`, db.Now(), listID)
	if err != nil {
		return p, err
	}
	type row struct {
		id                 string
		claimed, recurring bool
	}
	isLanded := make(map[string]bool, len(landed))
	for _, id := range landed {
		isLanded[id] = true
	}
	var older, arrived []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.claimed, &r.recurring); err != nil {
			rows.Close()
			return p, err
		}
		if isLanded[r.id] {
			arrived = append(arrived, r)
		} else {
			older = append(older, r)
		}
	}
	if err := rows.Close(); err != nil {
		return p, err
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	p.Cards = len(older) + len(arrived)
	excess := p.Cards - keep
	if excess <= 0 {
		return p, nil
	}
	// the newest min(N, landed) arrivals are protected; earlier arrivals of a batch larger than N may go
	order := append(older, arrived[:len(arrived)-min(keep, len(arrived))]...)
	// A skipped card is counted once, under its first reason (a claimed recurring card counts as claimed). The counts
	// are then clamped to the cards that really stay over N (excess minus the archived ones): skipped cards met after
	// the walk could still have filled the excess are not "over the limit", so the claimed ones are kept first.
	for _, r := range order {
		if len(p.IDs) == excess {
			break
		}
		switch {
		case r.claimed:
			p.SkippedClaimed++
		case r.recurring:
			p.SkippedRecurring++
		default:
			p.IDs = append(p.IDs, r.id)
		}
	}
	stay := excess - len(p.IDs) // the cards over N that stay because of a claim or a recurrence rule
	p.SkippedClaimed = min(p.SkippedClaimed, stay)
	p.SkippedRecurring = min(p.SkippedRecurring, stay-p.SkippedClaimed)
	return p, nil
}

// archiveChunk bounds the number of cards one UPDATE moves (the statement binds three values per card).
const archiveChunk = 500

// applyArchive moves ids (oldest first) from the column to its board's archive list, appended in that order, as
// card.archive would on this board: prev_list_id = the column, list_changed_at = now, is_closed kept. One UPDATE per
// chunk. It moves only the cards' placements IN THIS COLUMN: auto-archive is a column rule of one board, so a card
// placed on other boards stays active there (unlike card.archive, a card-level action that archives every placement).
func applyArchive(ctx context.Context, tx *sql.Tx, listID string, keep int, ids []string) (AutoArchive, error) {
	a := AutoArchive{ListID: listID, Keep: keep}
	if len(ids) == 0 {
		return a, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT board_id FROM lists WHERE id=?`, listID).Scan(&a.BoardID); err != nil {
		return a, err
	}
	arch, err := SystemList(ctx, tx, a.BoardID, "archive") // created on demand
	if err != nil {
		return a, err
	}
	a.ArchiveID = arch
	base := maxPos(ctx, tx, "card_placements", "list_id=?", arch)
	now := db.Now()
	for i := 0; i < len(ids); i += archiveChunk {
		chunk := ids[i:min(i+archiveChunk, len(ids))]
		in := "(?" + strings.Repeat(",?", len(chunk)-1) + ")"
		args := make([]any, 0, 4+3*len(chunk))
		args = append(args, arch, listID, now)
		for k, id := range chunk {
			args = append(args, id, base+float64(i+k+1)*db.PositionGap)
		}
		args = append(args, listID)
		for _, id := range chunk {
			args = append(args, id)
		}
		// the cards' placements in this column move (the mirror updates the cards columns), then updated_at
		if _, err := tx.ExecContext(ctx, `UPDATE card_placements SET list_id=?,prev_list_id=?,list_changed_at=?,position=CASE card_id`+
			strings.Repeat(" WHEN ? THEN ?", len(chunk))+` END WHERE list_id=? AND card_id IN `+in, args...); err != nil {
			return a, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET updated_at=? WHERE id IN `+in, append([]any{now}, args[len(args)-len(chunk):]...)...); err != nil {
			return a, err
		}
	}
	a.CardIDs = ids
	return a, nil
}

// trimColumn applies the archive-after rule to one column inside tx (landed: the cards that just landed).
func trimColumn(ctx context.Context, tx *sql.Tx, listID string, keep int, landed []string) (AutoArchive, error) {
	p, err := planArchive(ctx, tx, listID, keep, landed)
	if err != nil {
		return AutoArchive{ListID: listID, Keep: keep}, err
	}
	return applyArchive(ctx, tx, listID, keep, p.IDs)
}

// EnforceArchiveAfter applies a column's archive-after rule with no new arrival, inside tx: after a claim on one of
// its cards ended (card.release, the lease sweeper). A column without the setting, or an archive/trash list, is left
// alone. Emit the result after the commit with EmitAutoArchived.
func EnforceArchiveAfter(ctx context.Context, tx *sql.Tx, listID string) (LandResult, error) {
	var res LandResult
	var typ string
	var keep sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT type,archive_after FROM lists WHERE id=?`, listID).Scan(&typ, &keep); err != nil {
		if err == sql.ErrNoRows {
			return res, nil
		}
		return res, err
	}
	if !isFinite(typ) || !keep.Valid || keep.Int64 <= 0 {
		return res, nil
	}
	a, err := trimColumn(ctx, tx, listID, int(keep.Int64), nil)
	if err != nil {
		return res, err
	}
	if len(a.CardIDs) > 0 {
		res.Archived = append(res.Archived, a)
	}
	return res, nil
}

// EmitAutoArchived announces what the archive-after rule did, after the commit: one card.archived event per card
// (kind card.archived on cardMove, data auto: "archiveAfter", keep: N), so agents, webhooks and the durable stream see
// every card leave, all marked quiet so watchers get no notification per card; then one list.updated summary per
// column (autoArchived: K, keep: N), which is the column's Activity row and the board page's toast.
func EmitAutoArchived(e *ops.Env, res LandResult) {
	for _, a := range res.Archived {
		if len(a.CardIDs) == 0 {
			continue
		}
		for _, id := range a.CardIDs {
			// this board's placement only: a placement event (events.PlacementScoped; also named explicitly), the
			// card's other boards keep their placements and get only the card.placement.changed nudge
			emitCardOn(e, "cardMove", "card.moved", "card.archived", a.BoardID, id, map[string]any{
				"fromListId": a.ListID, "toListId": a.ArchiveID, "trash": false, "auto": "archiveAfter", "keep": a.Keep, "quiet": true}, []string{a.BoardID})
		}
		emit(e, "listUpdate", "list.updated", a.BoardID, map[string]any{"id": a.ListID, "autoArchived": len(a.CardIDs), "keep": a.Keep})
	}
}

// EmitClosedElsewhere announces a closed-state change caused by a landing on boardID to every board of a card that
// is on several boards (closed is per card, docs/DESIGN.md "Placements"): one quiet card.updated (changed isClosed)
// per such card, card-level so the event fan-out reaches the card's other boards, whose tiles show the new state.
// Single-placement cards get nothing extra (the move event carries the card's new state to its only board), so their
// event stream is unchanged. One placements query for the whole batch.
func EmitClosedElsewhere(e *ops.Env, boardID string, res LandResult) {
	ids := append(append([]string{}, res.Closed...), res.Reopened...)
	if len(ids) == 0 {
		return
	}
	multi := map[string]bool{}
	for i := 0; i < len(ids); i += 500 {
		chunk := ids[i:min(i+500, len(ids))]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		for _, id := range strs(e, `SELECT card_id FROM card_placements WHERE card_id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)
			GROUP BY card_id HAVING COUNT(*) > 1`, args...) {
			multi[id] = true
		}
	}
	for _, set := range []struct {
		ids    []string
		closed bool
	}{{res.Closed, true}, {res.Reopened, false}} {
		for _, id := range set.ids {
			if multi[id] {
				emitCard(e, "cardUpdate", "card.updated", "", boardID, id, map[string]any{"changed": []string{"isClosed"}, "isClosed": set.closed, "auto": "landing", "quiet": true})
			}
		}
	}
}
