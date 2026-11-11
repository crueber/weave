package boards

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

var colorHex = map[string]string{
	"muddy-grey": "#8a8f98", "autumn-leafs": "#b4572d", "morning-sky": "#8ec5e6", "antique-blue": "#5b7f95", "egg-yellow": "#f2d04c",
	"desert-sand": "#d9bc8c", "dark-granite": "#4b535c", "fresh-salad": "#7cc576", "lagoon-blue": "#2e9bb8", "midnight-blue": "#1f3a6e",
	"light-orange": "#f5a65b", "pumpkin-orange": "#e8741e", "light-concrete": "#c3c7cc", "sunny-grass": "#a6d142", "navy-blue": "#26407a",
	"lilac-eyes": "#a98bd0", "apricot-red": "#ee7d6a", "orange-peel": "#f59a2c", "silver-glint": "#aeb6bf", "bright-moss": "#5fae3d",
	"deep-ocean": "#1b5e86", "summer-sky": "#58b4e8", "berry-red": "#c8324a", "light-cocoa": "#a98672", "grey-stone": "#7d858e",
	"tank-green": "#4f6b3c", "coral-green": "#3fa88a", "sugar-plum": "#8c4a7d", "pink-tulip": "#ee86b4", "shady-rust": "#8c4b2f",
	"wet-rock": "#566370", "wet-moss": "#5d7a4f", "turquoise-sea": "#2bc3c0", "lavender-fields": "#9b95e0", "piggy-red": "#f29aa5",
	"light-mud": "#b9a58a", "gun-metal": "#3a4249", "modern-green": "#2f9e6a", "french-coast": "#4a7fd1", "sweet-lilac": "#c9a6e8",
	"red-burgundy": "#8a1f33", "pirate-gold": "#c9951f",
}

func init() {
	web.AddFuncs(template.FuncMap{
		"boardsColor": func(c any) string {
			var s string
			switch v := c.(type) {
			case string:
				s = v
			case *string:
				if v != nil {
					s = *v
				}
			}
			if h, ok := ResolveColor(s); ok {
				return h
			}
			return "#8a8f98"
		},
		"boardsInitials": func(name string) string {
			f := strings.Fields(name)
			switch len(f) {
			case 0:
				return "?"
			case 1:
				r := []rune(f[0])
				return strings.ToUpper(string(r[0]))
			}
			return strings.ToUpper(string([]rune(f[0])[0]) + string([]rune(f[len(f)-1])[0]))
		},
		"boardsColors": func() []string { return Colors },
		"boardsBool":   func(b *bool) bool { return b != nil && *b },
		"div100": func(a, b int) int {
			if b <= 0 {
				return 0
			}
			return a * 100 / b
		},
		"boardsHref": func(base, view, query string) template.URL {
			q := url.Values{}
			if view != "" {
				q.Set("view", view)
			}
			s := q.Encode()
			if query != "" {
				if s != "" {
					s += "&"
				}
				s += query
			}
			if s != "" {
				base += "?" + s
			}
			return template.URL(base)
		},
		"boardsEsc": url.QueryEscape,
		"boardsFloat": func(f *float64) float64 {
			if f == nil {
				return 0
			}
			return *f
		},
	})
}

type after func(c *web.Ctx, out any) error

// mut wires the browser route of a mutating op: bind -> ops.Call -> after().
func mut(opName string, a after) {
	o := ops.Get(opName)
	if o == nil {
		panic("boards: unknown op " + opName)
	}
	pattern := o.UI
	web.Route(pattern, func(c *web.Ctx) error {
		out, err := c.Do(opName)
		if err != nil {
			return err
		}
		return a(c, out)
	})
}

// mutPre is mut with a hook that may adjust the submitted form before the op binds it.
func mutPre(opName string, pre func(*http.Request), a after) {
	o := ops.Get(opName)
	if o == nil {
		panic("boards: unknown op " + opName)
	}
	web.Route(o.UI, func(c *web.Ctx) error {
		_ = c.R.ParseForm()
		pre(c.R)
		out, err := c.Do(opName)
		if err != nil {
			return err
		}
		return a(c, out)
	})
}

// normalizeRole maps the UI's three-way access choice (editor | commenter | viewer)
// onto the op's role + canComment pair.
func normalizeRole(r *http.Request) {
	switch r.Form.Get("role") {
	case "commenter":
		r.Form.Set("role", "viewer")
		r.Form.Set("canComment", "true")
	case "viewer":
		r.Form.Set("canComment", "false")
	}
}

func refresh(c *web.Ctx) error {
	c.W.Header().Set("HX-Trigger", `{"boards:refresh":true}`)
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

// boardParam finds the board a section mutation belongs to.
func boardParam(c *web.Ctx) string {
	if id := c.R.PathValue("boardId"); id != "" {
		return id
	}
	return c.R.FormValue("_board")
}

func registerUI() {
	registerQuick()
	// ---- board pages ----
	web.Route("GET /boards/{id}", func(c *web.Ctx) error {
		d, err := ops.Typed[BoardDetail](c.Env, "board.get", BoardGetIn{ID: c.R.PathValue("id")})
		if err != nil {
			return err
		}
		q := c.R.URL.Query()
		if q.Get("view") == "" {
			switch d.Board.DefaultView {
			case "calendar", "map":
				c.Redirect("/boards/" + d.Board.ID + "/" + d.Board.DefaultView)
				return nil
			}
		}
		pd, err := loadPage(c.Env, d.Board, q, false)
		if err != nil {
			return err
		}
		return c.Render("boards_board.html", pd)
	})
	web.Route("GET /ui/boards/{id}/body", func(c *web.Ctx) error {
		d, err := ops.Typed[BoardDetail](c.Env, "board.get", BoardGetIn{ID: c.R.PathValue("id")})
		if err != nil {
			return err
		}
		pd, err := loadPage(c.Env, d.Board, c.R.URL.Query(), false)
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_board.html", "boards_body", pd)
	})
	web.RoutePublic("GET /public/{token}", func(c *web.Ctx) error {
		d, err := ops.Typed[BoardDetail](c.Env, "board.public.get", PublicGetIn{Token: c.R.PathValue("token")})
		if err != nil {
			return err
		}
		pd, err := loadPage(c.Env, d.Board, c.R.URL.Query(), true)
		if err != nil {
			return err
		}
		pd.Members, pd.Saved = nil, nil
		pd.PageURL = "/public/" + c.R.PathValue("token")
		return c.Render("boards_public.html", pd)
	})
	web.Route("GET /boards/{id}/settings", func(c *web.Ctx) error {
		// settings are for members and managers; someone who can only view a public board goes back to the board
		if id := c.R.PathValue("id"); authz.BoardLevel(c.Env, id) > authz.None && !authz.IsBoardMember(c.Env, id) {
			c.Redirect("/boards/" + id)
			return nil
		}
		sd, err := loadSettings(c, c.R.PathValue("id"))
		if err != nil {
			return err
		}
		return c.Render("boards_settings.html", sd)
	})

	// ---- composite drag-drop endpoints (board page) ----
	web.Route("POST /ui/boards/{boardId}/cards/drop", dropCard)
	web.Route("POST /ui/boards/{boardId}/lists/drop", func(c *web.Ctx) error {
		if err := c.R.ParseForm(); err != nil {
			return ops.Invalid("bad form")
		}
		in := ListMoveIn{ID: c.R.FormValue("id"), AfterID: c.R.FormValue("afterId"), BeforeID: c.R.FormValue("beforeId")}
		if _, err := ops.Call(c.Env, "list.move", &in); err != nil {
			return err
		}
		return refresh(c)
	})

	// ---- boards ----
	web.Route(ops.Get("board.list").UI, func(c *web.Ctx) error {
		pid := c.R.PathValue("projectId")
		bs, err := ops.Typed[[]Board](c.Env, "board.list", BoardListIn{ProjectID: pid})
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_list.html", "page", map[string]any{"ProjectID": pid, "Boards": bs, "CanCreate": authz.ProjectLevel(c.Env, pid) >= authz.Manage})
	})
	web.Route("GET /ui/projects/{projectId}/boards/new", func(c *web.Ctx) error {
		pid := c.R.PathValue("projectId")
		if err := authz.RequireProject(c.Env, pid, authz.Manage); err != nil {
			return err
		}
		ts, err := ops.Typed[[]Board](c.Env, "board.template.list", TemplateListIn{})
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_list.html", "boards_new", map[string]any{"ProjectID": pid, "Templates": ts})
	})
	web.Route(ops.Get("board.template.list").UI, func(c *web.Ctx) error {
		ts, err := ops.Typed[[]Board](c.Env, "board.template.list", TemplateListIn{ProjectID: c.R.URL.Query().Get("projectId")})
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_list.html", "boards_templates", map[string]any{"Templates": ts})
	})
	gotoBoard := func(c *web.Ctx, out any) error { c.Redirect("/boards/" + out.(Board).ID); return nil }
	mut("board.create", gotoBoard)
	mut("board.import", gotoBoard)
	mut("board.instantiate", gotoBoard)
	mut("board.update", func(c *web.Ctx, out any) error {
		// The settings page has several autosaving forms that PATCH the board; each
		// names the section to re-render in _section.
		block := "boards_set_general"
		if c.R.FormValue("_section") == "views" {
			block = "boards_set_views"
		}
		return renderSectionSaved(c, out.(Board).ID, block)
	})
	mut("board.move", func(c *web.Ctx, out any) error { return refresh(c) })
	mut("board.setPublic", func(c *web.Ctx, out any) error { return renderSectionSaved(c, out.(Board).ID, "boards_set_sharing") })
	mut("board.template.publish", func(c *web.Ctx, out any) error { return renderSectionSaved(c, out.(Board).ID, "boards_set_templates") })
	web.Route(ops.Get("board.delete").UI, func(c *web.Ctx) error {
		b, err := GetBoard(c.R.Context(), c.Env.DB, c.R.PathValue("id"))
		if err != nil {
			return err
		}
		if _, err := c.Do("board.delete"); err != nil {
			return err
		}
		c.Redirect("/projects/" + b.ProjectID)
		return nil
	})
	web.Route(ops.Get("board.export").UI, func(c *web.Ctx) error {
		out, err := c.Do("board.export")
		if err != nil {
			return err
		}
		st := out.(*ops.Stream)
		c.W.Header().Set("Content-Type", st.ContentType)
		c.W.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", st.Name))
		_, _ = c.W.Write(st.Data)
		return nil
	})

	// ---- sections of the settings page (also usable as standalone fragments) ----
	sec := func(op, block string) {
		web.Route(ops.Get(op).UI, func(c *web.Ctx) error { return renderSection(c, c.R.PathValue("boardId"), block) })
	}
	sec("board.member.list", "boards_set_members")
	sec("label.list", "boards_set_labels")
	sec("labelgroup.list", "boards_set_labels")
	web.Route(ops.Get("filter.list").UI, func(c *web.Ctx) error { return renderFilters(c, c.R.PathValue("boardId")) })
	web.Route(ops.Get("list.list").UI, func(c *web.Ctx) error {
		ls, err := ops.Typed[[]List](c.Env, "list.list", ListListIn{BoardID: c.R.PathValue("boardId")})
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_board.html", "boards_list_options", ls)
	})
	toMembers := func(c *web.Ctx, out any) error { return renderSection(c, out.(Member).BoardID, "boards_set_members") }
	mutPre("board.member.add", normalizeRole, toMembers)
	mutPre("board.member.update", normalizeRole, toMembers)
	mut("board.member.remove", toMembers)
	toLabels := func(c *web.Ctx, _ any) error { return renderSection(c, boardParam(c), "boards_set_labels") }
	for _, op := range []string{"label.create", "label.update", "label.move", "label.delete", "labelgroup.create", "labelgroup.update", "labelgroup.delete"} {
		mut(op, toLabels)
	}
	// Drag-to-reorder on the labels list: delegates to label.move, then re-renders the section.
	web.Route("POST /ui/boards/{boardId}/labels/drop", func(c *web.Ctx) error {
		if err := c.R.ParseForm(); err != nil {
			return ops.Invalid("bad form")
		}
		boardID := c.R.PathValue("boardId")
		if err := authz.RequireBoard(c.Env, boardID, authz.Edit); err != nil {
			return err
		}
		form := url.Values{"id": {c.R.FormValue("id")}, "afterId": {c.R.FormValue("afterId")}, "beforeId": {c.R.FormValue("beforeId")}}
		if _, err := CallByForm(c.Env, "label.move", form); err != nil {
			return err
		}
		c.W.Header().Set("HX-Retarget", "#set-labels")
		c.W.Header().Set("HX-Reswap", "outerHTML")
		return renderSection(c, boardID, "boards_set_labels")
	})
	web.Route(ops.Get("label.project.list").UI, func(c *web.Ctx) error { return renderProjectLabels(c, c.R.PathValue("projectId")) })
	for _, op := range []string{"label.project.create", "labelgroup.project.create"} {
		mut(op, func(c *web.Ctx, _ any) error { return renderProjectLabels(c, c.R.PathValue("projectId")) })
	}
	for _, op := range []string{"filter.create", "filter.update", "filter.delete"} {
		mut(op, func(c *web.Ctx, out any) error {
			b := boardParam(c)
			if f, ok := out.(SavedFilter); ok {
				b = f.BoardID
			}
			if b == "" {
				b = c.R.FormValue("_board")
			}
			if c.R.FormValue("_ctx") == "settings" {
				return renderSection(c, b, "boards_set_filters")
			}
			return renderFilters(c, b)
		})
	}

	// ---- lists: all mutate the board body, which refreshes ----
	for _, op := range []string{"list.create", "list.update", "list.move", "list.delete", "list.archive", "list.sort", "list.clear", "list.moveCards"} {
		mut(op, func(c *web.Ctx, _ any) error { return refresh(c) })
	}
}

func renderProjectLabels(c *web.Ctx, projectID string) error {
	ls, err := ops.Typed[[]Label](c.Env, "label.project.list", ProjectLabelListIn{ProjectID: projectID})
	if err != nil {
		return err
	}
	gs := []LabelGroup{}
	rows, err := c.Env.DB.QueryContext(c.R.Context(), `SELECT id,board_id,project_id,name,single_select,position FROM label_groups WHERE project_id=? AND board_id IS NULL ORDER BY position`, projectID)
	if err == nil {
		for rows.Next() {
			var g LabelGroup
			_ = rows.Scan(&g.ID, &g.BoardID, &g.ProjectID, &g.Name, &g.SingleSelect, &g.Position)
			gs = append(gs, g)
		}
		rows.Close()
	}
	return c.RenderBlock("boards_list.html", "boards_projlabels", map[string]any{"ProjectID": projectID, "Labels": ls, "Groups": gs, "CanEdit": authz.ProjectLevel(c.Env, projectID) >= authz.Manage})
}

// SavedData feeds the saved-filter dropdown fragment.
type SavedData struct {
	BoardID string
	Saved   []SavedFilter
	Query   string
	View    string
}

func renderFilters(c *web.Ctx, boardID string) error {
	fs, err := ops.Typed[[]SavedFilter](c.Env, "filter.list", FilterListIn{BoardID: boardID})
	if err != nil {
		return err
	}
	return c.RenderBlock("boards_board.html", "boards_saved", SavedData{BoardID: boardID, Saved: fs, Query: c.R.FormValue("query"), View: c.R.FormValue("view")})
}

// ---- settings ----

type UserOpt struct{ ID, Name, Email string }

type SettingsData struct {
	Board            Board
	Level            string
	CanEdit          bool
	CanManage        bool
	Members          []Member
	Users            []UserOpt
	Groups           []UserOpt
	Labels           []Label
	LabelGroups      []LabelGroup
	Saved            []SavedFilter
	UseProjectLabels bool
	ProjectName      string
	Views            []string
	CardTypes        []string
	LabelRows        []LabelRow
	Active           string // section shown first (?section=)
	Done             bool   // render the "Saved" confirmation
	Sections         []SettingsSection
}

// LabelRow is a label with its neighbours, for the keyboard move up/down buttons.
type LabelRow struct {
	Label
	PrevID, NextID string
}

// SettingsSection is one entry of the settings navigation.
type SettingsSection struct{ ID, Title string }

func loadSettings(c *web.Ctx, boardID string) (*SettingsData, error) {
	d, err := ops.Typed[BoardDetail](c.Env, "board.get", BoardGetIn{ID: boardID})
	if err != nil {
		return nil, err
	}
	lvl := authz.BoardLevel(c.Env, boardID)
	if lvl > authz.None && !authz.IsBoardMember(c.Env, boardID) {
		return nil, ops.NotFound("board") // public viewers do not get the settings fragments either
	}
	sd := &SettingsData{Board: d.Board, Level: d.Level, CanEdit: lvl >= authz.Edit, CanManage: lvl >= authz.Manage, Members: d.Members, Labels: d.Labels, LabelGroups: d.LabelGroups,
		Views: []string{"kanban", "grid", "list", "calendar", "map"}, CardTypes: []string{"project", "story"}}
	for i, l := range sd.Labels {
		r := LabelRow{Label: l}
		if i > 0 {
			r.PrevID = sd.Labels[i-1].ID
		}
		if i+1 < len(sd.Labels) {
			r.NextID = sd.Labels[i+1].ID
		}
		sd.LabelRows = append(sd.LabelRows, r)
	}
	sd.Sections = []SettingsSection{{"general", "General"}, {"members", "Members & access"}, {"labels", "Labels"}, {"views", "Views & cards"}, {"filters", "Filters"},
		{"sharing", "Sharing & public link"}, {"templates", "Templates & export"}}
	if sd.CanManage {
		sd.Sections = append(sd.Sections, SettingsSection{"notifications", "Notifications"}, SettingsSection{"danger", "Danger zone"})
	}
	sd.Active = "general"
	if c.R != nil {
		for _, s := range sd.Sections {
			if c.R.URL.Query().Get("section") == s.ID {
				sd.Active = s.ID
			}
		}
	}
	_, sd.UseProjectLabels = projectOfBoard(c.Env, boardID)
	_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT name FROM projects WHERE id=?`, d.Board.ProjectID).Scan(&sd.ProjectName)
	if sd.Saved, err = filtersOf(c.Env, boardID); err != nil {
		return nil, err
	}
	if sd.CanManage {
		in := map[string]bool{}
		for _, m := range d.Members {
			if m.UserID != nil {
				in[*m.UserID] = true
			}
			if m.GroupID != nil {
				in[*m.GroupID] = true
			}
		}
		if rows, err := c.Env.DB.QueryContext(c.R.Context(), `SELECT id,name,email FROM users WHERE deleted_at IS NULL AND is_deactivated=0 ORDER BY name`); err == nil {
			for rows.Next() {
				var u UserOpt
				_ = rows.Scan(&u.ID, &u.Name, &u.Email)
				if !in[u.ID] {
					sd.Users = append(sd.Users, u)
				}
			}
			rows.Close()
		}
		if rows, err := c.Env.DB.QueryContext(c.R.Context(), `SELECT id,name FROM user_groups ORDER BY name`); err == nil {
			for rows.Next() {
				var g UserOpt
				_ = rows.Scan(&g.ID, &g.Name)
				if !in[g.ID] {
					sd.Groups = append(sd.Groups, g)
				}
			}
			rows.Close()
		}
	}
	return sd, nil
}

func renderSectionSaved(c *web.Ctx, boardID, block string) error {
	sd, err := loadSettings(c, boardID)
	if err != nil {
		return err
	}
	sd.Done = true
	return c.RenderBlock("boards_settings.html", block, sd)
}

func renderSection(c *web.Ctx, boardID, block string) error {
	sd, err := loadSettings(c, boardID)
	if err != nil {
		return err
	}
	return c.RenderBlock("boards_settings.html", block, sd)
}

// ---- card drop ----

// dropCard handles a card dragged on the kanban: it computes the new position
// from the neighbouring cards and delegates the move to the cards domain's
// "card.move" op (bound through its own input tags).
func dropCard(c *web.Ctx) error {
	if err := c.R.ParseForm(); err != nil {
		return ops.Invalid("bad form")
	}
	boardID := c.R.PathValue("boardId")
	if err := authz.RequireBoard(c.Env, boardID, authz.Edit); err != nil {
		return err
	}
	cardID, listID := c.R.FormValue("id"), c.R.FormValue("container")
	after, before := c.R.FormValue("afterId"), c.R.FormValue("beforeId")
	var n int
	_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT COUNT(*) FROM cards WHERE id=? AND board_id=?`, cardID, boardID).Scan(&n)
	if n == 0 {
		return ops.NotFound("card")
	}
	l, err := GetList(c.R.Context(), c.Env.DB, listID)
	if err != nil || l.BoardID != boardID {
		return ops.NotFound("list")
	}
	pos := PlaceBetween(c.R.Context(), c.Env.DB, "cards", "list_id=? AND id<>?", []any{listID, cardID}, after, before)
	form := url.Values{"id": {cardID}, "cardId": {cardID}, "listId": {listID}, "position": {fmt.Sprint(pos)},
		"afterId": {after}, "beforeId": {before}, "index": {c.R.FormValue("index")}}
	if _, err := CallByForm(c.Env, "card.move", form); err != nil {
		return err
	}
	return refresh(c)
}

// CallByForm invokes an op owned by another domain without importing its input
// types: every {param} of the op's path and every body field is bound from form.
func CallByForm(e *ops.Env, name string, form url.Values) (any, error) {
	o := ops.Get(name)
	if o == nil {
		return nil, ops.Invalid("operation " + name + " is not available")
	}
	r, err := http.NewRequestWithContext(e.Ctx, "POST", "/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, seg := range strings.Split(o.Path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			p := seg[1 : len(seg)-1]
			v := form.Get(p)
			if v == "" {
				v = form.Get("id")
			}
			r.SetPathValue(p, v)
		}
	}
	in, err := o.Bind(r)
	if err != nil {
		return nil, err
	}
	return ops.Call(e, name, in)
}
