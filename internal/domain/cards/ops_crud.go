package cards

import (
	"database/sql"
	"strconv"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
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
	ID   string `path:"id"`
	Full bool   `query:"full" desc:"true adds tasks, comments (last 50), attachments, custom fields and activity (last 50)"`
}

type CreateIn struct {
	ListID      string   `path:"listId" desc:"list to create the card in (active or closed list)"`
	Name        string   `json:"name" required:"true" desc:"card title"`
	Type        string   `json:"type" desc:"project | story (default: board default card type)"`
	Description string   `json:"description"`
	DueDate     string   `json:"dueDate" desc:"RFC3339, YYYY-MM-DD or YYYY-MM-DDTHH:MM"`
	Position    *float64 `json:"position" desc:"explicit position; default end of list"`
}

type UpdateIn struct {
	ID             string  `path:"id"`
	Name           *string `json:"name"`
	Description    *string `json:"description" desc:"markdown-lite; empty string clears"`
	Type           *string `json:"type" desc:"project | story"`
	DueDate        *string `json:"dueDate" desc:"empty string clears"`
	IsDueCompleted *bool   `json:"isDueCompleted"`
}

type MoveIn struct {
	ID       string   `path:"id"`
	ListID   string   `json:"listId" required:"true" desc:"target list (may be on another board of the caller)"`
	Position *float64 `json:"position" desc:"explicit position"`
	AfterID  string   `json:"afterId" desc:"place after this sibling card"`
	BeforeID string   `json:"beforeId" desc:"place before this sibling card"`
}

type ArchiveIn struct {
	ID    string `path:"id"`
	Trash bool   `json:"trash" desc:"true = move to the trash list, false = archive list"`
}

type DuplicateIn struct {
	ID   string `path:"id"`
	Name string `json:"name" desc:"name of the copy (default '<name> (copy)')"`
}

type CloseIn struct {
	ID     string `path:"id"`
	Closed *bool  `json:"closed" desc:"default true; false re-opens"`
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
		UI: "exempt:cards are rendered by the board shell (kanban/list/grid) and calendar/map views"}, cardList)
	ops.Register(ops.Op{Name: "card.get", Summary: "Get a card with labels, members and claim; full=true adds tasks, comments, attachments, custom fields, activity", Method: "GET", Path: "/cards/{id}",
		UI: "GET /ui/cards/{id}"}, cardGet)
	ops.Register(ops.Op{Name: "card.create", Summary: "Create a card at the end (or given position) of a list", Method: "POST", Path: "/lists/{listId}/cards",
		UI: "POST /ui/lists/{listId}/cards"}, cardCreate)
	ops.Register(ops.Op{Name: "card.update", Summary: "Update card title, description, type, due date or completion", Method: "PATCH", Path: "/cards/{id}",
		UI: "POST /ui/cards/{id}/update"}, cardUpdate)
	ops.Register(ops.Op{Name: "card.delete", Summary: "Delete a card (must be in the trash list, or a draft/template)", Method: "DELETE", Path: "/cards/{id}",
		UI: "POST /ui/cards/{id}/delete"}, cardDelete)
	ops.Register(ops.Op{Name: "card.move", Summary: "Move/reorder a card between lists or boards (applies list-type rules)", Method: "POST", Path: "/cards/{id}/move",
		UI: "POST /ui/cards/{id}/move"}, func(e *ops.Env, in MoveIn) (Card, error) {
		return doMove(e, in.ID, in.ListID, in.Position, in.AfterID, in.BeforeID)
	})
	ops.Register(ops.Op{Name: "card.archive", Summary: "Move a card to the board's archive (or trash) list", Method: "POST", Path: "/cards/{id}/archive",
		UI: "POST /ui/cards/{id}/archive"}, cardArchive)
	ops.Register(ops.Op{Name: "card.restore", Summary: "Restore an archived/trashed card to its previous list", Method: "POST", Path: "/cards/{id}/restore",
		UI: "POST /ui/cards/{id}/restore"}, cardRestore)
	ops.Register(ops.Op{Name: "card.duplicate", Summary: "Duplicate a card (labels, members, extras via hooks)", Method: "POST", Path: "/cards/{id}/duplicate",
		UI: "POST /ui/cards/{id}/duplicate"}, cardDuplicate)
	ops.Register(ops.Op{Name: "card.close", Summary: "Close (or re-open) a card; moves to the closed list when the board has one", Method: "POST", Path: "/cards/{id}/close",
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
	if _, err := viewCard(e, in.ID); err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, in.ID)
	if err != nil {
		return Card{}, err
	}
	if authz.BoardLevel(e, c.BoardID) < authz.Edit {
		c.ShareToken = nil
	}
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
	out.CustomFields = pull("customfieldgroup.list", map[string]any{"cardId": in.ID})
	out.Activity = pull("activity.card", map[string]any{"cardId": in.ID, "limit": 50})
	return out, nil
}

func cardList(e *ops.Env, in ListCardsIn) (ListOut, error) {
	if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
		return ListOut{}, err
	}
	q := `SELECT id FROM cards WHERE board_id=? AND is_draft=0 AND is_template=0`
	args := []any{in.BoardID}
	if in.ListID != "" {
		q += ` AND list_id=?`
		args = append(args, in.ListID)
	}
	if in.LabelID != "" {
		q += ` AND id IN (SELECT card_id FROM card_labels WHERE label_id=?)`
		args = append(args, in.LabelID)
	}
	if in.MemberID != "" {
		q += ` AND id IN (SELECT card_id FROM card_memberships WHERE user_id=?)`
		args = append(args, in.MemberID)
	}
	switch strings.ToLower(in.Claimed) {
	case "true", "1":
		q += ` AND claimed_by IS NOT NULL AND claimed_until>=?`
		args = append(args, db.Now())
	case "false", "0":
		q += ` AND (claimed_by IS NULL OR claimed_until IS NULL OR claimed_until<?)`
		args = append(args, db.Now())
	case "", "any":
	default:
		return ListOut{}, ops.Invalid("claimed must be true, false or any")
	}
	if t := strings.TrimSpace(in.Q); t != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(t) + "%"
		q += ` AND (name LIKE ? ESCAPE '\' OR COALESCE(description,'') LIKE ? ESCAPE '\')`
		args = append(args, like, like)
	}
	q += ` ORDER BY (SELECT position FROM lists WHERE id=cards.list_id), list_id, position, created_at, id`
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
	ids := strs(e, e.DB, q, args...)
	out := ListOut{Items: []Card{}}
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
		out.NextCursor = strconv.Itoa(offset + limit)
	}
	for _, id := range ids {
		c, err := loadCard(e, e.DB, id)
		if err != nil {
			continue
		}
		c.ShareToken = nil
		out.Items = append(out.Items, c)
	}
	return out, nil
}

func validType(t string) bool { return t == "project" || t == "story" }

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
	typ := in.Type
	if typ == "" {
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT default_card_type FROM boards WHERE id=?`, boardID).Scan(&typ)
		if !validType(typ) {
			typ = "project"
		}
	}
	if !validType(typ) {
		return Card{}, ops.Invalid("type must be project or story")
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
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		pos := positionFor(e.Ctx, tx, in.ListID, "", "", "", in.Position)
		_, err := tx.ExecContext(e.Ctx, `INSERT INTO cards(id,board_id,list_id,creator_user_id,type,position,name,description,due_date,is_closed,list_changed_at,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, boardID, in.ListID, e.User.UserID, typ, pos, name, desc, due, ltype == "closed", now, now)
		if err != nil {
			return err
		}
		var sub bool
		_ = tx.QueryRowContext(e.Ctx, `SELECT subscribe_own_cards FROM users WHERE id=?`, e.User.UserID).Scan(&sub)
		if sub {
			subscribe(e.Ctx, tx, id, e.User.UserID, false)
		}
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
	return c, nil
}

func cardUpdate(e *ops.Env, in UpdateIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	sets, args := []string{}, []any{}
	if in.Name != nil {
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
	}
	if in.Type != nil {
		if !validType(*in.Type) {
			return Card{}, ops.Invalid("type must be project or story")
		}
		sets, args = append(sets, "type=?"), append(args, *in.Type)
	}
	if in.DueDate != nil {
		if *in.DueDate == "" {
			sets = append(sets, "due_date=NULL", "is_due_completed=NULL")
		} else {
			d, err := normTime(*in.DueDate)
			if err != nil {
				return Card{}, err
			}
			sets, args = append(sets, "due_date=?"), append(args, d)
		}
	}
	if in.IsDueCompleted != nil {
		sets, args = append(sets, "is_due_completed=?"), append(args, *in.IsDueCompleted)
	}
	if len(sets) == 0 {
		return loadCard(e, e.DB, in.ID)
	}
	sets, args = append(sets, "updated_at=?"), append(args, db.Now(), in.ID)
	if _, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET `+strings.Join(sets, ",")+` WHERE id=?`, args...); err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, in.ID)
	if err == nil {
		emit(e, "cardUpdate", "card.updated", boardID, in.ID, map[string]any{"card": c})
	}
	return c, err
}

func cardDelete(e *ops.Env, in IDIn) (Deleted, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Deleted{}, err
	}
	var ltype string
	var draft, tmpl bool
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT l.type,c.is_draft,c.is_template FROM cards c JOIN lists l ON l.id=c.list_id WHERE c.id=?`, in.ID).Scan(&ltype, &draft, &tmpl)
	if ltype != "trash" && !draft && !tmpl {
		return Deleted{}, ops.Invalid("card must be in the trash list before it can be deleted")
	}
	c, _ := loadCard(e, e.DB, in.ID)
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		_, _ = tx.ExecContext(e.Ctx, `DELETE FROM card_editing WHERE card_id=?`, in.ID)
		_, err := tx.ExecContext(e.Ctx, `DELETE FROM cards WHERE id=?`, in.ID)
		return err
	})
	if err != nil {
		return Deleted{}, err
	}
	emit(e, "cardDelete", "card.deleted", boardID, in.ID, map[string]any{"name": c.Name, "listId": c.ListID})
	return Deleted{in.ID}, nil
}

// doMove implements card.move; shared by archive/restore/close/publish.
func doMove(e *ops.Env, id, listID string, explicit *float64, afterID, beforeID string) (Card, error) {
	srcBoard, err := editCard(e, id)
	if err != nil {
		return Card{}, err
	}
	dstBoard, dtype, err := listRef(e.Ctx, e.DB, listID)
	if err != nil {
		return Card{}, err
	}
	if dstBoard != srcBoard {
		if err := authz.RequireBoard(e, dstBoard, authz.Edit); err != nil {
			return Card{}, err
		}
	}
	old, err := loadCard(e, e.DB, id)
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
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		pos := positionFor(e.Ctx, tx, listID, id, afterID, beforeID, explicit)
		now := db.Now()
		if !listChanged {
			_, err := tx.ExecContext(e.Ctx, `UPDATE cards SET position=?,updated_at=? WHERE id=?`, pos, now, id)
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
		_, err := tx.ExecContext(e.Ctx, `UPDATE cards SET list_id=?,board_id=?,position=?,prev_list_id=?,is_closed=?,list_changed_at=?,updated_at=? WHERE id=?`,
			listID, dstBoard, pos, prev, dtype == "closed", now, now, id)
		if err != nil {
			return err
		}
		if dstBoard != srcBoard {
			// drop labels and members that do not exist on the destination board
			rows, _ := tx.QueryContext(e.Ctx, `SELECT label_id FROM card_labels WHERE card_id=?`, id)
			var drop []string
			for rows != nil && rows.Next() {
				var l string
				_ = rows.Scan(&l)
				if !labelAllowed(e.Ctx, tx, dstBoard, l) {
					drop = append(drop, l)
				}
			}
			if rows != nil {
				rows.Close()
			}
			for _, l := range drop {
				_, _ = tx.ExecContext(e.Ctx, `DELETE FROM card_labels WHERE card_id=? AND label_id=?`, id, l)
			}
			members := boardMemberIDs(e.Ctx, tx, dstBoard)
			for _, u := range old.MemberIDs {
				if !members[u] {
					_, _ = tx.ExecContext(e.Ctx, `DELETE FROM card_memberships WHERE card_id=? AND user_id=?`, id, u)
				}
			}
			_, _ = tx.ExecContext(e.Ctx, `DELETE FROM card_editing WHERE card_id=?`, id)
		}
		return nil
	})
	if err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, id)
	if err != nil {
		return c, err
	}
	if dstBoard != srcBoard {
		emit(e, "cardDelete", "card.deleted", srcBoard, id, map[string]any{"movedToBoardId": dstBoard, "listId": old.ListID})
	}
	kind := ""
	switch {
	case !listChanged:
	case (dtype == "archive" || dtype == "trash") && stype != "archive" && stype != "trash":
		kind = "card.archived"
	case (stype == "archive" || stype == "trash") && dtype != "archive" && dtype != "trash":
		kind = "card.restored"
	}
	emitKind(e, kind, "cardMove", "card.moved", dstBoard, id, map[string]any{"card": c, "fromListId": old.ListID, "toListId": listID, "fromBoardId": srcBoard, "trash": dtype == "trash"})
	return c, nil
}

func firstList(e *ops.Env, boardID, typ string) string {
	var id string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT id FROM lists WHERE board_id=? AND type=? ORDER BY position LIMIT 1`, boardID, typ).Scan(&id)
	return id
}

func cardArchive(e *ops.Env, in ArchiveIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	typ := "archive"
	if in.Trash {
		typ = "trash"
	}
	l := firstList(e, boardID, typ)
	if l == "" {
		return Card{}, ops.Invalid("board has no " + typ + " list")
	}
	return doMove(e, in.ID, l, nil, "", "")
}

func cardRestore(e *ops.Env, in IDIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, in.ID)
	if err != nil {
		return c, err
	}
	var stype string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT type FROM lists WHERE id=?`, c.ListID).Scan(&stype)
	if stype != "archive" && stype != "trash" {
		return c, ops.Invalid("card is not archived or trashed")
	}
	target := ""
	if c.PrevListID != nil {
		var pt, pb string
		if e.DB.QueryRowContext(e.Ctx, `SELECT type,board_id FROM lists WHERE id=?`, *c.PrevListID).Scan(&pt, &pb) == nil && pb == boardID && (pt == "active" || pt == "closed") {
			target = *c.PrevListID
		}
	}
	if target == "" {
		target = firstList(e, boardID, "active")
	}
	if target == "" {
		return c, ops.Invalid("board has no active list to restore into")
	}
	return doMove(e, in.ID, target, nil, "", "")
}

func cardClose(e *ops.Env, in CloseIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	closing := in.Closed == nil || *in.Closed
	c, err := loadCard(e, e.DB, in.ID)
	if err != nil {
		return c, err
	}
	var stype string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT type FROM lists WHERE id=?`, c.ListID).Scan(&stype)
	if stype == "archive" || stype == "trash" {
		return c, ops.Invalid("restore the card before closing it")
	}
	if closing {
		if cl := firstList(e, boardID, "closed"); cl != "" && stype != "closed" {
			return doMove(e, in.ID, cl, nil, "", "")
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
			return doMove(e, in.ID, target, nil, "", "")
		}
	}
	if _, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET is_closed=?,updated_at=? WHERE id=?`, closing, db.Now(), in.ID); err != nil {
		return c, err
	}
	c, _ = loadCard(e, e.DB, in.ID)
	emit(e, "cardUpdate", "card.updated", boardID, in.ID, map[string]any{"card": c})
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
	c, err := loadCard(e, e.DB, in.ID)
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
		if *in.Lat < -90 || *in.Lat > 90 || *in.Lng < -180 || *in.Lng > 180 {
			return Card{}, ops.Invalid("lat must be within -90..90 and lng within -180..180")
		}
		var nm any
		if strings.TrimSpace(in.Name) != "" {
			nm = strings.TrimSpace(in.Name)
		}
		_, err = e.DB.ExecContext(e.Ctx, `UPDATE cards SET lat=?,lng=?,location_name=?,updated_at=? WHERE id=?`, *in.Lat, *in.Lng, nm, db.Now(), in.ID)
	}
	if err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, in.ID)
	if err == nil {
		emit(e, "cardUpdate", "card.updated", boardID, in.ID, map[string]any{"card": c})
	}
	return c, err
}
