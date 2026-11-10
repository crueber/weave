package cardextras

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

var fieldTypes = map[string]bool{"text": true, "number": true, "date": true, "checkbox": true, "select": true, "url": true}

type FieldItem struct {
	ID                string   `json:"id"`
	GroupID           *string  `json:"groupId"`
	BaseGroupID       *string  `json:"baseGroupId"`
	Name              string   `json:"name"`
	Type              string   `json:"type"`
	Options           []string `json:"options"`
	Position          float64  `json:"position"`
	ShowOnFrontOfCard bool     `json:"showOnFrontOfCard"`
}

// RowItem is one row of a group's table for a card. Non-repeatable groups have a single row with id "".
type RowItem struct {
	ID       string            `json:"id"`
	CardID   string            `json:"cardId"`
	GroupID  string            `json:"groupId"`
	Position float64           `json:"position"`
	Values   map[string]string `json:"values"` // fieldId -> content
}

type GroupItem struct {
	ID          string      `json:"id"`
	Scope       string      `json:"scope"` // board | card
	BoardID     *string     `json:"boardId"`
	CardID      *string     `json:"cardId"`
	BaseGroupID *string     `json:"baseGroupId"`
	Name        string      `json:"name"`
	Repeatable  bool        `json:"repeatable"`
	Position    float64     `json:"position"`
	Fields      []FieldItem `json:"fields"`
	Rows        []RowItem   `json:"rows"`
}

// FieldsOut is the custom-field state of one card (groups that apply to it, with its values).
type ValueItem struct {
	CardID  string `json:"cardId"`
	GroupID string `json:"groupId"`
	FieldID string `json:"fieldId"`
	RowID   string `json:"rowId"`
	Content string `json:"content"`
}

type GroupListIn struct {
	CardID string `path:"cardId"`
}
type GroupCreateIn struct {
	BoardID     string `json:"boardId" desc:"Create a board-wide group (applies to every card of the board)"`
	CardID      string `json:"cardId" desc:"Create a group for this card only"`
	BaseGroupID string `json:"baseGroupId" desc:"Derive fields from a project base custom-field group"`
	Name        string `json:"name" desc:"Group name (defaults to the base group name)"`
	Repeatable  bool   `json:"repeatable" desc:"Repeatable groups are a table: each card can hold many rows"`
}
type GroupUpdateIn struct {
	ID         string   `path:"id"`
	Name       *string  `json:"name"`
	Repeatable *bool    `json:"repeatable" desc:"Only changeable while the group holds no values"`
	Position   *float64 `json:"position"`
	AfterID    string   `json:"afterId"`
	BeforeID   string   `json:"beforeId"`
}
type FieldCreateIn struct {
	GroupID           string   `json:"groupId" desc:"Add the field to this group"`
	BaseGroupID       string   `json:"baseGroupId" desc:"or add it to this project base group"`
	Name              string   `json:"name" required:"true"`
	Type              string   `json:"type" desc:"text|number|date|checkbox|select|url (default text)"`
	Options           []string `json:"options" desc:"Choices for select fields"`
	ShowOnFrontOfCard bool     `json:"showOnFrontOfCard"`
}
type FieldUpdateIn struct {
	ID                string    `path:"id"`
	Name              *string   `json:"name"`
	Type              *string   `json:"type" desc:"Changing the type clears the field's existing values"`
	Options           *[]string `json:"options"`
	ShowOnFrontOfCard *bool     `json:"showOnFrontOfCard"`
	Position          *float64  `json:"position"`
	AfterID           string    `json:"afterId"`
	BeforeID          string    `json:"beforeId"`
}
type ValueSetIn struct {
	CardID  string `path:"cardId"`
	GroupID string `json:"groupId" required:"true"`
	FieldID string `json:"fieldId" required:"true"`
	RowID   string `json:"rowId" desc:"Row id for repeatable groups; empty otherwise"`
	Content string `json:"content" desc:"Value; empty clears (checkbox: true|false)"`
}
type ValueClearIn struct {
	CardID  string `path:"cardId"`
	GroupID string `json:"groupId" required:"true"`
	FieldID string `json:"fieldId" required:"true"`
	RowID   string `json:"rowId"`
}
type RowCreateIn struct {
	CardID  string `path:"cardId"`
	GroupID string `path:"groupId"`
}
type RowUpdateIn struct {
	ID       string   `path:"id"`
	Position *float64 `json:"position"`
	AfterID  string   `json:"afterId"`
	BeforeID string   `json:"beforeId"`
}

type groupRow struct {
	ID                     string
	Board, Card, BaseGroup sql.NullString
	Name                   string
	Repeatable             bool
	Position               float64
}

func (g groupRow) boardID(e *ops.Env) (string, error) {
	if g.Board.Valid {
		return g.Board.String, nil
	}
	return authz.BoardOfCard(e, g.Card.String)
}

func loadGroup(e *ops.Env, q querier, id string) (groupRow, error) {
	var g groupRow
	var rep int
	if q.QueryRowContext(e.Ctx, `SELECT g.id,g.board_id,g.card_id,g.base_group_id,COALESCE(g.name,b.name,''),g.repeatable,g.position
		FROM custom_field_groups g LEFT JOIN base_custom_field_groups b ON b.id=g.base_group_id WHERE g.id=?`, id).
		Scan(&g.ID, &g.Board, &g.Card, &g.BaseGroup, &g.Name, &rep, &g.Position) != nil {
		return g, ops.NotFound("custom field group")
	}
	g.Repeatable = rep != 0
	return g, nil
}

func (g groupRow) scope() (string, []any) {
	if g.Board.Valid {
		return "board_id=?", []any{g.Board.String}
	}
	return "card_id=?", []any{g.Card.String}
}

func scanField(r *sql.Rows) (FieldItem, error) {
	var f FieldItem
	var gid, bid, opts sql.NullString
	var front int
	err := r.Scan(&f.ID, &gid, &bid, &f.Name, &f.Type, &opts, &f.Position, &front)
	f.GroupID, f.BaseGroupID, f.ShowOnFrontOfCard = strp(gid), strp(bid), front != 0
	f.Options = []string{}
	if opts.Valid {
		_ = json.Unmarshal([]byte(opts.String), &f.Options)
	}
	return f, err
}

const fieldSelect = `SELECT id,group_id,base_group_id,name,type,options,position,show_on_front_of_card FROM custom_fields`

func groupFields(e *ops.Env, q querier, g groupRow) ([]FieldItem, error) {
	if g.BaseGroup.Valid {
		return query(e.Ctx, q, scanField, fieldSelect+` WHERE group_id=? OR base_group_id=? ORDER BY position,id`, g.ID, g.BaseGroup.String)
	}
	return query(e.Ctx, q, scanField, fieldSelect+` WHERE group_id=? ORDER BY position,id`, g.ID)
}

func groupItem(g groupRow, fields []FieldItem) GroupItem {
	it := GroupItem{ID: g.ID, Scope: "card", BoardID: strp(g.Board), CardID: strp(g.Card), BaseGroupID: strp(g.BaseGroup),
		Name: g.Name, Repeatable: g.Repeatable, Position: g.Position, Fields: fields, Rows: []RowItem{}}
	if g.Board.Valid {
		it.Scope = "board"
	}
	return it
}

func appliesTo(e *ops.Env, g groupRow, cardID string) bool {
	var board string
	if e.DB.QueryRowContext(e.Ctx, `SELECT board_id FROM cards WHERE id=?`, cardID).Scan(&board) != nil {
		return false
	}
	if g.Board.Valid {
		return g.Board.String == board
	}
	return g.Card.String == cardID
}

func normalizeValue(typ string, options []string, content string) (string, error) {
	switch typ {
	case "number":
		f, err := strconv.ParseFloat(strings.TrimSpace(content), 64)
		if err != nil {
			return "", ops.Invalid("value must be a number")
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	case "date":
		c := strings.TrimSpace(content)
		if _, err := time.Parse("2006-01-02", c); err == nil {
			return c, nil
		}
		t, err := time.Parse(time.RFC3339, c)
		if err != nil {
			return "", ops.Invalid("value must be a date (YYYY-MM-DD or RFC3339)")
		}
		return t.UTC().Format(time.RFC3339), nil
	case "checkbox":
		switch strings.ToLower(strings.TrimSpace(content)) {
		case "true", "1", "on", "yes":
			return "true", nil
		case "false", "0", "off", "no":
			return "false", nil
		}
		return "", ops.Invalid("value must be true or false")
	case "select":
		for _, o := range options {
			if o == content {
				return content, nil
			}
		}
		return "", ops.Invalid("value is not one of the field's options")
	case "url":
		u, err := url.Parse(strings.TrimSpace(content))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return "", ops.Invalid("value must be an http(s) URL")
		}
		return u.String(), nil
	}
	if len(content) > 1<<16 {
		return "", ops.Invalid("value too long")
	}
	return content, nil
}

func cleanOptions(in []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, o := range in {
		o = strings.TrimSpace(o)
		if o != "" && !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out
}

// CardFields assembles every group applying to a card with fields, rows and values.
func cardFields(e *ops.Env, cardID string) ([]GroupItem, error) {
	var board string
	if e.DB.QueryRowContext(e.Ctx, `SELECT board_id FROM cards WHERE id=?`, cardID).Scan(&board) != nil {
		return nil, ops.NotFound("card")
	}
	groups, err := query(e.Ctx, e.DB, func(r *sql.Rows) (string, error) { var s string; return s, r.Scan(&s) },
		`SELECT id FROM custom_field_groups WHERE board_id=? OR card_id=? ORDER BY (board_id IS NULL), position, id`, board, cardID)
	if err != nil {
		return nil, err
	}
	type vk struct{ g, f, r string }
	vals := map[vk]string{}
	vs, err := query(e.Ctx, e.DB, func(r *sql.Rows) (ValueItem, error) {
		var v ValueItem
		var c sql.NullString
		err := r.Scan(&v.GroupID, &v.FieldID, &v.RowID, &c)
		v.Content = c.String
		return v, err
	}, `SELECT group_id,field_id,row_id,content FROM custom_field_values WHERE card_id=?`, cardID)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		vals[vk{v.GroupID, v.FieldID, v.RowID}] = v.Content
	}
	out := []GroupItem{}
	for _, id := range groups {
		g, err := loadGroup(e, e.DB, id)
		if err != nil {
			return nil, err
		}
		fields, err := groupFields(e, e.DB, g)
		if err != nil {
			return nil, err
		}
		it := groupItem(g, fields)
		rowIDs := []RowItem{{ID: "", CardID: cardID, GroupID: g.ID}}
		if g.Repeatable {
			rowIDs, err = query(e.Ctx, e.DB, func(r *sql.Rows) (RowItem, error) {
				ri := RowItem{CardID: cardID, GroupID: g.ID}
				return ri, r.Scan(&ri.ID, &ri.Position)
			}, `SELECT id,position FROM custom_field_rows WHERE card_id=? AND group_id=? ORDER BY position,id`, cardID, g.ID)
			if err != nil {
				return nil, err
			}
		}
		for _, ri := range rowIDs {
			ri.Values = map[string]string{}
			for _, f := range fields {
				if c, ok := vals[vk{g.ID, f.ID, ri.ID}]; ok {
					ri.Values[f.ID] = c
				}
			}
			it.Rows = append(it.Rows, ri)
		}
		out = append(out, it)
	}
	return out, nil
}

func init() {
	ops.Register(ops.Op{Name: "customfieldgroup.list", Summary: "Custom-field groups applying to a card (board-wide + card-level) with fields, rows and values",
		Method: "GET", Path: "/cards/{cardId}/custom-field-groups", Auth: ops.AuthPublic, UI: "GET /ui/cards/{id}/fields"},
		func(e *ops.Env, in GroupListIn) ([]GroupItem, error) {
			if _, err := authz.RequireCard(e, in.CardID, authz.View); err != nil {
				return nil, err
			}
			return cardFields(e, in.CardID)
		})

	ops.Register(ops.Op{Name: "customfieldgroup.create", Summary: "Create a custom-field group for a board (all cards) or a single card", Method: "POST", Path: "/custom-field-groups",
		UI: "POST /ui/custom-field-groups"}, func(e *ops.Env, in GroupCreateIn) (GroupItem, error) {
		if (in.BoardID == "") == (in.CardID == "") {
			return GroupItem{}, ops.Invalid("exactly one of boardId or cardId is required")
		}
		board, cardID := in.BoardID, in.CardID
		if cardID != "" {
			var err error
			if board, err = authz.RequireCard(e, cardID, authz.Edit); err != nil {
				return GroupItem{}, err
			}
		} else if err := authz.RequireBoard(e, board, authz.Edit); err != nil {
			return GroupItem{}, err
		}
		name := strings.TrimSpace(in.Name)
		var base any
		if in.BaseGroupID != "" {
			var bn sql.NullString
			if e.DB.QueryRowContext(e.Ctx, `SELECT b.name FROM base_custom_field_groups b JOIN boards d ON d.project_id=b.project_id WHERE b.id=? AND d.id=?`, in.BaseGroupID, board).Scan(&bn) != nil {
				return GroupItem{}, ops.Invalid("base group not found in this board's project")
			}
			base = in.BaseGroupID
		} else if name == "" {
			return GroupItem{}, ops.Invalid("name is required")
		}
		id := db.NewID()
		err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			var pos float64
			if cardID != "" {
				pos = endPosition(e.Ctx, tx, "custom_field_groups", "card_id=?", cardID)
			} else {
				pos = endPosition(e.Ctx, tx, "custom_field_groups", "board_id=?", board)
			}
			_, err := tx.ExecContext(e.Ctx, `INSERT INTO custom_field_groups(id,board_id,card_id,base_group_id,name,repeatable,position,created_at) VALUES(?,?,?,?,?,?,?,?)`,
				id, nz(in.BoardID), nz(cardID), base, nz(name), b2i(in.Repeatable), pos, db.Now())
			return err
		})
		if err != nil {
			return GroupItem{}, err
		}
		g, _ := loadGroup(e, e.DB, id)
		fields, _ := groupFields(e, e.DB, g)
		emit(e, "customFieldGroupCreate", board, cardID, "customfieldgroup.created", map[string]any{"groupId": id, "name": g.Name})
		return groupItem(g, fields), nil
	})

	groupAuth := func(e *ops.Env, id string, min authz.Level) (groupRow, string, error) {
		g, err := loadGroup(e, e.DB, id)
		if err != nil {
			return g, "", err
		}
		board, err := g.boardID(e)
		if err != nil {
			return g, "", err
		}
		return g, board, authz.RequireBoard(e, board, min)
	}

	ops.Register(ops.Op{Name: "customfieldgroup.update", Summary: "Rename, reposition or toggle repeatable on a custom-field group", Method: "PATCH", Path: "/custom-field-groups/{id}",
		UI: "POST /ui/custom-field-groups/{id}"}, func(e *ops.Env, in GroupUpdateIn) (GroupItem, error) {
		g, board, err := groupAuth(e, in.ID, authz.Edit)
		if err != nil {
			return GroupItem{}, err
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if in.Name != nil {
				n := strings.TrimSpace(*in.Name)
				if n == "" && !g.BaseGroup.Valid {
					return ops.Invalid("name cannot be empty")
				}
				if _, err := tx.ExecContext(e.Ctx, `UPDATE custom_field_groups SET name=? WHERE id=?`, nz(n), in.ID); err != nil {
					return err
				}
			}
			if in.Repeatable != nil && *in.Repeatable != g.Repeatable {
				var n int
				_ = tx.QueryRowContext(e.Ctx, `SELECT (SELECT COUNT(*) FROM custom_field_values WHERE group_id=?)+(SELECT COUNT(*) FROM custom_field_rows WHERE group_id=?)`, in.ID, in.ID).Scan(&n)
				if n > 0 {
					return ops.Conflict("cannot change repeatable while the group holds values; delete them first")
				}
				if _, err := tx.ExecContext(e.Ctx, `UPDATE custom_field_groups SET repeatable=? WHERE id=?`, b2i(*in.Repeatable), in.ID); err != nil {
					return err
				}
			}
			scope, args := g.scope()
			pos, ok, err := place(e.Ctx, tx, "custom_field_groups", scope, args, in.AfterID, in.BeforeID, in.Position)
			if err != nil {
				return err
			}
			if ok {
				_, err = tx.ExecContext(e.Ctx, `UPDATE custom_field_groups SET position=? WHERE id=?`, pos, in.ID)
			}
			return err
		})
		if err != nil {
			return GroupItem{}, err
		}
		g, _ = loadGroup(e, e.DB, in.ID)
		fields, _ := groupFields(e, e.DB, g)
		emit(e, "customFieldGroupUpdate", board, g.Card.String, "customfieldgroup.updated", map[string]any{"groupId": in.ID, "name": g.Name})
		return groupItem(g, fields), nil
	})

	ops.Register(ops.Op{Name: "customfieldgroup.delete", Summary: "Delete a custom-field group with its fields and values", Method: "DELETE", Path: "/custom-field-groups/{id}",
		UI: "POST /ui/custom-field-groups/{id}/delete"}, func(e *ops.Env, in IDIn) (DeletedOut, error) {
		g, board, err := groupAuth(e, in.ID, authz.Edit)
		if err != nil {
			return DeletedOut{}, err
		}
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM custom_field_groups WHERE id=?`, in.ID); err != nil {
			return DeletedOut{}, err
		}
		emit(e, "customFieldGroupDelete", board, g.Card.String, "customfieldgroup.deleted", map[string]any{"groupId": in.ID, "name": g.Name})
		return DeletedOut{ID: in.ID, CardID: g.Card.String}, nil
	})

	ops.Register(ops.Op{Name: "customfield.create", Summary: "Add a field to a custom-field group or a project base group", Method: "POST", Path: "/custom-fields",
		UI: "POST /ui/custom-fields"}, func(e *ops.Env, in FieldCreateIn) (FieldItem, error) {
		if (in.GroupID == "") == (in.BaseGroupID == "") {
			return FieldItem{}, ops.Invalid("exactly one of groupId or baseGroupId is required")
		}
		typ := in.Type
		if typ == "" {
			typ = "text"
		}
		if !fieldTypes[typ] {
			return FieldItem{}, ops.Invalid("type must be one of text, number, date, checkbox, select, url")
		}
		opts := cleanOptions(in.Options)
		if typ == "select" && len(opts) == 0 {
			return FieldItem{}, ops.Invalid("select fields need at least one option")
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			return FieldItem{}, ops.Invalid("name is required")
		}
		var board, cardID string
		if in.GroupID != "" {
			g, b, err := groupAuth(e, in.GroupID, authz.Edit)
			if err != nil {
				return FieldItem{}, err
			}
			if g.BaseGroup.Valid {
				return FieldItem{}, ops.Invalid("fields of a base-derived group are managed on the base group")
			}
			board, cardID = b, g.Card.String
		} else {
			var project string
			if e.DB.QueryRowContext(e.Ctx, `SELECT project_id FROM base_custom_field_groups WHERE id=?`, in.BaseGroupID).Scan(&project) != nil {
				return FieldItem{}, ops.NotFound("base custom field group")
			}
			if err := authz.RequireProject(e, project, authz.Manage); err != nil {
				return FieldItem{}, err
			}
		}
		ob, _ := json.Marshal(opts)
		id := db.NewID()
		err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			var pos float64
			if in.GroupID != "" {
				pos = endPosition(e.Ctx, tx, "custom_fields", "group_id=?", in.GroupID)
			} else {
				pos = endPosition(e.Ctx, tx, "custom_fields", "base_group_id=?", in.BaseGroupID)
			}
			_, err := tx.ExecContext(e.Ctx, `INSERT INTO custom_fields(id,group_id,base_group_id,name,type,options,position,show_on_front_of_card) VALUES(?,?,?,?,?,?,?,?)`,
				id, nz(in.GroupID), nz(in.BaseGroupID), name, typ, string(ob), pos, b2i(in.ShowOnFrontOfCard))
			return err
		})
		if err != nil {
			return FieldItem{}, err
		}
		if board != "" {
			emit(e, "customFieldCreate", board, cardID, "customfield.created", map[string]any{"fieldId": id, "name": name})
		}
		fs, err := query(e.Ctx, e.DB, scanField, fieldSelect+` WHERE id=?`, id)
		if err != nil || len(fs) == 0 {
			return FieldItem{}, err
		}
		return fs[0], nil
	})

	// fieldAuth resolves a field to its group/base group and authorizes the caller.
	fieldAuth := func(e *ops.Env, id string) (f FieldItem, board, cardID string, err error) {
		fs, err := query(e.Ctx, e.DB, scanField, fieldSelect+` WHERE id=?`, id)
		if err != nil {
			return
		}
		if len(fs) == 0 {
			err = ops.NotFound("custom field")
			return
		}
		f = fs[0]
		if f.GroupID != nil {
			var g groupRow
			if g, board, err = groupAuth(e, *f.GroupID, authz.Edit); err != nil {
				return
			}
			cardID = g.Card.String
			return
		}
		var project string
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT project_id FROM base_custom_field_groups WHERE id=?`, *f.BaseGroupID).Scan(&project)
		err = authz.RequireProject(e, project, authz.Manage)
		return
	}

	ops.Register(ops.Op{Name: "customfield.update", Summary: "Edit a custom field (name, type, options, front-of-card, position)", Method: "PATCH", Path: "/custom-fields/{id}",
		UI: "POST /ui/custom-fields/{id}"}, func(e *ops.Env, in FieldUpdateIn) (FieldItem, error) {
		f, board, cardID, err := fieldAuth(e, in.ID)
		if err != nil {
			return FieldItem{}, err
		}
		typ := f.Type
		if in.Type != nil {
			if !fieldTypes[*in.Type] {
				return FieldItem{}, ops.Invalid("type must be one of text, number, date, checkbox, select, url")
			}
			typ = *in.Type
		}
		opts := f.Options
		if in.Options != nil {
			opts = cleanOptions(*in.Options)
		}
		if typ == "select" && len(opts) == 0 {
			return FieldItem{}, ops.Invalid("select fields need at least one option")
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if in.Name != nil {
				n := strings.TrimSpace(*in.Name)
				if n == "" {
					return ops.Invalid("name cannot be empty")
				}
				if _, err := tx.ExecContext(e.Ctx, `UPDATE custom_fields SET name=? WHERE id=?`, n, in.ID); err != nil {
					return err
				}
			}
			if typ != f.Type {
				if _, err := tx.ExecContext(e.Ctx, `DELETE FROM custom_field_values WHERE field_id=?`, in.ID); err != nil {
					return err
				}
			}
			ob, _ := json.Marshal(opts)
			if _, err := tx.ExecContext(e.Ctx, `UPDATE custom_fields SET type=?,options=? WHERE id=?`, typ, string(ob), in.ID); err != nil {
				return err
			}
			if in.ShowOnFrontOfCard != nil {
				if _, err := tx.ExecContext(e.Ctx, `UPDATE custom_fields SET show_on_front_of_card=? WHERE id=?`, b2i(*in.ShowOnFrontOfCard), in.ID); err != nil {
					return err
				}
			}
			scope, args := "group_id=?", []any{""}
			if f.GroupID != nil {
				args[0] = *f.GroupID
			} else {
				scope, args[0] = "base_group_id=?", *f.BaseGroupID
			}
			pos, ok, err := place(e.Ctx, tx, "custom_fields", scope, args, in.AfterID, in.BeforeID, in.Position)
			if err != nil {
				return err
			}
			if ok {
				_, err = tx.ExecContext(e.Ctx, `UPDATE custom_fields SET position=? WHERE id=?`, pos, in.ID)
			}
			return err
		})
		if err != nil {
			return FieldItem{}, err
		}
		if board != "" {
			emit(e, "customFieldUpdate", board, cardID, "customfield.updated", map[string]any{"fieldId": in.ID})
		}
		fs, err := query(e.Ctx, e.DB, scanField, fieldSelect+` WHERE id=?`, in.ID)
		if err != nil || len(fs) == 0 {
			return FieldItem{}, err
		}
		return fs[0], nil
	})

	ops.Register(ops.Op{Name: "customfield.delete", Summary: "Delete a custom field and its values", Method: "DELETE", Path: "/custom-fields/{id}",
		UI: "POST /ui/custom-fields/{id}/delete"}, func(e *ops.Env, in IDIn) (DeletedOut, error) {
		f, board, cardID, err := fieldAuth(e, in.ID)
		if err != nil {
			return DeletedOut{}, err
		}
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM custom_fields WHERE id=?`, in.ID); err != nil {
			return DeletedOut{}, err
		}
		if board != "" {
			emit(e, "customFieldDelete", board, cardID, "customfield.deleted", map[string]any{"fieldId": in.ID, "name": f.Name})
		}
		return DeletedOut{ID: in.ID, CardID: cardID}, nil
	})

	// valueTarget validates (card, group, field, row) and returns the field and board.
	valueTarget := func(e *ops.Env, cardID, groupID, fieldID, rowID string) (FieldItem, groupRow, string, error) {
		board, err := authz.RequireCard(e, cardID, authz.Edit)
		if err != nil {
			return FieldItem{}, groupRow{}, "", err
		}
		g, err := loadGroup(e, e.DB, groupID)
		if err != nil || !appliesTo(e, g, cardID) {
			return FieldItem{}, g, "", ops.NotFound("custom field group")
		}
		fields, err := groupFields(e, e.DB, g)
		if err != nil {
			return FieldItem{}, g, "", err
		}
		var f FieldItem
		found := false
		for _, x := range fields {
			if x.ID == fieldID {
				f, found = x, true
			}
		}
		if !found {
			return f, g, "", ops.NotFound("custom field")
		}
		if g.Repeatable {
			var n int
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM custom_field_rows WHERE id=? AND card_id=? AND group_id=?`, rowID, cardID, groupID).Scan(&n)
			if rowID == "" || n == 0 {
				return f, g, "", ops.Invalid("rowId must reference an existing row of this repeatable group")
			}
		} else if rowID != "" {
			return f, g, "", ops.Invalid("rowId is only valid for repeatable groups")
		}
		return f, g, board, nil
	}

	ops.Register(ops.Op{Name: "customfieldvalue.set", Summary: "Set a card's value for a custom field (validated by field type); empty clears", Method: "PUT", Path: "/cards/{cardId}/custom-field-values",
		UI: "POST /ui/cards/{cardId}/custom-field-values"}, func(e *ops.Env, in ValueSetIn) (ValueItem, error) {
		f, _, board, err := valueTarget(e, in.CardID, in.GroupID, in.FieldID, in.RowID)
		if err != nil {
			return ValueItem{}, err
		}
		out := ValueItem{CardID: in.CardID, GroupID: in.GroupID, FieldID: in.FieldID, RowID: in.RowID}
		if in.Content == "" && f.Type != "checkbox" {
			if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM custom_field_values WHERE card_id=? AND group_id=? AND field_id=? AND row_id=?`, in.CardID, in.GroupID, in.FieldID, in.RowID); err != nil {
				return out, err
			}
			emit(e, "customFieldValueDelete", board, in.CardID, "customfieldvalue.updated", map[string]any{"fieldId": in.FieldID, "name": f.Name})
			return out, nil
		}
		content, err := normalizeValue(f.Type, f.Options, in.Content)
		if err != nil {
			return out, err
		}
		_, err = e.DB.ExecContext(e.Ctx, `INSERT INTO custom_field_values(id,card_id,group_id,field_id,row_id,content) VALUES(?,?,?,?,?,?)
			ON CONFLICT(card_id,group_id,field_id,row_id) DO UPDATE SET content=excluded.content`, db.NewID(), in.CardID, in.GroupID, in.FieldID, in.RowID, content)
		if err != nil {
			return out, err
		}
		out.Content = content
		emit(e, "customFieldValueUpdate", board, in.CardID, "customfieldvalue.updated", map[string]any{"fieldId": in.FieldID, "name": f.Name, "content": clip(content, 200)})
		return out, nil
	})

	ops.Register(ops.Op{Name: "customfieldvalue.clear", Summary: "Remove a card's value for a custom field", Method: "POST", Path: "/cards/{cardId}/custom-field-values/clear",
		UI: "POST /ui/cards/{cardId}/custom-field-values/clear"}, func(e *ops.Env, in ValueClearIn) (ValueItem, error) {
		f, _, board, err := valueTarget(e, in.CardID, in.GroupID, in.FieldID, in.RowID)
		if err != nil {
			return ValueItem{}, err
		}
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM custom_field_values WHERE card_id=? AND group_id=? AND field_id=? AND row_id=?`, in.CardID, in.GroupID, in.FieldID, in.RowID); err != nil {
			return ValueItem{}, err
		}
		emit(e, "customFieldValueDelete", board, in.CardID, "customfieldvalue.updated", map[string]any{"fieldId": in.FieldID, "name": f.Name})
		return ValueItem{CardID: in.CardID, GroupID: in.GroupID, FieldID: in.FieldID, RowID: in.RowID}, nil
	})

	ops.Register(ops.Op{Name: "customfieldrow.create", Summary: "Add a row to a repeatable custom-field group on a card", Method: "POST", Path: "/cards/{cardId}/custom-field-groups/{groupId}/rows",
		UI: "POST /ui/cards/{cardId}/custom-field-groups/{groupId}/rows"}, func(e *ops.Env, in RowCreateIn) (RowItem, error) {
		board, err := authz.RequireCard(e, in.CardID, authz.Edit)
		if err != nil {
			return RowItem{}, err
		}
		g, err := loadGroup(e, e.DB, in.GroupID)
		if err != nil || !appliesTo(e, g, in.CardID) {
			return RowItem{}, ops.NotFound("custom field group")
		}
		if !g.Repeatable {
			return RowItem{}, ops.Invalid("group is not repeatable")
		}
		id := db.NewID()
		var pos float64
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			pos = endPosition(e.Ctx, tx, "custom_field_rows", "card_id=? AND group_id=?", in.CardID, in.GroupID)
			_, err := tx.ExecContext(e.Ctx, `INSERT INTO custom_field_rows(id,card_id,group_id,position,created_at) VALUES(?,?,?,?,?)`, id, in.CardID, in.GroupID, pos, db.Now())
			return err
		})
		if err != nil {
			return RowItem{}, err
		}
		emit(e, "customFieldRowCreate", board, in.CardID, "customfieldvalue.updated", map[string]any{"groupId": in.GroupID, "name": g.Name})
		return RowItem{ID: id, CardID: in.CardID, GroupID: in.GroupID, Position: pos, Values: map[string]string{}}, nil
	})

	rowAuth := func(e *ops.Env, id string) (cardID, groupID, board string, err error) {
		if e.DB.QueryRowContext(e.Ctx, `SELECT r.card_id,r.group_id,c.board_id FROM custom_field_rows r JOIN cards c ON c.id=r.card_id WHERE r.id=?`, id).Scan(&cardID, &groupID, &board) != nil {
			return "", "", "", ops.NotFound("row")
		}
		return cardID, groupID, board, authz.RequireBoard(e, board, authz.Edit)
	}

	ops.Register(ops.Op{Name: "customfieldrow.update", Summary: "Reorder a row of a repeatable group", Method: "PATCH", Path: "/custom-field-rows/{id}",
		UI: "POST /ui/custom-field-rows/{id}"}, func(e *ops.Env, in RowUpdateIn) (RowItem, error) {
		cardID, groupID, board, err := rowAuth(e, in.ID)
		if err != nil {
			return RowItem{}, err
		}
		var pos float64
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			p, ok, err := place(e.Ctx, tx, "custom_field_rows", "card_id=? AND group_id=?", []any{cardID, groupID}, in.AfterID, in.BeforeID, in.Position)
			if err != nil {
				return err
			}
			if ok {
				if _, err = tx.ExecContext(e.Ctx, `UPDATE custom_field_rows SET position=? WHERE id=?`, p, in.ID); err != nil {
					return err
				}
			}
			return tx.QueryRowContext(e.Ctx, `SELECT position FROM custom_field_rows WHERE id=?`, in.ID).Scan(&pos)
		})
		if err != nil {
			return RowItem{}, err
		}
		emit(e, "customFieldRowUpdate", board, cardID, "customfieldvalue.updated", map[string]any{"groupId": groupID})
		return RowItem{ID: in.ID, CardID: cardID, GroupID: groupID, Position: pos, Values: map[string]string{}}, nil
	})

	ops.Register(ops.Op{Name: "customfieldrow.delete", Summary: "Delete a row of a repeatable group and its values", Method: "DELETE", Path: "/custom-field-rows/{id}",
		UI: "POST /ui/custom-field-rows/{id}/delete"}, func(e *ops.Env, in IDIn) (DeletedOut, error) {
		cardID, groupID, board, err := rowAuth(e, in.ID)
		if err != nil {
			return DeletedOut{}, err
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM custom_field_values WHERE card_id=? AND group_id=? AND row_id=?`, cardID, groupID, in.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(e.Ctx, `DELETE FROM custom_field_rows WHERE id=?`, in.ID)
			return err
		})
		if err != nil {
			return DeletedOut{}, err
		}
		emit(e, "customFieldRowDelete", board, cardID, "customfieldvalue.updated", map[string]any{"groupId": groupID})
		return DeletedOut{in.ID, cardID}, nil
	})
}
