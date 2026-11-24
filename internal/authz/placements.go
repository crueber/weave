package authz

import (
	"github.com/crueber/weave/internal/ops"
)

// Card access through placements (migration 0404, docs/DESIGN.md "Placements").
//
// A card is placed on one or more boards (card_placements; exactly one placement is the card's home, mirrored on
// cards.board_id/list_id/...). The caller's level on a card is the HIGHEST BoardLevel over the placements they can
// view; placements on boards they cannot view are never counted or named. With a single placement (every card until
// the placement ops exist) this is exactly BoardLevel of the card's board, so nothing changes for single-placement
// cards. All card-access decisions go through cardAccess; RequireCard, RequireCardEdit and Card are thin wrappers.

// CardAccess is the caller's access to one card.
type CardAccess struct {
	CardID string
	// HomeBoardID is the card's home board, or "" when the caller cannot view the home placement (a hidden
	// placement is never named).
	HomeBoardID string
	// BoardID is the context placement: the hint when the card is placed there and the caller can view it, else the
	// home when viewable, else the viewable placement with the highest level, then the oldest (placed_at, board id).
	BoardID string
	// Level is the maximum BoardLevel over the placements the caller can view (None when there is none).
	Level Level
	// Boards maps every viewable placement's board to the caller's level there.
	Boards map[string]Level
	// WorkerMember is the narrow worker rule (role worker, at least View on a placement, member of the card), which
	// lets a worker with a viewer membership edit the card's own fields. Only RequireCardEdit and Card.CanEdit read
	// it; it is NOT folded into Level, because claims and the cardextras ops do not follow the worker rule. It is
	// computed only when it can matter (Level is View or Comment).
	WorkerMember bool
}

// cardAccess loads the placements of cardID (one query) and the caller's levels on their boards (BoardLevel for one
// placement, BoardLevels otherwise: a constant number of queries). exists is false when the card has no placement
// (no such card).
func cardAccess(e *ops.Env, cardID, boardHint string) (a CardAccess, exists bool) {
	a = CardAccess{CardID: cardID, Boards: map[string]Level{}}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT board_id,is_home FROM card_placements WHERE card_id=? ORDER BY placed_at, board_id`, cardID)
	if err != nil {
		return a, false
	}
	var order []string
	home := ""
	for rows.Next() {
		var b string
		var isHome bool
		if rows.Scan(&b, &isHome) != nil {
			continue
		}
		order = append(order, b)
		if isHome {
			home = b
		}
	}
	rows.Close()
	if len(order) == 0 {
		return a, false
	}
	var levels map[string]Level
	if len(order) == 1 {
		levels = map[string]Level{order[0]: BoardLevel(e, order[0])}
	} else {
		levels = BoardLevels(e, order)
	}
	decide(&a, order, home, levels, boardHint)
	if a.Level == None {
		return a, true
	}
	if a.Level < Edit && e.User != nil && e.User.Role == "worker" {
		var n int
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM card_memberships WHERE card_id=? AND user_id=?`, cardID, e.User.UserID).Scan(&n)
		a.WorkerMember = n > 0
	}
	return a, true
}

// decide fills a's Level, Boards, HomeBoardID and BoardID from the card's placements (order: oldest first, by
// placed_at then board id), its home board and the caller's levels on those boards. It is the one selection rule of
// cardAccess and CardAccesses, so the single and the bulk form cannot drift.
func decide(a *CardAccess, order []string, home string, levels map[string]Level, hint string) {
	best := ""
	for _, b := range order { // oldest first, so ties keep the oldest placement
		l := levels[b]
		if l == None {
			continue
		}
		a.Boards[b] = l
		if l > a.Level {
			a.Level, best = l, b
		}
	}
	if a.Level == None {
		return
	}
	if _, ok := a.Boards[home]; ok {
		a.HomeBoardID = home
	}
	switch {
	case hint != "" && a.Boards[hint] != None:
		a.BoardID = hint
	case a.HomeBoardID != "":
		a.BoardID = a.HomeBoardID
	default:
		a.BoardID = best
	}
}

// CardAccessFor returns the caller's access to the card, with boardHint as the preferred context placement (""
// for the default). not_found when the card does not exist or the caller can view none of its placements.
func CardAccessFor(e *ops.Env, cardID, boardHint string) (CardAccess, error) {
	a, exists := cardAccess(e, cardID, boardHint)
	if !exists || a.Level == None {
		return CardAccess{CardID: cardID}, ops.NotFound("card")
	}
	return a, nil
}

// CardEditAccess is RequireCardEdit returning the whole CardAccess (boardHint: the preferred context placement, ""
// for the default): not_found when the caller cannot view the card, forbidden when they may not edit it (Edit on a
// viewable placement, or the worker rule). The card-level writes (archive, restore, trash, close, delete, place...)
// use it; a write that changes ONE placement also asks EditableOn for that placement's board.
func CardEditAccess(e *ops.Env, cardID, boardHint string) (CardAccess, error) {
	a, _ := cardAccess(e, cardID, boardHint)
	if a.Level == None {
		return CardAccess{CardID: cardID}, ops.NotFound("card")
	}
	if !cardEditable(a) {
		return a, ops.Forbidden()
	}
	return a, nil
}

// EditableOn reports whether the caller may change the card's placement on boardID (move or reorder it there,
// toggle that board's labels): Edit on that board, or the worker rule (a worker who is a member of the card) with
// View there. False for a placement the caller cannot view. With one placement it equals CardCaps.CanEdit.
func (a CardAccess) EditableOn(boardID string) bool {
	l, ok := a.Boards[boardID]
	if !ok {
		return false
	}
	return l >= Edit || (l >= View && a.WorkerMember)
}

// RequireCardOn is the check of a placement-scoped action (move, archive, restore, close or reorder within one
// board, that board's labels and fields): the card must be placed on boardID and the caller's BoardLevel there must
// be at least min. not_found when the placement does not exist or the board is not viewable, forbidden below min.
func RequireCardOn(e *ops.Env, cardID, boardID string, min Level) error {
	var n int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM card_placements WHERE card_id=? AND board_id=?`, cardID, boardID).Scan(&n)
	if n == 0 {
		return ops.NotFound("card")
	}
	l := BoardLevel(e, boardID)
	if l == None {
		return ops.NotFound("card")
	}
	if l < min {
		return ops.Forbidden()
	}
	return nil
}

// CardViewers returns, for each user among userIDs who can view the card, the boards of the placements they can
// view (home first, then by placed_at), in ONE query whatever the number of users and placements. Same rules as
// ViewersAmong (stored account: exists, not deleted, not deactivated; then a project manager, a direct or group
// board membership, or a public board), applied per placement board. Users who can view no placement are absent.
func CardViewers(e *ops.Env, cardID string, userIDs []string) map[string][]string {
	out := map[string][]string{}
	if len(userIDs) == 0 {
		return out
	}
	in, args := placeholders(userIDs)
	q := `SELECT u.id, p.board_id FROM card_placements p JOIN boards b ON b.id=p.board_id JOIN users u ON u.id IN (` + in + `)
		WHERE p.card_id=? AND u.deleted_at IS NULL AND u.is_deactivated=0 AND (
			b.is_public=1 OR
			EXISTS(SELECT 1 FROM project_managers pm WHERE pm.project_id=b.project_id AND pm.user_id=u.id) OR
			EXISTS(SELECT 1 FROM board_memberships m WHERE m.board_id=b.id AND
				(m.user_id=u.id OR m.group_id IN (SELECT group_id FROM user_group_members g WHERE g.user_id=u.id))))
		ORDER BY p.is_home DESC, p.placed_at, p.board_id`
	rows, err := e.DB.QueryContext(e.Ctx, q, append(args, cardID)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var u, b string
		if rows.Scan(&u, &b) == nil {
			out[u] = append(out[u], b)
		}
	}
	return out
}

// cardLevelsChunk bounds the number of card ids bound in one placements query.
const cardLevelsChunk = 500

// CardAccesses is CardAccessFor for many cards, for the bulk readers (search, dashboard, Settings > Watching): one
// placements query per 500 cards plus ONE BoardLevels call over the distinct boards, whatever the number of cards
// and placements. hint is the preferred context board for every card ("" = the default rule: the home when
// viewable, else the highest level, then the oldest placement). Cards the caller cannot view (or that do not exist)
// are absent. The worker rule is not computed (WorkerMember stays false: it is an edit rule, not a view rule).
func CardAccesses(e *ops.Env, cardIDs []string, hint string) map[string]CardAccess {
	out := map[string]CardAccess{}
	if len(cardIDs) == 0 {
		return out
	}
	type pl struct {
		order []string
		home  string
	}
	byCard := map[string]*pl{}
	seen := map[string]bool{}
	var boards []string
	for i := 0; i < len(cardIDs); i += cardLevelsChunk {
		in, args := placeholders(cardIDs[i:min(i+cardLevelsChunk, len(cardIDs))])
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT card_id,board_id,is_home FROM card_placements WHERE card_id IN (`+in+`) ORDER BY card_id, placed_at, board_id`, args...)
		if err != nil {
			return out
		}
		for rows.Next() {
			var c, b string
			var home bool
			if rows.Scan(&c, &b, &home) != nil {
				continue
			}
			p := byCard[c]
			if p == nil {
				p = &pl{}
				byCard[c] = p
			}
			p.order = append(p.order, b)
			if home {
				p.home = b
			}
			if !seen[b] {
				seen[b] = true
				boards = append(boards, b)
			}
		}
		rows.Close()
	}
	levels := BoardLevels(e, boards)
	for c, p := range byCard {
		a := CardAccess{CardID: c, Boards: map[string]Level{}}
		decide(&a, p.order, p.home, levels, hint)
		if a.Level != None {
			out[c] = a
		}
	}
	return out
}

// CardLevels is the caller's card level (the maximum over viewable placements, as CardAccessFor) for many cards
// (CardAccesses: constant queries). Cards the caller cannot view (or that do not exist) are absent.
func CardLevels(e *ops.Env, cardIDs []string) map[string]Level {
	out := map[string]Level{}
	for c, a := range CardAccesses(e, cardIDs, "") {
		out[c] = a.Level
	}
	return out
}

// AlsoOn counts, for the cards placed on boardID (only cardIDs when not empty), their OTHER placements on boards the
// caller can view: the data of the "also on N other boards" marker. Placements on boards the caller cannot view are
// never counted. One placements query plus one BoardLevels call (skipped when no card has another placement), so
// the cost is constant in the number of cards and placements. Cards with no other viewable placement are absent.
func AlsoOn(e *ops.Env, boardID string, cardIDs []string) map[string]int {
	out := map[string]int{}
	q := `SELECT o.card_id,o.board_id FROM card_placements p JOIN card_placements o ON o.card_id=p.card_id AND o.board_id<>p.board_id WHERE p.board_id=?`
	args := []any{boardID}
	if len(cardIDs) > 0 {
		in, ids := placeholders(cardIDs)
		q += ` AND p.card_id IN (` + in + `)`
		args = append(args, ids...)
	}
	rows, err := e.DB.QueryContext(e.Ctx, q, args...)
	if err != nil {
		return out
	}
	type pair struct{ card, board string }
	var pairs []pair
	seen := map[string]bool{}
	var boards []string
	for rows.Next() {
		var x pair
		if rows.Scan(&x.card, &x.board) != nil {
			continue
		}
		pairs = append(pairs, x)
		if !seen[x.board] {
			seen[x.board] = true
			boards = append(boards, x.board)
		}
	}
	rows.Close()
	if len(pairs) == 0 {
		return out
	}
	levels := BoardLevels(e, boards)
	for _, x := range pairs {
		if levels[x.board] >= View {
			out[x.card]++
		}
	}
	return out
}

// PlaceTargets is the rule of card.place as data: the boards the caller may put the card on. The card must be
// editable by the caller (CardEditAccess: Edit on a viewable placement, or the worker rule), not a draft or a card
// template, and not on a board template; a target is a board that is not a template, does not hold the card yet, has
// an active or closing column, and where the caller's BoardLevel is at least Edit (any project). Empty when the
// caller may not place the card at all. A constant number of queries: the card facts, one candidate query (boards
// of managed projects and owner/editor memberships, direct or through a group: the only sources of Edit) and one
// BoardLevels call, so the level rule is BoardLevel's own. Order: by project name, then board position.
func PlaceTargets(e *ops.Env, cardID string) []string {
	if e.User == nil {
		return nil
	}
	if _, err := CardEditAccess(e, cardID, ""); err != nil {
		return nil
	}
	var draft, tmpl bool
	var onTemplate int
	if e.DB.QueryRowContext(e.Ctx, `SELECT c.is_draft,c.is_template,(SELECT COUNT(*) FROM card_placements p JOIN boards b ON b.id=p.board_id
		WHERE p.card_id=c.id AND b.is_template=1) FROM cards c WHERE c.id=?`, cardID).Scan(&draft, &tmpl, &onTemplate) != nil || draft || tmpl || onTemplate > 0 {
		return nil
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT b.id FROM boards b LEFT JOIN projects pr ON pr.id=b.project_id
		WHERE b.is_template=0 AND b.id NOT IN (SELECT board_id FROM card_placements WHERE card_id=?1)
		AND EXISTS(SELECT 1 FROM lists l WHERE l.board_id=b.id AND l.type IN ('active','closed'))
		AND (b.project_id IN (SELECT project_id FROM project_managers WHERE user_id=?2) OR b.id IN (SELECT board_id FROM board_memberships
			WHERE role IN ('owner','editor') AND (user_id=?2 OR group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?2))))
		ORDER BY COALESCE(pr.name,''), b.project_id, b.position, b.id`, cardID, e.User.UserID)
	if err != nil {
		return nil
	}
	var cands []string
	for rows.Next() {
		var b string
		if rows.Scan(&b) == nil {
			cands = append(cands, b)
		}
	}
	rows.Close()
	if len(cands) == 0 {
		return nil
	}
	levels := BoardLevels(e, cands)
	out := cands[:0]
	for _, b := range cands {
		if levels[b] >= Edit {
			out = append(out, b)
		}
	}
	return out
}
