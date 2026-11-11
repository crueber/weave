package boards

import (
	"context"
	"database/sql"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// Board is the API representation of a board.
type Board struct {
	ID                       string  `json:"id"`
	ProjectID                string  `json:"projectId"`
	Position                 float64 `json:"position"`
	Name                     string  `json:"name"`
	DefaultView              string  `json:"defaultView"`
	DefaultCardType          string  `json:"defaultCardType"`
	LimitCardTypesToDefault  bool    `json:"limitCardTypesToDefault"`
	AlwaysDisplayCardCreator bool    `json:"alwaysDisplayCardCreator"`
	ExpandTaskListsByDefault bool    `json:"expandTaskListsByDefault"`
	ShowCardCounter          bool    `json:"showCardCounter"`
	ShowCardAges             bool    `json:"showCardAges"`
	IsPublic                 bool    `json:"isPublic"`
	PublicToken              string  `json:"publicToken,omitempty"`
	IsTemplate               bool    `json:"isTemplate"`
	HasInbox                 bool    `json:"hasInbox"`
	Color                    *string `json:"color,omitempty" desc:"custom board colour, #rrggbb"`
	CardColorMode            string  `json:"cardColorMode" desc:"pill (label pills) | fill (first label colours the whole card)"`
	CreatedAt                string  `json:"createdAt"`
	UpdatedAt                *string `json:"updatedAt,omitempty"`
}

const boardCols = `id,project_id,position,name,default_view,default_card_type,limit_card_types_to_default,always_display_card_creator,
	expand_task_lists_by_default,show_card_counter,show_card_ages,is_public,COALESCE(public_token,''),is_template,has_inbox,color,card_color_mode,created_at,updated_at`

func scanBoard(r interface{ Scan(...any) error }) (Board, error) {
	var b Board
	err := r.Scan(&b.ID, &b.ProjectID, &b.Position, &b.Name, &b.DefaultView, &b.DefaultCardType, &b.LimitCardTypesToDefault,
		&b.AlwaysDisplayCardCreator, &b.ExpandTaskListsByDefault, &b.ShowCardCounter, &b.ShowCardAges, &b.IsPublic, &b.PublicToken,
		&b.IsTemplate, &b.HasInbox, &b.Color, &b.CardColorMode, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

// GetBoard loads a board row without any permission check.
func GetBoard(ctx context.Context, q querier, id string) (Board, error) {
	b, err := scanBoard(q.QueryRowContext(ctx, `SELECT `+boardCols+` FROM boards WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return b, ops.NotFound("board")
	}
	return b, err
}

// BoardDetail is board.get's output.
type BoardDetail struct {
	Board       Board        `json:"board"`
	Level       string       `json:"level"`
	Lists       []List       `json:"lists"`
	Labels      []Label      `json:"labels"`
	LabelGroups []LabelGroup `json:"labelGroups"`
	Members     []Member     `json:"members"`
}

func loadDetail(e *ops.Env, b Board, withMembers bool) (BoardDetail, error) {
	d := BoardDetail{Board: b, Level: levelName(authz.BoardLevel(e, b.ID))}
	var err error
	if d.Lists, err = listsOf(e, b.ID, false); err != nil {
		return d, err
	}
	if d.Labels, err = labelsOf(e, b.ID); err != nil {
		return d, err
	}
	if d.LabelGroups, err = groupsOf(e, b.ID); err != nil {
		return d, err
	}
	if withMembers {
		if d.Members, err = membersOf(e, b.ID); err != nil {
			return d, err
		}
	}
	return d, nil
}

var boardViews = map[string]bool{"kanban": true, "grid": true, "list": true, "calendar": true, "map": true}

func validView(v string) bool     { return boardViews[v] }
func validCardType(t string) bool { return t == "project" || t == "story" }

// EnsureSystemLists creates the archive and trash lists of a board.
func EnsureSystemLists(ctx context.Context, q querier, boardID string) error {
	for _, t := range []string{"archive", "trash"} {
		var n int
		_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM lists WHERE board_id=? AND type=?`, boardID, t).Scan(&n)
		if n == 0 {
			if _, err := q.ExecContext(ctx, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES(?,?,?,NULL,NULL,?)`,
				db.NewID(), boardID, t, db.Now()); err != nil {
				return err
			}
		}
	}
	return nil
}

// SystemList returns the id of the board's archive or trash list.
func SystemList(ctx context.Context, q querier, boardID, typ string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM lists WHERE board_id=? AND type=?`, boardID, typ).Scan(&id)
	if err == sql.ErrNoRows {
		if err = EnsureSystemLists(ctx, q, boardID); err != nil {
			return "", err
		}
		err = q.QueryRowContext(ctx, `SELECT id FROM lists WHERE board_id=? AND type=?`, boardID, typ).Scan(&id)
	}
	return id, err
}

// --- inputs ---

type BoardListIn struct {
	ProjectID string `path:"projectId" desc:"project id"`
}
type BoardGetIn struct {
	ID string `path:"id" desc:"board id"`
}
type BoardCreateIn struct {
	ProjectID   string   `path:"projectId" desc:"project id"`
	Name        string   `json:"name" required:"true" desc:"board name"`
	Position    *float64 `json:"position" desc:"explicit position; default end of project"`
	DefaultView string   `json:"defaultView" desc:"kanban|grid|list|calendar|map (default kanban)"`
}
type BoardUpdateIn struct {
	ID                       string   `path:"id"`
	Name                     *string  `json:"name"`
	Position                 *float64 `json:"position" desc:"project managers only"`
	DefaultView              *string  `json:"defaultView" desc:"kanban|grid|list|calendar|map"`
	DefaultCardType          *string  `json:"defaultCardType" desc:"project|story"`
	LimitCardTypesToDefault  *bool    `json:"limitCardTypesToDefault"`
	AlwaysDisplayCardCreator *bool    `json:"alwaysDisplayCardCreator"`
	ExpandTaskListsByDefault *bool    `json:"expandTaskListsByDefault"`
	ShowCardCounter          *bool    `json:"showCardCounter"`
	ShowCardAges             *bool    `json:"showCardAges"`
	HasInbox                 *bool    `json:"hasInbox"`
	Color                    *string  `json:"color" desc:"board colour: #rrggbb or a preset name (crimson, orange, amber, lime, green, teal, sky, blue, indigo, violet, pink, slate); empty string clears it"`
	CardColorMode            *string  `json:"cardColorMode" desc:"pill (label pills, default) | fill (the card's first label by label order colours the whole card; colours are never mixed)"`
}
type BoardMoveIn struct {
	ID       string   `path:"id"`
	AfterID  string   `json:"afterId" desc:"board to place after (optional)"`
	BeforeID string   `json:"beforeId" desc:"board to place before (optional)"`
	Position *float64 `json:"position" desc:"explicit position (overrides afterId/beforeId)"`
}
type BoardPublicIn struct {
	ID       string `path:"id"`
	IsPublic bool   `json:"isPublic" desc:"enable anonymous read-only access"`
	Rotate   bool   `json:"rotate" desc:"generate a fresh public token"`
}
type PublicGetIn struct {
	Token string `path:"token" desc:"public board token"`
}
type TemplatePublishIn struct {
	ID         string `path:"id"`
	IsTemplate bool   `json:"isTemplate" desc:"mark the board as a reusable template"`
}
type TemplateListIn struct {
	ProjectID string `query:"projectId" desc:"limit to a project (optional)"`
}
type InstantiateIn struct {
	ProjectID  string `path:"projectId" desc:"target project"`
	TemplateID string `json:"templateId" required:"true" desc:"template board id"`
	Name       string `json:"name" required:"true" desc:"new board name"`
}

func registerBoardOps() {
	ops.Register(ops.Op{Name: "board.list", Summary: "List boards of a project visible to the caller", Method: "GET", Path: "/projects/{projectId}/boards",
		UI: ui("GET", "/projects/{projectId}/boards")}, boardList)
	ops.Register(ops.Op{Name: "board.get", Summary: "Get a board with lists, labels, label groups and members", Method: "GET", Path: "/boards/{id}",
		UI: "GET /boards/{id}"}, func(e *ops.Env, in BoardGetIn) (BoardDetail, error) {
		if err := authz.RequireBoard(e, in.ID, authz.View); err != nil {
			return BoardDetail{}, err
		}
		b, err := GetBoard(e.Ctx, e.DB, in.ID)
		if err != nil {
			return BoardDetail{}, err
		}
		lvl := authz.BoardLevel(e, in.ID)
		if lvl < authz.Manage {
			b.PublicToken = ""
		}
		return loadDetail(e, b, e.User != nil)
	})
	ops.Register(ops.Op{Name: "board.create", Summary: "Create a board in a project (project manager)", Method: "POST", Path: "/projects/{projectId}/boards",
		UI: ui("POST", "/projects/{projectId}/boards")}, boardCreate)
	ops.Register(ops.Op{Name: "board.update", Summary: "Update board name and settings", Method: "PATCH", Path: "/boards/{id}",
		UI: ui("PATCH", "/boards/{id}")}, boardUpdate)
	ops.Register(ops.Op{Name: "board.move", Summary: "Reorder a board within its project (project manager)", Method: "POST", Path: "/boards/{id}/move",
		UI: ui("POST", "/boards/{id}/move")}, boardMove)
	ops.Register(ops.Op{Name: "board.delete", Summary: "Delete a board and everything on it (project manager)", Method: "DELETE", Path: "/boards/{id}",
		UI: ui("DELETE", "/boards/{id}")}, func(e *ops.Env, in BoardGetIn) (Deleted, error) {
		if err := authz.RequireBoard(e, in.ID, authz.Manage); err != nil {
			return Deleted{}, err
		}
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM boards WHERE id=?`, in.ID); err != nil {
			return Deleted{}, err
		}
		emit(e, "boardDelete", "board.deleted", in.ID, map[string]any{"id": in.ID})
		return Deleted{in.ID}, nil
	})
	ops.Register(ops.Op{Name: "board.setPublic", Summary: "Enable/disable anonymous read-only access (and rotate the public token)", Method: "POST", Path: "/boards/{id}/public",
		UI: ui("POST", "/boards/{id}/public")}, boardSetPublic)
	ops.Register(ops.Op{Name: "board.public.get", Summary: "Anonymous read-only view of a public board", Method: "GET", Path: "/public/boards/{token}",
		Auth: ops.AuthPublic, UI: "GET /public/{token}"}, func(e *ops.Env, in PublicGetIn) (BoardDetail, error) {
		b, err := publicBoard(e, in.Token)
		if err != nil {
			return BoardDetail{}, err
		}
		b.PublicToken = ""
		d := BoardDetail{Board: b, Level: "view"}
		if d.Lists, err = listsOf(e, b.ID, false); err != nil {
			return d, err
		}
		if d.Labels, err = labelsOf(e, b.ID); err != nil {
			return d, err
		}
		d.LabelGroups, err = groupsOf(e, b.ID)
		return d, err
	})
	ops.Register(ops.Op{Name: "board.template.publish", Summary: "Publish/unpublish a board as a template", Method: "POST", Path: "/boards/{id}/template",
		UI: ui("POST", "/boards/{id}/template")}, func(e *ops.Env, in TemplatePublishIn) (Board, error) {
		if err := authz.RequireBoard(e, in.ID, authz.Manage); err != nil {
			return Board{}, err
		}
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE boards SET is_template=?,updated_at=? WHERE id=?`, boolInt(in.IsTemplate), db.Now(), in.ID); err != nil {
			return Board{}, err
		}
		b, err := GetBoard(e.Ctx, e.DB, in.ID)
		emit(e, "boardUpdate", "board.updated", in.ID, map[string]any{"id": in.ID})
		return b, err
	})
	ops.Register(ops.Op{Name: "board.template.list", Summary: "List board templates visible to the caller", Method: "GET", Path: "/board-templates",
		UI: "GET /ui/board-templates"}, templateList)
	ops.Register(ops.Op{Name: "board.instantiate", Summary: "Create a board from a template in a project", Method: "POST", Path: "/projects/{projectId}/boards/from-template",
		UI: ui("POST", "/projects/{projectId}/boards/from-template")}, instantiate)
}

func publicBoard(e *ops.Env, token string) (Board, error) {
	if token == "" {
		return Board{}, ops.NotFound("board")
	}
	b, err := scanBoard(e.DB.QueryRowContext(e.Ctx, `SELECT `+boardCols+` FROM boards WHERE public_token=? AND is_public=1`, token))
	if err != nil {
		return b, ops.NotFound("board")
	}
	return b, nil
}

func boardList(e *ops.Env, in BoardListIn) ([]Board, error) {
	var n int
	if err := e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM projects WHERE id=?`, in.ProjectID).Scan(&n); err != nil || n == 0 {
		return nil, ops.NotFound("project")
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT `+boardCols+` FROM boards WHERE project_id=? ORDER BY position, id`, in.ProjectID)
	if err != nil {
		return nil, err
	}
	var all []Board
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, b)
	}
	rows.Close()
	out := []Board{}
	for _, b := range all {
		if l := authz.BoardLevel(e, b.ID); l >= authz.View {
			if l < authz.Manage {
				b.PublicToken = ""
			}
			out = append(out, b)
		}
	}
	if len(out) == 0 && authz.ProjectLevel(e, in.ProjectID) == authz.None {
		return nil, ops.NotFound("project")
	}
	return out, nil
}

func boardCreate(e *ops.Env, in BoardCreateIn) (Board, error) {
	if err := authz.RequireProject(e, in.ProjectID, authz.Manage); err != nil {
		return Board{}, err
	}
	if in.Name == "" {
		return Board{}, ops.Invalid("name is required")
	}
	if err := ops.CheckLen("name", in.Name, ops.MaxNameLen); err != nil {
		return Board{}, err
	}
	view := in.DefaultView
	if view == "" {
		view = "kanban"
	}
	if !validView(view) {
		return Board{}, ops.Invalid("invalid defaultView")
	}
	id := db.NewID()
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		return insertBoard(e, tx, id, in.ProjectID, in.Name, view, in.Position)
	})
	if err != nil {
		return Board{}, err
	}
	emit(e, "boardCreate", "board.created", id, map[string]any{"id": id, "projectId": in.ProjectID})
	e.Hub.Publish(projectEvent("board.created", in.ProjectID, id))
	return GetBoard(e.Ctx, e.DB, id)
}

func insertBoard(e *ops.Env, tx *sql.Tx, id, projectID, name, view string, pos *float64) error {
	var p float64
	if pos != nil {
		p = *pos
	} else {
		p = appendPos(e.Ctx, tx, "boards", "project_id=?", projectID)
	}
	now := db.Now()
	if _, err := tx.ExecContext(e.Ctx, `INSERT INTO boards(id,project_id,position,name,default_view,created_at) VALUES(?,?,?,?,?,?)`,
		id, projectID, p, name, view, now); err != nil {
		return err
	}
	if err := EnsureSystemLists(e.Ctx, tx, id); err != nil {
		return err
	}
	if e.User != nil {
		if _, err := tx.ExecContext(e.Ctx, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`,
			db.NewID(), id, e.User.UserID, "editor", now); err != nil {
			return err
		}
	}
	return nil
}

func boardUpdate(e *ops.Env, in BoardUpdateIn) (Board, error) {
	if err := authz.RequireBoard(e, in.ID, authz.Edit); err != nil {
		return Board{}, err
	}
	sets, args := []string{}, []any{}
	add := func(col string, v any) { sets = append(sets, col+"=?"); args = append(args, v) }
	if in.Name != nil {
		if *in.Name == "" {
			return Board{}, ops.Invalid("name must not be empty")
		}
		if err := ops.CheckLen("name", *in.Name, ops.MaxNameLen); err != nil {
			return Board{}, err
		}
		add("name", *in.Name)
	}
	if in.Position != nil {
		if authz.BoardLevel(e, in.ID) < authz.Manage {
			return Board{}, ops.Forbidden()
		}
		add("position", *in.Position)
	}
	if in.DefaultView != nil {
		if !validView(*in.DefaultView) {
			return Board{}, ops.Invalid("invalid defaultView")
		}
		add("default_view", *in.DefaultView)
	}
	if in.DefaultCardType != nil {
		if !validCardType(*in.DefaultCardType) {
			return Board{}, ops.Invalid("invalid defaultCardType")
		}
		add("default_card_type", *in.DefaultCardType)
	}
	if in.Color != nil {
		hex, err := NormalizeColor(*in.Color)
		if err != nil {
			return Board{}, err
		}
		if hex == "" {
			add("color", nil)
		} else {
			add("color", hex)
		}
	}
	if in.CardColorMode != nil {
		if !validCardColorMode(*in.CardColorMode) {
			return Board{}, ops.Invalid("cardColorMode must be pill or fill")
		}
		add("card_color_mode", *in.CardColorMode)
	}
	for col, v := range map[string]*bool{"limit_card_types_to_default": in.LimitCardTypesToDefault, "always_display_card_creator": in.AlwaysDisplayCardCreator,
		"expand_task_lists_by_default": in.ExpandTaskListsByDefault, "show_card_counter": in.ShowCardCounter, "show_card_ages": in.ShowCardAges, "has_inbox": in.HasInbox} {
		if v != nil {
			add(col, boolInt(*v))
		}
	}
	if len(sets) > 0 {
		add("updated_at", db.Now())
		args = append(args, in.ID)
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE boards SET `+join(sets)+` WHERE id=?`, args...); err != nil {
			return Board{}, err
		}
		emit(e, "boardUpdate", "board.updated", in.ID, map[string]any{"id": in.ID})
	}
	b, err := GetBoard(e.Ctx, e.DB, in.ID)
	if authz.BoardLevel(e, in.ID) < authz.Manage {
		b.PublicToken = ""
	}
	return b, err
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

func boardMove(e *ops.Env, in BoardMoveIn) (Board, error) {
	if err := authz.RequireBoard(e, in.ID, authz.Manage); err != nil {
		return Board{}, err
	}
	b, err := GetBoard(e.Ctx, e.DB, in.ID)
	if err != nil {
		return b, err
	}
	pos := 0.0
	if in.Position != nil {
		pos = *in.Position
	} else {
		pos = PlaceBetween(e.Ctx, e.DB, "boards", "project_id=? AND id<>?", []any{b.ProjectID, b.ID}, in.AfterID, in.BeforeID)
	}
	if _, err := e.DB.ExecContext(e.Ctx, `UPDATE boards SET position=?,updated_at=? WHERE id=?`, pos, db.Now(), in.ID); err != nil {
		return b, err
	}
	emit(e, "boardUpdate", "board.updated", in.ID, map[string]any{"id": in.ID, "position": pos})
	return GetBoard(e.Ctx, e.DB, in.ID)
}

func boardSetPublic(e *ops.Env, in BoardPublicIn) (Board, error) {
	if err := authz.RequireBoard(e, in.ID, authz.Manage); err != nil {
		return Board{}, err
	}
	var tok sql.NullString
	var was bool
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT public_token,is_public FROM boards WHERE id=?`, in.ID).Scan(&tok, &was)
	// a link that was switched off must never come back to life: re-enabling issues a fresh token
	if (in.IsPublic && (!tok.Valid || !was)) || in.Rotate {
		tok = sql.NullString{String: db.Token(16), Valid: true}
	}
	if _, err := e.DB.ExecContext(e.Ctx, `UPDATE boards SET is_public=?,public_token=?,updated_at=? WHERE id=?`,
		boolInt(in.IsPublic), tok, db.Now(), in.ID); err != nil {
		return Board{}, err
	}
	emit(e, "boardUpdate", "board.updated", in.ID, map[string]any{"id": in.ID, "isPublic": in.IsPublic})
	return GetBoard(e.Ctx, e.DB, in.ID)
}

func templateList(e *ops.Env, in TemplateListIn) ([]Board, error) {
	q := `SELECT ` + boardCols + ` FROM boards WHERE is_template=1`
	args := []any{}
	if in.ProjectID != "" {
		q += ` AND project_id=?`
		args = append(args, in.ProjectID)
	}
	rows, err := e.DB.QueryContext(e.Ctx, q+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	var all []Board
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, b)
	}
	rows.Close()
	out := []Board{}
	for _, b := range all {
		if authz.BoardLevel(e, b.ID) >= authz.View {
			b.PublicToken = ""
			out = append(out, b)
		}
	}
	return out, nil
}

// instantiate copies lists, labels, label groups, settings and cards (with
// labels and task lists) of a template board into a new board.
func instantiate(e *ops.Env, in InstantiateIn) (Board, error) {
	if err := authz.RequireProject(e, in.ProjectID, authz.Manage); err != nil {
		return Board{}, err
	}
	if err := authz.RequireBoard(e, in.TemplateID, authz.View); err != nil {
		return Board{}, err
	}
	src, err := GetBoard(e.Ctx, e.DB, in.TemplateID)
	if err != nil {
		return Board{}, err
	}
	if !src.IsTemplate {
		return Board{}, ops.Invalid("board is not a template")
	}
	if in.Name == "" {
		return Board{}, ops.Invalid("name is required")
	}
	if err := ops.CheckLen("name", in.Name, ops.MaxNameLen); err != nil {
		return Board{}, err
	}
	id := db.NewID()
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		if err := insertBoard(e, tx, id, in.ProjectID, in.Name, src.DefaultView, nil); err != nil {
			return err
		}
		if _, err := tx.ExecContext(e.Ctx, `UPDATE boards SET default_card_type=?,limit_card_types_to_default=?,always_display_card_creator=?,
			expand_task_lists_by_default=?,show_card_counter=?,show_card_ages=?,has_inbox=?,color=?,card_color_mode=? WHERE id=?`, src.DefaultCardType, boolInt(src.LimitCardTypesToDefault),
			boolInt(src.AlwaysDisplayCardCreator), boolInt(src.ExpandTaskListsByDefault), boolInt(src.ShowCardCounter), boolInt(src.ShowCardAges), boolInt(src.HasInbox), src.Color, src.CardColorMode, id); err != nil {
			return err
		}
		return copyBoardContent(e.Ctx, tx, src.ID, id, in.ProjectID)
	})
	if err != nil {
		return Board{}, err
	}
	emit(e, "boardCreate", "board.created", id, map[string]any{"id": id, "projectId": in.ProjectID})
	e.Hub.Publish(projectEvent("board.created", in.ProjectID, id))
	return GetBoard(e.Ctx, e.DB, id)
}

func copyBoardContent(ctx context.Context, tx *sql.Tx, srcID, dstID, dstProject string) error {
	now := db.Now()
	var useProject bool
	_ = tx.QueryRowContext(ctx, `SELECT use_project_labels FROM projects WHERE id=?`, dstProject).Scan(&useProject)
	groupMap, labelMap, listMap := map[string]string{}, map[string]string{}, map[string]string{}
	if !useProject {
		rows, err := tx.QueryContext(ctx, `SELECT id,name,single_select,position FROM label_groups WHERE board_id=?`, srcID)
		if err != nil {
			return err
		}
		type g struct {
			id, name string
			ss       bool
			pos      float64
		}
		var gs []g
		for rows.Next() {
			var x g
			_ = rows.Scan(&x.id, &x.name, &x.ss, &x.pos)
			gs = append(gs, x)
		}
		rows.Close()
		for _, x := range gs {
			nid := db.NewID()
			groupMap[x.id] = nid
			if _, err := tx.ExecContext(ctx, `INSERT INTO label_groups(id,board_id,name,single_select,position) VALUES(?,?,?,?,?)`, nid, dstID, x.name, x.ss, x.pos); err != nil {
				return err
			}
		}
		rows, err = tx.QueryContext(ctx, `SELECT id,group_id,name,color,position FROM labels WHERE board_id=?`, srcID)
		if err != nil {
			return err
		}
		type l struct {
			id    string
			grp   sql.NullString
			name  sql.NullString
			color string
			pos   float64
		}
		var ls []l
		for rows.Next() {
			var x l
			_ = rows.Scan(&x.id, &x.grp, &x.name, &x.color, &x.pos)
			ls = append(ls, x)
		}
		rows.Close()
		for _, x := range ls {
			nid := db.NewID()
			labelMap[x.id] = nid
			var grp any
			if x.grp.Valid {
				grp = groupMap[x.grp.String]
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO labels(id,board_id,group_id,name,color,position,created_at) VALUES(?,?,?,?,?,?,?)`, nid, dstID, grp, x.name, x.color, x.pos, now); err != nil {
				return err
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,type,position,name,color FROM lists WHERE board_id=? AND type IN ('active','closed') ORDER BY position`, srcID)
	if err != nil {
		return err
	}
	type li struct {
		id, typ string
		pos     float64
		name    sql.NullString
		color   sql.NullString
	}
	var lists []li
	for rows.Next() {
		var x li
		_ = rows.Scan(&x.id, &x.typ, &x.pos, &x.name, &x.color)
		lists = append(lists, x)
	}
	rows.Close()
	for _, x := range lists {
		nid := db.NewID()
		listMap[x.id] = nid
		if _, err := tx.ExecContext(ctx, `INSERT INTO lists(id,board_id,type,position,name,color,created_at) VALUES(?,?,?,?,?,?,?)`, nid, dstID, x.typ, x.pos, x.name, x.color, now); err != nil {
			return err
		}
	}
	// cards
	crows, err := tx.QueryContext(ctx, `SELECT id,list_id,type,position,name,description FROM cards WHERE board_id=? AND is_draft=0 AND is_template=0 AND list_id IN
		(SELECT id FROM lists WHERE board_id=? AND type IN ('active','closed')) ORDER BY position`, srcID, srcID)
	if err != nil {
		return err
	}
	type cd struct {
		id, list, typ, name string
		pos                 sql.NullFloat64
		desc                sql.NullString
	}
	var cards []cd
	for crows.Next() {
		var x cd
		_ = crows.Scan(&x.id, &x.list, &x.typ, &x.pos, &x.name, &x.desc)
		cards = append(cards, x)
	}
	crows.Close()
	for _, x := range cards {
		nid := db.NewID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO cards(id,board_id,list_id,type,position,name,description,list_changed_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			nid, dstID, listMap[x.list], x.typ, x.pos, x.name, x.desc, now, now); err != nil {
			return err
		}
		for old, nl := range labelMap {
			_, _ = tx.ExecContext(ctx, `INSERT INTO card_labels(card_id,label_id) SELECT ?,? WHERE EXISTS(SELECT 1 FROM card_labels WHERE card_id=? AND label_id=?)`, nid, nl, x.id, old)
		}
		trs, err := tx.QueryContext(ctx, `SELECT id,position,name,show_on_front_of_card,hide_completed_tasks FROM task_lists WHERE card_id=?`, x.id)
		if err != nil {
			return err
		}
		type tl struct {
			id, name string
			pos      float64
			a, b     bool
		}
		var tls []tl
		for trs.Next() {
			var t tl
			_ = trs.Scan(&t.id, &t.pos, &t.name, &t.a, &t.b)
			tls = append(tls, t)
		}
		trs.Close()
		for _, t := range tls {
			ntl := db.NewID()
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_lists(id,card_id,position,name,show_on_front_of_card,hide_completed_tasks,created_at) VALUES(?,?,?,?,?,?,?)`,
				ntl, nid, t.pos, t.name, t.a, t.b, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,task_list_id,position,name,is_completed,created_at)
				SELECT lower(hex(randomblob(11))),?,position,name,0,? FROM tasks WHERE task_list_id=?`, ntl, now, t.id); err != nil {
				return err
			}
		}
	}
	return nil
}
