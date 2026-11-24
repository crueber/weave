package boards

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

// List is a board column. Types: active, closed (both shown on the kanban),
// archive and trash (system lists, position null).
type List struct {
	ID          string   `json:"id"`
	BoardID     string   `json:"boardId"`
	Type        string   `json:"type"`
	Position    *float64 `json:"position,omitempty"`
	Name        *string  `json:"name,omitempty"`
	Color       *string  `json:"color,omitempty"`
	Description *string  `json:"description,omitempty" desc:"brief for this column: what a card here asks for (agents read it)"`
	WIPLimit    *int     `json:"wipLimit,omitempty" desc:"soft work-in-progress limit: how many cards this column should hold at most (advisory, nothing is blocked); absent = no limit"`
	// ArchiveAfter is the auto-archive setting (lists.archive_after); independent of the WIP limit.
	ArchiveAfter *int    `json:"archiveAfter,omitempty" desc:"auto-archive: the column keeps at most this many cards; when more land here, the ones that have been in it longest go to the board's archive (cards with a live claim and, for now, recurring cards are skipped); absent = off"`
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    *string `json:"updatedAt,omitempty"`
	// IsSubscribed is set by list.get only: the caller's own watch state. Every other path (board.get lists, board.tree,
	// events, exports, templates) leaves it nil, so it never travels.
	IsSubscribed *bool `json:"isSubscribed,omitempty" desc:"list.get only: whether you watch this column"`
}

// maxListDescLen caps a column description (characters; it ends up in agent prompts).
const maxListDescLen = 2000

// MaxWIPLimit caps a column's soft WIP limit.
const MaxWIPLimit = 10000

// checkWIP validates a requested limit: 0 clears it (nil), 1..MaxWIPLimit sets it.
func checkWIP(n int) (*int, error) {
	if n < 0 || n > MaxWIPLimit {
		return nil, ops.Invalid(fmt.Sprintf("wipLimit must be between 0 and %d (0 removes the limit)", MaxWIPLimit))
	}
	if n == 0 {
		return nil, nil
	}
	return &n, nil
}

const listCols = `id,board_id,type,position,name,color,description,wip_limit,archive_after,created_at,updated_at`

func scanList(r interface{ Scan(...any) error }) (List, error) {
	var l List
	err := r.Scan(&l.ID, &l.BoardID, &l.Type, &l.Position, &l.Name, &l.Color, &l.Description, &l.WIPLimit, &l.ArchiveAfter, &l.CreatedAt, &l.UpdatedAt)
	return l, err
}

func listsOf(e *ops.Env, boardID string, onlyFinite bool) ([]List, error) {
	q := `SELECT ` + listCols + ` FROM lists WHERE board_id=?`
	if onlyFinite {
		q += ` AND type IN ('active','closed')`
	}
	rows, err := e.DB.QueryContext(e.Ctx, q+` ORDER BY position IS NULL, position, id`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []List{}
	for rows.Next() {
		l, err := scanList(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetList loads a list (no permission check).
func GetList(ctx context.Context, q querier, id string) (List, error) {
	l, err := scanList(q.QueryRowContext(ctx, `SELECT `+listCols+` FROM lists WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return l, ops.NotFound("list")
	}
	return l, err
}

func requireList(e *ops.Env, id string, min authz.Level) (List, error) {
	l, err := GetList(e.Ctx, e.DB, id)
	if err != nil {
		return l, err
	}
	return l, authz.RequireBoard(e, l.BoardID, min)
}

func isFinite(t string) bool { return t == "active" || t == "closed" }

type ListListIn struct {
	BoardID string `path:"boardId"`
}
type ListCreateIn struct {
	BoardID      string   `path:"boardId"`
	Name         string   `json:"name" required:"true"`
	Type         string   `json:"type" desc:"active (default) | closed = the column closes cards that land here: cards created in or moved to it are marked closed, moving them to an active column re-opens them"`
	Color        string   `json:"color" desc:"column colour: #rrggbb or a preset name (crimson, orange, amber, lime, green, teal, sky, blue, indigo, violet, pink, slate); omit for none"`
	Description  string   `json:"description" desc:"brief for this column: what a card that lands here asks for; agents read it"`
	WIPLimit     *int     `json:"wipLimit" desc:"soft WIP limit (1-10000): advisory only, nothing is blocked; omit or 0 for none"`
	ArchiveAfter *int     `json:"archiveAfter" desc:"auto-archive (1-10000): keep at most this many cards; when more land here, the ones that have been in the column longest go to the board's archive; omit or 0 for off"`
	Position     *float64 `json:"position"`
}
type ListUpdateIn struct {
	ID             string   `path:"id"`
	Name           *string  `json:"name"`
	Color          *string  `json:"color" desc:"column colour: #rrggbb or a preset name; empty string clears the colour"`
	Type           *string  `json:"type" desc:"closed = close cards that land here (created in or moved to the column; moving them to an active column re-opens them), active = do not; see existingCards for the cards already in the column (list.typePreview counts them)"`
	Description    *string  `json:"description" desc:"brief for this column: what a card that lands here asks for; empty string clears it"`
	WIPLimit       *int     `json:"wipLimit" desc:"soft WIP limit (1-10000): the column header shows n/limit and flags it when over; advisory only, nothing is blocked; 0 removes the limit"`
	Position       *float64 `json:"position"`
	ArchiveAfter   *int     `json:"archiveAfter" desc:"auto-archive (1-10000): the column keeps at most this many cards; whenever more land here, the ones that have been in it longest (oldest list change first) go to the board's archive, where card.restore brings them back; cards with a live claim and, for now, cards with a recurrence rule are skipped (they still count); 0 turns it off. Setting or lowering it applies at once to the cards already here: when that would archive cards, confirmArchive=true is required (409 conflict with the count otherwise; list.archivePreview counts them)"`
	ConfirmArchive bool     `json:"confirmArchive" desc:"with archiveAfter: confirm that the cards over the new limit are archived right now"`
	// ExistingCards only matters when Type changes the column's closing setting.
	ExistingCards string `json:"existingCards" desc:"when type changes: flip (default) = also close the open cards already in the column (switching to closed) or re-open its closed cards (switching to active); keep = leave them as they are, the setting applies only to cards that land from now on"`
}

// ListArchivePreviewIn asks what setting a column's archive-after to N would archive right now.
type ListArchivePreviewIn struct {
	ID           string `path:"id"`
	ArchiveAfter int    `query:"archiveAfter" desc:"the setting to preview (1-10000); omit or 0 for the column's current setting"`
}

// ListArchivePreview counts what list.update archiveAfter=N would do to the cards already in the column.
type ListArchivePreview struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Current          int    `json:"current" desc:"the column's archive-after setting now (0 = off)"`
	Keep             int    `json:"keep" desc:"the setting previewed: the column keeps this many cards"`
	Cards            int    `json:"cards" desc:"cards in the column (drafts and card templates are not counted; closed cards are)"`
	WillArchive      int    `json:"willArchive" desc:"cards that would go to the archive now (oldest first); list.update needs confirmArchive=true when this is above 0"`
	SkippedClaimed   int    `json:"skippedClaimed" desc:"cards over the limit that stay for now because they hold a live claim"`
	SkippedRecurring int    `json:"skippedRecurring" desc:"cards over the limit that stay because they have a recurrence rule (recurring cards are never auto-archived, for now)"`
}

// ListTypePreviewIn asks what switching a column's closing setting would do to the cards already in it.
type ListTypePreviewIn struct {
	ID   string `path:"id"`
	Type string `query:"type" desc:"the type to switch to: closed | active; omit for the opposite of the current type"`
}

// ListTypePreview counts the cards a list.update type switch would change (with existingCards=flip).
type ListTypePreview struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CurrentType string `json:"currentType" desc:"the column's type now"`
	Type        string `json:"type" desc:"the type previewed"`
	Cards       int    `json:"cards" desc:"cards in the column (drafts and card templates are not counted)"`
	WillClose   int    `json:"willClose" desc:"open cards that switching to closed closes (with existingCards=flip)"`
	WillReopen  int    `json:"willReopen" desc:"closed cards that switching to active re-opens (with existingCards=flip)"`
}

// Changes is the number of cards the previewed switch flips (0 when the type does not change).
func (p ListTypePreview) Changes() int { return p.WillClose + p.WillReopen }

type ListMoveIn struct {
	ID       string   `path:"id"`
	AfterID  string   `json:"afterId" desc:"list to place after"`
	BeforeID string   `json:"beforeId" desc:"list to place before"`
	Position *float64 `json:"position"`
}
type ListRefIn struct {
	ID string `path:"id"`
}
type ListSortIn struct {
	ID    string `path:"id"`
	Field string `json:"field" required:"true" desc:"name | dueDate | createdAt"`
	Order string `json:"order" desc:"asc (default) | desc"`
}
type ListMoveCardsIn struct {
	ID     string `path:"id"`
	ListID string `json:"listId" required:"true" desc:"target list id (same board)"`
}
type ListMoveCardsOut struct {
	Moved int `json:"moved"`
}

func registerListOps() {
	ops.Register(ops.Op{Name: "list.list", Summary: "List a board's lists (including archive and trash system lists)", Method: "GET", Path: "/boards/{boardId}/lists",
		UI: ui("GET", "/boards/{boardId}/lists")}, func(e *ops.Env, in ListListIn) ([]List, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return nil, err
		}
		return listsOf(e, in.BoardID, false)
	})
	ops.Register(ops.Op{Name: "list.get", Summary: "Get one list (column) of a board", Method: "GET", Path: "/lists/{id}",
		UI: "GET /lists/{id}"}, func(e *ops.Env, in ListRefIn) (List, error) {
		l, err := requireList(e, in.ID, authz.View)
		if err != nil {
			return l, err
		}
		if !isFinite(l.Type) {
			return List{}, ops.NotFound("list") // archive and trash are system lists, not columns
		}
		if e.User != nil {
			w := listWatched(e, l.ID)
			l.IsSubscribed = &w
		}
		return l, nil
	})
	ops.Register(ops.Op{Name: "list.create", Summary: "Create a list (column) on a board", Method: "POST", Path: "/boards/{boardId}/lists",
		UI: ui("POST", "/boards/{boardId}/lists")}, func(e *ops.Env, in ListCreateIn) (List, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.Edit); err != nil {
			return List{}, err
		}
		typ := in.Type
		if typ == "" {
			typ = "active"
		}
		if !isFinite(typ) {
			return List{}, ops.Invalid("type must be active or closed")
		}
		if in.Name == "" {
			return List{}, ops.Invalid("name is required")
		}
		if err := ops.CheckLen("name", in.Name, ops.MaxNameLen); err != nil {
			return List{}, err
		}
		if err := ops.CheckLen("description", in.Description, maxListDescLen); err != nil {
			return List{}, err
		}
		color, err := NormalizeColor(in.Color)
		if err != nil {
			return List{}, err
		}
		var wip *int
		if in.WIPLimit != nil {
			if wip, err = checkWIP(*in.WIPLimit); err != nil {
				return List{}, err
			}
		}
		var keep *int
		if in.ArchiveAfter != nil {
			if keep, err = checkArchiveAfter(*in.ArchiveAfter); err != nil {
				return List{}, err
			}
		}
		pos := appendPos(e.Ctx, e.DB, "lists", "board_id=? AND type IN ('active','closed')", in.BoardID)
		if in.Position != nil {
			pos = *in.Position
		}
		id := db.NewID()
		if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO lists(id,board_id,type,position,name,color,description,wip_limit,archive_after,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			id, in.BoardID, typ, pos, in.Name, nullStr(color), nullStr(in.Description), wip, keep, db.Now()); err != nil {
			return List{}, err
		}
		emit(e, "listCreate", "list.created", in.BoardID, map[string]any{"id": id})
		return GetList(e.Ctx, e.DB, id)
	})
	ops.Register(ops.Op{Name: "list.update", Summary: "Rename / recolour a list, set its description (brief for agents), soft WIP limit or auto-archive (archiveAfter: keep the newest N cards), switch whether it closes cards that land here (type closed|active), or change its position", Method: "PATCH", Path: "/lists/{id}",
		UI: ui("PATCH", "/lists/{id}")}, func(e *ops.Env, in ListUpdateIn) (List, error) {
		l, err := requireList(e, in.ID, authz.Edit)
		if err != nil {
			return l, err
		}
		if !isFinite(l.Type) {
			return l, ops.Forbidden()
		}
		oldType := l.Type
		if in.Name != nil {
			if *in.Name == "" {
				return l, ops.Invalid("name must not be empty")
			}
			if err := ops.CheckLen("name", *in.Name, ops.MaxNameLen); err != nil {
				return l, err
			}
			l.Name = in.Name
		}
		if in.Color != nil {
			hex, err := NormalizeColor(*in.Color)
			if err != nil {
				return l, err
			}
			if hex == "" {
				l.Color = nil
			} else {
				l.Color = &hex
			}
		}
		if in.Type != nil {
			if !isFinite(*in.Type) {
				return l, ops.Invalid("type must be active or closed")
			}
			l.Type = *in.Type
		}
		switch in.ExistingCards {
		case "", "flip", "keep":
		default:
			return l, ops.Invalid("existingCards must be flip or keep")
		}
		keep := in.ExistingCards == "keep"
		if in.Description != nil {
			if err := ops.CheckLen("description", *in.Description, maxListDescLen); err != nil {
				return l, err
			}
			if strings.TrimSpace(*in.Description) == "" {
				l.Description = nil
			} else {
				l.Description = in.Description
			}
		}
		if in.WIPLimit != nil {
			if l.WIPLimit, err = checkWIP(*in.WIPLimit); err != nil {
				return l, err
			}
		}
		oldKeep := l.ArchiveAfter
		if in.ArchiveAfter != nil {
			if l.ArchiveAfter, err = checkArchiveAfter(*in.ArchiveAfter); err != nil {
				return l, err
			}
		}
		if in.Position != nil {
			l.Position = in.Position
		}
		var flipped, trimmed LandResult
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			flipped, trimmed = LandResult{}, LandResult{}
			if _, err := tx.ExecContext(e.Ctx, `UPDATE lists SET name=?,color=?,type=?,position=?,description=?,wip_limit=?,archive_after=?,updated_at=? WHERE id=?`, l.Name, l.Color, l.Type, l.Position, l.Description, l.WIPLimit, l.ArchiveAfter, db.Now(), l.ID); err != nil {
				return err
			}
			// Auto-archive applies at once to the cards already in the column whenever the request names archiveAfter
			// (setting it, lowering it, or re-sending it after a claim ended). Raising it or turning it off archives
			// nothing. Counted inside this transaction, so the 409 count is exact.
			if in.ArchiveAfter != nil && l.ArchiveAfter != nil {
				p, err := planArchive(e.Ctx, tx, l.ID, *l.ArchiveAfter, nil)
				if err != nil {
					return err
				}
				if len(p.IDs) > 0 && !in.ConfirmArchive {
					return ops.Conflict(fmt.Sprintf("archiveAfter=%d would archive %s now (the column holds %d%s); resend with confirmArchive=true to archive them", *l.ArchiveAfter, cardsN(len(p.IDs)), p.Cards, skippedNote(p)))
				}
				a, err := applyArchive(e.Ctx, tx, l.ID, *l.ArchiveAfter, p.IDs)
				if err != nil {
					return err
				}
				if len(a.CardIDs) > 0 {
					trimmed.Archived = append(trimmed.Archived, a)
				}
			}
			if l.Type == oldType || keep {
				return nil
			}
			// A switch of the closing setting re-lands the column's cards (existingCards=flip, the default): switching
			// to closed closes them, back to active re-opens them. With keep they stay as they are and the rule
			// applies only to cards that land from now on.
			var err error
			flipped, err = relandList(e.Ctx, tx, l.ID)
			return err
		})
		if err != nil {
			return l, err
		}
		data := map[string]any{"id": l.ID}
		if in.ArchiveAfter != nil && intOr0(oldKeep) != intOr0(l.ArchiveAfter) {
			data["archiveAfter"] = intOr0(l.ArchiveAfter)
		}
		if l.Type != oldType {
			data["type"], data["closes"] = l.Type, l.Type == "closed"
			data["existingCards"] = map[bool]string{true: "keep", false: "flip"}[keep]
			data["closed"], data["reopened"] = len(flipped.Closed), len(flipped.Reopened)
		}
		emit(e, "listUpdate", "list.updated", l.BoardID, data)
		// One card event per flipped card (realtime, durable stream, webhooks, activity), marked quiet so the
		// notification planner stays silent: a bulk switch must not send every watcher one close/reopen per card.
		for _, id := range flipped.Closed {
			emitCard(e, "cardUpdate", "card.updated", "", l.BoardID, id, map[string]any{"changed": []string{"isClosed"}, "isClosed": true, "auto": "listType", "quiet": true})
		}
		for _, id := range flipped.Reopened {
			emitCard(e, "cardUpdate", "card.updated", "", l.BoardID, id, map[string]any{"changed": []string{"isClosed"}, "isClosed": false, "auto": "listType", "quiet": true})
		}
		EmitAutoArchived(e, trimmed) // the retroactive pass: quiet card.archived per card + one summary
		return GetList(e.Ctx, e.DB, l.ID)
	})
	ops.Register(ops.Op{Name: "list.archivePreview", Summary: "Count the cards that setting the column's auto-archive (list.update archiveAfter=N) would archive right now; read-only", Method: "GET", Path: "/lists/{id}/archive-preview",
		UI: ui("GET", "/lists/{id}/archive-preview")}, func(e *ops.Env, in ListArchivePreviewIn) (ListArchivePreview, error) {
		l, err := requireList(e, in.ID, authz.Edit)
		if err != nil {
			return ListArchivePreview{}, err
		}
		if !isFinite(l.Type) {
			return ListArchivePreview{}, ops.NotFound("list")
		}
		p := ListArchivePreview{ID: l.ID, Current: intOr0(l.ArchiveAfter), Keep: in.ArchiveAfter}
		if l.Name != nil {
			p.Name = *l.Name
		}
		if p.Keep == 0 {
			p.Keep = p.Current
		}
		if _, err := checkArchiveAfter(p.Keep); err != nil {
			return p, err
		}
		if p.Keep == 0 { // off: nothing is archived; still report the count
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM card_placements p JOIN cards c ON c.id=p.card_id WHERE p.list_id=? AND c.is_draft=0 AND c.is_template=0`, l.ID).Scan(&p.Cards)
			return p, nil
		}
		plan, err := planArchive(e.Ctx, e.DB, l.ID, p.Keep, nil)
		if err != nil {
			return p, err
		}
		p.Cards, p.WillArchive, p.SkippedClaimed, p.SkippedRecurring = plan.Cards, len(plan.IDs), plan.SkippedClaimed, plan.SkippedRecurring
		return p, nil
	})
	ops.Register(ops.Op{Name: "list.typePreview", Summary: "Count the cards a switch of the column's closing setting (list.update type) would close or re-open; read-only", Method: "GET", Path: "/lists/{id}/type-preview",
		UI: ui("GET", "/lists/{id}/type-preview")}, func(e *ops.Env, in ListTypePreviewIn) (ListTypePreview, error) {
		l, err := requireList(e, in.ID, authz.Edit)
		if err != nil {
			return ListTypePreview{}, err
		}
		if !isFinite(l.Type) {
			return ListTypePreview{}, ops.NotFound("list")
		}
		p := ListTypePreview{ID: l.ID, CurrentType: l.Type, Type: in.Type}
		if l.Name != nil {
			p.Name = *l.Name
		}
		switch p.Type {
		case "":
			p.Type = map[bool]string{true: "active", false: "closed"}[l.Type == "closed"]
		case "active", "closed":
		default:
			return p, ops.Invalid("type must be active or closed")
		}
		var open, closed int
		if err := e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*), COALESCE(SUM(c.is_closed),0) FROM card_placements p JOIN cards c ON c.id=p.card_id
			WHERE p.list_id=? AND c.is_draft=0 AND c.is_template=0`, l.ID).Scan(&p.Cards, &closed); err != nil {
			return p, err
		}
		open = p.Cards - closed
		if p.Type != l.Type {
			if p.Type == "closed" {
				p.WillClose = open
			} else {
				p.WillReopen = closed
			}
		}
		return p, nil
	})
	ops.Register(ops.Op{Name: "list.move", Summary: "Reorder a list among the board's lists", Method: "POST", Path: "/lists/{id}/move",
		UI: ui("POST", "/lists/{id}/move")}, func(e *ops.Env, in ListMoveIn) (List, error) {
		l, err := requireList(e, in.ID, authz.Edit)
		if err != nil {
			return l, err
		}
		if !isFinite(l.Type) {
			return l, ops.Forbidden()
		}
		var pos float64
		if in.Position != nil {
			pos = *in.Position
		} else {
			pos = PlaceBetween(e.Ctx, e.DB, "lists", "board_id=? AND type IN ('active','closed') AND id<>?", []any{l.BoardID, l.ID}, in.AfterID, in.BeforeID)
		}
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE lists SET position=?,updated_at=? WHERE id=?`, pos, db.Now(), l.ID); err != nil {
			return l, err
		}
		emit(e, "listUpdate", "list.moved", l.BoardID, map[string]any{"id": l.ID, "position": pos})
		return GetList(e.Ctx, e.DB, l.ID)
	})
	ops.Register(ops.Op{Name: "list.delete", Summary: "Delete a list; its cards are moved to the trash list", Method: "DELETE", Path: "/lists/{id}",
		UI: ui("DELETE", "/lists/{id}")}, func(e *ops.Env, in ListRefIn) (Deleted, error) {
		return retire(e, in.ID, "trash")
	})
	ops.Register(ops.Op{Name: "list.archive", Summary: "Archive a list: its cards move to the archive list and the list is removed", Method: "POST", Path: "/lists/{id}/archive",
		UI: ui("POST", "/lists/{id}/archive")}, func(e *ops.Env, in ListRefIn) (Deleted, error) {
		return retire(e, in.ID, "archive")
	})
	ops.Register(ops.Op{Name: "list.sort", Summary: "Sort a list's cards by name, due date or creation time", Method: "POST", Path: "/lists/{id}/sort",
		UI: ui("POST", "/lists/{id}/sort")}, func(e *ops.Env, in ListSortIn) (List, error) {
		l, err := requireList(e, in.ID, authz.Edit)
		if err != nil {
			return l, err
		}
		var order string
		switch in.Field {
		case "name":
			order = "name COLLATE NOCASE"
		case "dueDate":
			order = "due_date IS NULL, due_date"
		case "createdAt":
			order = "created_at"
		default:
			return l, ops.Invalid("field must be name, dueDate or createdAt")
		}
		switch in.Order {
		case "", "asc":
		case "desc":
			order = fmt.Sprintf("%s DESC", order)
			if in.Field == "dueDate" {
				order = "due_date IS NULL, due_date DESC"
			}
		default:
			return l, ops.Invalid("order must be asc or desc")
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(e.Ctx, `SELECT id FROM cards WHERE id IN (SELECT card_id FROM card_placements WHERE list_id=?) ORDER BY `+order+`, id`, l.ID)
			if err != nil {
				return err
			}
			var ids []string
			for rows.Next() {
				var id string
				_ = rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			for i, id := range ids {
				if _, err := tx.ExecContext(e.Ctx, `UPDATE card_placements SET position=? WHERE card_id=? AND list_id=?`, float64(i+1)*db.PositionGap, id, l.ID); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return l, err
		}
		emit(e, "listSort", "list.updated", l.BoardID, map[string]any{"id": l.ID, "sorted": true})
		return l, nil
	})
	ops.Register(ops.Op{Name: "list.clear", Summary: "Empty the trash list: permanently delete its cards (a card that is also on another board is only removed from this board)", Method: "POST", Path: "/lists/{id}/clear",
		UI: ui("POST", "/lists/{id}/clear")}, func(e *ops.Env, in ListRefIn) (List, error) {
		l, err := requireList(e, in.ID, authz.Edit)
		if err != nil {
			return l, err
		}
		if l.Type != "trash" {
			return l, ops.Forbidden()
		}
		// A card whose only placement is in this trash is deleted. A card that also lives on another board is only
		// taken off this one (card.unplace's rules: a home here moves to its oldest other placement), never deleted.
		var gone, unplaced []string
		rehomed := map[string]string{}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			gone, unplaced, rehomed = nil, nil, map[string]string{}
			rows, err := tx.QueryContext(e.Ctx, `SELECT p.card_id,(SELECT COUNT(*) FROM card_placements o WHERE o.card_id=p.card_id) FROM card_placements p
				WHERE p.list_id=? ORDER BY p.position, p.card_id`, l.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id string
				var n int
				if err := rows.Scan(&id, &n); err != nil {
					rows.Close()
					return err
				}
				if n > 1 && RemoveFromBoards != nil {
					unplaced = append(unplaced, id)
				} else {
					gone = append(gone, id)
				}
			}
			rows.Close()
			for _, id := range unplaced {
				h, err := RemoveFromBoards(e, tx, id, []string{l.BoardID})
				if err != nil {
					return err
				}
				if h != "" {
					rehomed[id] = h
				}
			}
			for _, id := range gone {
				if _, err := tx.ExecContext(e.Ctx, `DELETE FROM cards WHERE id=?`, id); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return l, err
		}
		for _, id := range gone { // the card was on this board only (its one placement was here)
			emitCardOn(e, "cardDelete", "card.deleted", "", l.BoardID, id, map[string]any{"listId": l.ID, "cleared": true}, []string{l.BoardID})
		}
		for _, id := range unplaced { // removed from this board; it stays on its other boards
			emitCardOn(e, events.TypeUnplace, events.StreamUnplaced, "", l.BoardID, id, map[string]any{"listId": l.ID, "cleared": true}, []string{l.BoardID})
			if h := rehomed[id]; h != "" {
				emitCard(e, "cardUpdate", "card.updated", "", h, id, map[string]any{"changed": []string{"home"}})
			}
		}
		emit(e, "listClear", "list.updated", l.BoardID, map[string]any{"id": l.ID, "cleared": true})
		return l, nil
	})
	ops.Register(ops.Op{Name: "list.moveCards", Summary: "Move all cards of a list into another list of the same board", Method: "POST", Path: "/lists/{id}/move-cards",
		UI: ui("POST", "/lists/{id}/move-cards")}, func(e *ops.Env, in ListMoveCardsIn) (ListMoveCardsOut, error) {
		l, err := requireList(e, in.ID, authz.Edit)
		if err != nil {
			return ListMoveCardsOut{}, err
		}
		dst, err := GetList(e.Ctx, e.DB, in.ListID)
		if err != nil || dst.BoardID != l.BoardID {
			return ListMoveCardsOut{}, ops.NotFound("target list")
		}
		if dst.ID == l.ID {
			return ListMoveCardsOut{}, ops.Invalid("target list must differ")
		}
		moved, landed, err := moveAll(e, l.ID, dst.ID)
		if err != nil {
			return ListMoveCardsOut{}, err
		}
		n := len(moved)
		emitMoved(e, l.BoardID, l.ID, dst.ID, dst.Type, moved)
		EmitClosedElsewhere(e, l.BoardID, landed) // closed is per card: the cards' other boards show the new state
		emit(e, "listMoveCards", "list.updated", l.BoardID, map[string]any{"id": l.ID, "targetId": dst.ID})
		EmitAutoArchived(e, landed) // the target's archive-after rule (a batch larger than N keeps its newest N)
		return ListMoveCardsOut{n}, nil
	})
}

// moveAll appends every card of src to dst keeping order. The LandResult carries what dst's archive-after rule
// archived (emit it with EmitAutoArchived after the moved events).
func moveAll(e *ops.Env, srcID, dstID string) ([]string, LandResult, error) {
	var ids []string
	var res LandResult
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		base := maxPos(e.Ctx, tx, "card_placements", "list_id=?", dstID)
		rows, err := tx.QueryContext(e.Ctx, `SELECT card_id FROM card_placements WHERE list_id=? ORDER BY position, card_id`, srcID)
		if err != nil {
			return err
		}
		ids = nil
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		now := db.Now()
		lands := make([]Landing, 0, len(ids))
		for i, id := range ids {
			if _, err := tx.ExecContext(e.Ctx, `UPDATE card_placements SET list_id=?,prev_list_id=?,position=?,list_changed_at=? WHERE card_id=? AND list_id=?`,
				dstID, srcID, base+float64(i+1)*db.PositionGap, now, id, srcID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, now, id); err != nil {
				return err
			}
			lands = append(lands, Landing{CardID: id, ListID: dstID})
		}
		res, err = Landed(e.Ctx, tx, lands...) // into a closed column closes, into an active one re-opens, archive/trash keep
		return err
	})
	return ids, res, err
}

// emitMoved announces each card moved by a bulk list operation, so agents listening
// to the target list see the cards arrive (and those on the source see them leave).
func emitMoved(e *ops.Env, boardID, from, to, toType string, ids []string) {
	kind := ""
	if toType == "archive" || toType == "trash" {
		kind = "card.archived"
	}
	for _, id := range ids {
		// a column operation moves this board's placements only: a placement event (events.PlacementScoped) recorded
		// on this board; the card's other boards get the card.placement.changed nudge, and a close or re-open it
		// causes still notifies everyone following the card (closed is per card)
		emitCard(e, "cardMove", "card.moved", kind, boardID, id, map[string]any{"fromListId": from, "toListId": to, "trash": toType == "trash"})
	}
}

func retire(e *ops.Env, id, sysType string) (Deleted, error) {
	l, err := requireList(e, id, authz.Edit)
	if err != nil {
		return Deleted{}, err
	}
	if !isFinite(l.Type) {
		return Deleted{}, ops.Forbidden()
	}
	dst, err := SystemList(e.Ctx, e.DB, l.BoardID, sysType)
	if err != nil {
		return Deleted{}, err
	}
	moved, _, err := moveAll(e, l.ID, dst) // into archive/trash: no archive-after rule there
	if err != nil {
		return Deleted{}, err
	}
	emitMoved(e, l.BoardID, l.ID, dst, sysType, moved)
	if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM lists WHERE id=?`, l.ID); err != nil {
		return Deleted{}, err
	}
	emit(e, "listDelete", "list.deleted", l.BoardID, map[string]any{"id": l.ID})
	return Deleted{l.ID}, nil
}

// skippedNote words the cards over the limit that stay (for the 409 message): "" when none.
func skippedNote(p archivePlan) string {
	var parts []string
	if p.SkippedClaimed > 0 {
		parts = append(parts, fmt.Sprintf("%d over the limit stay while claimed", p.SkippedClaimed))
	}
	if p.SkippedRecurring > 0 {
		parts = append(parts, fmt.Sprintf("%d recurring %s over the limit", p.SkippedRecurring, map[bool]string{true: "card stays", false: "cards stay"}[p.SkippedRecurring == 1]))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, ", ")
}

func intOr0(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
