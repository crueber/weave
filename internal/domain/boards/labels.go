package boards

import (
	"database/sql"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// Label is a board label or (when ProjectID is set) a project-wide label.
type Label struct {
	ID        string  `json:"id"`
	BoardID   *string `json:"boardId,omitempty"`
	ProjectID *string `json:"projectId,omitempty"`
	GroupID   *string `json:"groupId,omitempty"`
	Name      *string `json:"name,omitempty"`
	Color     string  `json:"color"`
	Position  float64 `json:"position"`
	CreatedAt string  `json:"createdAt"`
}

// LabelGroup groups labels; single-select groups allow one label per card.
type LabelGroup struct {
	ID           string  `json:"id"`
	BoardID      *string `json:"boardId,omitempty"`
	ProjectID    *string `json:"projectId,omitempty"`
	Name         string  `json:"name"`
	SingleSelect bool    `json:"singleSelect"`
	Position     float64 `json:"position"`
}

const labelCols = `id,board_id,project_id,group_id,name,color,position,created_at`

func scanLabel(r interface{ Scan(...any) error }) (Label, error) {
	var l Label
	err := r.Scan(&l.ID, &l.BoardID, &l.ProjectID, &l.GroupID, &l.Name, &l.Color, &l.Position, &l.CreatedAt)
	return l, err
}

func projectOfBoard(e *ops.Env, boardID string) (pid string, useProject bool) {
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT b.project_id,p.use_project_labels FROM boards b JOIN projects p ON p.id=b.project_id WHERE b.id=?`, boardID).Scan(&pid, &useProject)
	return
}

func labelsOf(e *ops.Env, boardID string) ([]Label, error) {
	pid, use := projectOfBoard(e, boardID)
	q, args := `SELECT `+labelCols+` FROM labels WHERE board_id=?`, []any{boardID}
	if use {
		q += ` OR (project_id=? AND board_id IS NULL)`
		args = append(args, pid)
	}
	rows, err := e.DB.QueryContext(e.Ctx, q+` ORDER BY position,id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Label{}
	for rows.Next() {
		l, err := scanLabel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func groupsOf(e *ops.Env, boardID string) ([]LabelGroup, error) {
	pid, use := projectOfBoard(e, boardID)
	q, args := `SELECT id,board_id,project_id,name,single_select,position FROM label_groups WHERE board_id=?`, []any{boardID}
	if use {
		q += ` OR (project_id=? AND board_id IS NULL)`
		args = append(args, pid)
	}
	rows, err := e.DB.QueryContext(e.Ctx, q+` ORDER BY position,id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LabelGroup{}
	for rows.Next() {
		var g LabelGroup
		if err := rows.Scan(&g.ID, &g.BoardID, &g.ProjectID, &g.Name, &g.SingleSelect, &g.Position); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// requireLabel checks edit rights on a label's scope (board Edit, project Manage).
func requireLabel(e *ops.Env, id string) (Label, error) {
	l, err := scanLabel(e.DB.QueryRowContext(e.Ctx, `SELECT `+labelCols+` FROM labels WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return l, ops.NotFound("label")
	}
	if err != nil {
		return l, err
	}
	if l.BoardID != nil {
		return l, authz.RequireBoard(e, *l.BoardID, authz.Edit)
	}
	return l, authz.RequireProject(e, *l.ProjectID, authz.Manage)
}

func requireGroup(e *ops.Env, id string) (LabelGroup, error) {
	var g LabelGroup
	err := e.DB.QueryRowContext(e.Ctx, `SELECT id,board_id,project_id,name,single_select,position FROM label_groups WHERE id=?`, id).
		Scan(&g.ID, &g.BoardID, &g.ProjectID, &g.Name, &g.SingleSelect, &g.Position)
	if err == sql.ErrNoRows {
		return g, ops.NotFound("label group")
	}
	if err != nil {
		return g, err
	}
	if g.BoardID != nil {
		return g, authz.RequireBoard(e, *g.BoardID, authz.Edit)
	}
	return g, authz.RequireProject(e, *g.ProjectID, authz.Manage)
}

type LabelListIn struct {
	BoardID string `path:"boardId"`
}
type LabelCreateIn struct {
	BoardID  string   `path:"boardId"`
	Name     string   `json:"name" desc:"label name (may be empty for colour-only labels)"`
	Color    string   `json:"color" required:"true" desc:"palette colour name (e.g. berry-red) or #rrggbb"`
	GroupID  string   `json:"groupId" desc:"label group id"`
	Position *float64 `json:"position"`
}
type ProjectLabelListIn struct {
	ProjectID string `path:"projectId"`
}
type ProjectLabelCreateIn struct {
	ProjectID string   `path:"projectId"`
	Name      string   `json:"name"`
	Color     string   `json:"color" required:"true"`
	GroupID   string   `json:"groupId"`
	Position  *float64 `json:"position"`
}
type LabelUpdateIn struct {
	ID      string  `path:"id"`
	Name    *string `json:"name"`
	Color   *string `json:"color"`
	GroupID *string `json:"groupId" desc:"empty string removes the label from its group"`
}
type LabelMoveIn struct {
	ID       string   `path:"id"`
	AfterID  string   `json:"afterId"`
	BeforeID string   `json:"beforeId"`
	Position *float64 `json:"position"`
}
type LabelRefIn struct {
	ID string `path:"id"`
}
type GroupCreateIn struct {
	BoardID      string `path:"boardId"`
	Name         string `json:"name" required:"true"`
	SingleSelect bool   `json:"singleSelect" desc:"only one label of the group per card"`
}
type ProjectGroupCreateIn struct {
	ProjectID    string `path:"projectId"`
	Name         string `json:"name" required:"true"`
	SingleSelect bool   `json:"singleSelect"`
}
type GroupUpdateIn struct {
	ID           string   `path:"id"`
	Name         *string  `json:"name"`
	SingleSelect *bool    `json:"singleSelect"`
	Position     *float64 `json:"position"`
}

// groupFits reports whether a label group belongs to the same board (or the same project) as
// the label being created/updated, so labels cannot be attached to another project's groups.
func groupFits(e *ops.Env, groupID, boardID, projectID string) bool {
	var gb, gp sql.NullString
	if e.DB.QueryRowContext(e.Ctx, `SELECT board_id,project_id FROM label_groups WHERE id=?`, groupID).Scan(&gb, &gp) != nil {
		return false
	}
	if boardID != "" {
		if gb.Valid && gb.String == boardID {
			return true
		}
		pid, _ := projectOfBoard(e, boardID)
		return gp.Valid && pid != "" && gp.String == pid
	}
	return gp.Valid && gp.String == projectID
}

func createLabel(e *ops.Env, boardID, projectID string, in LabelCreateIn) (Label, error) {
	if !validColor(in.Color) {
		return Label{}, ops.Invalid("invalid color")
	}
	if err := ops.CheckLen("name", in.Name, ops.MaxNameLen); err != nil {
		return Label{}, err
	}
	var scope string
	var args []any
	var bcol, pcol any
	if boardID != "" {
		scope, args, bcol = "board_id=?", []any{boardID}, boardID
	} else {
		scope, args, pcol = "project_id=? AND board_id IS NULL", []any{projectID}, projectID
	}
	if in.GroupID != "" {
		if !groupFits(e, in.GroupID, boardID, projectID) {
			return Label{}, ops.NotFound("label group")
		}
	}
	id := db.NewID()
	pos := appendPos(e.Ctx, e.DB, "labels", scope, args...)
	if in.Position != nil {
		pos = *in.Position
	}
	if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO labels(id,board_id,project_id,group_id,name,color,position,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		id, bcol, pcol, nullStr(in.GroupID), nullStr(in.Name), in.Color, pos, db.Now()); err != nil {
		return Label{}, err
	}
	l, err := scanLabel(e.DB.QueryRowContext(e.Ctx, `SELECT `+labelCols+` FROM labels WHERE id=?`, id))
	return l, err
}

func labelBoards(e *ops.Env, l Label) []string {
	if l.BoardID != nil {
		return []string{*l.BoardID}
	}
	var out []string
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT id FROM boards WHERE project_id=?`, *l.ProjectID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func emitLabel(e *ops.Env, typ, name string, l Label) {
	for _, b := range labelBoards(e, l) {
		emit(e, typ, name, b, map[string]any{"id": l.ID})
	}
}

func registerLabelOps() {
	ops.Register(ops.Op{Name: "label.list", Summary: "List labels visible on a board (incl. project-wide labels when enabled)", Method: "GET", Path: "/boards/{boardId}/labels",
		UI: ui("GET", "/boards/{boardId}/labels")}, func(e *ops.Env, in LabelListIn) ([]Label, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return nil, err
		}
		return labelsOf(e, in.BoardID)
	})
	ops.Register(ops.Op{Name: "label.create", Summary: "Create a label on a board (project-wide when the project uses project labels)", Method: "POST", Path: "/boards/{boardId}/labels",
		UI: ui("POST", "/boards/{boardId}/labels")}, func(e *ops.Env, in LabelCreateIn) (Label, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.Edit); err != nil {
			return Label{}, err
		}
		pid, use := projectOfBoard(e, in.BoardID)
		var l Label
		var err error
		if use {
			if err = authz.RequireProject(e, pid, authz.Manage); err != nil {
				return l, err
			}
			l, err = createLabel(e, "", pid, in)
		} else {
			l, err = createLabel(e, in.BoardID, "", in)
		}
		if err == nil {
			emitLabel(e, "labelCreate", "label.created", l)
		}
		return l, err
	})
	ops.Register(ops.Op{Name: "label.project.list", Summary: "List project-wide labels", Method: "GET", Path: "/projects/{projectId}/labels",
		UI: ui("GET", "/projects/{projectId}/labels")}, func(e *ops.Env, in ProjectLabelListIn) ([]Label, error) {
		if err := authz.RequireProject(e, in.ProjectID, authz.View); err != nil {
			return nil, err
		}
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT `+labelCols+` FROM labels WHERE project_id=? AND board_id IS NULL ORDER BY position,id`, in.ProjectID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []Label{}
		for rows.Next() {
			l, err := scanLabel(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, l)
		}
		return out, rows.Err()
	})
	ops.Register(ops.Op{Name: "label.project.create", Summary: "Create a project-wide label (project manager)", Method: "POST", Path: "/projects/{projectId}/labels",
		UI: ui("POST", "/projects/{projectId}/labels")}, func(e *ops.Env, in ProjectLabelCreateIn) (Label, error) {
		if err := authz.RequireProject(e, in.ProjectID, authz.Manage); err != nil {
			return Label{}, err
		}
		l, err := createLabel(e, "", in.ProjectID, LabelCreateIn{Name: in.Name, Color: in.Color, GroupID: in.GroupID, Position: in.Position})
		if err == nil {
			emitLabel(e, "labelCreate", "label.created", l)
		}
		return l, err
	})
	ops.Register(ops.Op{Name: "label.update", Summary: "Update a label's name, colour or group", Method: "PATCH", Path: "/labels/{id}",
		UI: ui("PATCH", "/labels/{id}")}, func(e *ops.Env, in LabelUpdateIn) (Label, error) {
		l, err := requireLabel(e, in.ID)
		if err != nil {
			return l, err
		}
		if in.Name != nil {
			if err := ops.CheckLen("name", *in.Name, ops.MaxNameLen); err != nil {
				return l, err
			}
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE labels SET name=? WHERE id=?`, nullStr(*in.Name), in.ID); err != nil {
				return l, err
			}
		}
		if in.Color != nil {
			if !validColor(*in.Color) {
				return l, ops.Invalid("invalid color")
			}
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE labels SET color=? WHERE id=?`, *in.Color, in.ID); err != nil {
				return l, err
			}
		}
		if in.GroupID != nil {
			if *in.GroupID != "" {
				bid, pid := "", ""
				if l.BoardID != nil {
					bid = *l.BoardID
				}
				if l.ProjectID != nil {
					pid = *l.ProjectID
				}
				if !groupFits(e, *in.GroupID, bid, pid) {
					return l, ops.NotFound("label group")
				}
			}
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE labels SET group_id=? WHERE id=?`, nullStr(*in.GroupID), in.ID); err != nil {
				return l, err
			}
		}
		l, err = scanLabel(e.DB.QueryRowContext(e.Ctx, `SELECT `+labelCols+` FROM labels WHERE id=?`, in.ID))
		emitLabel(e, "labelUpdate", "label.updated", l)
		return l, err
	})
	ops.Register(ops.Op{Name: "label.move", Summary: "Reorder a label", Method: "POST", Path: "/labels/{id}/move",
		UI: ui("POST", "/labels/{id}/move")}, func(e *ops.Env, in LabelMoveIn) (Label, error) {
		l, err := requireLabel(e, in.ID)
		if err != nil {
			return l, err
		}
		pos := 0.0
		if in.Position != nil {
			pos = *in.Position
		} else if l.BoardID != nil {
			pos = PlaceBetween(e.Ctx, e.DB, "labels", "board_id=? AND id<>?", []any{*l.BoardID, l.ID}, in.AfterID, in.BeforeID)
		} else {
			pos = PlaceBetween(e.Ctx, e.DB, "labels", "project_id=? AND board_id IS NULL AND id<>?", []any{*l.ProjectID, l.ID}, in.AfterID, in.BeforeID)
		}
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE labels SET position=? WHERE id=?`, pos, in.ID); err != nil {
			return l, err
		}
		l.Position = pos
		emitLabel(e, "labelUpdate", "label.updated", l)
		return l, nil
	})
	ops.Register(ops.Op{Name: "label.delete", Summary: "Delete a label (removed from all cards)", Method: "DELETE", Path: "/labels/{id}",
		UI: ui("DELETE", "/labels/{id}")}, func(e *ops.Env, in LabelRefIn) (Deleted, error) {
		l, err := requireLabel(e, in.ID)
		if err != nil {
			return Deleted{}, err
		}
		boards := labelBoards(e, l)
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM labels WHERE id=?`, in.ID); err != nil {
			return Deleted{}, err
		}
		for _, b := range boards {
			emit(e, "labelDelete", "label.deleted", b, map[string]any{"id": in.ID})
		}
		return Deleted{in.ID}, nil
	})

	ops.Register(ops.Op{Name: "labelgroup.list", Summary: "List label groups visible on a board", Method: "GET", Path: "/boards/{boardId}/label-groups",
		UI: ui("GET", "/boards/{boardId}/label-groups")}, func(e *ops.Env, in LabelListIn) ([]LabelGroup, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return nil, err
		}
		return groupsOf(e, in.BoardID)
	})
	ops.Register(ops.Op{Name: "labelgroup.create", Summary: "Create a label group on a board", Method: "POST", Path: "/boards/{boardId}/label-groups",
		UI: ui("POST", "/boards/{boardId}/label-groups")}, func(e *ops.Env, in GroupCreateIn) (LabelGroup, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.Edit); err != nil {
			return LabelGroup{}, err
		}
		pid, use := projectOfBoard(e, in.BoardID)
		if use {
			if err := authz.RequireProject(e, pid, authz.Manage); err != nil {
				return LabelGroup{}, err
			}
			return createGroup(e, "", pid, in.Name, in.SingleSelect, in.BoardID)
		}
		return createGroup(e, in.BoardID, "", in.Name, in.SingleSelect, in.BoardID)
	})
	ops.Register(ops.Op{Name: "labelgroup.project.create", Summary: "Create a project-wide label group (project manager)", Method: "POST", Path: "/projects/{projectId}/label-groups",
		UI: ui("POST", "/projects/{projectId}/label-groups")}, func(e *ops.Env, in ProjectGroupCreateIn) (LabelGroup, error) {
		if err := authz.RequireProject(e, in.ProjectID, authz.Manage); err != nil {
			return LabelGroup{}, err
		}
		return createGroup(e, "", in.ProjectID, in.Name, in.SingleSelect, "")
	})
	ops.Register(ops.Op{Name: "labelgroup.update", Summary: "Rename / reorder a label group or toggle single-select", Method: "PATCH", Path: "/label-groups/{id}",
		UI: ui("PATCH", "/label-groups/{id}")}, func(e *ops.Env, in GroupUpdateIn) (LabelGroup, error) {
		g, err := requireGroup(e, in.ID)
		if err != nil {
			return g, err
		}
		if in.Name != nil {
			if *in.Name == "" {
				return g, ops.Invalid("name must not be empty")
			}
			g.Name = *in.Name
		}
		if in.SingleSelect != nil {
			g.SingleSelect = *in.SingleSelect
		}
		if in.Position != nil {
			g.Position = *in.Position
		}
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE label_groups SET name=?,single_select=?,position=? WHERE id=?`, g.Name, boolInt(g.SingleSelect), g.Position, g.ID); err != nil {
			return g, err
		}
		emitGroup(e, g)
		return g, nil
	})
	ops.Register(ops.Op{Name: "labelgroup.delete", Summary: "Delete a label group (labels are kept, ungrouped)", Method: "DELETE", Path: "/label-groups/{id}",
		UI: ui("DELETE", "/label-groups/{id}")}, func(e *ops.Env, in LabelRefIn) (Deleted, error) {
		g, err := requireGroup(e, in.ID)
		if err != nil {
			return Deleted{}, err
		}
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM label_groups WHERE id=?`, in.ID); err != nil {
			return Deleted{}, err
		}
		emitGroup(e, g)
		return Deleted{in.ID}, nil
	})
}

func emitGroup(e *ops.Env, g LabelGroup) {
	for _, b := range labelBoards(e, Label{BoardID: g.BoardID, ProjectID: g.ProjectID}) {
		emit(e, "labelGroupUpdate", "label.updated", b, map[string]any{"groupId": g.ID})
	}
}

func createGroup(e *ops.Env, boardID, projectID, name string, ss bool, emitBoard string) (LabelGroup, error) {
	if name == "" {
		return LabelGroup{}, ops.Invalid("name is required")
	}
	var scope string
	var args []any
	var bcol, pcol any
	if boardID != "" {
		scope, args, bcol = "board_id=?", []any{boardID}, boardID
	} else {
		scope, args, pcol = "project_id=? AND board_id IS NULL", []any{projectID}, projectID
	}
	id := db.NewID()
	pos := appendPos(e.Ctx, e.DB, "label_groups", scope, args...)
	if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO label_groups(id,board_id,project_id,name,single_select,position) VALUES(?,?,?,?,?,?)`, id, bcol, pcol, name, boolInt(ss), pos); err != nil {
		return LabelGroup{}, err
	}
	g := LabelGroup{ID: id, Name: name, SingleSelect: ss, Position: pos}
	if boardID != "" {
		g.BoardID = &boardID
	} else {
		g.ProjectID = &projectID
	}
	emitGroup(e, g)
	return g, nil
}
