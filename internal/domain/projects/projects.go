package projects

import (
	"database/sql"
	"html/template"
	"regexp"
	"sort"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// Gradients is the allowed set of named Planka background presets, kept as stored
// data for compatibility (name -> legacy "from,to"). Weave renders every preset as a
// flat solid colour, see GradientSolids.
var Gradients = map[string]string{
	"old-lime": "#d4d47b,#7a9a3a", "ocean-dive": "#2a6f97,#61a5c2", "tzepesch-style": "#7b4b94,#e07a5f",
	"jungle-mesh": "#1b4332,#52b788", "strawberry-dust": "#ff758f,#ffb3c1", "purple-rose": "#6a4c93,#c77dff",
	"sun-scream": "#f9c74f,#f3722c", "warm-rust": "#b5651d,#e9c46a", "sky-change": "#4cc9f0,#4361ee",
	"green-eyes": "#2d6a4f,#95d5b2", "blue-xchange": "#0077b6,#90e0ef", "blood-orange": "#d00000,#faa307",
	"sour-peel": "#ffd60a,#7fb800", "green-ninja": "#0b3d0b,#38b000", "algae-green": "#1a936f,#88d498",
	"coral-reef": "#ff6b6b,#4ecdc4", "steel-grey": "#434343,#8b949e", "heat-waves": "#ff4e50,#f9d423",
	"velvet-lounge": "#5f0f40,#9a031e", "purple-rain": "#3c096c,#9d4edd", "blue-steel": "#274060,#5c7aa0",
	"blueish-curve": "#1d3557,#457b9d", "prism-light": "#a8dadc,#e0aaff", "green-mist": "#cce3de,#6b9080",
	"red-curtain": "#7d0a0a,#e63946",
}

// GradientSolids maps each preset to the single flat colour it renders as.
var GradientSolids = map[string]string{
	"old-lime": "#9aab4f", "ocean-dive": "#3f88ad", "tzepesch-style": "#a8639c",
	"jungle-mesh": "#2f7a5a", "strawberry-dust": "#f58da0", "purple-rose": "#9060b8",
	"sun-scream": "#f59e3a", "warm-rust": "#cf8a3a", "sky-change": "#3f7de8",
	"green-eyes": "#4f9a78", "blue-xchange": "#2a95cc", "blood-orange": "#e24a12",
	"sour-peel": "#a9c915", "green-ninja": "#238a12", "algae-green": "#47b283",
	"coral-reef": "#ff6b6b", "steel-grey": "#69727b", "heat-waves": "#f98a3c",
	"velvet-lounge": "#7d0f4a", "purple-rain": "#6a2aa8", "blue-steel": "#41608a",
	"blueish-curve": "#33597a", "prism-light": "#b9b2e6", "green-mist": "#8aae9f",
	"red-curtain": "#b5202c",
}

// GradientNames returns the sorted gradient names (for pickers).
func GradientNames() []string {
	n := make([]string, 0, len(Gradients))
	for k := range Gradients {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

var hexColor = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// Project is the API representation of a project.
type Project struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	IsHidden           bool     `json:"isHidden"`
	UseProjectLabels   bool     `json:"useProjectLabels"`
	BackgroundType     string   `json:"backgroundType"` // "" | color | gradient | image
	BackgroundColor    string   `json:"backgroundColor"`
	BackgroundGradient string   `json:"backgroundGradient"`
	BackgroundImageID  string   `json:"backgroundImageId"`
	OwnerManagerID     string   `json:"ownerManagerId"`
	IsFavorite         bool     `json:"isFavorite"`
	ManagerUserIDs     []string `json:"managerUserIds"`
	BoardCount         int      `json:"boardCount"`
	CanManage          bool     `json:"canManage"`
	CreatedAt          string   `json:"createdAt"`
	UpdatedAt          string   `json:"updatedAt"`
}

// BackgroundCSS renders the inline style for a project's background.
func BackgroundCSS(p Project) template.CSS {
	switch p.BackgroundType {
	case "color":
		if hexColor.MatchString(p.BackgroundColor) {
			return template.CSS("background:" + p.BackgroundColor)
		}
	case "gradient":
		if _, ok := Gradients[p.BackgroundGradient]; ok {
			return template.CSS("background:" + GradientSolids[p.BackgroundGradient])
		}
	case "image":
		if isID(p.BackgroundImageID) && isID(p.ID) {
			return template.CSS("background:center/cover url(/projects/" + p.ID + "/background-images/" + p.BackgroundImageID + "/thumbnail)")
		}
	}
	return ""
}

// FlatColor is the one flat colour of a colour / gradient background ("" for images and no background).
func FlatColor(p Project) string {
	switch p.BackgroundType {
	case "color":
		if hexColor.MatchString(p.BackgroundColor) {
			return p.BackgroundColor
		}
	case "gradient":
		if _, ok := Gradients[p.BackgroundGradient]; ok {
			return GradientSolids[p.BackgroundGradient]
		}
	}
	return ""
}

// HeroCSS renders the inline style for the project page banner. An image uses the full-size original
// (the 480px thumbnail would blur across the banner) and never repeats; the banner itself is filled edge
// to edge by CSS (.projects-hero-band). A flat colour is NOT painted as a full-saturation fill (large
// area rule): it is exposed as --hero-color and CSS draws a faint tint plus a 4px full-colour band.
func HeroCSS(p Project) template.CSS {
	if p.BackgroundType == "image" && isID(p.BackgroundImageID) && isID(p.ID) {
		return template.CSS("background:center/cover no-repeat url(/projects/" + p.ID + "/background-images/" + p.BackgroundImageID + "/original)")
	}
	if c := FlatColor(p); c != "" {
		return template.CSS("--hero-color:" + c)
	}
	return ""
}

// CoverCSS renders the inline style for the cover strip of a project tile on the home page: the thumbnail
// for images, --hero-color (tint + bar by CSS) for flat colours.
func CoverCSS(p Project) template.CSS {
	if c := FlatColor(p); c != "" {
		return template.CSS("--hero-color:" + c)
	}
	return BackgroundCSS(p)
}

var idRe = regexp.MustCompile(`^[0-9a-f]+$`)

func isID(s string) bool { return s != "" && idRe.MatchString(s) }

const projCols = `p.id,p.name,COALESCE(p.description,''),p.is_hidden,p.use_project_labels,COALESCE(p.background_type,''),
 COALESCE(p.background_color,''),COALESCE(p.background_gradient,''),COALESCE(p.background_image_id,''),COALESCE(p.owner_manager_id,''),
 EXISTS(SELECT 1 FROM project_favorites f WHERE f.project_id=p.id AND f.user_id=?),
 COALESCE((SELECT group_concat(user_id) FROM project_managers m WHERE m.project_id=p.id),''),
 (SELECT COUNT(*) FROM boards b WHERE b.project_id=p.id AND b.is_template=0),p.created_at,COALESCE(p.updated_at,'')`

func scanProject(sc interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var mgrs string
	err := sc.Scan(&p.ID, &p.Name, &p.Description, &p.IsHidden, &p.UseProjectLabels, &p.BackgroundType, &p.BackgroundColor,
		&p.BackgroundGradient, &p.BackgroundImageID, &p.OwnerManagerID, &p.IsFavorite, &mgrs, &p.BoardCount, &p.CreatedAt, &p.UpdatedAt)
	p.ManagerUserIDs = []string{}
	if mgrs != "" {
		p.ManagerUserIDs = strings.Split(mgrs, ",")
	}
	return p, err
}

func uid(e *ops.Env) string {
	if e.User == nil {
		return ""
	}
	return e.User.UserID
}

// IsManager reports whether the caller is admin or a manager of the project.
func IsManager(e *ops.Env, projectID string) bool {
	return authz.ProjectLevel(e, projectID) >= authz.Manage
}

// Get returns a project the caller may view (not_found otherwise).
func Get(e *ops.Env, id string) (Project, error) {
	if err := authz.RequireProject(e, id, authz.View); err != nil {
		return Project{}, err
	}
	p, err := scanProject(e.DB.QueryRowContext(e.Ctx, `SELECT `+projCols+` FROM projects p WHERE p.id=?`, uid(e), id))
	if err != nil {
		return p, ops.NotFound("project")
	}
	p.CanManage = IsManager(e, id)
	return p, nil
}

// ListOpts filters List.
type ListOpts struct {
	Search        string
	IncludeHidden bool
	Order         string // byDefault | alphabetically | byCreationTime
}

// List returns projects visible to the caller.
func List(e *ops.Env, o ListOpts) ([]Project, error) {
	if e.User == nil {
		return nil, ops.Unauthorized()
	}
	q := `SELECT ` + projCols + ` FROM projects p WHERE 1=1`
	args := []any{uid(e)}
	if e.User.Role != "admin" {
		q += ` AND (p.id IN (SELECT project_id FROM project_managers WHERE user_id=?) OR p.id IN (
			SELECT b.project_id FROM boards b JOIN board_memberships m ON m.board_id=b.id WHERE m.user_id=? OR
			m.group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?)))`
		args = append(args, e.User.UserID, e.User.UserID, e.User.UserID)
	}
	if !o.IncludeHidden {
		q += ` AND p.is_hidden=0`
	}
	if s := strings.TrimSpace(o.Search); s != "" {
		q += ` AND p.name LIKE ? ESCAPE '\'`
		args = append(args, "%"+strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)+"%")
	}
	switch o.Order {
	case "alphabetically":
		q += ` ORDER BY p.name COLLATE NOCASE, p.id`
	case "byCreationTime":
		q += ` ORDER BY p.created_at DESC, p.id DESC`
	default:
		q += ` ORDER BY p.created_at, p.id`
	}
	rows, err := e.DB.QueryContext(e.Ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		p.CanManage = e.User.Role == "admin" || contains(p.ManagerUserIDs, e.User.UserID)
		out = append(out, p)
	}
	return out, rows.Err()
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func publish(e *ops.Env, name, projectID string, data any) {
	e.Hub.Publish(realtime.Event{Name: name, Room: "project:" + projectID, Data: data})
}

// ---- inputs/outputs ----

type IDIn struct {
	ID string `path:"id" desc:"Project id"`
}

type ListIn struct {
	Search string `query:"search" desc:"Filter by name substring"`
	Hidden bool   `query:"hidden" desc:"Include hidden projects"`
	Order  string `query:"order" desc:"byDefault | alphabetically | byCreationTime (default: your preference)"`
}

type CreateIn struct {
	Name        string `json:"name" required:"true" desc:"Project name (max 128)"`
	Description string `json:"description" desc:"Description (max 1024)"`
}

type UpdateIn struct {
	ID                 string  `path:"id" desc:"Project id"`
	Name               *string `json:"name" desc:"Project name (max 128)"`
	Description        *string `json:"description" desc:"Description (max 1024); empty clears"`
	IsHidden           *bool   `json:"isHidden" desc:"Hide from the home page"`
	UseProjectLabels   *bool   `json:"useProjectLabels" desc:"Share one label set across all boards of the project"`
	BackgroundType     *string `json:"backgroundType" desc:"color | gradient | image | empty to clear"`
	BackgroundColor    *string `json:"backgroundColor" desc:"Hex colour like #1d6fa5 (type color)"`
	BackgroundGradient *string `json:"backgroundGradient" desc:"Named gradient (type gradient), e.g. ocean-dive"`
	BackgroundImageID  *string `json:"backgroundImageId" desc:"Uploaded background image id (type image)"`
}

type DeleteOut struct {
	ID string `json:"id"`
}

func init() {
	ops.Register(ops.Op{Name: "project.list", Summary: "List projects visible to you", Method: "GET", Path: "/projects",
		UI: "GET /"}, func(e *ops.Env, in ListIn) ([]Project, error) {
		order := in.Order
		if order == "" {
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT default_projects_order FROM users WHERE id=?`, uid(e)).Scan(&order)
		}
		return List(e, ListOpts{Search: in.Search, IncludeHidden: in.Hidden, Order: order})
	})

	ops.Register(ops.Op{Name: "project.get", Summary: "Get a project", Method: "GET", Path: "/projects/{id}",
		UI: "GET /projects/{id}"}, func(e *ops.Env, in IDIn) (Project, error) { return Get(e, in.ID) })

	ops.Register(ops.Op{Name: "project.create", Summary: "Create a project (you become its manager)", Method: "POST", Path: "/projects",
		UI: "POST /ui/projects"}, func(e *ops.Env, in CreateIn) (Project, error) {
		if !authz.CanCreateProject(e) {
			return Project{}, ops.Forbidden()
		}
		name := strings.TrimSpace(in.Name)
		if name == "" || len(name) > 128 {
			return Project{}, ops.Invalid("name is required (max 128 characters)")
		}
		if len(in.Description) > 1024 {
			return Project{}, ops.Invalid("description too long (max 1024)")
		}
		id, mid, now := db.NewID(), db.NewID(), db.Now()
		err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO projects(id,name,description,owner_manager_id,created_at) VALUES(?,?,?,?,?)`,
				id, name, nullStr(in.Description), mid, now); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES(?,?,?,?)`, mid, id, e.User.UserID, now)
			return err
		})
		if err != nil {
			return Project{}, err
		}
		p, _ := Get(e, id)
		publish(e, "project.created", id, p)
		e.Hub.Publish(realtime.Event{Name: "project.created", Room: "user:" + e.User.UserID, Data: p})
		return p, nil
	})

	ops.Register(ops.Op{Name: "project.update", Summary: "Update a project (name, description, hidden, labels toggle, background)", Method: "PATCH", Path: "/projects/{id}",
		UI: "POST /ui/projects/{id}"}, updateProject)

	ops.Register(ops.Op{Name: "project.delete", Summary: "Delete a project and everything in it", Method: "DELETE", Path: "/projects/{id}",
		UI: "DELETE /ui/projects/{id}"}, func(e *ops.Env, in IDIn) (DeleteOut, error) {
		if err := authz.RequireProject(e, in.ID, authz.Manage); err != nil {
			return DeleteOut{}, err
		}
		files := imageFiles(e, in.ID)
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM projects WHERE id=?`, in.ID); err != nil {
			return DeleteOut{}, err
		}
		removeFiles(e, files)
		publish(e, "project.deleted", in.ID, map[string]string{"id": in.ID})
		return DeleteOut{in.ID}, nil
	})

	ops.Register(ops.Op{Name: "project.favorite", Summary: "Mark a project as your favorite", Method: "POST", Path: "/projects/{id}/favorite",
		UI: "POST /ui/projects/{id}/favorite"}, func(e *ops.Env, in IDIn) (Project, error) { return setFavorite(e, in.ID, true) })
	ops.Register(ops.Op{Name: "project.unfavorite", Summary: "Remove a project from your favorites", Method: "DELETE", Path: "/projects/{id}/favorite",
		UI: "DELETE /ui/projects/{id}/favorite"}, func(e *ops.Env, in IDIn) (Project, error) { return setFavorite(e, in.ID, false) })
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func setFavorite(e *ops.Env, id string, on bool) (Project, error) {
	if err := authz.RequireProject(e, id, authz.View); err != nil {
		return Project{}, err
	}
	var err error
	if on {
		_, err = e.DB.ExecContext(e.Ctx, `INSERT OR IGNORE INTO project_favorites(project_id,user_id) VALUES(?,?)`, id, e.User.UserID)
	} else {
		_, err = e.DB.ExecContext(e.Ctx, `DELETE FROM project_favorites WHERE project_id=? AND user_id=?`, id, e.User.UserID)
	}
	if err != nil {
		return Project{}, err
	}
	p, err := Get(e, id)
	if err == nil {
		e.Hub.Publish(realtime.Event{Name: "project.updated", Room: "user:" + e.User.UserID, Data: p})
	}
	return p, err
}

func updateProject(e *ops.Env, in UpdateIn) (Project, error) {
	if err := authz.RequireProject(e, in.ID, authz.Manage); err != nil {
		return Project{}, err
	}
	cur, err := Get(e, in.ID)
	if err != nil {
		return cur, err
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || len(n) > 128 {
			return cur, ops.Invalid("name is required (max 128 characters)")
		}
		cur.Name = n
	}
	if in.Description != nil {
		if len(*in.Description) > 1024 {
			return cur, ops.Invalid("description too long (max 1024)")
		}
		cur.Description = strings.TrimSpace(*in.Description)
	}
	if in.IsHidden != nil {
		cur.IsHidden = *in.IsHidden
	}
	if in.UseProjectLabels != nil {
		cur.UseProjectLabels = *in.UseProjectLabels
	}
	if in.BackgroundColor != nil {
		if *in.BackgroundColor != "" && !hexColor.MatchString(*in.BackgroundColor) {
			return cur, ops.Invalid("backgroundColor must be a hex colour like #1d6fa5")
		}
		cur.BackgroundColor = *in.BackgroundColor
	}
	if in.BackgroundGradient != nil {
		if _, ok := Gradients[*in.BackgroundGradient]; !ok && *in.BackgroundGradient != "" {
			return cur, ops.Invalid("unknown gradient " + *in.BackgroundGradient)
		}
		cur.BackgroundGradient = *in.BackgroundGradient
	}
	if in.BackgroundImageID != nil {
		if *in.BackgroundImageID != "" {
			var n int
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM background_images WHERE id=? AND project_id=?`, *in.BackgroundImageID, in.ID).Scan(&n)
			if n == 0 {
				return cur, ops.NotFound("background image")
			}
		}
		cur.BackgroundImageID = *in.BackgroundImageID
	}
	if in.BackgroundType != nil {
		switch *in.BackgroundType {
		case "", "color", "gradient", "image":
			cur.BackgroundType = *in.BackgroundType
		default:
			return cur, ops.Invalid("backgroundType must be color, gradient, image or empty")
		}
	}
	switch cur.BackgroundType {
	case "color":
		if cur.BackgroundColor == "" {
			return cur, ops.Invalid("backgroundColor is required for type color")
		}
	case "gradient":
		if cur.BackgroundGradient == "" {
			return cur, ops.Invalid("backgroundGradient is required for type gradient")
		}
	case "image":
		if cur.BackgroundImageID == "" {
			return cur, ops.Invalid("backgroundImageId is required for type image")
		}
	}
	_, err = e.DB.ExecContext(e.Ctx, `UPDATE projects SET name=?,description=?,is_hidden=?,use_project_labels=?,background_type=?,background_color=?,
		background_gradient=?,background_image_id=?,updated_at=? WHERE id=?`, cur.Name, nullStr(cur.Description), cur.IsHidden, cur.UseProjectLabels,
		nullStr(cur.BackgroundType), nullStr(cur.BackgroundColor), nullStr(cur.BackgroundGradient), nullStr(cur.BackgroundImageID), db.Now(), in.ID)
	if err != nil {
		return cur, err
	}
	p, _ := Get(e, in.ID)
	publish(e, "project.updated", in.ID, p)
	return p, nil
}
