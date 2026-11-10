package projects

import (
	"encoding/json"
	"html/template"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

type section struct {
	Title    string
	Projects []Project
}

type homeData struct {
	Sections            []section
	Search, View, Order string
	Hidden, CanCreate   bool
}

type boardRef struct {
	ID, Name string
	Style    template.CSS // --board-color from boards.color (validated #rrggbb only)
}

type pageData struct {
	P              Project
	Boards         []boardRef
	CanCreateBoard bool
}

type gradient struct {
	Name string
	CSS  template.CSS
}

type candidate struct{ ID, Name, Email string }

type settingsData struct {
	P          Project
	Tabs       [][2]string
	Gradients  []gradient
	Images     []BackgroundImage
	Managers   []Manager
	Candidates []candidate
	Groups     []BaseGroup
	FieldTypes []string
}

func init() {
	web.AddFuncs(template.FuncMap{"projects_bg": BackgroundCSS, "projects_cover": CoverCSS, "projects_hero": HeroCSS})

	web.Route("GET /{$}", home)
	web.Route("GET /ui/projects/list", func(c *web.Ctx) error { return renderList(c) })
	web.Route("GET /ui/projects/new", func(c *web.Ctx) error {
		if !authz.CanCreateProject(c.Env) {
			return ops.Forbidden()
		}
		return c.RenderBlock("projects_home.html", "projects-new", nil)
	})
	web.Route("POST /ui/projects", func(c *web.Ctx) error {
		out, err := c.Do("project.create")
		if err != nil {
			return err
		}
		c.Redirect("/projects/" + out.(Project).ID)
		return nil
	})
	web.Route("GET /projects/{id}", projectPage)
	web.Route("POST /ui/projects/{id}", func(c *web.Ctx) error {
		if _, err := c.Do("project.update"); err != nil {
			return err
		}
		return renderTab(c, c.R.PathValue("id"), c.R.FormValue("tab"))
	})
	web.Route("DELETE /ui/projects/{id}", func(c *web.Ctx) error {
		if _, err := c.Do("project.delete"); err != nil {
			return err
		}
		c.Redirect("/")
		return nil
	})
	web.Route("POST /ui/projects/{id}/favorite", func(c *web.Ctx) error { return favorite(c, "project.favorite") })
	web.Route("DELETE /ui/projects/{id}/favorite", func(c *web.Ctx) error { return favorite(c, "project.unfavorite") })

	web.Route("GET /ui/projects/{id}/settings", func(c *web.Ctx) error {
		d, err := loadSettings(c, c.R.PathValue("id"), "general")
		if err != nil {
			return err
		}
		return c.RenderBlock("projects_project.html", "projects-settings", d)
	})
	web.Route("GET /ui/projects/{id}/settings/{tab}", func(c *web.Ctx) error {
		return renderTab(c, c.R.PathValue("id"), c.R.PathValue("tab"))
	})

	web.Route("POST /ui/projects/{id}/managers", func(c *web.Ctx) error {
		if _, err := c.Do("project.manager.add"); err != nil {
			return err
		}
		return renderTab(c, c.R.PathValue("id"), "managers")
	})
	web.Route("DELETE /ui/projects/{id}/managers/{managerId}", func(c *web.Ctx) error {
		if _, err := c.Do("project.manager.remove"); err != nil {
			return err
		}
		return renderTab(c, c.R.PathValue("id"), "managers")
	})

	web.Route("POST /ui/projects/{id}/background-images", func(c *web.Ctx) error {
		if _, err := c.Do("project.background.upload"); err != nil {
			return err
		}
		return renderTab(c, c.R.PathValue("id"), "background")
	})
	web.Route("DELETE /ui/projects/{id}/background-images/{imageId}", func(c *web.Ctx) error {
		if _, err := c.Do("project.background.delete"); err != nil {
			return err
		}
		return renderTab(c, c.R.PathValue("id"), "background")
	})
	web.Route("GET /projects/{id}/background-images/{imageId}/{variant}", func(c *web.Ctx) error {
		out, err := c.Do("project.background.file")
		if err != nil {
			return err
		}
		st := out.(*ops.Stream)
		c.W.Header().Set("Content-Type", st.ContentType)
		c.W.Header().Set("Cache-Control", "private, max-age=86400")
		_, _ = c.W.Write(st.Data)
		return nil
	})

	fieldsTab := func(op string) func(*web.Ctx) error {
		return func(c *web.Ctx) error {
			if op == "project.field.create" {
				// the options textarea/input is comma separated; the op wants a JSON array
				_ = c.R.ParseForm()
				var opts []string
				for _, o := range strings.Split(c.R.Form.Get("options"), ",") {
					if o = strings.TrimSpace(o); o != "" {
						opts = append(opts, o)
					}
				}
				b, _ := json.Marshal(opts)
				c.R.Form.Set("options", string(b))
			}
			if _, err := c.Do(op); err != nil {
				return err
			}
			pid := c.R.FormValue("pid")
			if pid == "" {
				pid = c.R.PathValue("id")
			}
			return renderTab(c, pid, "fields")
		}
	}
	web.Route("POST /ui/projects/{id}/fieldgroups", fieldsTab("project.fieldgroup.create"))
	web.Route("POST /ui/fieldgroups/{id}", fieldsTab("project.fieldgroup.update"))
	web.Route("DELETE /ui/fieldgroups/{id}", fieldsTab("project.fieldgroup.delete"))
	web.Route("POST /ui/fieldgroups/{id}/fields", fieldsTab("project.field.create"))
	web.Route("POST /ui/basefields/{id}", fieldsTab("project.field.update"))
	web.Route("DELETE /ui/basefields/{id}", fieldsTab("project.field.delete"))
}

func canCreateBoard(e *ops.Env, projectID string) bool { return IsManager(e, projectID) }

// boardURL finds the new board's id in an op result of unknown type.
func boardURL(out any, projectID string) string {
	b, _ := json.Marshal(out)
	var v struct {
		ID   string `json:"id"`
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
	}
	_ = json.Unmarshal(b, &v)
	if v.ID == "" {
		v.ID = v.Item.ID
	}
	if isID(v.ID) {
		return "/boards/" + v.ID
	}
	return "/projects/" + projectID
}

func favorite(c *web.Ctx, op string) error {
	out, err := c.Do(op)
	if err != nil {
		return err
	}
	if c.R.URL.Query().Get("ctx") == "home" {
		return renderList(c)
	}
	return c.RenderBlock("projects_project.html", "projects-star-block", map[string]any{"P": out.(Project), "Ctx": "page"})
}

func userPrefs(c *web.Ctx) (view, order string) {
	_ = c.Env.DB.QueryRowContext(c.Env.Ctx, `SELECT COALESCE(default_home_view,''),COALESCE(default_projects_order,'') FROM users WHERE id=?`, c.Env.User.UserID).Scan(&view, &order)
	return
}

func buildHome(c *web.Ctx) (homeData, error) {
	q := c.R.URL.Query()
	view, order := userPrefs(c)
	if v := q.Get("view"); v != "" {
		view = v
	}
	if o := q.Get("order"); o != "" {
		order = o
	}
	if view != "gridProjects" {
		view = "groupedProjects"
	}
	switch order {
	case "alphabetically", "byCreationTime":
	default:
		order = "byDefault"
	}
	d := homeData{Search: q.Get("search"), View: view, Order: order, Hidden: q.Get("hidden") == "true", CanCreate: authz.CanCreateProject(c.Env)}
	ps, err := List(c.Env, ListOpts{Search: d.Search, IncludeHidden: d.Hidden, Order: order})
	if err != nil {
		return d, err
	}
	var fav, mine, team []Project
	for _, p := range ps {
		switch {
		case p.IsFavorite:
			fav = append(fav, p)
		case p.CanManage:
			mine = append(mine, p)
		default:
			team = append(team, p)
		}
	}
	add := func(title string, p []Project) {
		if len(p) > 0 {
			d.Sections = append(d.Sections, section{title, p})
		}
	}
	if view == "gridProjects" {
		all := append(append(append([]Project{}, fav...), mine...), team...)
		if d.Search == "" || len(all) > 0 {
			add("", all)
		}
		return d, nil
	}
	add("Favorites", fav)
	add("My projects", mine)
	add("Team projects", team)
	return d, nil
}

func home(c *web.Ctx) error {
	d, err := buildHome(c)
	if err != nil {
		return err
	}
	return c.Render("projects_home.html", d)
}

func renderList(c *web.Ctx) error {
	d, err := buildHome(c)
	if err != nil {
		return err
	}
	return c.RenderBlock("projects_home.html", "projects-list", d)
}

func projectPage(c *web.Ctx) error {
	p, err := Get(c.Env, c.R.PathValue("id"))
	if err != nil {
		return err
	}
	d := pageData{P: p, CanCreateBoard: p.CanManage && ops.Get("board.create") != nil}
	// Read-only listing of the boards the caller can open (page-specific join).
	q := `SELECT b.id,b.name,COALESCE(b.color,'') FROM boards b WHERE b.project_id=? AND b.is_template=0`
	args := []any{p.ID}
	if !p.CanManage {
		q += ` AND b.id IN (SELECT board_id FROM board_memberships WHERE user_id=? OR group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?))`
		args = append(args, c.Env.User.UserID, c.Env.User.UserID)
	}
	rows, err := c.Env.DB.QueryContext(c.Env.Ctx, q+` ORDER BY b.position,b.id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b boardRef
		var col string
		if err := rows.Scan(&b.ID, &b.Name, &col); err != nil {
			return err
		}
		if hexColor.MatchString(col) {
			b.Style = template.CSS("--board-color:" + col)
		}
		d.Boards = append(d.Boards, b)
	}
	return c.Render("projects_project.html", d)
}

var tabs = [][2]string{{"general", "General"}, {"background", "Background"}, {"managers", "Managers"}, {"fields", "Custom fields"}}

func loadSettings(c *web.Ctx, id, tab string) (settingsData, error) {
	p, err := Get(c.Env, id)
	if err != nil {
		return settingsData{}, err
	}
	if !p.CanManage {
		return settingsData{}, ops.Forbidden()
	}
	d := settingsData{P: p, Tabs: tabs, FieldTypes: FieldTypes}
	switch tab {
	case "background":
		for _, n := range GradientNames() {
			d.Gradients = append(d.Gradients, gradient{n, BackgroundCSS(Project{BackgroundType: "gradient", BackgroundGradient: n})})
		}
		d.Images, err = ops.Typed[[]BackgroundImage](c.Env, "project.background.list", IDIn{id})
	case "managers":
		if d.Managers, err = ops.Typed[[]Manager](c.Env, "project.manager.list", IDIn{id}); err != nil {
			return d, err
		}
		// Read-only candidate lookup on users (page-specific join).
		rows, qerr := c.Env.DB.QueryContext(c.Env.Ctx, `SELECT id,name,email FROM users WHERE role IN ('admin','projectOwner')
			AND is_deactivated=0 AND deleted_at IS NULL AND id NOT IN (SELECT user_id FROM project_managers WHERE project_id=?) ORDER BY name COLLATE NOCASE`, id)
		if qerr != nil {
			return d, qerr
		}
		defer rows.Close()
		for rows.Next() {
			var cd candidate
			if err := rows.Scan(&cd.ID, &cd.Name, &cd.Email); err != nil {
				return d, err
			}
			d.Candidates = append(d.Candidates, cd)
		}
	case "fields":
		d.Groups, err = ops.Typed[[]BaseGroup](c.Env, "project.fieldgroup.list", IDIn{id})
	}
	return d, err
}

func renderTab(c *web.Ctx, id, tab string) error {
	switch tab {
	case "background", "managers", "fields":
	default:
		tab = "general"
	}
	d, err := loadSettings(c, id, tab)
	if err != nil {
		return err
	}
	return c.RenderBlock("projects_project.html", "projects-tab-"+tab, d)
}
