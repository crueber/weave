package projects

import (
	"encoding/json"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// FieldTypes are the valid custom-field types.
var FieldTypes = []string{"text", "number", "date", "checkbox", "select", "url"}

// BaseField is a field template inside a base custom-field group.
type BaseField struct {
	ID                string   `json:"id"`
	GroupID           string   `json:"baseGroupId"`
	Name              string   `json:"name"`
	Type              string   `json:"type"`
	Options           []string `json:"options"`
	Position          float64  `json:"position"`
	ShowOnFrontOfCard bool     `json:"showOnFrontOfCard"`
}

// BaseGroup is a project-level custom-field group template.
type BaseGroup struct {
	ID        string      `json:"id"`
	ProjectID string      `json:"projectId"`
	Name      string      `json:"name"`
	CreatedAt string      `json:"createdAt"`
	Fields    []BaseField `json:"fields"`
}

type GroupCreateIn struct {
	ID   string `path:"id" desc:"Project id"`
	Name string `json:"name" required:"true" desc:"Group name (max 128)"`
}
type GroupIn struct {
	ID string `path:"id" desc:"Base custom field group id"`
}
type GroupUpdateIn struct {
	ID   string `path:"id" desc:"Base custom field group id"`
	Name string `json:"name" required:"true" desc:"Group name (max 128)"`
}
type FieldCreateIn struct {
	ID                string   `path:"id" desc:"Base custom field group id"`
	Name              string   `json:"name" required:"true" desc:"Field name (max 128)"`
	Type              string   `json:"type" desc:"text|number|date|checkbox|select|url (default text)"`
	Options           []string `json:"options" desc:"Choices for type select"`
	ShowOnFrontOfCard bool     `json:"showOnFrontOfCard" desc:"Show the value on the card front"`
}
type FieldIn struct {
	ID string `path:"id" desc:"Base custom field id"`
}
type FieldUpdateIn struct {
	ID                string    `path:"id" desc:"Base custom field id"`
	Name              *string   `json:"name" desc:"Field name"`
	Type              *string   `json:"type" desc:"text|number|date|checkbox|select|url"`
	Options           *[]string `json:"options" desc:"Choices for type select"`
	ShowOnFrontOfCard *bool     `json:"showOnFrontOfCard" desc:"Show the value on the card front"`
	Position          *float64  `json:"position" desc:"Sort position"`
}

// BaseGroups lists the base custom-field groups (with fields) of a project.
func BaseGroups(e *ops.Env, projectID string) ([]BaseGroup, error) {
	if err := authz.RequireProject(e, projectID, authz.View); err != nil {
		return nil, err
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,project_id,COALESCE(name,''),created_at FROM base_custom_field_groups WHERE project_id=? ORDER BY created_at,id`, projectID)
	if err != nil {
		return nil, err
	}
	out := []BaseGroup{}
	idx := map[string]int{}
	for rows.Next() {
		var g BaseGroup
		if err := rows.Scan(&g.ID, &g.ProjectID, &g.Name, &g.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		g.Fields = []BaseField{}
		idx[g.ID] = len(out)
		out = append(out, g)
	}
	rows.Close()
	frows, err := e.DB.QueryContext(e.Ctx, `SELECT f.id,f.base_group_id,f.name,f.type,COALESCE(f.options,''),f.position,f.show_on_front_of_card
		FROM custom_fields f JOIN base_custom_field_groups g ON g.id=f.base_group_id WHERE g.project_id=? ORDER BY f.position,f.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	for frows.Next() {
		var f BaseField
		var opts string
		if err := frows.Scan(&f.ID, &f.GroupID, &f.Name, &f.Type, &opts, &f.Position, &f.ShowOnFrontOfCard); err != nil {
			return nil, err
		}
		f.Options = []string{}
		if opts != "" {
			_ = json.Unmarshal([]byte(opts), &f.Options)
		}
		if i, ok := idx[f.GroupID]; ok {
			out[i].Fields = append(out[i].Fields, f)
		}
	}
	return out, frows.Err()
}

func groupProject(e *ops.Env, groupID string) (string, error) {
	var pid string
	if e.DB.QueryRowContext(e.Ctx, `SELECT project_id FROM base_custom_field_groups WHERE id=?`, groupID).Scan(&pid) != nil {
		return "", ops.NotFound("base custom field group")
	}
	return pid, nil
}

func fieldProject(e *ops.Env, fieldID string) (string, error) {
	var pid string
	if e.DB.QueryRowContext(e.Ctx, `SELECT g.project_id FROM custom_fields f JOIN base_custom_field_groups g ON g.id=f.base_group_id WHERE f.id=?`, fieldID).Scan(&pid) != nil {
		return "", ops.NotFound("base custom field")
	}
	return pid, nil
}

func validType(t string) bool {
	for _, x := range FieldTypes {
		if x == t {
			return true
		}
	}
	return false
}

func cleanOptions(t string, opts []string) (string, error) {
	var clean []string
	for _, o := range opts {
		if o = strings.TrimSpace(o); o != "" {
			clean = append(clean, o)
		}
	}
	if t != "select" {
		return "", nil
	}
	if len(clean) == 0 {
		return "", ops.Invalid("select fields need at least one option")
	}
	b, _ := json.Marshal(clean)
	return string(b), nil
}

func getGroup(e *ops.Env, projectID, groupID string) BaseGroup {
	gs, _ := BaseGroups(e, projectID)
	for _, g := range gs {
		if g.ID == groupID {
			return g
		}
	}
	return BaseGroup{}
}

func init() {
	ops.Register(ops.Op{Name: "project.fieldgroup.list", Summary: "List base custom field groups (with fields) of a project", Method: "GET",
		Path: "/projects/{id}/base-custom-field-groups", UI: "GET /ui/projects/{id}/settings/fields"},
		func(e *ops.Env, in IDIn) ([]BaseGroup, error) { return BaseGroups(e, in.ID) })

	ops.Register(ops.Op{Name: "project.fieldgroup.create", Summary: "Create a base custom field group", Method: "POST",
		Path: "/projects/{id}/base-custom-field-groups", UI: "POST /ui/projects/{id}/fieldgroups"},
		func(e *ops.Env, in GroupCreateIn) (BaseGroup, error) {
			if err := authz.RequireProject(e, in.ID, authz.Manage); err != nil {
				return BaseGroup{}, err
			}
			n := strings.TrimSpace(in.Name)
			if n == "" || len(n) > 128 {
				return BaseGroup{}, ops.Invalid("name is required (max 128 characters)")
			}
			gid := db.NewID()
			if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO base_custom_field_groups(id,project_id,name,created_at) VALUES(?,?,?,?)`, gid, in.ID, n, db.Now()); err != nil {
				return BaseGroup{}, err
			}
			g := getGroup(e, in.ID, gid)
			publish(e, "baseCustomFieldGroup.created", in.ID, g)
			return g, nil
		})

	ops.Register(ops.Op{Name: "project.fieldgroup.update", Summary: "Rename a base custom field group", Method: "PATCH",
		Path: "/base-custom-field-groups/{id}", UI: "POST /ui/fieldgroups/{id}"},
		func(e *ops.Env, in GroupUpdateIn) (BaseGroup, error) {
			pid, err := groupProject(e, in.ID)
			if err != nil {
				return BaseGroup{}, err
			}
			if err := authz.RequireProject(e, pid, authz.Manage); err != nil {
				return BaseGroup{}, err
			}
			n := strings.TrimSpace(in.Name)
			if n == "" || len(n) > 128 {
				return BaseGroup{}, ops.Invalid("name is required (max 128 characters)")
			}
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE base_custom_field_groups SET name=? WHERE id=?`, n, in.ID); err != nil {
				return BaseGroup{}, err
			}
			g := getGroup(e, pid, in.ID)
			publish(e, "baseCustomFieldGroup.updated", pid, g)
			return g, nil
		})

	ops.Register(ops.Op{Name: "project.fieldgroup.delete", Summary: "Delete a base custom field group and its field templates", Method: "DELETE",
		Path: "/base-custom-field-groups/{id}", UI: "DELETE /ui/fieldgroups/{id}"},
		func(e *ops.Env, in GroupIn) (DeleteOut, error) {
			pid, err := groupProject(e, in.ID)
			if err != nil {
				return DeleteOut{}, err
			}
			if err := authz.RequireProject(e, pid, authz.Manage); err != nil {
				return DeleteOut{}, err
			}
			if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM base_custom_field_groups WHERE id=?`, in.ID); err != nil {
				return DeleteOut{}, err
			}
			publish(e, "baseCustomFieldGroup.deleted", pid, map[string]string{"id": in.ID})
			return DeleteOut{in.ID}, nil
		})

	ops.Register(ops.Op{Name: "project.field.create", Summary: "Create a field template in a base custom field group", Method: "POST",
		Path: "/base-custom-field-groups/{id}/custom-fields", UI: "POST /ui/fieldgroups/{id}/fields"},
		func(e *ops.Env, in FieldCreateIn) (BaseField, error) {
			pid, err := groupProject(e, in.ID)
			if err != nil {
				return BaseField{}, err
			}
			if err := authz.RequireProject(e, pid, authz.Manage); err != nil {
				return BaseField{}, err
			}
			n := strings.TrimSpace(in.Name)
			if n == "" || len(n) > 128 {
				return BaseField{}, ops.Invalid("name is required (max 128 characters)")
			}
			t := in.Type
			if t == "" {
				t = "text"
			}
			if !validType(t) {
				return BaseField{}, ops.Invalid("type must be one of " + strings.Join(FieldTypes, ", "))
			}
			opts, err := cleanOptions(t, in.Options)
			if err != nil {
				return BaseField{}, err
			}
			var max float64
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT COALESCE(MAX(position),0) FROM custom_fields WHERE base_group_id=?`, in.ID).Scan(&max)
			fid := db.NewID()
			if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO custom_fields(id,base_group_id,name,type,options,position,show_on_front_of_card) VALUES(?,?,?,?,?,?,?)`,
				fid, in.ID, n, t, nullStr(opts), max+db.PositionGap, in.ShowOnFrontOfCard); err != nil {
				return BaseField{}, err
			}
			f := findField(e, pid, fid)
			publish(e, "baseCustomField.created", pid, f)
			return f, nil
		})

	ops.Register(ops.Op{Name: "project.field.update", Summary: "Update a base custom field template", Method: "PATCH",
		Path: "/base-custom-fields/{id}", UI: "POST /ui/basefields/{id}"},
		func(e *ops.Env, in FieldUpdateIn) (BaseField, error) {
			pid, err := fieldProject(e, in.ID)
			if err != nil {
				return BaseField{}, err
			}
			if err := authz.RequireProject(e, pid, authz.Manage); err != nil {
				return BaseField{}, err
			}
			f := findField(e, pid, in.ID)
			if in.Name != nil {
				n := strings.TrimSpace(*in.Name)
				if n == "" || len(n) > 128 {
					return f, ops.Invalid("name is required (max 128 characters)")
				}
				f.Name = n
			}
			if in.Type != nil {
				if !validType(*in.Type) {
					return f, ops.Invalid("type must be one of " + strings.Join(FieldTypes, ", "))
				}
				f.Type = *in.Type
			}
			if in.Options != nil {
				f.Options = *in.Options
			}
			if in.ShowOnFrontOfCard != nil {
				f.ShowOnFrontOfCard = *in.ShowOnFrontOfCard
			}
			if in.Position != nil {
				f.Position = *in.Position
			}
			opts, err := cleanOptions(f.Type, f.Options)
			if err != nil {
				return f, err
			}
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE custom_fields SET name=?,type=?,options=?,show_on_front_of_card=?,position=? WHERE id=?`,
				f.Name, f.Type, nullStr(opts), f.ShowOnFrontOfCard, f.Position, in.ID); err != nil {
				return f, err
			}
			f = findField(e, pid, in.ID)
			publish(e, "baseCustomField.updated", pid, f)
			return f, nil
		})

	ops.Register(ops.Op{Name: "project.field.delete", Summary: "Delete a base custom field template", Method: "DELETE",
		Path: "/base-custom-fields/{id}", UI: "DELETE /ui/basefields/{id}"},
		func(e *ops.Env, in FieldIn) (DeleteOut, error) {
			pid, err := fieldProject(e, in.ID)
			if err != nil {
				return DeleteOut{}, err
			}
			if err := authz.RequireProject(e, pid, authz.Manage); err != nil {
				return DeleteOut{}, err
			}
			if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM custom_fields WHERE id=?`, in.ID); err != nil {
				return DeleteOut{}, err
			}
			publish(e, "baseCustomField.deleted", pid, map[string]string{"id": in.ID})
			return DeleteOut{in.ID}, nil
		})
}

func findField(e *ops.Env, projectID, fieldID string) BaseField {
	gs, _ := BaseGroups(e, projectID)
	for _, g := range gs {
		for _, f := range g.Fields {
			if f.ID == fieldID {
				return f
			}
		}
	}
	return BaseField{}
}
