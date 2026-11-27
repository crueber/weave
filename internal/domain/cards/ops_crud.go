package cards

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

type IDIn struct {
	ID string `path:"id" desc:"card id"`
}

type ListCardsIn struct {
	BoardID  string `path:"boardId" desc:"board id"`
	ListID   string `query:"listId" desc:"only cards of this list"`
	LabelID  string `query:"labelId" desc:"only cards with this label"`
	MemberID string `query:"memberId" desc:"only cards this user is a member of"`
	Claimed  string `query:"claimed" desc:"true | false | any (default): filter by active claim (expired leases count as unclaimed)"`
	Q        string `query:"q" desc:"substring match on name or description"`
	Limit    int    `query:"limit" desc:"page size (default: all, max 500)"`
	Cursor   string `query:"cursor" desc:"nextCursor of the previous page"`
}

type GetIn struct {
	ID      string `path:"id"`
	BoardID string `query:"boardId" desc:"context board when the card is on several boards: boardId, listId and position (and the labels and board fields shown) describe the card on that board; default: the home board when you can see it, else the board where you have the most access"`
	Full    bool   `query:"full" desc:"true adds tasks, comments (last 50), attachments, custom fields and activity (last 50)"`
}

type CreateIn struct {
	ListID      string   `path:"listId" desc:"list to create the card in (active or closed list)"`
	Name        string   `json:"name" required:"true" desc:"card title"`
	Description string   `json:"description"`
	DueDate     string   `json:"dueDate" desc:"RFC3339, YYYY-MM-DD or YYYY-MM-DDTHH:MM"`
	Position    *float64 `json:"position" desc:"explicit position; default end of list"`
}

type UpdateIn struct {
	ID             string  `path:"id"`
	Name           *string `json:"name"`
	Description    *string `json:"description" desc:"markdown-lite; empty string clears"`
	DueDate        *string `json:"dueDate" desc:"empty string clears"`
	IsDueCompleted *bool   `json:"isDueCompleted"`
}

type MoveIn struct {
	ID       string   `path:"id"`
	ListID   string   `json:"listId" required:"true" desc:"target list (may be on another board of the caller)"`
	BoardID  string   `json:"boardId" desc:"which placement moves when the card is on several boards (the board it moves on or away from); default: its placement on listId's board, else the only board you can see it on"`
	Position *float64 `json:"position" desc:"explicit position"`
	AfterID  string   `json:"afterId" desc:"place after this sibling card"`
	BeforeID string   `json:"beforeId" desc:"place before this sibling card"`
}

type ArchiveIn struct {
	ID    string `path:"id"`
	Trash bool   `json:"trash" desc:"true = move to the trash list, false = archive list"`
}

type DuplicateIn struct {
	ID      string `path:"id"`
	Name    string `json:"name" desc:"name of the copy (default '<name> (copy)')"`
	BoardID string `json:"boardId" desc:"the board to copy the card on when it is on several boards (the copy is on that board only, next to the card); default: its home board when you can see it"`
}

type CloseIn struct {
	ID      string `path:"id"`
	Closed  *bool  `json:"closed" desc:"default true; false re-opens"`
	BoardID string `json:"boardId" desc:"when the card is on several boards: the board whose placement moves into its closing column (or out of it when re-opening); closed is per card either way. Default: its home board when you can edit there; a board you may not edit there falls back to that default"`
}

type CoverIn struct {
	ID           string `path:"id"`
	AttachmentID string `json:"attachmentId" desc:"image attachment of this card; empty removes the cover"`
}

type DueIn struct {
	ID      string `path:"id"`
	DueDate string `json:"dueDate" desc:"empty clears the due date"`
}

type DueCompleteIn struct {
	ID        string `path:"id"`
	Completed *bool  `json:"completed" desc:"default true"`
}

type DueOut struct {
	Card
	NextCardID string `json:"nextCardId,omitempty" desc:"set when completion spawned the next recurring card"`
}

type LocationIn struct {
	ID    string   `path:"id"`
	Lat   *float64 `json:"lat"`
	Lng   *float64 `json:"lng"`
	Name  string   `json:"locationName"`
	Clear bool     `json:"clear" desc:"remove the location"`
}

type ListOut struct {
	Items      []Card `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

type Deleted struct {
	ID string `json:"id"`
}

func init() {
	ops.Register(ops.Op{Name: "card.list", Summary: "List the cards of a board (excludes drafts and templates)", Method: "GET", Path: "/boards/{boardId}/cards",
		UI: "exempt:cards are rendered by the board shell (board/list/grid) and calendar/map views"}, cardList)
	ops.Register(ops.Op{Name: "card.get", Summary: "Get a card with labels, members and claim; full=true adds tasks, comments, attachments, custom fields, activity", Method: "GET", Path: "/cards/{id}",
		UI: "GET /ui/cards/{id}"}, cardGet)
	ops.Register(ops.Op{Name: "card.create", Summary: "Create a card at the end (or given position) of a list", Method: "POST", Path: "/lists/{listId}/cards",
		UI: "POST /ui/lists/{listId}/cards"}, cardCreate)
	ops.Register(ops.Op{Name: "card.update", Summary: "Update card title, description, due date or completion", Method: "PATCH", Path: "/cards/{id}",
		UI: "POST /ui/cards/{id}/update"}, cardUpdate)
	ops.Register(ops.Op{Name: "card.delete", Summary: "Delete a card permanently, from every board (it must be in the trash on every board it is on, or be a draft/template)", Method: "DELETE", Path: "/cards/{id}",
		UI: "POST /ui/cards/{id}/delete"}, cardDelete)
	ops.Register(ops.Op{Name: "card.move", Summary: "Move/reorder a card between lists or boards (applies list-type rules); on a card that is on several boards it moves one board's placement (boardId)", Method: "POST", Path: "/cards/{id}/move",
		UI: "POST /ui/cards/{id}/move"}, func(e *ops.Env, in MoveIn) (Card, error) {
		return doMove(e, in.ID, in.BoardID, in.ListID, in.Position, in.AfterID, in.BeforeID)
	})
	ops.Register(ops.Op{Name: "card.archive", Summary: "Archive (or trash) a card: it moves to the archive (trash) list of every board it is on", Method: "POST", Path: "/cards/{id}/archive",
		UI: "POST /ui/cards/{id}/archive"}, cardArchive)
	ops.Register(ops.Op{Name: "card.restore", Summary: "Restore an archived/trashed card: on every board it is on, it returns to its previous list (or that board's first open list)", Method: "POST", Path: "/cards/{id}/restore",
		UI: "POST /ui/cards/{id}/restore"}, cardRestore)
	ops.Register(ops.Op{Name: "card.duplicate", Summary: "Duplicate a card (labels, members, extras via hooks)", Method: "POST", Path: "/cards/{id}/duplicate",
		UI: "POST /ui/cards/{id}/duplicate"}, cardDuplicate)
	ops.Register(ops.Op{Name: "card.close", Summary: "Close (or re-open) a card (closed is per card, on every board); moves it to the closing list of its board when that board has one", Method: "POST", Path: "/cards/{id}/close",
		UI: "POST /ui/cards/{id}/close"}, cardClose)
	ops.Register(ops.Op{Name: "card.cover.set", Summary: "Set or clear the card cover (an image attachment)", Method: "PUT", Path: "/cards/{id}/cover",
		UI: "POST /ui/cards/{id}/cover"}, cardCover)
	ops.Register(ops.Op{Name: "card.due.set", Summary: "Set or clear the due date", Method: "PUT", Path: "/cards/{id}/due",
		UI: "POST /ui/cards/{id}/due"}, cardDueSet)
	ops.Register(ops.Op{Name: "card.due.complete", Summary: "Mark the due date completed/uncompleted (may spawn a recurring card)", Method: "POST", Path: "/cards/{id}/due/complete",
		UI: "POST /ui/cards/{id}/due/complete"}, cardDueComplete)
	ops.Register(ops.Op{Name: "card.location.set", Summary: "Set or clear the card's map location", Method: "PUT", Path: "/cards/{id}/location",
		UI: "POST /ui/cards/{id}/location"}, cardLocation)
}

func cardGet(e *ops.Env, in GetIn) (Card, error) {
	a, err := authz.CardAccessFor(e, in.ID, in.BoardID)
	if err != nil {
		return Card{}, err
	}
	c, err := loadCardOn(e, e.DB, in.ID, a.BoardID)
	if err != nil {
		return Card{}, err
	}
	if a.HomeBoardID == "" || a.Boards[a.HomeBoardID] < authz.Edit { // a share link is the home board's decision
		c.ShareToken = nil
	}
	c.HomeBoardID, c.Placements = a.HomeBoardID, placementsFor(e, a)
	c.AlsoOnBoards = len(c.Placements) - 1
	out := c
	if !in.Full {
		return out, nil
	}
	// extras come from the cardextras ops (same authz, same shapes as their own endpoints)
	pull := func(op string, args map[string]any) any {
		v, err := ops.CallMap(e, op, args)
		if err != nil {
			return nil
		}
		return v
	}
	out.Tasks = pull("tasklist.list", map[string]any{"cardId": in.ID})
	out.Comments = pull("comment.list", map[string]any{"cardId": in.ID, "limit": 50})
	out.Attachments = pull("attachment.list", map[string]any{"cardId": in.ID})
	out.CustomFields = pull("customfieldgroup.list", map[string]any{"cardId": in.ID, "boardId": a.BoardID})
	out.Activity = pull("activity.card", map[string]any{"cardId": in.ID, "boardId": a.BoardID, "limit": 50})
	return out, nil
}

func cardList(e *ops.Env, in ListCardsIn) (ListOut, error) {
	if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
		return ListOut{}, err
	}
	// the cards placed on this board, each once, with this board's column and position
	q := `SELECT c.id FROM card_placements p JOIN cards c ON c.id=p.card_id WHERE p.board_id=?1 AND c.is_draft=0 AND c.is_template=0`
	args := []any{in.BoardID}
	if in.ListID != "" {
		q += ` AND p.list_id=?`
		args = append(args, in.ListID)
	}
	if in.LabelID != "" { // only a label shown on this board matches (another board's label on the same card does not)
		q += ` AND c.id IN (SELECT cl.card_id FROM card_labels cl JOIN labels l ON l.id=cl.label_id WHERE cl.label_id=? AND ` + db.LabelOnBoard("l", "?1") + `)`
		args = append(args, in.LabelID)
	}
	if in.MemberID != "" {
		q += ` AND c.id IN (SELECT card_id FROM card_memberships WHERE user_id=?)`
		args = append(args, in.MemberID)
	}
	switch strings.ToLower(in.Claimed) {
	case "true", "1":
		q += ` AND c.claimed_by IS NOT NULL AND c.claimed_until>=?`
		args = append(args, db.Now())
	case "false", "0":
		q += ` AND (c.claimed_by IS NULL OR c.claimed_until IS NULL OR c.claimed_until<?)`
		args = append(args, db.Now())
	case "", "any":
	default:
		return ListOut{}, ops.Invalid("claimed must be true, false or any")
	}
	if t := strings.TrimSpace(in.Q); t != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(t) + "%"
		q += ` AND (c.name LIKE ? ESCAPE '\' OR COALESCE(c.description,'') LIKE ? ESCAPE '\')`
		args = append(args, like, like)
	}
	q += ` ORDER BY (SELECT position FROM lists WHERE id=p.list_id), p.list_id, p.position, c.created_at, c.id`
	offset := 0
	if in.Cursor != "" {
		n, err := strconv.Atoi(in.Cursor)
		if err != nil || n < 0 {
			return ListOut{}, ops.Invalid("invalid cursor")
		}
		offset = n
	}
	limit := in.Limit
	if limit < 0 || limit > 500 {
		return ListOut{}, ops.Invalid("limit must be 0..500")
	}
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit+1, offset)
	} else if offset > 0 {
		q += ` LIMIT -1 OFFSET ?`
		args = append(args, offset)
	}
	ids, err := strs(e, e.DB, q, args...)
	if err != nil {
		return ListOut{}, err
	}
	out := ListOut{Items: []Card{}}
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
		out.NextCursor = strconv.Itoa(offset + limit)
	}
	if len(ids) == 0 {
		return out, nil
	}
	also := authz.AlsoOn(e, in.BoardID, ids)
	cards, err := loadCardsOn(e, e.DB, in.BoardID, ids)
	if err != nil {
		return ListOut{}, err
	}
	for i := range cards {
		cards[i].ShareToken = nil
		cards[i].AlsoOnBoards = also[cards[i].ID]
	}
	out.Items = cards
	return out, nil
}

func cardCreate(e *ops.Env, in CreateIn) (Card, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Card{}, ops.Invalid("name is required")
	}
	if err := ops.CheckLen("name", name, ops.MaxCardNameLen); err != nil {
		return Card{}, err
	}
	if len(in.Description) > ops.MaxDescBytes {
		return Card{}, ops.Invalid("description is too long (max 200 KB)")
	}
	boardID, ltype, err := listRef(e.Ctx, e.DB, in.ListID)
	if err != nil {
		return Card{}, err
	}
	if err := authz.RequireBoard(e, boardID, authz.Edit); err != nil {
		return Card{}, err
	}
	if ltype != "active" && ltype != "closed" {
		return Card{}, ops.Invalid("cards can only be created in active or closed lists")
	}
	var due any
	if in.DueDate != "" {
		d, err := normTime(in.DueDate)
		if err != nil {
			return Card{}, err
		}
		due = d
	}
	var desc any
	if in.Description != "" {
		desc = in.Description
	}
	id := db.NewID()
	now := db.Now()
	var landed boards.LandResult
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		pos, err := positionFor(e.Ctx, tx, in.ListID, "", "", "", in.Position)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(e.Ctx, `INSERT INTO cards(id,board_id,list_id,creator_user_id,position,name,description,due_date,list_changed_at,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, id, boardID, in.ListID, e.User.UserID, pos, name, desc, due, now, now)
		if err != nil {
			return err
		}
		if landed, err = boards.Landed(e.Ctx, tx, boards.Landing{CardID: id, ListID: in.ListID}); err != nil {
			return err
		}
		autoWatch(e.Ctx, tx, id, e.User.UserID, "subscribe_own_cards") // "Watch cards I create"
		return nil
	})
	if err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, id)
	if err != nil {
		return c, err
	}
	emit(e, "cardCreate", "card.created", boardID, id, map[string]any{"card": c, "listId": in.ListID})
	boards.EmitAutoArchived(e, landed) // the column's archive-after rule (never the new card itself)
	return c, nil
}

func cardUpdate(e *ops.Env, in UpdateIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	sets, args := []string{}, []any{}
	var prevDue *string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT due_date FROM cards WHERE id=?`, in.ID).Scan(&prevDue)
	changed := []string{} // event Data "changed": which fields this update touched (notify reacts to dueDate)
	if in.Name != nil {
		changed = append(changed, "name")
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			return Card{}, ops.Invalid("name cannot be empty")
		}
		if err := ops.CheckLen("name", n, ops.MaxCardNameLen); err != nil {
			return Card{}, err
		}
		sets, args = append(sets, "name=?"), append(args, n)
	}
	if in.Description != nil {
		if len(*in.Description) > ops.MaxDescBytes {
			return Card{}, ops.Invalid("description is too long (max 200 KB)")
		}
		var d any
		if strings.TrimSpace(*in.Description) != "" {
			d = *in.Description
		}
		sets, args = append(sets, "description=?"), append(args, d)
		changed = append(changed, "description")
	}
	clearingDue := in.DueDate != nil && *in.DueDate == ""
	if clearingDue && in.IsDueCompleted != nil && *in.IsDueCompleted {
		return Card{}, ops.Invalid("cannot mark a cleared due date as completed")
	}
	if in.DueDate != nil {
		if clearingDue {
			sets = append(sets, "due_date=NULL", "is_due_completed=NULL")
			if prevDue != nil {
				changed = append(changed, "dueDate")
			}
		} else {
			d, err := normTime(*in.DueDate)
			if err != nil {
				return Card{}, err
			}
			sets, args = append(sets, "due_date=?"), append(args, d)
			if prevDue == nil || *prevDue != d {
				changed = append(changed, "dueDate")
			}
		}
	}
	if in.IsDueCompleted != nil && !clearingDue {
		sets, args = append(sets, "is_due_completed=?"), append(args, *in.IsDueCompleted)
		changed = append(changed, "isDueCompleted")
	}
	if len(sets) == 0 {
		return loadCardOn(e, e.DB, in.ID, boardID)
	}
	sets, args = append(sets, "updated_at=?"), append(args, db.Now(), in.ID)
	if _, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET `+strings.Join(sets, ",")+` WHERE id=?`, args...); err != nil {
		return Card{}, err
	}
	c, err := loadCardOn(e, e.DB, in.ID, boardID)
	if err == nil {
		emit(e, "cardUpdate", "card.updated", boardID, in.ID, map[string]any{"card": c, "changed": changed})
	}
	return c, err
}

func cardDelete(e *ops.Env, in IDIn) (Deleted, error) {
	a, err := authz.CardEditAccess(e, in.ID, "")
	if err != nil {
		return Deleted{}, err
	}
	boardID := a.BoardID
	// "in the trash" is every placement in a trash list (card.archive trash=true trashes the card on every board)
	state, err := readCardTrashState(e.Ctx, e.DB, in.ID)
	if err != nil {
		return Deleted{}, err
	}
	if state.InTrash < state.Placed && !state.Draft && !state.Template {
		if state.Placed > 1 {
			return Deleted{}, ops.Invalid("card must be in the trash on every board it is on before it can be deleted (trash it with card.archive trash=true)")
		}
		return Deleted{}, ops.Invalid("card must be in the trash list before it can be deleted")
	}
	var gone deletedCardData
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		var deleted bool
		gone, deleted, err = deleteCardTx(e, tx, in.ID, boardID, true)
		if err == nil && !deleted {
			err = ops.Conflict("card changed before it could be deleted")
		}
		return err
	})
	if err != nil {
		return Deleted{}, err
	}
	// a card-level event: it concerns every board the card was on (named, as the placements are gone)
	emitScoped(e, "", "cardDelete", "card.deleted", boardID, in.ID, map[string]any{"name": gone.Name, "listId": gone.ListID}, gone.Boards)
	return Deleted{in.ID}, nil
}

// cardTrashState is the shared card.delete eligibility rule. Query errors are returned so a failed check can never
// turn into permission to delete.
type cardTrashState struct {
	Placed, InTrash int
	Draft, Template bool
}

func readCardTrashState(ctx context.Context, q querier, cardID string) (cardTrashState, error) {
	var s cardTrashState
	err := q.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(l.type='trash'),0) FROM card_placements p JOIN lists l ON l.id=p.list_id WHERE p.card_id=?`, cardID).Scan(&s.Placed, &s.InTrash)
	if err != nil {
		return s, err
	}
	err = q.QueryRowContext(ctx, `SELECT is_draft,is_template FROM cards WHERE id=?`, cardID).Scan(&s.Draft, &s.Template)
	return s, err
}

type deletedCardData struct {
	Name, ListID string
	Boards       []string
}

// deleteCardTx rechecks eligibility inside the write transaction, reads the event data before the placements cascade,
// and deletes the card. allowDraftTemplate preserves card.delete's existing draft/template exception; bulk delete passes
// false and therefore skips anything that is not fully in trash.
func deleteCardTx(e *ops.Env, tx *sql.Tx, cardID, boardID string, allowDraftTemplate bool) (deletedCardData, bool, error) {
	var out deletedCardData
	state, err := readCardTrashState(e.Ctx, tx, cardID)
	if err != nil {
		return out, false, err
	}
	if state.InTrash < state.Placed && !(allowDraftTemplate && (state.Draft || state.Template)) {
		return out, false, nil
	}
	c, err := loadCardOn(e, tx, cardID, boardID)
	if err != nil {
		return out, false, err
	}
	out.Name, out.ListID = c.Name, c.ListID
	out.Boards, err = placementBoards(e, tx, cardID)
	if err != nil {
		return out, false, err
	}
	if _, err := tx.ExecContext(e.Ctx, `DELETE FROM card_editing WHERE card_id=?`, cardID); err != nil {
		return out, false, err
	}
	if _, err := tx.ExecContext(e.Ctx, `DELETE FROM cards WHERE id=?`, cardID); err != nil {
		return out, false, err
	}
	return out, true, nil
}

// doMove moves ONE placement of a card (card.move, the claim/release moves, closing into a closing column): the one
// resolveMove picks (boardHint, else the placement on listID's board, else the only one the caller sees). A reorder
// or a column change stays on its board; a move to another board relocates that placement there (Conflict when the
// card is already on it; the home stays the home). It returns the card as seen on listID's board.
func doMove(e *ops.Env, id, boardHint, listID string, explicit *float64, afterID, beforeID string) (Card, error) {
	a, err := authz.CardEditAccess(e, id, "")
	if err != nil {
		return Card{}, err
	}
	srcBoard, dstBoard, dtype, err := resolveMove(e, a, id, boardHint, listID)
	if err != nil {
		return Card{}, err
	}
	old, err := loadCardOn(e, e.DB, id, srcBoard)
	if err != nil {
		return Card{}, err
	}
	if old.IsTemplate {
		return Card{}, ops.Invalid("templates cannot be moved")
	}
	if old.IsDraft {
		return Card{}, ops.Invalid("publish the draft before moving it")
	}
	var stype string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT type FROM lists WHERE id=?`, old.ListID).Scan(&stype)
	listChanged := old.ListID != listID
	var landed boards.LandResult
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		landed = boards.LandResult{}
		if dstBoard != srcBoard && placedOn(e.Ctx, tx, id, dstBoard) { // placed there meanwhile
			return ops.Conflict("the card is already on that board")
		}
		pos, err := positionFor(e.Ctx, tx, listID, id, afterID, beforeID, explicit)
		if err != nil {
			return err
		}
		now := db.Now()
		// the move is a write of the card's placement on the source board (migration 0404); the cards columns follow
		// the home placement through the mirror triggers, and updated_at stays a card write
		if !listChanged {
			if err := onePlacement(tx.ExecContext(e.Ctx, `UPDATE card_placements SET position=? WHERE card_id=? AND board_id=?`, pos, id, srcBoard)); err != nil {
				return err
			}
			_, err := tx.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, now, id)
			return err
		}
		var prev any // remember where an archived/trashed/closed card came from
		switch dtype {
		case "archive", "trash", "closed":
			if (stype == "archive" || stype == "trash") && old.PrevListID != nil {
				prev = *old.PrevListID
			} else {
				prev = old.ListID
			}
		}
		if err := onePlacement(tx.ExecContext(e.Ctx, `UPDATE card_placements SET board_id=?,list_id=?,position=?,prev_list_id=?,list_changed_at=? WHERE card_id=? AND board_id=?`,
			dstBoard, listID, pos, prev, now, id, srcBoard)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, now, id); err != nil {
			return err
		}
		// the closed state follows the destination (archive/trash keep it), and the destination's archive-after rule
		// runs (restoring into a full column archives its oldest other card); a reorder above never gets here
		if landed, err = boards.Landed(e.Ctx, tx, boards.Landing{CardID: id, ListID: listID}); err != nil {
			return err
		}
		if dstBoard != srcBoard {
			// drop labels and members that no board the card is on now allows (with one placement: the destination's)
			if err := pruneAfterLeaving(e.Ctx, tx, id, old.MemberIDs); err != nil {
				return err
			}
			_, _ = tx.ExecContext(e.Ctx, `DELETE FROM card_editing WHERE card_id=?`, id)
		}
		return nil
	})
	if err != nil {
		return Card{}, err
	}
	c, err := loadCardOn(e, e.DB, id, dstBoard)
	if err != nil {
		return c, err
	}
	if dstBoard != srcBoard {
		// the source half of a relocation concerns the source board only (the card's other boards keep it)
		emitScoped(e, "", "cardDelete", "card.deleted", srcBoard, id, map[string]any{"movedToBoardId": dstBoard, "listId": old.ListID}, []string{srcBoard})
	}
	kind := ""
	switch {
	case !listChanged:
	case (dtype == "archive" || dtype == "trash") && stype != "archive" && stype != "trash":
		kind = "card.archived"
	case (stype == "archive" || stype == "trash") && dtype != "archive" && dtype != "trash":
		kind = "card.restored"
	}
	// one placement moved: a placement event (events.PlacementScoped), recorded on its board only (an archive kind
	// here is that board's placement only); the card's other boards get the card.placement.changed nudge, and a close
	// or re-open it causes still notifies everyone following the card (closed is per card)
	emitKind(e, kind, "cardMove", "card.moved", dstBoard, id, map[string]any{"card": c, "fromListId": old.ListID, "toListId": listID, "fromBoardId": srcBoard, "trash": dtype == "trash"})
	boards.EmitClosedElsewhere(e, dstBoard, landed)
	boards.EmitAutoArchived(e, landed)
	return c, nil
}

func firstList(e *ops.Env, boardID, typ string) string {
	id, _ := firstListQ(e.Ctx, e.DB, boardID, typ)
	return id
}

func firstListQ(ctx context.Context, q querier, boardID, typ string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM lists WHERE board_id=? AND type=? ORDER BY position LIMIT 1`, boardID, typ).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// cardArchive archives (or trashes) the CARD: every placement, on every board (hidden ones included), moves to its
// board's archive (trash) list, remembering its column for card.restore. The context board must have the list (as
// before); another board without one gets it created. A placement already there only moves to the end of it.
func cardArchive(e *ops.Env, in ArchiveIn) (Card, error) {
	a, err := authz.CardEditAccess(e, in.ID, "")
	if err != nil {
		return Card{}, err
	}
	boardID := editContext(a)
	typ := "archive"
	if in.Trash {
		typ = "trash"
	}
	l := firstList(e, boardID, typ)
	if l == "" {
		return Card{}, ops.Invalid("board has no " + typ + " list")
	}
	old, err := loadCardOn(e, e.DB, in.ID, boardID)
	if err != nil {
		return Card{}, err
	}
	if old.IsTemplate {
		return Card{}, ops.Invalid("templates cannot be moved")
	}
	if old.IsDraft {
		return Card{}, ops.Invalid("publish the draft before moving it")
	}
	var stype string                // the context placement's column type (the event describes it)
	moves := map[string][2]string{} // every board's placement move (events.PlacementLists)
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		clear(moves)
		pls, err := placementsTx(e.Ctx, tx, in.ID)
		if err != nil {
			return err
		}
		now := db.Now()
		for _, p := range pls {
			target := l
			if p.Board != boardID {
				if target, err = boards.SystemList(e.Ctx, tx, p.Board, typ); err != nil {
					return err
				}
			} else {
				stype = p.ListType
			}
			pos, err := positionFor(e.Ctx, tx, target, in.ID, "", "", nil)
			if err != nil {
				return err
			}
			moves[p.Board] = [2]string{p.List, target}
			if p.List == target { // already there: to the end of it
				if err := onePlacement(tx.ExecContext(e.Ctx, `UPDATE card_placements SET position=? WHERE card_id=? AND board_id=?`, pos, in.ID, p.Board)); err != nil {
					return err
				}
				continue
			}
			prev := p.List // remember where it came from (an archived card trashed keeps its original column)
			if (p.ListType == "archive" || p.ListType == "trash") && p.Prev != nil {
				prev = *p.Prev
			}
			if err := onePlacement(tx.ExecContext(e.Ctx, `UPDATE card_placements SET list_id=?,position=?,prev_list_id=?,list_changed_at=? WHERE card_id=? AND board_id=?`,
				target, pos, prev, now, in.ID, p.Board)); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, now, in.ID)
		return err // archive/trash lists keep the closed state: no landing
	})
	if err != nil {
		return Card{}, err
	}
	c, err := loadCardOn(e, e.DB, in.ID, boardID)
	if err != nil {
		return c, err
	}
	kind := ""
	if old.ListID != l && stype != "archive" && stype != "trash" {
		kind = "card.archived"
	}
	// a card-level event (events.CardLevel): the fan-out reaches every board the card is on, each copy with that
	// board's own placement (its archive or trash column), even when the context placement was already there
	emitKind(e, kind, "cardMove", "card.moved", boardID, in.ID, map[string]any{"card": c, "fromListId": old.ListID, "toListId": l, "fromBoardId": boardID, "trash": in.Trash, events.CardLevel: true, events.PlacementLists: moves})
	return c, nil
}

// cardRestore restores the CARD: every placement in an archive or trash list (on every board, hidden ones included)
// returns to its previous column when that column is still an open or closing column of its board, else to that
// board's first open column. Placements that are not archived stay where they are (an auto-archive archives one
// board's placement only). Landings in closing columns run last: one closing column closes the card.
func cardRestore(e *ops.Env, in IDIn) (Card, error) {
	a, err := authz.CardEditAccess(e, in.ID, "")
	if err != nil {
		return Card{}, err
	}
	boardID := editContext(a)
	if _, err := loadCardOn(e, e.DB, in.ID, boardID); err != nil {
		return Card{}, err
	}
	var res restoreTxResult
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		var restored bool
		res, restored, err = restoreCardTx(e, tx, in.ID, boardID)
		if err == nil && !restored {
			err = ops.Invalid("card is not archived or trashed")
		}
		return err
	})
	if err != nil {
		return Card{}, err
	}
	ev, err := loadCardOn(e, e.DB, in.ID, res.EventBoard)
	if err != nil {
		return ev, err
	}
	moves := make(map[string][2]string, len(res.Moves))
	for _, m := range res.Moves {
		moves[m.Board] = [2]string{m.From, m.Target}
	}
	// a card-level event (events.CardLevel): every board the card is on hears it with its own placement
	emitKind(e, "card.restored", "cardMove", "card.moved", res.EventBoard, in.ID, map[string]any{"card": ev, "fromListId": res.EventFrom, "toListId": res.EventTo, "fromBoardId": res.EventBoard, "trash": false, events.CardLevel: true, events.PlacementLists: moves})
	boards.EmitClosedElsewhere(e, res.EventBoard, res.Landed)
	boards.EmitAutoArchived(e, res.Landed)
	return loadCardOn(e, e.DB, in.ID, boardID)
}

type restoreMove struct {
	Board, From, Target string
}

type restoreTxResult struct {
	Moves                          []restoreMove
	Landed                         boards.LandResult
	EventBoard, EventFrom, EventTo string
}

// restoreCardTx is the shared single-card/bulk restore transaction. It re-reads every placement in tx, restores all
// archived/trashed placements (hidden boards included), and calls Landed once for this card. The fallback is exactly
// card.restore's first active column: a valid previous closed column is allowed, but an unrelated closed column is not.
func restoreCardTx(e *ops.Env, tx *sql.Tx, cardID, boardID string) (restoreTxResult, bool, error) {
	var out restoreTxResult
	var draft, tmpl bool
	if err := tx.QueryRowContext(e.Ctx, `SELECT is_draft,is_template FROM cards WHERE id=?`, cardID).Scan(&draft, &tmpl); err != nil {
		return out, false, err
	}
	pls, err := placementsTx(e.Ctx, tx, cardID)
	if err != nil {
		return out, false, err
	}
	type planned struct {
		p      placementRow
		target string
	}
	var todo []planned
	for _, p := range pls {
		if p.ListType != "archive" && p.ListType != "trash" {
			continue
		}
		target, err := restoreTarget(e.Ctx, tx, p.Board, boardID, p.Prev)
		if err != nil {
			return out, false, err
		}
		todo = append(todo, planned{p, target})
	}
	if len(todo) == 0 {
		return out, false, nil
	}
	if tmpl {
		return out, false, ops.Invalid("templates cannot be moved")
	}
	if draft {
		return out, false, ops.Invalid("publish the draft before moving it")
	}
	now := db.Now()
	var lands []boards.Landing
	for _, r := range todo {
		pos, err := positionFor(e.Ctx, tx, r.target, cardID, "", "", nil)
		if err != nil {
			return out, false, err
		}
		var prev any // restoring into a closing column remembers where the card was before it was archived
		if _, lt, _ := listRef(e.Ctx, tx, r.target); lt == "closed" {
			prev = r.p.List
			if r.p.Prev != nil {
				prev = *r.p.Prev
			}
		}
		if err := onePlacement(tx.ExecContext(e.Ctx, `UPDATE card_placements SET list_id=?,position=?,prev_list_id=?,list_changed_at=? WHERE card_id=? AND board_id=?`,
			r.target, pos, prev, now, cardID, r.p.Board)); err != nil {
			return out, false, err
		}
		lands = append(lands, boards.Landing{CardID: cardID, ListID: r.target})
		out.Moves = append(out.Moves, restoreMove{Board: r.p.Board, From: r.p.List, Target: r.target})
	}
	if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, now, cardID); err != nil {
		return out, false, err
	}
	if out.Landed, err = boards.Landed(e.Ctx, tx, orderLandings(e.Ctx, tx, lands)...); err != nil {
		return out, false, err
	}
	// The authorized acting board is the origin when its placement was archived; otherwise use the first restored one.
	out.EventBoard, out.EventFrom, out.EventTo = todo[0].p.Board, todo[0].p.List, todo[0].target
	for _, m := range out.Moves {
		if m.Board == boardID {
			out.EventBoard, out.EventFrom, out.EventTo = m.Board, m.From, m.Target
			break
		}
	}
	return out, true, nil
}

// restoreTarget is the shared restore destination rule: a previous column is valid only on the same board and only
// when active or closed; otherwise use that board's first active column. It never falls back to an unrelated closed
// column. contextBoard selects the existing single-card error wording.
func restoreTarget(ctx context.Context, q querier, boardID, contextBoard string, prev *string) (string, error) {
	target := ""
	if prev != nil {
		var pt, pb string
		err := q.QueryRowContext(ctx, `SELECT type,board_id FROM lists WHERE id=?`, *prev).Scan(&pt, &pb)
		if err != nil && err != sql.ErrNoRows {
			return "", err
		}
		if err == nil && pb == boardID && (pt == "active" || pt == "closed") {
			target = *prev
		}
	}
	if target != "" {
		return target, nil
	}
	target, err := firstListQ(ctx, q, boardID, "active")
	if err != nil || target != "" {
		return target, err
	}
	if boardID == contextBoard {
		return "", ops.Invalid("board has no active list to restore into")
	}
	return "", ops.Invalid("a board the card is on has no active list to restore into")
}

// cardClose closes (or re-opens) the CARD (closed is per card: it shows closed on every board). On the context board
// it moves as before: into that board's first closing column when it has one, out of a closing column (to its
// previous or the first open column) when re-opening; otherwise the flag is set in place.
func cardClose(e *ops.Env, in CloseIn) (Card, error) {
	a, err := authz.CardEditAccess(e, in.ID, in.BoardID)
	if err != nil {
		return Card{}, err
	}
	if in.BoardID != "" && a.BoardID != in.BoardID { // not a placement the caller can see
		return Card{}, ops.NotFound("card")
	}
	// the placement that moves: the board given (the card sheet's context) when the caller may change it there, else
	// the default (closing is a card-level write: Edit on any placement is enough, the flag is the card's)
	boardID := in.BoardID
	switch {
	case boardID != "" && a.EditableOn(boardID):
	case a.HomeBoardID != "" && a.EditableOn(a.HomeBoardID):
		boardID = a.HomeBoardID
	default:
		boardID = editContext(a)
	}
	closing := in.Closed == nil || *in.Closed
	c, err := loadCardOn(e, e.DB, in.ID, boardID)
	if err != nil {
		return c, err
	}
	if c.IsDraft {
		return c, ops.Invalid("publish the draft before closing it")
	}
	if c.IsTemplate {
		return c, ops.Invalid("templates cannot be closed")
	}
	var stype string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT type FROM lists WHERE id=?`, c.ListID).Scan(&stype)
	if stype == "archive" || stype == "trash" {
		return c, ops.Invalid("restore the card before closing it")
	}
	if closing {
		if cl := firstList(e, boardID, "closed"); cl != "" && stype != "closed" {
			return doMove(e, in.ID, boardID, cl, nil, "", "")
		}
	} else if stype == "closed" {
		target := ""
		if c.PrevListID != nil {
			var pt string
			if e.DB.QueryRowContext(e.Ctx, `SELECT type FROM lists WHERE id=? AND board_id=?`, *c.PrevListID, boardID).Scan(&pt) == nil && pt == "active" {
				target = *c.PrevListID
			}
		}
		if target == "" {
			target = firstList(e, boardID, "active")
		}
		if target != "" {
			return doMove(e, in.ID, boardID, target, nil, "", "")
		}
	}
	was := c.IsClosed
	if _, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET is_closed=?,updated_at=? WHERE id=?`, closing, db.Now(), in.ID); err != nil {
		return c, err
	}
	c, _ = loadCardOn(e, e.DB, in.ID, boardID)
	changed := []string{}
	if was != closing {
		changed = append(changed, "isClosed")
	}
	emit(e, "cardUpdate", "card.updated", boardID, in.ID, map[string]any{"card": c, "changed": changed})
	return c, nil
}

func cardCover(e *ops.Env, in CoverIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	var v any
	if in.AttachmentID != "" {
		var n int
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM attachments WHERE id=? AND card_id=? AND is_image=1`, in.AttachmentID, in.ID).Scan(&n)
		if n == 0 {
			return Card{}, ops.Invalid("attachment must be an image attached to this card")
		}
		v = in.AttachmentID
	}
	if _, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET cover_attachment_id=?,updated_at=? WHERE id=?`, v, db.Now(), in.ID); err != nil {
		return Card{}, err
	}
	c, err := loadCardOn(e, e.DB, in.ID, boardID)
	if err == nil {
		emit(e, "cardUpdate", "card.updated", boardID, in.ID, map[string]any{"card": c})
	}
	return c, err
}

func cardDueSet(e *ops.Env, in DueIn) (Card, error) {
	d := in.DueDate
	return cardUpdate(e, UpdateIn{ID: in.ID, DueDate: &d})
}

func cardDueComplete(e *ops.Env, in DueCompleteIn) (DueOut, error) {
	completed := in.Completed == nil || *in.Completed
	c, err := loadCard(e, e.DB, in.ID)
	if err != nil {
		return DueOut{}, err
	}
	if _, err := editCard(e, in.ID); err != nil {
		return DueOut{}, err
	}
	if c.DueDate == nil {
		return DueOut{}, ops.Invalid("card has no due date")
	}
	c, err = cardUpdate(e, UpdateIn{ID: in.ID, IsDueCompleted: &completed})
	out := DueOut{Card: c}
	if err != nil || !completed {
		return out, err
	}
	if r := parseRecurrence(c.Recurrence); r != nil && r.Trigger == "completed" {
		next, err := spawnNext(e, in.ID)
		if err != nil {
			return out, err
		}
		out.NextCardID = next
	}
	return out, nil
}

func cardLocation(e *ops.Env, in LocationIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	if in.Clear || (in.Lat == nil && in.Lng == nil) {
		_, err = e.DB.ExecContext(e.Ctx, `UPDATE cards SET lat=NULL,lng=NULL,location_name=NULL,updated_at=? WHERE id=?`, db.Now(), in.ID)
	} else {
		if in.Lat == nil || in.Lng == nil {
			return Card{}, ops.Invalid("lat and lng are both required")
		}
		if !(*in.Lat >= -90 && *in.Lat <= 90 && *in.Lng >= -180 && *in.Lng <= 180) {
			return Card{}, ops.Invalid("lat must be within -90..90 and lng within -180..180")
		}
		var nm any
		if s := strings.TrimSpace(in.Name); s != "" {
			if err := ops.CheckLen("locationName", s, ops.MaxNameLen); err != nil {
				return Card{}, err
			}
			nm = s
		}
		_, err = e.DB.ExecContext(e.Ctx, `UPDATE cards SET lat=?,lng=?,location_name=?,updated_at=? WHERE id=?`, *in.Lat, *in.Lng, nm, db.Now(), in.ID)
	}
	if err != nil {
		return Card{}, err
	}
	c, err := loadCardOn(e, e.DB, in.ID, boardID)
	if err == nil {
		emit(e, "cardUpdate", "card.updated", boardID, in.ID, map[string]any{"card": c})
	}
	return c, err
}
