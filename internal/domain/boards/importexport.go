package boards

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// ---- intermediate representation shared by all importers ----

type impList struct {
	Key, Name, Type, Color string
	Pos                    float64
}
type impGroup struct {
	Key, Name    string
	SingleSelect bool
}
type impLabel struct {
	Key, Name, Color, Group string
}
type impTask struct {
	Name string
	Done bool
}
type impTaskList struct {
	Name  string
	Tasks []impTask
}
type impComment struct {
	Text, CreatedAt string
}
type impCard struct {
	ListKey, Name, Desc, Due, Type string
	DueDone                        bool
	Pos                            float64
	LabelKeys                      []string
	MemberEmails                   []string
	TaskLists                      []impTaskList
	Comments                       []impComment
}
type impBoard struct {
	Name          string
	Color         string // normalised #rrggbb or ""
	CardColorMode string // pill|fill or ""
	Lists         []impList
	Groups        []impGroup
	Labels        []impLabel
	Cards         []impCard
}

// ---- Weave/Planka JSON export document ----

type exportDoc struct {
	Format  string         `json:"format"`
	Version int            `json:"version"`
	Board   map[string]any `json:"board"`
	Lists   []exportList   `json:"lists"`
	Groups  []exportGroup  `json:"labelGroups"`
	Labels  []exportLabel  `json:"labels"`
	Cards   []exportCard   `json:"cards"`
}
type exportList struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Color    string  `json:"color,omitempty"`
	Position float64 `json:"position"`
}
type exportGroup struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SingleSelect bool   `json:"singleSelect"`
}
type exportLabel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Color   string `json:"color"`
	GroupID string `json:"groupId,omitempty"`
}
type exportTaskList struct {
	Name  string       `json:"name"`
	Tasks []exportTask `json:"tasks"`
}
type exportTask struct {
	Name        string `json:"name"`
	IsCompleted bool   `json:"isCompleted"`
}
type exportComment struct {
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
	User      string `json:"user,omitempty"`
}
type exportCard struct {
	ID             string           `json:"id"`
	ListID         string           `json:"listId"`
	Name           string           `json:"name"`
	Description    string           `json:"description,omitempty"`
	Type           string           `json:"type"`
	DueDate        string           `json:"dueDate,omitempty"`
	IsDueCompleted bool             `json:"isDueCompleted,omitempty"`
	Position       float64          `json:"position"`
	LabelIDs       []string         `json:"labelIds,omitempty"`
	MemberEmails   []string         `json:"memberEmails,omitempty"`
	TaskLists      []exportTaskList `json:"taskLists,omitempty"`
	Comments       []exportComment  `json:"comments,omitempty"`
}

type ImportIn struct {
	ProjectID string    `path:"projectId"`
	Format    string    `json:"format" required:"true" desc:"planka (Weave/Planka JSON) | trello | csv"`
	Name      string    `json:"name" desc:"board name (default: name from the file)"`
	Data      string    `json:"data" desc:"file content (alternative to file upload)"`
	File      *ops.File `json:"file" desc:"uploaded export file"`
}
type ExportIn struct {
	ID     string `path:"id"`
	Format string `query:"format" desc:"json (default) | csv"`
}

func registerImportExportOps() {
	ops.Register(ops.Op{Name: "board.import", Summary: "Create a board from a Planka/Weave JSON export, a Trello JSON export or a CSV (project manager)", Method: "POST", MaxBody: 64 << 20, Path: "/projects/{projectId}/boards/import",
		UI: ui("POST", "/projects/{projectId}/boards/import")}, importBoard)
	ops.Register(ops.Op{Name: "board.export", Summary: "Export a board as JSON (round-trips through board.import) or CSV", Method: "GET", Path: "/boards/{id}/export",
		Binary: true, UI: ui("GET", "/boards/{id}/export")}, exportBoard)
}

func importBoard(e *ops.Env, in ImportIn) (Board, error) {
	if err := authz.RequireProject(e, in.ProjectID, authz.Manage); err != nil {
		return Board{}, err
	}
	var data []byte
	switch {
	case in.File != nil && len(in.File.Data) > 0:
		data = in.File.Data
	case in.Data != "":
		data = []byte(in.Data)
	default:
		return Board{}, ops.Invalid("no import data provided")
	}
	var ib impBoard
	var err error
	switch in.Format {
	case "planka", "weave", "json":
		ib, err = parseWeave(data)
	case "trello":
		ib, err = parseTrello(data)
	case "csv":
		ib, err = parseCSV(data)
	default:
		return Board{}, ops.Invalid("format must be planka, trello or csv")
	}
	if err != nil {
		return Board{}, ops.Invalid("invalid import file: " + err.Error())
	}
	name := in.Name
	if name == "" {
		name = ib.Name
	}
	if name == "" {
		name = "Imported board"
	}
	id := db.NewID()
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		if err := insertBoard(e, tx, id, in.ProjectID, name, "kanban", nil); err != nil {
			return err
		}
		uid := ""
		if e.User != nil {
			uid = e.User.UserID
		}
		if _, err := tx.ExecContext(e.Ctx, `INSERT INTO board_imports(id,user_id,source,status,created_at) VALUES(?,?,?,?,?)`, db.NewID(), uid, in.Format, "done", db.Now()); err != nil {
			return err
		}
		return writeImport(e, tx, id, in.ProjectID, ib)
	})
	if err != nil {
		return Board{}, err
	}
	emit(e, "boardCreate", "board.created", id, map[string]any{"id": id, "projectId": in.ProjectID, "imported": true})
	e.Hub.Publish(projectEvent("board.created", in.ProjectID, id))
	return GetBoard(e.Ctx, e.DB, id)
}

func writeImport(e *ops.Env, tx *sql.Tx, boardID, projectID string, ib impBoard) error {
	ctx, now := e.Ctx, db.Now()
	if ib.Color != "" || ib.CardColorMode != "" {
		mode := ib.CardColorMode
		if mode == "" {
			mode = CardColorPill
		}
		if _, err := tx.ExecContext(ctx, `UPDATE boards SET color=?,card_color_mode=? WHERE id=?`, nullStr(ib.Color), mode, boardID); err != nil {
			return err
		}
	}
	var useProject bool
	_ = tx.QueryRowContext(ctx, `SELECT use_project_labels FROM projects WHERE id=?`, projectID).Scan(&useProject)
	groups := map[string]string{}
	if !useProject {
		for i, g := range ib.Groups {
			id := db.NewID()
			groups[g.Key] = id
			if _, err := tx.ExecContext(ctx, `INSERT INTO label_groups(id,board_id,name,single_select,position) VALUES(?,?,?,?,?)`, id, boardID, g.Name, boolInt(g.SingleSelect), float64(i+1)*db.PositionGap); err != nil {
				return err
			}
		}
	}
	labels := map[string]string{}
	for i, l := range ib.Labels {
		color := l.Color
		if !validColor(color) {
			color = colorFor(l.Name + l.Key)
		}
		// With project-wide labels, reuse a matching project label by name+colour or create one.
		if useProject {
			var id string
			err := tx.QueryRowContext(ctx, `SELECT id FROM labels WHERE project_id=? AND board_id IS NULL AND COALESCE(name,'')=? AND color=?`, projectID, l.Name, color).Scan(&id)
			if err == sql.ErrNoRows {
				id = db.NewID()
				_, err = tx.ExecContext(ctx, `INSERT INTO labels(id,project_id,name,color,position,created_at) VALUES(?,?,?,?,?,?)`, id, projectID, nullStr(l.Name), color,
					appendPos(ctx, tx, "labels", "project_id=? AND board_id IS NULL", projectID), now)
			}
			if err != nil {
				return err
			}
			labels[l.Key] = id
			continue
		}
		id := db.NewID()
		labels[l.Key] = id
		var grp any
		if g, ok := groups[l.Group]; ok {
			grp = g
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO labels(id,board_id,group_id,name,color,position,created_at) VALUES(?,?,?,?,?,?,?)`, id, boardID, grp, nullStr(l.Name), color, float64(i+1)*db.PositionGap, now); err != nil {
			return err
		}
	}
	lists := map[string]string{}
	var first string
	for i, l := range ib.Lists {
		switch l.Type {
		case "archive", "trash":
			id, err := SystemList(ctx, tx, boardID, l.Type)
			if err != nil {
				return err
			}
			lists[l.Key] = id
			continue
		}
		typ := "active"
		if l.Type == "closed" {
			typ = "closed"
		}
		pos := l.Pos
		if pos == 0 {
			pos = float64(i+1) * db.PositionGap
		}
		id := db.NewID()
		lists[l.Key] = id
		if first == "" {
			first = id
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lists(id,board_id,type,position,name,color,created_at) VALUES(?,?,?,?,?,?,?)`, id, boardID, typ, pos, l.Name, nullStr(l.Color), now); err != nil {
			return err
		}
	}
	emails := map[string]string{}
	for ci, c := range ib.Cards {
		lid, ok := lists[c.ListKey]
		if !ok {
			if first == "" {
				first = db.NewID()
				if _, err := tx.ExecContext(ctx, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES(?,?,?,?,?,?)`, first, boardID, "active", db.PositionGap, "Imported", now); err != nil {
					return err
				}
			}
			lid = first
		}
		pos := c.Pos
		if pos == 0 {
			pos = float64(ci+1) * db.PositionGap
		}
		typ := c.Type
		if typ != "story" {
			typ = "project"
		}
		cid := db.NewID()
		var due any
		var done any
		if c.Due != "" {
			due = c.Due
			done = boolInt(c.DueDone)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cards(id,board_id,list_id,creator_user_id,type,position,name,description,due_date,is_due_completed,comments_total,list_changed_at,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, cid, boardID, lid, userOrNil(e), typ, pos, c.Name, nullStr(c.Desc), due, done, len(c.Comments), now, now); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, k := range c.LabelKeys {
			if lab, ok := labels[k]; ok && !seen[lab] {
				seen[lab] = true
				if _, err := tx.ExecContext(ctx, `INSERT INTO card_labels(card_id,label_id) VALUES(?,?)`, cid, lab); err != nil {
					return err
				}
			}
		}
		for _, em := range c.MemberEmails {
			uid, ok := emails[em]
			if !ok {
				_ = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email=? AND deleted_at IS NULL`, em).Scan(&uid)
				emails[em] = uid
			}
			if uid != "" {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO card_memberships(card_id,user_id) VALUES(?,?)`, cid, uid); err != nil {
					return err
				}
			}
		}
		for ti, tl := range c.TaskLists {
			tlid := db.NewID()
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_lists(id,card_id,position,name,created_at) VALUES(?,?,?,?,?)`, tlid, cid, float64(ti+1)*db.PositionGap, nonEmpty(tl.Name, "Tasks"), now); err != nil {
				return err
			}
			for k, t := range tl.Tasks {
				if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,task_list_id,position,name,is_completed,created_at) VALUES(?,?,?,?,?,?)`, db.NewID(), tlid, float64(k+1)*db.PositionGap, t.Name, boolInt(t.Done), now); err != nil {
					return err
				}
			}
		}
		for _, cm := range c.Comments {
			at := nonEmpty(cm.CreatedAt, now)
			if _, err := tx.ExecContext(ctx, `INSERT INTO comments(id,card_id,user_id,text,created_at) VALUES(?,?,?,?,?)`, db.NewID(), cid, userOrNil(e), cm.Text, at); err != nil {
				return err
			}
		}
	}
	return nil
}

func userOrNil(e *ops.Env) any {
	if e.User == nil {
		return nil
	}
	return e.User.UserID
}

func nonEmpty(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func colorFor(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return Colors[int(h.Sum32())%len(Colors)]
}

// ---- parsers ----

func parseWeave(data []byte) (impBoard, error) {
	var d exportDoc
	if err := json.Unmarshal(data, &d); err != nil {
		return impBoard{}, err
	}
	if len(d.Lists) == 0 && len(d.Cards) == 0 {
		return impBoard{}, fmt.Errorf("no lists or cards found")
	}
	ib := impBoard{}
	if n, ok := d.Board["name"].(string); ok {
		ib.Name = n
	}
	// Colours from a file are untrusted: anything that does not validate is dropped, never stored.
	if c, ok := d.Board["color"].(string); ok {
		ib.Color, _ = NormalizeColor(c)
	}
	if m, ok := d.Board["cardColorMode"].(string); ok && validCardColorMode(m) {
		ib.CardColorMode = m
	}
	for _, l := range d.Lists {
		lc, _ := NormalizeColor(l.Color)
		ib.Lists = append(ib.Lists, impList{Key: l.ID, Name: l.Name, Type: l.Type, Color: lc, Pos: l.Position})
	}
	for _, g := range d.Groups {
		ib.Groups = append(ib.Groups, impGroup{Key: g.ID, Name: g.Name, SingleSelect: g.SingleSelect})
	}
	for _, l := range d.Labels {
		ib.Labels = append(ib.Labels, impLabel{Key: l.ID, Name: l.Name, Color: l.Color, Group: l.GroupID})
	}
	for _, c := range d.Cards {
		ic := impCard{ListKey: c.ListID, Name: c.Name, Desc: c.Description, Due: c.DueDate, DueDone: c.IsDueCompleted, Type: c.Type, Pos: c.Position, LabelKeys: c.LabelIDs, MemberEmails: c.MemberEmails}
		for _, tl := range c.TaskLists {
			x := impTaskList{Name: tl.Name}
			for _, t := range tl.Tasks {
				x.Tasks = append(x.Tasks, impTask{t.Name, t.IsCompleted})
			}
			ic.TaskLists = append(ic.TaskLists, x)
		}
		for _, cm := range c.Comments {
			ic.Comments = append(ic.Comments, impComment{cm.Text, cm.CreatedAt})
		}
		ib.Cards = append(ib.Cards, ic)
	}
	return ib, nil
}

var trelloColors = map[string]string{"green": "bright-moss", "yellow": "egg-yellow", "orange": "pumpkin-orange", "red": "berry-red", "purple": "lilac-eyes",
	"blue": "lagoon-blue", "sky": "summer-sky", "lime": "sunny-grass", "pink": "pink-tulip", "black": "dark-granite", "": "muddy-grey", "grey": "grey-stone"}

func parseTrello(data []byte) (impBoard, error) {
	var t struct {
		Name  string `json:"name"`
		Lists []struct {
			ID     string  `json:"id"`
			Name   string  `json:"name"`
			Closed bool    `json:"closed"`
			Pos    float64 `json:"pos"`
		} `json:"lists"`
		Labels []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Color string `json:"color"`
		} `json:"labels"`
		Cards []struct {
			ID       string   `json:"id"`
			IDList   string   `json:"idList"`
			Name     string   `json:"name"`
			Desc     string   `json:"desc"`
			Closed   bool     `json:"closed"`
			Due      string   `json:"due"`
			DueDone  bool     `json:"dueComplete"`
			Pos      float64  `json:"pos"`
			IDLabels []string `json:"idLabels"`
		} `json:"cards"`
		Checklists []struct {
			IDCard     string `json:"idCard"`
			Name       string `json:"name"`
			Pos        float64
			CheckItems []struct {
				Name  string  `json:"name"`
				State string  `json:"state"`
				Pos   float64 `json:"pos"`
			} `json:"checkItems"`
		} `json:"checklists"`
		Actions []struct {
			Type string `json:"type"`
			Date string `json:"date"`
			Data struct {
				Text string `json:"text"`
				Card struct {
					ID string `json:"id"`
				} `json:"card"`
			} `json:"data"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return impBoard{}, err
	}
	if len(t.Lists) == 0 {
		return impBoard{}, fmt.Errorf("not a Trello board export (no lists)")
	}
	ib := impBoard{Name: t.Name}
	sort.SliceStable(t.Lists, func(i, j int) bool { return t.Lists[i].Pos < t.Lists[j].Pos })
	closedList := map[string]bool{}
	for i, l := range t.Lists {
		if l.Closed {
			closedList[l.ID] = true
			continue
		}
		ib.Lists = append(ib.Lists, impList{Key: l.ID, Name: l.Name, Type: "active", Pos: float64(i+1) * db.PositionGap})
	}
	ib.Lists = append(ib.Lists, impList{Key: "__archive", Type: "archive"})
	for _, l := range t.Labels {
		ib.Labels = append(ib.Labels, impLabel{Key: l.ID, Name: l.Name, Color: trelloColors[l.Color]})
	}
	cardIdx := map[string]int{}
	sort.SliceStable(t.Cards, func(i, j int) bool { return t.Cards[i].Pos < t.Cards[j].Pos })
	for i, c := range t.Cards {
		ic := impCard{ListKey: c.IDList, Name: c.Name, Desc: c.Desc, Due: c.Due, DueDone: c.DueDone, Pos: float64(i+1) * db.PositionGap, LabelKeys: c.IDLabels}
		if c.Closed || closedList[c.IDList] {
			ic.ListKey = "__archive"
		}
		cardIdx[c.ID] = len(ib.Cards)
		ib.Cards = append(ib.Cards, ic)
	}
	sort.SliceStable(t.Checklists, func(i, j int) bool { return t.Checklists[i].Pos < t.Checklists[j].Pos })
	for _, cl := range t.Checklists {
		idx, ok := cardIdx[cl.IDCard]
		if !ok {
			continue
		}
		items := cl.CheckItems
		sort.SliceStable(items, func(i, j int) bool { return items[i].Pos < items[j].Pos })
		tl := impTaskList{Name: cl.Name}
		for _, it := range items {
			tl.Tasks = append(tl.Tasks, impTask{it.Name, it.State == "complete"})
		}
		ib.Cards[idx].TaskLists = append(ib.Cards[idx].TaskLists, tl)
	}
	for i := len(t.Actions) - 1; i >= 0; i-- { // Trello lists newest first
		a := t.Actions[i]
		if a.Type != "commentCard" {
			continue
		}
		if idx, ok := cardIdx[a.Data.Card.ID]; ok {
			ib.Cards[idx].Comments = append(ib.Cards[idx].Comments, impComment{a.Data.Text, a.Date})
		}
	}
	return ib, nil
}

func parseCSV(data []byte) (impBoard, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	if err != nil {
		return impBoard{}, err
	}
	if len(rows) < 2 {
		return impBoard{}, fmt.Errorf("CSV needs a header row and at least one card")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	find := func(names ...string) int {
		for _, n := range names {
			if i, ok := col[n]; ok {
				return i
			}
		}
		return -1
	}
	cList, cName, cDesc := find("list", "column", "status"), find("name", "title", "card"), find("description", "desc")
	cDue, cLabels, cMembers, cDone := find("due", "due date", "duedate"), find("labels", "label"), find("members", "assignees"), find("completed", "done")
	if cName < 0 {
		return impBoard{}, fmt.Errorf("CSV needs a name/title column")
	}
	get := func(row []string, i int) string {
		if i < 0 || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	ib := impBoard{}
	lists, labels := map[string]bool{}, map[string]bool{}
	for _, row := range rows[1:] {
		name := get(row, cName)
		if name == "" {
			continue
		}
		ln := nonEmpty(get(row, cList), "Imported")
		if !lists[ln] {
			lists[ln] = true
			ib.Lists = append(ib.Lists, impList{Key: ln, Name: ln, Type: "active", Pos: float64(len(ib.Lists)+1) * db.PositionGap})
		}
		c := impCard{ListKey: ln, Name: name, Desc: get(row, cDesc), Due: normDue(get(row, cDue)), Pos: float64(len(ib.Cards)+1) * db.PositionGap}
		c.DueDone = strings.EqualFold(get(row, cDone), "true") || get(row, cDone) == "1" || strings.EqualFold(get(row, cDone), "yes")
		for _, l := range splitList(get(row, cLabels)) {
			if !labels[l] {
				labels[l] = true
				ib.Labels = append(ib.Labels, impLabel{Key: l, Name: l, Color: colorFor(l)})
			}
			c.LabelKeys = append(c.LabelKeys, l)
		}
		c.MemberEmails = splitList(get(row, cMembers))
		ib.Cards = append(ib.Cards, c)
	}
	if len(ib.Cards) == 0 {
		return ib, fmt.Errorf("no cards found")
	}
	return ib, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' || r == '|' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func normDue(s string) string {
	if s == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02", "01/02/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// ---- export ----

func exportBoard(e *ops.Env, in ExportIn) (*ops.Stream, error) {
	if err := authz.RequireBoard(e, in.ID, authz.View); err != nil {
		return nil, err
	}
	b, err := GetBoard(e.Ctx, e.DB, in.ID)
	if err != nil {
		return nil, err
	}
	doc, err := buildExport(e.Ctx, e.DB, b)
	if err != nil {
		return nil, err
	}
	slug := strings.Map(func(r rune) rune {
		if r == ' ' {
			return '-'
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, b.Name)
	if slug == "" {
		slug = "board"
	}
	switch in.Format {
	case "", "json":
		out, _ := json.MarshalIndent(doc, "", "  ")
		return &ops.Stream{Name: slug + ".json", ContentType: "application/json", Data: out}, nil
	case "csv":
		return &ops.Stream{Name: slug + ".csv", ContentType: "text/csv; charset=utf-8", Data: exportCSV(doc)}, nil
	}
	return nil, ops.Invalid("format must be json or csv")
}

func buildExport(ctx context.Context, d *db.DB, b Board) (*exportDoc, error) {
	doc := &exportDoc{Format: "weave-board", Version: 1, Board: map[string]any{"name": b.Name, "defaultView": b.DefaultView, "defaultCardType": b.DefaultCardType, "cardColorMode": b.CardColorMode}}
	if b.Color != nil {
		doc.Board["color"] = *b.Color
	}
	rows, err := d.QueryContext(ctx, `SELECT id,type,COALESCE(position,0),COALESCE(name,''),COALESCE(color,'') FROM lists WHERE board_id=? ORDER BY position IS NULL, position`, b.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l exportList
		_ = rows.Scan(&l.ID, &l.Type, &l.Position, &l.Name, &l.Color)
		doc.Lists = append(doc.Lists, l)
	}
	rows.Close()
	rows, err = d.QueryContext(ctx, `SELECT id,name,single_select FROM label_groups WHERE board_id=? ORDER BY position`, b.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g exportGroup
		_ = rows.Scan(&g.ID, &g.Name, &g.SingleSelect)
		doc.Groups = append(doc.Groups, g)
	}
	rows.Close()
	rows, err = d.QueryContext(ctx, `SELECT id,COALESCE(name,''),color,COALESCE(group_id,'') FROM labels WHERE board_id=? OR id IN
		(SELECT cl.label_id FROM card_labels cl JOIN cards c ON c.id=cl.card_id WHERE c.board_id=?) ORDER BY position`, b.ID, b.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l exportLabel
		_ = rows.Scan(&l.ID, &l.Name, &l.Color, &l.GroupID)
		doc.Labels = append(doc.Labels, l)
	}
	rows.Close()
	rows, err = d.QueryContext(ctx, `SELECT id,list_id,name,COALESCE(description,''),type,COALESCE(due_date,''),COALESCE(is_due_completed,0),COALESCE(position,0)
		FROM cards WHERE board_id=? AND is_draft=0 AND is_template=0 ORDER BY position`, b.ID)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for rows.Next() {
		var c exportCard
		_ = rows.Scan(&c.ID, &c.ListID, &c.Name, &c.Description, &c.Type, &c.DueDate, &c.IsDueCompleted, &c.Position)
		idx[c.ID] = len(doc.Cards)
		doc.Cards = append(doc.Cards, c)
	}
	rows.Close()
	each := func(q string, fn func(id, a, b string, c bool)) error {
		rs, err := d.QueryContext(ctx, q, b.ID)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var id, a, bb string
			var c bool
			_ = rs.Scan(&id, &a, &bb, &c)
			fn(id, a, bb, c)
		}
		return rs.Err()
	}
	if err := each(`SELECT cl.card_id,cl.label_id,'',0 FROM card_labels cl JOIN cards c ON c.id=cl.card_id WHERE c.board_id=?`, func(id, a, _ string, _ bool) {
		if i, ok := idx[id]; ok {
			doc.Cards[i].LabelIDs = append(doc.Cards[i].LabelIDs, a)
		}
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT cm.card_id,u.email,'',0 FROM card_memberships cm JOIN users u ON u.id=cm.user_id JOIN cards c ON c.id=cm.card_id WHERE c.board_id=?`, func(id, a, _ string, _ bool) {
		if i, ok := idx[id]; ok {
			doc.Cards[i].MemberEmails = append(doc.Cards[i].MemberEmails, a)
		}
	}); err != nil {
		return nil, err
	}
	tlIdx := map[string][2]int{}
	if err := each(`SELECT tl.card_id,tl.id,tl.name,0 FROM task_lists tl JOIN cards c ON c.id=tl.card_id WHERE c.board_id=? ORDER BY tl.position`, func(id, tlid, name string, _ bool) {
		if i, ok := idx[id]; ok {
			doc.Cards[i].TaskLists = append(doc.Cards[i].TaskLists, exportTaskList{Name: name})
			tlIdx[tlid] = [2]int{i, len(doc.Cards[i].TaskLists) - 1}
		}
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT t.task_list_id,t.name,'',t.is_completed FROM tasks t JOIN task_lists tl ON tl.id=t.task_list_id JOIN cards c ON c.id=tl.card_id WHERE c.board_id=? ORDER BY t.position`, func(tlid, name, _ string, done bool) {
		if p, ok := tlIdx[tlid]; ok {
			tl := &doc.Cards[p[0]].TaskLists[p[1]]
			tl.Tasks = append(tl.Tasks, exportTask{name, done})
		}
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT cm.card_id,cm.text,cm.created_at,0 FROM comments cm JOIN cards c ON c.id=cm.card_id WHERE c.board_id=? ORDER BY cm.created_at`, func(id, text, at string, _ bool) {
		if i, ok := idx[id]; ok {
			doc.Cards[i].Comments = append(doc.Cards[i].Comments, exportComment{Text: text, CreatedAt: at})
		}
	}); err != nil {
		return nil, err
	}
	return doc, nil
}

func exportCSV(doc *exportDoc) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	listName := map[string]string{}
	for _, l := range doc.Lists {
		n := l.Name
		if n == "" {
			n = strings.ToUpper(l.Type[:1]) + l.Type[1:]
		}
		listName[l.ID] = n
	}
	labelName := map[string]string{}
	for _, l := range doc.Labels {
		labelName[l.ID] = nonEmpty(l.Name, l.Color)
	}
	_ = w.Write([]string{"list", "name", "description", "due", "completed", "labels", "members"})
	for _, c := range doc.Cards {
		var ls []string
		for _, id := range c.LabelIDs {
			ls = append(ls, labelName[id])
		}
		done := ""
		if c.IsDueCompleted {
			done = "true"
		}
		_ = w.Write([]string{listName[c.ListID], c.Name, c.Description, c.DueDate, done, strings.Join(ls, ";"), strings.Join(c.MemberEmails, ";")})
	}
	w.Flush()
	_, _ = io.Discard.Write(nil)
	return buf.Bytes()
}
