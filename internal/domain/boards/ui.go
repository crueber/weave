package boards

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/crueber/weave/internal/agentprompt"
	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/domain/auth"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

func init() {
	web.AddFuncs(template.FuncMap{
		"add1": func(n int) int { return n + 1 },
		"boardsColor": func(c any) string {
			if h, ok := ResolveColor(colorStr(c)); ok {
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
		"boardsInt": func(n *int) int {
			if n == nil {
				return 0
			}
			return *n
		},
		"div100": func(a, b int) int {
			if b <= 0 {
				return 0
			}
			return a * 100 / b
		},
		// viewLabel is the visible name of a view type; the value stays "kanban" in URLs and data.
		"viewLabel": func(v string) string {
			if v == "kanban" {
				return "board"
			}
			return v
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
	registerInbox()
	// ---- board pages ----
	web.Route("GET /boards/{id}", func(c *web.Ctx) error {
		d, err := ops.Typed[BoardDetail](c.Env, "board.get", BoardGetIn{ID: c.R.PathValue("id")})
		if err != nil {
			return err
		}
		q := c.R.URL.Query()
		if q.Get("view") == "" && boardShow(q) == boardShowActive {
			switch d.Board.DefaultView {
			case "calendar", "map":
				c.Redirect("/boards/" + d.Board.ID + "/" + d.Board.DefaultView)
				return nil
			}
		}
		pd, err := loadPage(c.Env, d, q, false)
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
		pd, err := loadPage(c.Env, d, c.R.URL.Query(), false)
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
		pd, err := loadPage(c.Env, d, c.R.URL.Query(), true)
		if err != nil {
			return err
		}
		pd.PageURL = "/public/" + c.R.PathValue("token")
		return c.Render("boards_public.html", pd)
	})
	web.Route("GET /boards/{id}/settings", func(c *web.Ctx) error {
		// settings are for members and managers; someone who can only view a public board goes back to the board
		if id := c.R.PathValue("id"); authz.BoardLevel(c.Env, id) > authz.None && !authz.IsBoardMember(c.Env, id) {
			c.Redirect("/boards/" + id)
			return nil
		}
		sd, err := loadSettings(c, c.R.PathValue("id"), "")
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
		return c.RenderBlock("boards_list.html", "page", map[string]any{"ProjectID": pid, "Boards": bs, "CanCreate": authz.Project(c.Env, pid).CanCreateBoard})
	})
	registerGalleryUI()
	gotoBoard := func(c *web.Ctx, out any) error { c.Redirect("/boards/" + out.(Board).ID); return nil }
	mut("board.create", gotoBoard)
	mut("board.import", gotoBoard)
	mutPre("board.instantiate", func(r *http.Request) {
		// the gallery sends the "include sample cards" choice as a checkbox plus a hidden false; an empty view means "the template's own"
		if r.Form.Get("defaultView") == "" {
			r.Form.Del("defaultView")
		}
	}, gotoBoard)
	mut("board.update", func(c *web.Ctx, out any) error {
		// The settings page has several autosaving forms that PATCH the board; each
		// names the section to re-render in _section.
		block := "boards_set_general"
		switch c.R.FormValue("_section") {
		case "views":
			block = "boards_set_views"
		case "agents":
			block = "boards_set_agents"
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
	mut("board.member.remove", func(c *web.Ctx, out any) error {
		m := out.(Member)
		if !authz.IsBoardMember(c.Env, m.BoardID) { // the caller left the board: there is no settings page to show any more
			to := "/"
			var pid string
			if c.Env.DB.QueryRowContext(c.Env.Ctx, `SELECT project_id FROM boards WHERE id=?`, m.BoardID).Scan(&pid) == nil && authz.ProjectLevel(c.Env, pid) >= authz.View {
				to = "/projects/" + pid
			}
			c.Redirect(to)
			return nil
		}
		return toMembers(c, out)
	})
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
			if c.R.FormValue("_ctx") == "settings" {
				return renderSection(c, b, "boards_set_filters")
			}
			return renderFilters(c, b)
		})
	}

	// ---- lists: all mutate the board body, which refreshes ----
	for _, op := range []string{"list.create", "list.move", "list.delete", "list.archive", "list.sort", "list.clear", "list.moveCards"} {
		mut(op, func(c *web.Ctx, _ any) error { return refresh(c) })
	}
	// the column sheet sends an empty WIP limit when the limit is switched off: that clears it (0 means off)
	mutPre("list.update", func(r *http.Request) {
		if r.Form.Has("wipLimit") && strings.TrimSpace(r.Form.Get("wipLimit")) == "" {
			r.Form.Set("wipLimit", "0")
		}
		if r.Form.Has("archiveAfter") && strings.TrimSpace(r.Form.Get("archiveAfter")) == "" {
			r.Form.Set("archiveAfter", "0") // "Turn off"
		}
	}, func(c *web.Ctx, _ any) error { return refresh(c) })
	registerListSheetUI()
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
	return c.RenderBlock("boards_list.html", "boards_projlabels", map[string]any{"ProjectID": projectID, "Labels": ls, "Groups": gs, "CanEdit": authz.Project(c.Env, projectID).CanManage})
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
	Caps             authz.BoardCaps // every section and control asks this (authz.Board), never the level
	CanEdit          bool            // Caps.CanEdit
	CanManage        bool            // Caps.CanManage
	Members          []Member
	Users            []UserOpt
	Groups           []UserOpt
	AllGroups        []UserOpt // every user group, for the template visibility picker
	Labels           []Label
	LabelGroups      []LabelGroup
	Saved            []SavedFilter
	UseProjectLabels bool
	ProjectName      string
	Views            []string
	LabelRows        []LabelRow
	Active           string // section shown first (?section=)
	Done             bool   // render the "Saved" confirmation
	Sections         []SettingsSection
	AgentCols        []AgentCol // "Agents" section: one row per column with its live listeners
	Prompt           PromptEdit // "Agents" section: the board prompt text editor (managers)
	// Leave board: the caller's own direct membership (board.member.remove allows any member to remove their own),
	// and whether the last-owner rule refuses it right now (LeaveBlocked, the same check the op runs).
	MyMemberID   string
	LeaveBlocked bool
}

// PromptEdit feeds the bs_prompt editor of an editable agent prompt.
type PromptEdit struct {
	Text   string // the custom text, else the text that applies without one
	Custom bool   // a custom text is stored
	From   string // what applies without a custom text ("the admin default" / "Weave's built-in text")
	Tokens []agentprompt.Token
}

// newPromptEdit builds the editor data for scope given the object's own text ("" for the admin defaults).
func newPromptEdit(e *ops.Env, scope, own string) PromptEdit {
	def, admin := notify.AgentPromptDefault(e, scope)
	pe := PromptEdit{Text: def, From: "Weave's built-in text", Tokens: agentprompt.Tokens(scope)}
	if admin {
		pe.From = "the admin default"
	}
	if own != "" {
		pe.Text, pe.Custom = own, true
	}
	return pe
}

// AgentCol is one column row of the board settings "Agents" section.
type AgentCol struct {
	List      List
	Name      string
	Listeners []ListenerOut
}

// LabelRow is a label with its neighbours, for the keyboard move up/down buttons.
type LabelRow struct {
	Label
	PrevID, NextID string
}

// SettingsSection is one entry of the settings navigation.
type SettingsSection struct{ ID, Title string }

// loadSettings builds settings page data. section is "" for the full page, or a section block name
// (boards_set_*) so section re-renders skip loading user.list, group.list, filtersOf and AgentCols when unused.
func loadSettings(c *web.Ctx, boardID, section string) (*SettingsData, error) {
	d, err := ops.Typed[BoardDetail](c.Env, "board.get", BoardGetIn{ID: boardID})
	if err != nil {
		return nil, err
	}
	caps := authz.Board(c.Env, boardID)
	if !caps.CanOpenSettings {
		return nil, ops.NotFound("board") // public viewers do not get the settings fragments either
	}
	full := section == ""
	sd := &SettingsData{Board: d.Board, Level: d.Level, Caps: caps, CanEdit: caps.CanEdit, CanManage: caps.CanManage, Members: d.Members, Labels: d.Labels, LabelGroups: d.LabelGroups,
		Views: []string{"kanban", "grid", "list", "calendar", "map"}}
	if full || section == "boards_set_labels" || section == "boards_set_views" {
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
	}
	if full {
		sd.Sections = settingsSections(caps)
		sd.Active = "general"
		if c.R != nil {
			for _, s := range sd.Sections {
				if c.R.URL.Query().Get("section") == s.ID {
					sd.Active = s.ID
				}
			}
		}
	}
	if full || section == "boards_set_agents" {
		if caps.CanEditPrompt {
			sd.Prompt = newPromptEdit(c.Env, agentprompt.ScopeBoard, d.Board.AgentPrompt)
		}
		for _, l := range d.Lists {
			if !isFinite(l.Type) {
				continue
			}
			ac := AgentCol{List: l, Name: listName(l)}
			if c.Env.Hub != nil {
				for _, x := range c.Env.Hub.Listeners.ForList(l.ID) {
					ac.Listeners = append(ac.Listeners, ListenerOut{UserID: x.UserID, Name: x.Name, IsAgent: x.IsAgent, Label: x.Label, Since: x.Since})
				}
			}
			sd.AgentCols = append(sd.AgentCols, ac)
		}
	}
	if full || section == "boards_set_general" || section == "boards_set_labels" {
		_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT name,use_project_labels FROM projects WHERE id=?`, d.Board.ProjectID).Scan(&sd.ProjectName, &sd.UseProjectLabels)
	}
	if full || section == "boards_set_members" {
		if caps.CanRemoveSelfMember {
			for _, m := range d.Members {
				if m.UserID != nil && *m.UserID == c.Env.User.UserID {
					sd.MyMemberID, sd.LeaveBlocked = m.ID, checkOwnerLeaves(c.Env, c.Env.DB, m) != nil
				}
			}
		}
	}
	if full || section == "boards_set_filters" {
		if sd.Saved, err = filtersOf(c.Env, boardID); err != nil {
			return nil, err
		}
	}
	needUsers := caps.CanManageMembers && (full || section == "boards_set_members")
	needGroups := needUsers || (caps.CanPublishTemplate && (full || section == "boards_set_templates"))
	if needUsers || needGroups {
		in := map[string]bool{}
		for _, m := range d.Members {
			if m.UserID != nil {
				in[*m.UserID] = true
			}
			if m.GroupID != nil {
				in[*m.GroupID] = true
			}
		}
		// People and groups come from the caller's own directory (user.list / group.list), so a board owner with a
		// restricted directory (worker) only finds the accounts they already share a board with, and only admins
		// see e-mail addresses.
		if needUsers {
			if ul, err := ops.Typed[auth.UserList](c.Env, "user.list", auth.ListIn{}); err == nil {
				for _, u := range ul.Items {
					if u == nil || in[u.ID] || u.IsDeactivated {
						continue
					}
					o := UserOpt{ID: u.ID, Name: u.Name, Email: u.Email}
					if o.Email == "" && u.Username != nil && *u.Username != "" {
						o.Email = "@" + *u.Username
					}
					sd.Users = append(sd.Users, o)
				}
			}
		}
		if needGroups {
			if gl, err := ops.Typed[auth.GroupList](c.Env, "group.list", auth.NoInput{}); err == nil {
				for _, g := range gl.Items {
					if g == nil {
						continue
					}
					o := UserOpt{ID: g.ID, Name: g.Name}
					sd.AllGroups = append(sd.AllGroups, o)
					if !in[g.ID] {
						sd.Groups = append(sd.Groups, o)
					}
				}
			}
		}
	}
	return sd, nil
}

// settingsSections is the board settings navigation: a section is listed iff the caller can use it (the panels are
// rendered under the same conditions in boards_settings.html), so nobody lands on an empty "only owners can..." page.
func settingsSections(caps authz.BoardCaps) []SettingsSection {
	out := []SettingsSection{{"general", "General"}, {"members", "Members & access"}, {"labels", "Labels"}, {"views", "Views & cards"}, {"filters", "Filters"}}
	if caps.CanShare {
		out = append(out, SettingsSection{"sharing", "Sharing & public link"})
	}
	if caps.CanPublishTemplate {
		out = append(out, SettingsSection{"templates", "Templates & export"})
	} else {
		out = append(out, SettingsSection{"templates", "Export"})
	}
	out = append(out, SettingsSection{"agents", "Agents"})
	if caps.CanManageNotify {
		out = append(out, SettingsSection{"notifications", "Notifications"})
	}
	if caps.CanManageWebhooks {
		out = append(out, SettingsSection{"webhooks", "Webhooks"})
	}
	if caps.CanDelete {
		out = append(out, SettingsSection{"danger", "Danger zone"})
	}
	return out
}

func renderSectionSaved(c *web.Ctx, boardID, block string) error {
	sd, err := loadSettings(c, boardID, block)
	if err != nil {
		return err
	}
	sd.Done = true
	return c.RenderBlock("boards_settings.html", block, sd)
}

func renderSection(c *web.Ctx, boardID, block string) error {
	sd, err := loadSettings(c, boardID, block)
	if err != nil {
		return err
	}
	return c.RenderBlock("boards_settings.html", block, sd)
}

// ---- card drop ----

// dropCard handles a card dragged in the board view: it computes the new position
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
	var draft bool
	_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT COUNT(*),COALESCE(MAX(c.is_draft),0) FROM card_placements p JOIN cards c ON c.id=p.card_id WHERE p.card_id=? AND p.board_id=?`, cardID, boardID).Scan(&n, &draft)
	if n == 0 {
		return ops.NotFound("card")
	}
	l, err := GetList(c.R.Context(), c.Env.DB, listID)
	if err != nil || l.BoardID != boardID {
		return ops.NotFound("list")
	}
	pos, err := PlaceCardBetween(c.R.Context(), c.Env.DB, listID, cardID, after, before)
	if err != nil {
		return err
	}
	if draft { // dragged out of the Inbox onto a column: publish it there
		if err := draftDrop(c, cardID, listID, pos); err != nil {
			return err
		}
		return refresh(c)
	}
	// boardId: the card may be on several boards; a drop on this board moves THIS board's placement
	form := url.Values{"id": {cardID}, "cardId": {cardID}, "listId": {listID}, "boardId": {boardID}, "position": {fmt.Sprint(pos)},
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
	// the form is the query string too: an op input tagged query:"x" (card.label.remove's boardId) binds from the URL
	r, err := http.NewRequestWithContext(e.Ctx, "POST", "/?"+form.Encode(), strings.NewReader(form.Encode()))
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
