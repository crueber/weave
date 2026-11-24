package cards

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

// One card on several boards: the write side (docs/DESIGN.md "Placements").
//
//   - card.place puts a card on another board (a new, non-home placement in one of its columns); card.unplace
//     ("Remove from board") takes it off one board; card.placements lists the boards the caller can see.
//   - card.move moves ONE placement (resolveMove picks it); archive, restore, trash, close and delete are CARD
//     actions (every placement); the closed state is per card.
//   - Leaving a board (unplace, a cross-board move, emptying a trash, deleting a board or project) drops the labels
//     and members no remaining board allows (pruneAfterLeaving), and a home that leaves hands over to the oldest other
//     placement (removePlacementsTx).

type PlaceIn struct {
	ID       string   `path:"id" desc:"card id"`
	ListID   string   `json:"listId" required:"true" desc:"column of the board to place the card on (an active or closing column); the new placement's board is this column's board"`
	BoardID  string   `json:"boardId" desc:"the board to place the card on (optional: it is listId's board; must match when given)"`
	Position *float64 `json:"position" desc:"explicit position in the column; default the end"`
	AfterID  string   `json:"afterId" desc:"place after this card of the column"`
	BeforeID string   `json:"beforeId" desc:"place before this card of the column"`
}

type UnplaceIn struct {
	ID      string `path:"id" desc:"card id"`
	BoardID string `path:"boardId" desc:"the board to remove the card from"`
}

type UnplaceOut struct {
	CardID         string `json:"cardId"`
	BoardID        string `json:"boardId" desc:"the board the card was removed from"`
	NewHomeBoardID string `json:"newHomeBoardId,omitempty" desc:"set when the card's home was that board: its new home (the oldest other placement), when you can see it"`
}

type PlacementsIn struct {
	ID string `path:"id" desc:"card id"`
}

// PlacementInfo is one board a card is on (card.placements: only boards the caller can view).
type PlacementInfo struct {
	BoardID     string   `json:"boardId"`
	BoardName   string   `json:"boardName"`
	ProjectID   string   `json:"projectId"`
	ProjectName string   `json:"projectName"`
	ListID      string   `json:"listId"`
	ListName    string   `json:"listName"`
	ListType    string   `json:"listType" desc:"active | closed (closes cards) | archive | trash"`
	Position    *float64 `json:"position"`
	IsHome      bool     `json:"isHome" desc:"the card's home board (share links, the default context)"`
	PlacedAt    string   `json:"placedAt"`
	CanEdit     bool     `json:"canEdit" desc:"you may move the card on this board or remove it from this board"`
}

type PlacementsOut struct {
	CardID string          `json:"cardId"`
	Items  []PlacementInfo `json:"items" desc:"home first, then by placement time; boards you cannot see are never listed"`
}

func init() {
	ops.Register(ops.Op{Name: "card.place", Summary: "Place a card on another board too (the same card, not a copy): it appears in a column of that board and keeps its place on its other boards. Needs edit access to the card and to that board",
		Method: "POST", Path: "/cards/{id}/placements", UI: "POST /ui/cards/{id}/placements"}, cardPlace)
	ops.Register(ops.Op{Name: "card.unplace", Summary: "Remove a card from one board (\"Remove from board\"): it stays on its other boards; the last board cannot be removed (archive or delete the card instead)",
		Method: "DELETE", Path: "/cards/{id}/placements/{boardId}", UI: "POST /ui/cards/{id}/placements/{boardId}/delete"}, cardUnplace)
	ops.Register(ops.Op{Name: "card.placements", Summary: "List the boards a card is on (only boards you can see), with each board's column",
		Method: "GET", Path: "/cards/{id}/placements", UI: "GET /ui/cards/{id}/placements"}, cardPlacements)

	boards.RemoveFromBoards = removePlacementsTx
	boards.BeforeBoardsDelete = beforeBoardsDelete
}

// ---- the placement core (inside a write transaction; no authz)

// placementRow is one placement of a card with its column type.
type placementRow struct {
	Board, List, ListType string
	Prev                  *string
	IsHome                bool
}

// placementsTx lists every placement of a card (hidden ones included: card-level writes act on all of them), home
// first, then by placed_at and board id.
func placementsTx(ctx context.Context, q querier, cardID string) ([]placementRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.board_id,p.list_id,l.type,p.prev_list_id,p.is_home FROM card_placements p JOIN lists l ON l.id=p.list_id
		WHERE p.card_id=? ORDER BY p.is_home DESC, p.placed_at, p.board_id`, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []placementRow
	for rows.Next() {
		var p placementRow
		if err := rows.Scan(&p.Board, &p.List, &p.ListType, &p.Prev, &p.IsHome); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func placementCount(ctx context.Context, q querier, cardID string) (n int) {
	_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_placements WHERE card_id=?`, cardID).Scan(&n)
	return
}

func placedOn(ctx context.Context, q querier, cardID, boardID string) bool {
	var n int
	_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_placements WHERE card_id=? AND board_id=?`, cardID, boardID).Scan(&n)
	return n > 0
}

func placedInList(ctx context.Context, q querier, cardID, listID string) bool {
	var n int
	_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_placements WHERE card_id=? AND list_id=?`, cardID, listID).Scan(&n)
	return n > 0
}

// pruneAfterLeaving drops the card's labels and members that no board it is still on allows (labelAllowed; a direct
// or group membership of any placement board), after it left a board. With one placement left this is exactly the
// cross-board card.move rule ("labels and members that do not exist on the destination board"). It reads the raw
// card_labels rows (Card.LabelIDs is filtered to one board).
func pruneAfterLeaving(ctx context.Context, tx *sql.Tx, cardID string, memberIDs []string) error {
	onBoards := strs(&ops.Env{Ctx: ctx}, tx, `SELECT board_id FROM card_placements WHERE card_id=?`, cardID)
	labels := strs(&ops.Env{Ctx: ctx}, tx, `SELECT label_id FROM card_labels WHERE card_id=?`, cardID)
	for _, l := range labels {
		keep := false
		for _, b := range onBoards {
			if labelAllowed(ctx, tx, b, l) {
				keep = true
				break
			}
		}
		if !keep {
			if _, err := tx.ExecContext(ctx, `DELETE FROM card_labels WHERE card_id=? AND label_id=?`, cardID, l); err != nil {
				return err
			}
		}
	}
	if memberIDs == nil {
		memberIDs = strs(&ops.Env{Ctx: ctx}, tx, `SELECT user_id FROM card_memberships WHERE card_id=?`, cardID)
	}
	if len(memberIDs) == 0 {
		return nil
	}
	members := map[string]bool{}
	for _, b := range onBoards {
		for u := range boardMemberIDs(ctx, tx, b) {
			members[u] = true
		}
	}
	for _, u := range memberIDs {
		if !members[u] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM card_memberships WHERE card_id=? AND user_id=?`, cardID, u); err != nil {
				return err
			}
		}
	}
	return nil
}

// removePlacementsTx is boards.RemoveFromBoards: it takes cardID off boardIDs inside tx. When the home is among them,
// the oldest placement on another board (placed_at, then board id) becomes the home FIRST (cp_home_rehome moves the
// cards mirror: board, column, position), so the card never has zero or two homes. Then the placements go and the
// labels and members no remaining board allows are dropped. It refuses to remove a card's last placement.
func removePlacementsTx(e *ops.Env, tx *sql.Tx, cardID string, boardIDs []string) (newHome string, err error) {
	doomed := map[string]bool{}
	for _, b := range boardIDs {
		doomed[b] = true
	}
	rows, err := tx.QueryContext(e.Ctx, `SELECT board_id,is_home FROM card_placements WHERE card_id=? ORDER BY placed_at, board_id`, cardID)
	if err != nil {
		return "", err
	}
	home := ""
	var keep, drop []string
	for rows.Next() {
		var b string
		var isHome bool
		if err := rows.Scan(&b, &isHome); err != nil {
			rows.Close()
			return "", err
		}
		if isHome {
			home = b
		}
		if doomed[b] {
			drop = append(drop, b)
		} else {
			keep = append(keep, b)
		}
	}
	rows.Close()
	if len(drop) == 0 {
		return "", nil
	}
	if len(keep) == 0 {
		return "", fmt.Errorf("card %s: refusing to remove its last placement", cardID)
	}
	members := strs(e, tx, `SELECT user_id FROM card_memberships WHERE card_id=?`, cardID)
	if doomed[home] {
		newHome = keep[0]
		if _, err := tx.ExecContext(e.Ctx, `UPDATE card_placements SET is_home=0 WHERE card_id=? AND board_id=?`, cardID, home); err != nil {
			return "", err
		}
		if err := onePlacement(tx.ExecContext(e.Ctx, `UPDATE card_placements SET is_home=1 WHERE card_id=? AND board_id=?`, cardID, newHome)); err != nil {
			return "", err
		}
	}
	for _, b := range drop {
		if _, err := tx.ExecContext(e.Ctx, `DELETE FROM card_placements WHERE card_id=? AND board_id=?`, cardID, b); err != nil {
			return "", err
		}
	}
	if err := pruneAfterLeaving(e.Ctx, tx, cardID, members); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, db.Now(), cardID)
	return newHome, err
}

// beforeBoardsDelete is boards.BeforeBoardsDelete: inside the deleting transaction, every card placed on a doomed
// board AND on a surviving one is taken off the doomed boards (a doomed home moves to the oldest surviving
// placement); cards on doomed boards only are left to the cascade. after announces, on each kept card's (new) home,
// card.updated with changed ["placements"] (plus "home" when it moved).
func beforeBoardsDelete(e *ops.Env, tx *sql.Tx, boardIDs []string) (func(), error) {
	ph := "?"
	args := make([]any, 0, 2*len(boardIDs))
	for i, b := range boardIDs {
		if i > 0 {
			ph += ",?"
		}
		args = append(args, b)
	}
	args = append(args, args...)
	cardIDs := strs(e, tx, `SELECT DISTINCT card_id FROM card_placements WHERE board_id IN (`+ph+`)
		AND card_id IN (SELECT card_id FROM card_placements WHERE board_id NOT IN (`+ph+`)) ORDER BY card_id`, args...)
	type kept struct {
		card, home string
		rehomed    bool
	}
	var out []kept
	for _, id := range cardIDs {
		h, err := removePlacementsTx(e, tx, id, boardIDs)
		if err != nil {
			return nil, err
		}
		k := kept{card: id, home: h, rehomed: h != ""}
		if h == "" {
			if err := tx.QueryRowContext(e.Ctx, `SELECT board_id FROM card_placements WHERE card_id=? AND is_home=1`, id).Scan(&k.home); err != nil {
				return nil, err
			}
		}
		out = append(out, k)
	}
	return func() {
		for _, k := range out {
			changed := []string{"placements"}
			if k.rehomed {
				changed = append(changed, "home")
			}
			emit(e, "cardUpdate", "card.updated", k.home, k.card, map[string]any{"changed": changed})
		}
	}, nil
}

// ---- card.place

func cardPlace(e *ops.Env, in PlaceIn) (Card, error) {
	if _, err := authz.CardEditAccess(e, in.ID, ""); err != nil { // edit the card (on any board the caller sees)
		return Card{}, err
	}
	dst, ltype, err := listRef(e.Ctx, e.DB, in.ListID)
	if err != nil {
		return Card{}, err
	}
	if err := authz.RequireBoard(e, dst, authz.Edit); err != nil {
		return Card{}, err
	}
	if in.BoardID != "" && in.BoardID != dst {
		return Card{}, ops.Invalid("listId is not a column of boardId")
	}
	var draft, tmpl bool
	if err := e.DB.QueryRowContext(e.Ctx, `SELECT is_draft,is_template FROM cards WHERE id=?`, in.ID).Scan(&draft, &tmpl); err != nil {
		return Card{}, ops.NotFound("card")
	}
	if draft {
		return Card{}, ops.Invalid("publish the draft before placing it on another board")
	}
	if tmpl {
		return Card{}, ops.Invalid("card templates cannot be placed on boards")
	}
	var onTemplate int // the target, or a board the card is on, is a board template
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM boards WHERE is_template=1 AND (id=? OR id IN (SELECT board_id FROM card_placements WHERE card_id=?))`, dst, in.ID).Scan(&onTemplate)
	if onTemplate > 0 {
		return Card{}, ops.Invalid("cards on board templates cannot be placed, and cards cannot be placed on a board template")
	}
	if ltype != "active" && ltype != "closed" {
		return Card{}, ops.Invalid("a card can only be placed in an active or closing column")
	}
	var landed boards.LandResult
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		landed = boards.LandResult{}
		if placedOn(e.Ctx, tx, in.ID, dst) {
			return ops.Conflict("the card is already on that board")
		}
		pos, err := positionFor(e.Ctx, tx, in.ListID, in.ID, in.AfterID, in.BeforeID, in.Position)
		if err != nil {
			return err
		}
		now := db.Now()
		_, err = tx.ExecContext(e.Ctx, `INSERT INTO card_placements(card_id,board_id,list_id,position,list_changed_at,is_home,placed_by,placed_at) VALUES(?,?,?,?,?,0,?,?)`,
			in.ID, dst, in.ListID, pos, now, e.User.UserID, now)
		if db.IsUnique(err) {
			return ops.Conflict("the card is already on that board")
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, now, in.ID); err != nil {
			return err
		}
		// a landing: a closing column closes the card (everywhere: closed is per card), an ordinary one re-opens it,
		// and the column's archive-after rule trims THIS board's placements (never the card that just landed)
		landed, err = boards.Landed(e.Ctx, tx, boards.Landing{CardID: in.ID, ListID: in.ListID})
		return err
	})
	if err != nil {
		return Card{}, err
	}
	c, err := loadCardOn(e, e.DB, in.ID, dst)
	if err != nil {
		return c, err
	}
	// a placement event: it concerns the new board (agents listening on its columns see an arrival)
	emitScoped(e, "", events.TypePlace, events.StreamPlaced, dst, in.ID, map[string]any{"card": c, "listId": in.ListID}, []string{dst})
	boards.EmitClosedElsewhere(e, dst, landed)
	boards.EmitAutoArchived(e, landed)
	return cardGet(e, GetIn{ID: in.ID, BoardID: dst})
}

// ---- card.unplace ("Remove from board")

func cardUnplace(e *ops.Env, in UnplaceIn) (UnplaceOut, error) {
	if err := authz.RequireCardOn(e, in.ID, in.BoardID, authz.Edit); err != nil {
		return UnplaceOut{}, err
	}
	out := UnplaceOut{CardID: in.ID, BoardID: in.BoardID}
	var listID, newHome string
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		if placementCount(e.Ctx, tx, in.ID) < 2 {
			return ops.Invalid("this is the only board the card is on: archive or delete the card instead")
		}
		if err := tx.QueryRowContext(e.Ctx, `SELECT list_id FROM card_placements WHERE card_id=? AND board_id=?`, in.ID, in.BoardID).Scan(&listID); err != nil {
			return ops.NotFound("card")
		}
		var err error
		newHome, err = removePlacementsTx(e, tx, in.ID, []string{in.BoardID})
		return err
	})
	if err != nil {
		return out, err
	}
	// the placement is gone: name the board explicitly (a placement event of that board)
	emitScoped(e, "", events.TypeUnplace, events.StreamUnplaced, in.BoardID, in.ID, map[string]any{"listId": listID}, []string{in.BoardID})
	if newHome != "" {
		if c, err := loadCardOn(e, e.DB, in.ID, newHome); err == nil {
			emit(e, "cardUpdate", "card.updated", newHome, in.ID, map[string]any{"card": c, "changed": []string{"home"}})
		}
		if authz.BoardLevel(e, newHome) >= authz.View { // never name a board the caller cannot see
			out.NewHomeBoardID = newHome
		}
	}
	return out, nil
}

// ---- card.placements

func cardPlacements(e *ops.Env, in PlacementsIn) (PlacementsOut, error) {
	a, err := authz.CardAccessFor(e, in.ID, "")
	if err != nil {
		return PlacementsOut{}, err
	}
	out := PlacementsOut{CardID: in.ID, Items: []PlacementInfo{}}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT p.board_id,b.name,b.project_id,COALESCE(pr.name,''),p.list_id,COALESCE(l.name,l.type),l.type,p.position,p.is_home,p.placed_at
		FROM card_placements p JOIN boards b ON b.id=p.board_id JOIN lists l ON l.id=p.list_id LEFT JOIN projects pr ON pr.id=b.project_id
		WHERE p.card_id=? ORDER BY p.is_home DESC, p.placed_at, p.board_id`, in.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var x PlacementInfo
		if err := rows.Scan(&x.BoardID, &x.BoardName, &x.ProjectID, &x.ProjectName, &x.ListID, &x.ListName, &x.ListType, &x.Position, &x.IsHome, &x.PlacedAt); err != nil {
			return out, err
		}
		l, ok := a.Boards[x.BoardID]
		if !ok { // a hidden placement is never named or counted
			continue
		}
		x.CanEdit = l >= authz.Edit
		out.Items = append(out.Items, x)
	}
	return out, rows.Err()
}

// ---- which placement a move moves

// resolveMove picks the placement a move of the card into listID acts on, and checks the caller may make it:
//
//   - boardHint names it (a placement the caller can see, else not_found);
//   - else the card's placement on listID's board (a reorder or a column change within that board);
//   - else the card's only placement the caller can see (a relocation to listID's board);
//   - else Invalid: the card is on several boards, pass boardId.
//
// The caller must be able to edit that placement (authz EditableOn), and a relocation needs Edit on the destination
// board and is a Conflict when the card is already there. dst and dtype are listID's board and column type.
func resolveMove(e *ops.Env, a authz.CardAccess, cardID, boardHint, listID string) (src, dst, dtype string, err error) {
	dst, dtype, err = listRef(e.Ctx, e.DB, listID)
	if err != nil {
		return "", "", "", err
	}
	switch {
	case boardHint != "":
		if _, ok := a.Boards[boardHint]; !ok {
			return "", "", "", ops.NotFound("card")
		}
		src = boardHint
	case placedOn(e.Ctx, e.DB, cardID, dst):
		if _, ok := a.Boards[dst]; !ok { // placed there, but on a board the caller cannot see
			return "", "", "", authz.RequireBoard(e, dst, authz.Edit)
		}
		src = dst
	case len(a.Boards) == 1:
		for b := range a.Boards {
			src = b
		}
	default:
		return "", "", "", ops.Invalid("the card is on several boards: pass boardId (the board whose placement moves)")
	}
	if !a.EditableOn(src) {
		return "", "", "", ops.Forbidden()
	}
	if dst != src {
		if err := authz.RequireBoard(e, dst, authz.Edit); err != nil {
			return "", "", "", err
		}
		if placedOn(e.Ctx, e.DB, cardID, dst) {
			return "", "", "", ops.Conflict("the card is already on that board: move its placement there instead (boardId)")
		}
	}
	return src, dst, dtype, nil
}

// editContext is the board a card-level write that also moves or creates on ONE board acts on (close into a closing
// column, the board of a duplicate, the archive list that must exist): the access's context (the hint, else the home)
// when the caller may edit there, else the first board (by id) of the viewable placements they may edit. A caller
// who edits the card on any board therefore never trips over a read-only context board.
func editContext(a authz.CardAccess) string {
	if a.EditableOn(a.BoardID) {
		return a.BoardID
	}
	ids := make([]string, 0, len(a.Boards))
	for b := range a.Boards {
		ids = append(ids, b)
	}
	sort.Strings(ids)
	for _, b := range ids {
		if a.EditableOn(b) {
			return b
		}
	}
	return a.BoardID
}

// placementBoard is the board of a placement-scoped card write without a list argument (labels): boardHint when the
// caller can see that placement (else not_found), else the only placement the caller can see, else Invalid.
func placementBoard(a authz.CardAccess, boardHint, what string) (string, error) {
	if boardHint != "" {
		if _, ok := a.Boards[boardHint]; !ok {
			return "", ops.NotFound("card")
		}
		return boardHint, nil
	}
	if len(a.Boards) > 1 {
		return "", ops.Invalid("the card is on several boards: pass boardId (" + what + ")")
	}
	return a.BoardID, nil
}

// orderLandings puts landings in closing columns last, so that when one write lands a card in an ordinary column on
// one board and in a closing column on another (a restore, a recurrence spawn), the card ends up closed: a closing
// column closes the card everywhere, whatever the order of the placements. Stable otherwise.
func orderLandings(ctx context.Context, q querier, lands []boards.Landing) []boards.Landing {
	if len(lands) < 2 {
		return lands
	}
	closing := map[string]bool{}
	for _, l := range lands {
		if _, ok := closing[l.ListID]; !ok {
			closing[l.ListID] = listType(ctx, q, l.ListID) == "closed"
		}
	}
	out := append([]boards.Landing(nil), lands...)
	sort.SliceStable(out, func(i, j int) bool { return !closing[out[i].ListID] && closing[out[j].ListID] })
	return out
}
