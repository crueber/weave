package boards

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/ops"
)

// Board templates come from two places:
//
//   - built-in templates: JSON manifests embedded in the binary (templates/*.json), ids "builtin:<slug>";
//   - published templates: ordinary boards flagged is_template, id = board id, shared according to
//     template_visibility (see authz.TemplateVisible).
//
// Both are turned into the importer's representation (impBoard) and created by createImported, so a new board is
// built by exactly one code path whether it comes from a file, a built-in or a user's template.

//go:embed templates/*.json
var builtinFS embed.FS

const builtinPrefix = "builtin:"

// templateCategories is the gallery's category rail, in display order.
var templateCategories = []struct{ ID, Name string }{
	{"basics", "Basics"}, {"software", "Software"}, {"product", "Product"}, {"sales-marketing", "Sales and marketing"},
	{"people", "People"}, {"personal", "Personal and home"}, {"planning", "Planning"},
}

// TemplateCategory is one entry of the gallery's category rail.
type TemplateCategory struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TemplateCategories returns the built-in categories in display order.
func TemplateCategories() []TemplateCategory {
	out := make([]TemplateCategory, len(templateCategories))
	for i, c := range templateCategories {
		out[i] = TemplateCategory{c.ID, c.Name}
	}
	return out
}

func categoryName(id string) string {
	for _, c := range templateCategories {
		if c.ID == id {
			return c.Name
		}
	}
	return ""
}

// manifest is one built-in template file: gallery metadata around a weave-board document.
type manifest struct {
	ID                  string    `json:"id"`
	Version             int       `json:"version"`
	Category            string    `json:"category"`
	Name                string    `json:"name"`
	Summary             string    `json:"summary"`
	Audience            string    `json:"audience"`
	Tags                []string  `json:"tags"`
	Order               int       `json:"order"` // position inside the category
	DefaultView         string    `json:"defaultView"`
	IncludesSampleCards bool      `json:"includesSampleCards"`
	SampleCardsDefault  bool      `json:"sampleCardsDefault"`
	Board               exportDoc `json:"board"`
}

type builtin struct {
	m  manifest
	ib impBoard
}

var (
	builtinOnce sync.Once
	builtinList []builtin
	builtinErrs []error
)

// loadBuiltins parses the embedded manifests once. A broken manifest is skipped (and recorded for the test) rather than
// taking the server down.
func loadBuiltins() []builtin {
	builtinOnce.Do(func() {
		files, _ := builtinFS.ReadDir("templates")
		for _, f := range files {
			data, err := builtinFS.ReadFile("templates/" + f.Name())
			if err != nil {
				builtinErrs = append(builtinErrs, err)
				continue
			}
			b, err := parseManifest(data)
			if err != nil {
				builtinErrs = append(builtinErrs, fmt.Errorf("%s: %w", f.Name(), err))
				continue
			}
			builtinList = append(builtinList, b)
		}
		sort.SliceStable(builtinList, func(i, j int) bool { return builtinList[i].m.sortKey() < builtinList[j].m.sortKey() })
	})
	return builtinList
}

// sortKey orders the gallery: category rail order, then the manifest's own order.
func (m manifest) sortKey() string {
	for i, c := range templateCategories {
		if c.ID == m.Category {
			return fmt.Sprintf("%d:%04d:%s", i, m.Order, m.ID)
		}
	}
	return "9:" + m.ID
}

func parseManifest(data []byte) (builtin, error) {
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return builtin{}, err
	}
	if !strings.HasPrefix(m.ID, builtinPrefix) || len(m.ID) == len(builtinPrefix) {
		return builtin{}, fmt.Errorf("id must be %s<slug>", builtinPrefix)
	}
	if m.Name == "" || categoryName(m.Category) == "" {
		return builtin{}, fmt.Errorf("name and a known category are required")
	}
	if len(m.Board.Lists) == 0 {
		return builtin{}, fmt.Errorf("no lists")
	}
	ib := impFromDoc(&m.Board)
	if m.DefaultView != "" {
		if !validView(m.DefaultView) {
			return builtin{}, fmt.Errorf("bad defaultView %q", m.DefaultView)
		}
		ib.View = m.DefaultView
	}
	if ib.View == "" {
		ib.View = "kanban"
	}
	m.DefaultView = ib.View
	m.IncludesSampleCards = len(ib.Cards) > 0
	return builtin{m: m, ib: ib}, nil
}

func findBuiltin(id string) (builtin, bool) {
	for _, b := range loadBuiltins() {
		if b.m.ID == id {
			return b, true
		}
	}
	return builtin{}, false
}

// hiddenTemplates is the admin's list of built-in ids to hide (config key templates.hidden: a JSON array, or ids
// separated by commas/whitespace).
func hiddenTemplates(e *ops.Env) map[string]bool {
	raw := strings.TrimSpace(notify.Config(e, "templates.hidden"))
	out := map[string]bool{}
	if raw == "" {
		return out
	}
	var ids []string
	if json.Unmarshal([]byte(raw), &ids) != nil {
		ids = strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';' })
	}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

// ---- API shapes ----

// TemplateColumn is one column of a template; the description is only filled in by board.template.get.
type TemplateColumn struct {
	Name         string `json:"name"`
	Color        string `json:"color,omitempty"`
	WIPLimit     int    `json:"wipLimit,omitempty"`
	ArchiveAfter int    `json:"archiveAfter,omitempty" desc:"auto-archive: the column keeps the newest N cards"`
	Closed       bool   `json:"closed,omitempty"`
	Description  string `json:"description,omitempty"`
}

// TemplateInfo is one gallery row (board.template.list).
type TemplateInfo struct {
	ID                  string           `json:"id" desc:"builtin:<slug> or the id of a published board"`
	Name                string           `json:"name"`
	Source              string           `json:"source" desc:"builtin | published"`
	Category            string           `json:"category,omitempty" desc:"built-ins only: basics|software|product|sales-marketing|people|personal|planning"`
	CategoryName        string           `json:"categoryName,omitempty"`
	Summary             string           `json:"summary,omitempty"`
	Audience            string           `json:"audience,omitempty"`
	Tags                []string         `json:"tags,omitempty"`
	DefaultView         string           `json:"defaultView"`
	IncludesSampleCards bool             `json:"includesSampleCards"`
	SampleCardsDefault  bool             `json:"sampleCardsDefault" desc:"whether the gallery pre-ticks \"include sample cards\" for this template"`
	Project             string           `json:"project,omitempty" desc:"published templates: the project the board lives in"`
	ProjectID           string           `json:"projectId,omitempty"`
	Publisher           string           `json:"publisher,omitempty" desc:"published templates: who published it"`
	Visibility          string           `json:"visibility,omitempty" desc:"published templates: private | group | public"`
	Group               string           `json:"group,omitempty" desc:"published templates shared with a group: its name"`
	Columns             []TemplateColumn `json:"columns"`
}

// TemplatePreviewLabel, TemplatePreviewCard and TemplatePreviewFields describe a template's content for the preview.
type TemplatePreviewLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	Group string `json:"group,omitempty"`
}
type TemplatePreviewCard struct {
	Name          string   `json:"name"`
	Column        string   `json:"column"`
	Labels        []string `json:"labels,omitempty"`
	Tasks         int      `json:"tasks,omitempty"`
	DueOffsetDays *int     `json:"dueOffsetDays,omitempty"`
}
type TemplatePreviewFields struct {
	Name   string   `json:"name"`
	Fields []string `json:"fields"`
}

// TemplatePreview is board.template.get: everything the gallery shows before a board is created.
type TemplatePreview struct {
	TemplateInfo
	LabelGroups  []string                `json:"labelGroups,omitempty" desc:"names of single-select label groups"`
	Labels       []TemplatePreviewLabel  `json:"labels"`
	Cards        []TemplatePreviewCard   `json:"cards"`
	CustomFields []TemplatePreviewFields `json:"customFields,omitempty"`
}

type TemplateListIn struct {
	ProjectID string `query:"projectId" desc:"limit published templates to boards of this project (optional)"`
	Query     string `query:"q" desc:"search words matched against name, summary, tags, category, project and publisher"`
	Category  string `query:"category" desc:"basics|software|product|sales-marketing|people|personal|planning, or published for user-published templates only"`
}
type TemplateGetIn struct {
	TemplateID string `path:"templateId" desc:"builtin:<slug> or a published board id"`
}
type TemplatePublishIn struct {
	ID         string `path:"id"`
	IsTemplate bool   `json:"isTemplate" desc:"mark the board as a reusable template (false unpublishes it)"`
	Visibility string `json:"visibility" desc:"who may use it: private (you and people with access to the board; the default for a new template) | group | public (every signed-in user). Omitted on an already published template keeps its visibility"`
	GroupID    string `json:"groupId" desc:"the user group, when visibility is group"`
}
type InstantiateIn struct {
	ProjectID   string `path:"projectId" desc:"target project"`
	TemplateID  string `json:"templateId" required:"true" desc:"a built-in template id (builtin:<slug>, see board.template.list) or the id of a published template board"`
	Name        string `json:"name" required:"true" desc:"new board name"`
	SampleCards *bool  `json:"sampleCards" desc:"include the template's sample cards (default true). Cards carry their task lists and custom-field values; due dates are relative to now"`
	DefaultView string `json:"defaultView" desc:"override the template's default view: kanban|grid|list|calendar|map"`
}

func registerTemplateOps() {
	ops.Register(ops.Op{Name: "board.template.publish", Summary: "Publish/unpublish a board as a template and choose who may use it (board manager). Due dates of its cards become offsets from the publish moment; unpublish and publish again to refresh them", Method: "POST", Path: "/boards/{id}/template",
		UI: ui("POST", "/boards/{id}/template")}, templatePublish)
	ops.Register(ops.Op{Name: "board.template.list", Summary: "List the board templates the caller may use: built-in ones (with category and summary) and published boards, searchable with q and category", Method: "GET", Path: "/board-templates",
		UI: "GET /ui/board-templates"}, templateList)
	ops.Register(ops.Op{Name: "board.template.get", Summary: "Preview a board template: columns (with limits and agent briefs), labels, sample cards, custom fields", Method: "GET", Path: "/board-templates/{templateId}",
		UI: "GET /ui/board-templates/{templateId}"}, templateGet)
	ops.Register(ops.Op{Name: "board.instantiate", Summary: "Create a board in a project from a built-in or published template (whoever may create boards there, see board.create)", Method: "POST", Path: "/projects/{projectId}/boards/from-template",
		UI: ui("POST", "/projects/{projectId}/boards/from-template")}, instantiate)
}

func validTemplateVisibility(v string) bool { return v == "private" || v == "group" || v == "public" }

func templatePublish(e *ops.Env, in TemplatePublishIn) (Board, error) {
	if err := authz.RequireBoard(e, in.ID, authz.Manage); err != nil {
		return Board{}, err
	}
	now := db.Now()
	var was bool
	var curVis string
	var curGroup sql.NullString
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT is_template,template_visibility,template_group_id FROM boards WHERE id=?`, in.ID).Scan(&was, &curVis, &curGroup)
	if !in.IsTemplate {
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE boards SET is_template=0,template_visibility='private',template_group_id=NULL,template_publisher_id=NULL,template_published_at=NULL,updated_at=? WHERE id=?`, now, in.ID); err != nil {
			return Board{}, err
		}
	} else {
		vis, group := in.Visibility, any(nil)
		switch {
		case vis == "" && was:
			vis = curVis
			if curGroup.Valid {
				group = curGroup.String
			}
		case vis == "":
			vis = "private"
		case !validTemplateVisibility(vis):
			return Board{}, ops.Invalid("visibility must be private, group or public")
		}
		if in.GroupID != "" {
			group = in.GroupID
		}
		if vis == "group" {
			var n int
			if g, _ := group.(string); g != "" {
				_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM user_groups WHERE id=?`, g).Scan(&n)
			}
			if n == 0 {
				return Board{}, ops.Invalid("choose the user group (groupId) to share the template with")
			}
		} else {
			group = nil
		}
		// the due-date anchor and the publisher are fixed when the template is first published
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE boards SET is_template=1,template_visibility=?,template_group_id=?,
			template_publisher_id=CASE WHEN ? THEN template_publisher_id ELSE ? END,
			template_published_at=CASE WHEN ? THEN template_published_at ELSE ? END,updated_at=? WHERE id=?`,
			vis, group, boolInt(was), userOrNil(e), boolInt(was), now, now, in.ID); err != nil {
			return Board{}, err
		}
	}
	b, err := GetBoard(e.Ctx, e.DB, in.ID)
	emit(e, "boardUpdate", "board.updated", in.ID, map[string]any{"id": in.ID})
	return b, err
}

// ---- assembling templates ----

type tpl struct {
	info TemplateInfo
	ib   impBoard
}

func columnsOf(ib impBoard, withDesc bool) []TemplateColumn {
	cols := []TemplateColumn{}
	for _, l := range ib.Lists {
		if l.Type == "archive" || l.Type == "trash" {
			continue
		}
		c := TemplateColumn{Name: l.Name, Color: l.Color, Closed: l.Type == "closed"}
		if v := importWIP(l.WIP); v != nil {
			c.WIPLimit = v.(int)
		}
		if v := importWIP(l.ArchiveAfter); v != nil {
			c.ArchiveAfter = v.(int)
		}
		if withDesc {
			c.Description = l.Desc
		}
		cols = append(cols, c)
	}
	return cols
}

func builtinTpl(b builtin, withDesc bool) tpl {
	m := b.m
	return tpl{ib: b.ib, info: TemplateInfo{ID: m.ID, Name: m.Name, Source: "builtin", Category: m.Category, CategoryName: categoryName(m.Category), Summary: m.Summary,
		Audience: m.Audience, Tags: m.Tags, DefaultView: m.DefaultView, IncludesSampleCards: m.IncludesSampleCards, SampleCardsDefault: m.SampleCardsDefault, Columns: columnsOf(b.ib, withDesc)}}
}

// publishedTpl loads a published board as a template. The caller has checked visibility.
func publishedTpl(e *ops.Env, id string, withDesc, includeCards bool) (tpl, error) {
	src, err := GetBoard(e.Ctx, e.DB, id)
	if err != nil {
		return tpl{}, err
	}
	doc, err := buildExport(e.Ctx, e.DB, src, exportOpts{includeCards: includeCards, includeActivity: false})
	if err != nil {
		return tpl{}, err
	}
	var anchorStr, project, publisher, group sql.NullString
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT b.template_published_at,p.name,u.name,g.name FROM boards b JOIN projects p ON p.id=b.project_id
		LEFT JOIN users u ON u.id=b.template_publisher_id LEFT JOIN user_groups g ON g.id=b.template_group_id WHERE b.id=?`, id).Scan(&anchorStr, &project, &publisher, &group)
	anchor, _ := time.Parse(time.RFC3339, anchorStr.String)
	ib := impFromDoc(doc)
	ib.Name = ""
	// Archive and trash are not part of a template; neither are the people, the discussion or the progress.
	skip := map[string]bool{}
	lists := ib.Lists[:0]
	for _, l := range ib.Lists {
		if l.Type == "archive" || l.Type == "trash" {
			skip[l.Key] = true
			continue
		}
		lists = append(lists, l)
	}
	ib.Lists = lists
	cards := ib.Cards[:0]
	for _, c := range ib.Cards {
		if skip[c.ListKey] {
			continue
		}
		c.MemberEmails, c.Comments, c.DueDone, c.Closed = nil, nil, false, false // closed columns still close their cards
		// Relative due dates: days between the card's due date and the moment the template was published.
		if t, err := time.Parse(time.RFC3339, c.Due); err == nil && !anchor.IsZero() {
			d := int(math.Round(t.Sub(anchor).Hours() / 24))
			c.DueOffsetDays = &d
		}
		c.Due = ""
		for i := range c.TaskLists {
			for j := range c.TaskLists[i].Tasks {
				c.TaskLists[i].Tasks[j].Done = false
			}
		}
		cards = append(cards, c)
	}
	ib.Cards = cards
	if ib.View == "" {
		ib.View = "kanban"
	}
	info := TemplateInfo{ID: id, Name: src.Name, Source: "published", DefaultView: ib.View, IncludesSampleCards: len(ib.Cards) > 0, SampleCardsDefault: len(ib.Cards) > 0,
		Project: project.String, ProjectID: src.ProjectID, Publisher: publisher.String, Visibility: src.TemplateVisibility, Group: group.String, Columns: columnsOf(ib, withDesc)}
	return tpl{info: info, ib: ib}, nil
}

// loadTemplate resolves a template id for the caller: built-ins need only a signed-in user (and must not be hidden by
// the admin); published boards must be visible to the caller (authz.TemplateVisible).
func loadTemplate(e *ops.Env, id string, withDesc, includeCards bool) (tpl, error) {
	if strings.HasPrefix(id, builtinPrefix) {
		b, ok := findBuiltin(id)
		if !ok || hiddenTemplates(e)[id] {
			return tpl{}, ops.NotFound("template")
		}
		return builtinTpl(b, withDesc), nil
	}
	if !authz.TemplateVisible(e, id) {
		if authz.BoardLevel(e, id) >= authz.View {
			return tpl{}, ops.Invalid("board is not a template")
		}
		return tpl{}, ops.NotFound("template")
	}
	return publishedTpl(e, id, withDesc, includeCards)
}

func matchesQuery(t TemplateInfo, q string) bool {
	hay := strings.ToLower(strings.Join([]string{t.Name, t.Summary, t.Audience, strings.Join(t.Tags, " "), t.CategoryName, t.Project, t.Publisher}, " "))
	for _, w := range strings.Fields(strings.ToLower(q)) {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

func templateList(e *ops.Env, in TemplateListIn) ([]TemplateInfo, error) {
	out := []TemplateInfo{}
	if in.Category != "" && in.Category != "published" && categoryName(in.Category) == "" {
		return nil, ops.Invalid("unknown category")
	}
	if in.ProjectID == "" && in.Category != "published" {
		hidden := hiddenTemplates(e)
		for _, b := range loadBuiltins() {
			if hidden[b.m.ID] || (in.Category != "" && b.m.Category != in.Category) {
				continue
			}
			if t := builtinTpl(b, false).info; matchesQuery(t, in.Query) {
				out = append(out, t)
			}
		}
	}
	if (in.Category == "" || in.Category == "published") && e.User != nil {
		pub, err := listPublishedTemplates(e, in.ProjectID, in.Query)
		if err != nil {
			return nil, err
		}
		out = append(out, pub...)
	}
	return out, nil
}

// listPublishedTemplates loads all published templates visible to the caller (matching projectID and query) in a
// constant number of queries: 1 query for template boards + project/publisher/group names, authz.BoardLevels + 1
// query for caller's user_group_members, 1 batched lists query, and 1 grouped card count query.
func listPublishedTemplates(e *ops.Env, projectID, query string) ([]TemplateInfo, error) {
	q := `SELECT b.id,b.name,b.default_view,b.project_id,b.template_visibility,b.template_group_id,b.template_publisher_id,
		COALESCE(p.name,''),COALESCE(u.name,''),COALESCE(g.name,'')
		FROM boards b JOIN projects p ON p.id=b.project_id
		LEFT JOIN users u ON u.id=b.template_publisher_id
		LEFT JOIN user_groups g ON g.id=b.template_group_id
		WHERE b.is_template=1`
	var args []any
	if projectID != "" {
		q += ` AND b.project_id=?`
		args = append(args, projectID)
	}
	rows, err := e.DB.QueryContext(e.Ctx, q+` ORDER BY b.name,b.id LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	type cand struct {
		info      TemplateInfo
		groupID   sql.NullString
		publisher sql.NullString
	}
	var cands []cand
	var candIDs []string
	needGroups := false
	for rows.Next() {
		var c cand
		c.info.Source = "published"
		c.info.Columns = []TemplateColumn{}
		if err := rows.Scan(&c.info.ID, &c.info.Name, &c.info.DefaultView, &c.info.ProjectID, &c.info.Visibility,
			&c.groupID, &c.publisher, &c.info.Project, &c.info.Publisher, &c.info.Group); err != nil {
			rows.Close()
			return nil, err
		}
		if c.info.Visibility == "group" && c.groupID.Valid {
			needGroups = true
		}
		cands = append(cands, c)
		candIDs = append(candIDs, c.info.ID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}
	levels := authz.BoardLevels(e, candIDs)
	myGroups := map[string]bool{}
	if needGroups {
		for _, gid := range strs(e, `SELECT group_id FROM user_group_members WHERE user_id=?`, e.User.UserID) {
			myGroups[gid] = true
		}
	}
	var out []TemplateInfo
	byID := map[string]int{}
	var visArgs []any
	for _, c := range cands {
		visible := levels[c.info.ID] >= authz.View ||
			(c.publisher.Valid && c.publisher.String == e.User.UserID) ||
			c.info.Visibility == "public" ||
			(c.info.Visibility == "group" && c.groupID.Valid && myGroups[c.groupID.String])
		if !visible || !matchesQuery(c.info, query) {
			continue
		}
		byID[c.info.ID] = len(out)
		out = append(out, c.info)
		visArgs = append(visArgs, c.info.ID)
	}
	if len(out) == 0 {
		return nil, nil
	}
	inClause := "?" + strings.Repeat(",?", len(visArgs)-1)
	lrows, err := e.DB.QueryContext(e.Ctx, `SELECT board_id,COALESCE(name,''),COALESCE(color,''),COALESCE(wip_limit,0),COALESCE(archive_after,0),type
		FROM lists WHERE board_id IN (`+inClause+`) AND type IN ('active','closed') ORDER BY board_id, position, id`, visArgs...)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var bid, typ string
		var col TemplateColumn
		if err := lrows.Scan(&bid, &col.Name, &col.Color, &col.WIPLimit, &col.ArchiveAfter, &typ); err != nil {
			lrows.Close()
			return nil, err
		}
		col.Closed = typ == "closed"
		if i, ok := byID[bid]; ok {
			out[i].Columns = append(out[i].Columns, col)
		}
	}
	if err := lrows.Close(); err != nil {
		return nil, err
	}
	if err := lrows.Err(); err != nil {
		return nil, err
	}
	crows, err := e.DB.QueryContext(e.Ctx, `SELECT p.board_id, COUNT(*)
		FROM card_placements p JOIN cards c ON c.id=p.card_id JOIN lists l ON l.id=p.list_id
		WHERE p.board_id IN (`+inClause+`) AND c.is_draft=0 AND c.is_template=0 AND l.type IN ('active','closed')
		GROUP BY p.board_id`, visArgs...)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var bid string
		var n int
		if err := crows.Scan(&bid, &n); err != nil {
			crows.Close()
			return nil, err
		}
		if i, ok := byID[bid]; ok && n > 0 {
			out[i].IncludesSampleCards, out[i].SampleCardsDefault = true, true
		}
	}
	if err := crows.Close(); err != nil {
		return nil, err
	}
	return out, crows.Err()
}

func templateGet(e *ops.Env, in TemplateGetIn) (TemplatePreview, error) {
	t, err := loadTemplate(e, in.TemplateID, true, true)
	if err != nil {
		return TemplatePreview{}, err
	}
	p := TemplatePreview{TemplateInfo: t.info, Labels: []TemplatePreviewLabel{}, Cards: []TemplatePreviewCard{}}
	groupName := map[string]string{}
	for _, g := range t.ib.Groups {
		groupName[g.Key] = g.Name
		if g.SingleSelect {
			p.LabelGroups = append(p.LabelGroups, g.Name)
		}
	}
	labelName := map[string]string{}
	for _, l := range t.ib.Labels {
		labelName[l.Key] = l.Name
		p.Labels = append(p.Labels, TemplatePreviewLabel{Name: l.Name, Color: l.Color, Group: groupName[l.Group]})
	}
	listName := map[string]string{}
	for _, l := range t.ib.Lists {
		listName[l.Key] = l.Name
	}
	for _, c := range t.ib.Cards {
		pc := TemplatePreviewCard{Name: c.Name, Column: listName[c.ListKey], DueOffsetDays: c.DueOffsetDays}
		for _, k := range c.LabelKeys {
			if n := labelName[k]; n != "" {
				pc.Labels = append(pc.Labels, n)
			}
		}
		for _, tl := range c.TaskLists {
			pc.Tasks += len(tl.Tasks)
		}
		p.Cards = append(p.Cards, pc)
	}
	for _, g := range t.ib.FieldGroups {
		if g.Card != "" {
			continue
		}
		pf := TemplatePreviewFields{Name: g.Name}
		for _, f := range g.Fields {
			pf.Fields = append(pf.Fields, f.Name)
		}
		p.CustomFields = append(p.CustomFields, pf)
	}
	return p, nil
}

func instantiate(e *ops.Env, in InstantiateIn) (Board, error) {
	if err := authz.RequireCreateBoard(e, in.ProjectID); err != nil {
		return Board{}, err
	}
	if in.Name == "" {
		return Board{}, ops.Invalid("name is required")
	}
	if err := ops.CheckLen("name", in.Name, ops.MaxNameLen); err != nil {
		return Board{}, err
	}
	if in.DefaultView != "" && !validView(in.DefaultView) {
		return Board{}, ops.Invalid("defaultView must be kanban, grid, list, calendar or map")
	}
	wantCards := in.SampleCards == nil || *in.SampleCards
	t, err := loadTemplate(e, in.TemplateID, false, wantCards)
	if err != nil {
		return Board{}, err
	}
	ib := t.ib
	if !wantCards {
		ib.Cards = nil
	}
	ib.Name = in.Name
	return createImported(e, in.ProjectID, ib, importOpts{Name: in.Name, View: in.DefaultView, Event: map[string]any{"fromTemplate": in.TemplateID}})
}
