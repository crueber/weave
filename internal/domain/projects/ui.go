package projects

import (
	"encoding/json"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/crueber/weave/internal/agentprompt"
	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/domain/notify"
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
	Tile     *boardTile   // statistics; nil when they could not be computed (the tile shows the name only)
	stats    boards.BoardStats
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

// settingsSection is one entry of the settings navigation (same shape as boards.SettingsSection).
type settingsSection struct{ ID, Title string }

type settingsData struct {
	P          Project
	Sections   []settingsSection
	Active     string // section shown first (?section=)
	Done       bool   // render the "Saved" confirmation
	Gradients  []gradient
	Images     []BackgroundImage
	Managers   []Manager
	Candidates []candidate
	Groups     []BaseGroup
	FieldTypes []string
	Prompt     promptEdit // "Agents": the project prompt text editor
	IsAdmin    bool
}

// promptEdit feeds the shared bs_prompt editor (same shape as boards.PromptEdit).
type promptEdit struct {
	Text   string // the project's own text, else the text that applies without one
	Custom bool
	From   string
	Tokens []agentprompt.Token
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
		return renderSection(c, c.R.PathValue("id"), sectionParam(c), true)
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

	// The settings are a page (like board settings); the old dialog URL now lands there.
	web.Route("GET /projects/{id}/settings", func(c *web.Ctx) error {
		d, err := loadSettings(c, c.R.PathValue("id"))
		if err != nil {
			return err
		}
		return c.Render("projects_settings.html", d)
	})
	web.Route("GET /ui/projects/{id}/settings", func(c *web.Ctx) error {
		if _, err := loadSettings(c, c.R.PathValue("id")); err != nil {
			return err
		}
		c.Redirect("/projects/" + c.R.PathValue("id") + "/settings")
		return nil
	})
	web.Route("GET /ui/projects/{id}/settings/{tab}", func(c *web.Ctx) error {
		return renderSection(c, c.R.PathValue("id"), c.R.PathValue("tab"), false)
	})

	web.Route("POST /ui/projects/{id}/managers", func(c *web.Ctx) error {
		if _, err := c.Do("project.manager.add"); err != nil {
			return err
		}
		return renderSection(c, c.R.PathValue("id"), "managers", false)
	})
	web.Route("DELETE /ui/projects/{id}/managers/{managerId}", func(c *web.Ctx) error {
		if _, err := c.Do("project.manager.remove"); err != nil {
			return err
		}
		return renderSection(c, c.R.PathValue("id"), "managers", false)
	})

	web.Route("POST /ui/projects/{id}/background-images", func(c *web.Ctx) error {
		if _, err := c.Do("project.background.upload"); err != nil {
			return err
		}
		return renderSection(c, c.R.PathValue("id"), "background", false)
	})
	web.Route("DELETE /ui/projects/{id}/background-images/{imageId}", func(c *web.Ctx) error {
		if _, err := c.Do("project.background.delete"); err != nil {
			return err
		}
		return renderSection(c, c.R.PathValue("id"), "background", false)
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
			return renderSection(c, pid, "fields", false)
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
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	// Statistics for every listed board in a constant number of queries, then the order: most recently active
	// first (no activity: board position), computed once here. Nothing re-sorts the tiles while the page is open.
	ids := make([]string, len(d.Boards))
	for i, b := range d.Boards {
		ids[i] = b.ID
	}
	st, err := boards.Stats(c.Env, ids)
	if err != nil {
		return err
	}
	now := time.Now()
	for i := range d.Boards {
		if s, ok := st[d.Boards[i].ID]; ok {
			d.Boards[i].stats = s
			d.Boards[i].Tile = newTile(s, now)
		}
	}
	sort.SliceStable(d.Boards, func(i, j int) bool { return boards.ActivityLess(d.Boards[i].stats, d.Boards[j].stats) })
	return c.Render("projects_project.html", d)
}

var sections = []settingsSection{{"general", "General"}, {"background", "Background"}, {"managers", "Managers"}, {"fields", "Custom fields"}, {"agents", "Agents"}, {"danger", "Danger zone"}}

// sectionParam is the section a UI mutation came from (_section, or the legacy tab field).
func sectionParam(c *web.Ctx) string {
	if s := c.R.FormValue("_section"); s != "" {
		return s
	}
	return c.R.FormValue("tab")
}

// loadSettings loads everything the settings page shows; managers only (others get 403 as before).
func loadSettings(c *web.Ctx, id string) (settingsData, error) {
	p, err := Get(c.Env, id)
	if err != nil {
		return settingsData{}, err
	}
	if !p.CanManage {
		return settingsData{}, ops.Forbidden()
	}
	d := settingsData{P: p, Sections: sections, Active: "general", FieldTypes: FieldTypes, IsAdmin: c.Env.User != nil && c.Env.User.Role == "admin"}
	def, admin := notify.AgentPromptDefault(c.Env, agentprompt.ScopeProject)
	d.Prompt = promptEdit{Text: def, From: "Weave's built-in text", Tokens: agentprompt.Tokens(agentprompt.ScopeProject)}
	if admin {
		d.Prompt.From = "the admin default"
	}
	if p.AgentPrompt != "" {
		d.Prompt.Text, d.Prompt.Custom = p.AgentPrompt, true
	}
	if c.R != nil {
		q := c.R.URL.Query().Get("section")
		for _, s := range sections {
			if s.ID == q {
				d.Active = q
			}
		}
	}
	for _, n := range GradientNames() {
		d.Gradients = append(d.Gradients, gradient{n, BackgroundCSS(Project{BackgroundType: "gradient", BackgroundGradient: n})})
	}
	if d.Images, err = ops.Typed[[]BackgroundImage](c.Env, "project.background.list", IDIn{id}); err != nil {
		return d, err
	}
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
	if err := rows.Err(); err != nil {
		return d, err
	}
	d.Groups, err = ops.Typed[[]BaseGroup](c.Env, "project.fieldgroup.list", IDIn{id})
	return d, err
}

// renderSection re-renders one settings section after a change; done shows the "Saved" tick.
func renderSection(c *web.Ctx, id, section string, done bool) error {
	switch section {
	case "background", "managers", "fields", "agents":
	default:
		section = "general"
	}
	d, err := loadSettings(c, id)
	if err != nil {
		return err
	}
	d.Done = done
	return c.RenderBlock("projects_settings.html", "projects_set_"+section, d)
}
