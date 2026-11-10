package boards

import (
	"database/sql"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// SavedFilter stores a per-user board filter as a URL query string
// (q=text&label=ID&member=ID|me|none&due=overdue|today|week|none|done).
type SavedFilter struct {
	ID        string `json:"id"`
	BoardID   string `json:"boardId"`
	Name      string `json:"name"`
	Query     string `json:"query"`
	CreatedAt string `json:"createdAt"`
}

func filtersOf(e *ops.Env, boardID string) ([]SavedFilter, error) {
	if e.User == nil {
		return []SavedFilter{}, nil
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,board_id,name,query,created_at FROM saved_filters WHERE board_id=? AND user_id=? ORDER BY created_at,id`, boardID, e.User.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SavedFilter{}
	for rows.Next() {
		var f SavedFilter
		if err := rows.Scan(&f.ID, &f.BoardID, &f.Name, &f.Query, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

type FilterListIn struct {
	BoardID string `path:"boardId"`
}
type FilterCreateIn struct {
	BoardID string `path:"boardId"`
	Name    string `json:"name" required:"true"`
	Query   string `json:"query" required:"true" desc:"URL query: q, label, member, due"`
}
type FilterUpdateIn struct {
	ID    string  `path:"id"`
	Name  *string `json:"name"`
	Query *string `json:"query"`
}
type FilterRefIn struct {
	ID string `path:"id"`
}

func ownFilter(e *ops.Env, id string) (SavedFilter, error) {
	var f SavedFilter
	var uid string
	err := e.DB.QueryRowContext(e.Ctx, `SELECT id,board_id,name,query,created_at,user_id FROM saved_filters WHERE id=?`, id).Scan(&f.ID, &f.BoardID, &f.Name, &f.Query, &f.CreatedAt, &uid)
	if err == sql.ErrNoRows || (err == nil && (e.User == nil || uid != e.User.UserID)) {
		return f, ops.NotFound("filter")
	}
	return f, err
}

func registerFilterOps() {
	ops.Register(ops.Op{Name: "filter.list", Summary: "List the caller's saved filters of a board", Method: "GET", Path: "/boards/{boardId}/filters",
		UI: ui("GET", "/boards/{boardId}/filters")}, func(e *ops.Env, in FilterListIn) ([]SavedFilter, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return nil, err
		}
		return filtersOf(e, in.BoardID)
	})
	ops.Register(ops.Op{Name: "filter.create", Summary: "Save a board filter", Method: "POST", Path: "/boards/{boardId}/filters",
		UI: ui("POST", "/boards/{boardId}/filters")}, func(e *ops.Env, in FilterCreateIn) (SavedFilter, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return SavedFilter{}, err
		}
		id := db.NewID()
		now := db.Now()
		if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO saved_filters(id,board_id,user_id,name,query,created_at) VALUES(?,?,?,?,?,?)`, id, in.BoardID, e.User.UserID, in.Name, in.Query, now); err != nil {
			return SavedFilter{}, err
		}
		return SavedFilter{ID: id, BoardID: in.BoardID, Name: in.Name, Query: in.Query, CreatedAt: now}, nil
	})
	ops.Register(ops.Op{Name: "filter.update", Summary: "Rename or change a saved filter", Method: "PATCH", Path: "/filters/{id}",
		UI: ui("PATCH", "/filters/{id}")}, func(e *ops.Env, in FilterUpdateIn) (SavedFilter, error) {
		f, err := ownFilter(e, in.ID)
		if err != nil {
			return f, err
		}
		if in.Name != nil && *in.Name != "" {
			f.Name = *in.Name
		}
		if in.Query != nil {
			f.Query = *in.Query
		}
		_, err = e.DB.ExecContext(e.Ctx, `UPDATE saved_filters SET name=?,query=? WHERE id=?`, f.Name, f.Query, f.ID)
		return f, err
	})
	ops.Register(ops.Op{Name: "filter.delete", Summary: "Delete a saved filter", Method: "DELETE", Path: "/filters/{id}",
		UI: ui("DELETE", "/filters/{id}")}, func(e *ops.Env, in FilterRefIn) (Deleted, error) {
		f, err := ownFilter(e, in.ID)
		if err != nil {
			return Deleted{}, err
		}
		_, err = e.DB.ExecContext(e.Ctx, `DELETE FROM saved_filters WHERE id=?`, f.ID)
		return Deleted{f.ID}, err
	})
}
