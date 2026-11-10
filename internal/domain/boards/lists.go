package boards

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// List is a board column. Types: active, closed (both shown on the kanban),
// archive and trash (system lists, position null).
type List struct {
	ID        string   `json:"id"`
	BoardID   string   `json:"boardId"`
	Type      string   `json:"type"`
	Position  *float64 `json:"position,omitempty"`
	Name      *string  `json:"name,omitempty"`
	Color     *string  `json:"color,omitempty"`
	CreatedAt string   `json:"createdAt"`
	UpdatedAt *string  `json:"updatedAt,omitempty"`
}

const listCols = `id,board_id,type,position,name,color,created_at,updated_at`

func scanList(r interface{ Scan(...any) error }) (List, error) {
	var l List
	err := r.Scan(&l.ID, &l.BoardID, &l.Type, &l.Position, &l.Name, &l.Color, &l.CreatedAt, &l.UpdatedAt)
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
	BoardID  string   `path:"boardId"`
	Name     string   `json:"name" required:"true"`
	Type     string   `json:"type" desc:"active (default) | closed"`
	Color    string   `json:"color" desc:"column colour: #rrggbb or a preset name (crimson, orange, amber, lime, green, teal, sky, blue, indigo, violet, pink, slate); omit for none"`
	Position *float64 `json:"position"`
}
type ListUpdateIn struct {
	ID       string   `path:"id"`
	Name     *string  `json:"name"`
	Color    *string  `json:"color" desc:"column colour: #rrggbb or a preset name; empty string clears the colour"`
	Type     *string  `json:"type" desc:"switch between active and closed"`
	Position *float64 `json:"position"`
}
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
		color, err := NormalizeColor(in.Color)
		if err != nil {
			return List{}, err
		}
		pos := appendPos(e.Ctx, e.DB, "lists", "board_id=? AND type IN ('active','closed')", in.BoardID)
		if in.Position != nil {
			pos = *in.Position
		}
		id := db.NewID()
		if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO lists(id,board_id,type,position,name,color,created_at) VALUES(?,?,?,?,?,?,?)`,
			id, in.BoardID, typ, pos, in.Name, nullStr(color), db.Now()); err != nil {
			return List{}, err
		}
		emit(e, "listCreate", "list.created", in.BoardID, map[string]any{"id": id})
		return GetList(e.Ctx, e.DB, id)
	})
	ops.Register(ops.Op{Name: "list.update", Summary: "Rename / recolour a list, change its type or position", Method: "PATCH", Path: "/lists/{id}",
		UI: ui("PATCH", "/lists/{id}")}, func(e *ops.Env, in ListUpdateIn) (List, error) {
		l, err := requireList(e, in.ID, authz.Edit)
		if err != nil {
			return l, err
		}
		if !isFinite(l.Type) {
			return l, ops.Forbidden()
		}
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
		if in.Position != nil {
			l.Position = in.Position
		}
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE lists SET name=?,color=?,type=?,position=?,updated_at=? WHERE id=?`, l.Name, l.Color, l.Type, l.Position, db.Now(), l.ID); err != nil {
			return l, err
		}
		emit(e, "listUpdate", "list.updated", l.BoardID, map[string]any{"id": l.ID})
		return GetList(e.Ctx, e.DB, l.ID)
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
			rows, err := tx.QueryContext(e.Ctx, `SELECT id FROM cards WHERE list_id=? ORDER BY `+order+`, id`, l.ID)
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
				if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET position=? WHERE id=?`, float64(i+1)*db.PositionGap, id); err != nil {
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
	ops.Register(ops.Op{Name: "list.clear", Summary: "Permanently delete all cards of the trash list", Method: "POST", Path: "/lists/{id}/clear",
		UI: ui("POST", "/lists/{id}/clear")}, func(e *ops.Env, in ListRefIn) (List, error) {
		l, err := requireList(e, in.ID, authz.Edit)
		if err != nil {
			return l, err
		}
		if l.Type != "trash" {
			return l, ops.Forbidden()
		}
		gone := strs(e, `SELECT id FROM cards WHERE list_id=?`, l.ID)
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM cards WHERE list_id=?`, l.ID); err != nil {
			return l, err
		}
		for _, id := range gone {
			emitCard(e, "cardDelete", "card.deleted", "", l.BoardID, id, map[string]any{"listId": l.ID, "cleared": true})
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
		moved, err := moveAll(e, l.ID, dst.ID)
		if err != nil {
			return ListMoveCardsOut{}, err
		}
		n := len(moved)
		emitMoved(e, l.BoardID, l.ID, dst.ID, dst.Type, moved)
		emit(e, "listMoveCards", "list.updated", l.BoardID, map[string]any{"id": l.ID, "targetId": dst.ID})
		return ListMoveCardsOut{n}, nil
	})
}

// moveAll appends every card of src to dst keeping order.
func moveAll(e *ops.Env, srcID, dstID string) ([]string, error) {
	var ids []string
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		base := maxPos(e.Ctx, tx, "cards", "list_id=?", dstID)
		rows, err := tx.QueryContext(e.Ctx, `SELECT id FROM cards WHERE list_id=? ORDER BY position, id`, srcID)
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
		for i, id := range ids {
			if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET list_id=?,prev_list_id=?,position=?,list_changed_at=?,updated_at=? WHERE id=?`,
				dstID, srcID, base+float64(i+1)*db.PositionGap, now, now, id); err != nil {
				return err
			}
		}
		return nil
	})
	return ids, err
}

// emitMoved announces each card moved by a bulk list operation, so agents listening
// to the target list see the cards arrive (and those on the source see them leave).
func emitMoved(e *ops.Env, boardID, from, to, toType string, ids []string) {
	kind := ""
	if toType == "archive" || toType == "trash" {
		kind = "card.archived"
	}
	for _, id := range ids {
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
	moved, err := moveAll(e, l.ID, dst)
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
