package boards

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/crueber/weave/internal/agentprompt"
	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// ---- intermediate representation shared by all importers ----

type impList struct {
	Key, Name, Type, Color, Desc string
	Pos                          float64
	WIP                          int
	ArchiveAfter                 int // auto-archive (0 = off)
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
	Name                  string
	Tasks                 []impTask
	ShowOnFront, HideDone bool
}
type impComment struct {
	Text, CreatedAt string
}
type impCard struct {
	Key                      string // id inside the document; custom-field values are addressed by it
	ListKey, Name, Desc, Due string
	DueDone                  bool
	Closed                   bool   // closed in the file; a card landing in a closed-type column is closed regardless
	ClosedAt                 string // optional RFC3339 UTC timestamp from export
	DueOffsetDays            *int   // relative due date (days from the moment the board is created); wins over Due
	Pos                      float64
	LabelKeys                []string
	MemberEmails             []string
	TaskLists                []impTaskList
	Comments                 []impComment
	Values                   []FieldValueDoc
}
type impBoard struct {
	Name              string
	Color             string // normalised #rrggbb or ""
	BackgroundPattern string // validated pattern slug or ""
	CardColorMode     string // pill|fill or ""
	AgentPrompt   string // the board's own agent prompt text (normalised), "" = default
	View          string // default view; "" = the board view (stored as kanban)
	Flags         impFlags
	Stated        map[string]bool // settings the document actually names (board.template.apply only applies those)
	Lists         []impList
	Groups        []impGroup
	Labels        []impLabel
	Cards         []impCard
	FieldGroups   []FieldGroupDoc
}

// impFlags are the board's on/off settings (they travel with exports and templates).
type impFlags struct {
	AlwaysCreator, ExpandTasks, CardCounter, CardAges, Inbox bool
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
	// CustomFields are the board's custom-field groups (additive: files without them import as before).
	CustomFields []FieldGroupDoc `json:"customFields,omitempty"`
}
type exportList struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	Color        string  `json:"color,omitempty"`
	Description  string  `json:"description,omitempty"`
	WIPLimit     int     `json:"wipLimit,omitempty"`
	ArchiveAfter int     `json:"archiveAfter,omitempty"`
	Position     float64 `json:"position"`
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
	Name          string       `json:"name"`
	Tasks         []exportTask `json:"tasks"`
	ShowOnFront   bool         `json:"showOnFrontOfCard,omitempty"`
	HideCompleted bool         `json:"hideCompletedTasks,omitempty"`
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
	DueDate        string           `json:"dueDate,omitempty"`
	DueOffsetDays  *int             `json:"dueOffsetDays,omitempty"`
	IsDueCompleted bool             `json:"isDueCompleted,omitempty"`
	IsClosed       bool             `json:"isClosed,omitempty"`
	ClosedAt       string           `json:"closedAt,omitempty"`
	Position       float64          `json:"position"`
	LabelIDs       []string         `json:"labelIds,omitempty"`
	MemberEmails   []string         `json:"memberEmails,omitempty"`
	TaskLists      []exportTaskList `json:"taskLists,omitempty"`
	Comments       []exportComment  `json:"comments,omitempty"`
	CustomValues   []FieldValueDoc  `json:"customFieldValues,omitempty"`
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
	ops.Register(ops.Op{Name: "board.import", Summary: "Create a board from a Planka/Weave JSON export, a Trello JSON export or a CSV (whoever may create boards in the project, see board.create)", Method: "POST", MaxBody: 64 << 20, Path: "/projects/{projectId}/boards/import",
		UI: ui("POST", "/projects/{projectId}/boards/import")}, importBoard)
	ops.Register(ops.Op{Name: "board.export", Summary: "Export a board as JSON (round-trips through board.import) or CSV", Method: "GET", Path: "/boards/{id}/export",
		Binary: true, UI: ui("GET", "/boards/{id}/export")}, exportBoard)
}

func importBoard(e *ops.Env, in ImportIn) (Board, error) {
	if err := authz.RequireCreateBoard(e, in.ProjectID); err != nil {
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
	if err := ops.CheckLen("name", name, ops.MaxNameLen); err != nil {
		return Board{}, err
	}
	return createImported(e, in.ProjectID, ib, importOpts{Name: name, Source: in.Format, Event: map[string]any{"imported": true}})
}

// importOpts tunes createImported.
type importOpts struct {
	Name   string
	View   string         // overrides the document's default view when valid
	Source string         // recorded in board_imports; empty for built-in and published templates
	Event  map[string]any // extra fields for the board.created event
}

// createImported is the one place that turns a parsed board document into a new board: file imports, built-in
// templates and published templates all go through it, so labels, colours and limits are validated the same way.
func createImported(e *ops.Env, projectID string, ib impBoard, o importOpts) (Board, error) {
	view := "kanban"
	if validView(ib.View) {
		view = ib.View
	}
	if validView(o.View) {
		view = o.View
	}
	id := db.NewID()
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		if err := insertBoard(e, tx, id, projectID, o.Name, view, nil); err != nil {
			return err
		}
		if o.Source != "" {
			if _, err := tx.ExecContext(e.Ctx, `INSERT INTO board_imports(id,user_id,source,status,created_at) VALUES(?,?,?,?,?)`, db.NewID(), strOrEmpty(userOrNil(e)), o.Source, "done", db.Now()); err != nil {
				return err
			}
		}
		return writeImport(e, tx, id, projectID, ib, time.Now().UTC())
	})
	if err != nil {
		return Board{}, err
	}
	data := map[string]any{"id": id, "projectId": projectID}
	for k, v := range o.Event {
		data[k] = v
	}
	emit(e, "boardCreate", "board.created", id, data)
	e.Hub.Publish(projectEvent("board.created", projectID, id))
	return GetBoard(e.Ctx, e.DB, id)
}

func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}

// dueFromOffset turns a relative due date (days from now) into the stored timestamp; offsets outside +-10 years are dropped.
func dueFromOffset(now time.Time, days *int) (string, bool) {
	if days == nil || *days < -3650 || *days > 3650 {
		return "", false
	}
	return now.AddDate(0, 0, *days).UTC().Format(time.RFC3339), true
}

// writeImport fills a freshly inserted board from ib. now anchors relative due dates (dueOffsetDays).
//
// It is composed of the parts board.template.apply reuses on an existing board (see templateapply.go): the board
// settings, the labels with their groups, the columns, the cards and the custom-field groups. The composition here is
// the import/instantiate behaviour; the apply path picks other options for labels and settings.
func writeImport(e *ops.Env, tx *sql.Tx, boardID, projectID string, ib impBoard, nowT time.Time) error {
	ctx, now := e.Ctx, db.Now()
	if err := writeImportSettings(ctx, tx, boardID, ib); err != nil {
		return err
	}
	labels, err := writeImportLabels(ctx, tx, boardID, projectID, ib, now)
	if err != nil {
		return err
	}
	wl, err := writeLists(ctx, tx, boardID, ib.Lists, now, func(i int, l impList) float64 {
		if l.Pos != 0 {
			return l.Pos
		}
		return float64(i+1) * db.PositionGap
	})
	if err != nil {
		return err
	}
	wc, err := writeCards(e, tx, boardID, ib.Cards, wl, labels, nowT, now, nil)
	if err != nil {
		return err
	}
	// The file's closed state is kept (a card closed in place, an archived closed card); cards that land in a
	// closed-type column are closed whatever the file says. Use LandedKeepClosed here so closed+capped columns are
	// not trimmed twice; the unique loop over wl.order below trims every capped column once.
	if _, err := LandedKeepClosed(ctx, tx, wc.closedLands...); err != nil {
		return err
	}
	// A column with auto-archive keeps its newest N cards from the start (all cards arrive at once: the top of the
	// column counts as oldest). The board is new, so nobody watches it yet: no per-card events are sent.
	for _, id := range wl.order {
		if n, ok := wl.capped[id]; ok {
			if _, err := trimColumn(ctx, tx, id, n, nil); err != nil {
				return err
			}
		}
	}
	if len(ib.FieldGroups) > 0 && ImportCustomFields != nil {
		if err := ImportCustomFields(ctx, tx, boardID, ib.FieldGroups, wc.values, wc.ids); err != nil {
			return err
		}
	}
	return nil
}

// writeImportSettings writes every board setting of the document, the agent prompt included (a new board).
func writeImportSettings(ctx context.Context, tx *sql.Tx, boardID string, ib impBoard) error {
	mode := ib.CardColorMode
	if mode == "" {
		mode = CardColorFill
	}
	f := ib.Flags
	_, err := tx.ExecContext(ctx, `UPDATE boards SET color=?,background_pattern=?,card_color_mode=?,always_display_card_creator=?,
		expand_task_lists_by_default=?,show_card_counter=?,show_card_ages=?,has_inbox=?,agent_prompt=? WHERE id=?`, nullStr(ib.Color), nullStr(ib.BackgroundPattern), mode,
		boolInt(f.AlwaysCreator), boolInt(f.ExpandTasks), boolInt(f.CardCounter), boolInt(f.CardAges), boolInt(f.Inbox), nullStr(ib.AgentPrompt), boardID)
	return err
}

// importLabelColor is the stored colour of a document label: its own when valid, else one derived from its name.
func importLabelColor(l impLabel) string {
	if validColor(l.Color) {
		return l.Color
	}
	return colorFor(l.Name + l.Key)
}

// writeImportLabels creates the document's label groups and labels on a new board and returns document key -> label
// id. With project-wide labels, a project label with the same name and colour is reused, else one is created (and no
// groups are made).
func writeImportLabels(ctx context.Context, tx *sql.Tx, boardID, projectID string, ib impBoard, now string) (map[string]string, error) {
	var useProject bool
	_ = tx.QueryRowContext(ctx, `SELECT use_project_labels FROM projects WHERE id=?`, projectID).Scan(&useProject)
	groups := map[string]string{}
	if !useProject {
		for i, g := range ib.Groups {
			id := db.NewID()
			groups[g.Key] = id
			if _, err := tx.ExecContext(ctx, `INSERT INTO label_groups(id,board_id,name,single_select,position) VALUES(?,?,?,?,?)`, id, boardID, g.Name, boolInt(g.SingleSelect), float64(i+1)*db.PositionGap); err != nil {
				return nil, err
			}
		}
	}
	labels := map[string]string{}
	var projPos float64
	if useProject && len(ib.Labels) > 0 {
		projPos = maxPos(ctx, tx, "labels", "project_id=? AND board_id IS NULL", projectID)
	}
	for i, l := range ib.Labels {
		color := importLabelColor(l)
		// With project-wide labels, reuse a matching project label by name+colour or create one.
		if useProject {
			var id string
			err := tx.QueryRowContext(ctx, `SELECT id FROM labels WHERE project_id=? AND board_id IS NULL AND COALESCE(name,'')=? AND color=?`, projectID, l.Name, color).Scan(&id)
			if err == sql.ErrNoRows {
				id = db.NewID()
				projPos += db.PositionGap
				_, err = tx.ExecContext(ctx, `INSERT INTO labels(id,project_id,name,color,position,created_at) VALUES(?,?,?,?,?,?)`, id, projectID, nullStr(l.Name), color, projPos, now)
			}
			if err != nil {
				return nil, err
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
			return nil, err
		}
	}
	return labels, nil
}

// writtenLists is what writeLists created: document key -> list id, which of them close cards, their archive-after
// setting, the first finite column (the fallback for cards whose column is unknown) and the finite ids in order.
type writtenLists struct {
	ids    map[string]string
	closed map[string]bool
	capped map[string]int
	first  string
	order  []string
}

// writeLists inserts the document's columns at pos(i, list). Archive and trash entries map to the board's system
// lists (created on demand).
func writeLists(ctx context.Context, tx *sql.Tx, boardID string, lists []impList, now string, pos func(int, impList) float64) (*writtenLists, error) {
	w := &writtenLists{ids: map[string]string{}, closed: map[string]bool{}, capped: map[string]int{}}
	for i, l := range lists {
		switch l.Type {
		case "archive", "trash":
			id, err := SystemList(ctx, tx, boardID, l.Type)
			if err != nil {
				return nil, err
			}
			w.ids[l.Key] = id
			continue
		}
		typ := "active"
		if l.Type == "closed" {
			typ = "closed"
		}
		id := db.NewID()
		w.ids[l.Key] = id
		w.closed[id] = typ == "closed"
		w.order = append(w.order, id)
		if w.first == "" {
			w.first = id
		}
		keep := importWIP(l.ArchiveAfter) // same bounds as the WIP limit (1-10000); out of range = off
		if keep != nil {
			w.capped[id] = keep.(int)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lists(id,board_id,type,position,name,color,description,wip_limit,archive_after,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, boardID, typ, pos(i, l), l.Name, nullStr(l.Color), nullStr(l.Desc), importWIP(l.WIP), keep, now); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// writtenCards is what writeCards created: every card with its column (lands), the ones in closing columns
// (closedLands), document key -> card id, and the custom-field values to store per document key.
type writtenCards struct {
	lands       []Landing
	closedLands []Landing
	ids         map[string]string
	values      map[string][]FieldValueDoc
}

// writeCards inserts the document's cards with their labels, members (matched by e-mail), task lists and comments.
// A card whose column is unknown goes to w.first (an "Imported" column is created when the board has none). base
// offsets the positions per column (nil = none: a new board).
func writeCards(e *ops.Env, tx *sql.Tx, boardID string, cards []impCard, w *writtenLists, labels map[string]string, nowT time.Time, now string, base map[string]float64) (writtenCards, error) {
	ctx := e.Ctx
	out := writtenCards{ids: map[string]string{}, values: map[string][]FieldValueDoc{}}
	emails := map[string]string{}
	// When appending onto columns that already hold cards (base != nil), shift any column whose template cards have
	// non-positive positions so they always land strictly after base[lid] while preserving their relative order.
	var shift map[string]float64
	if base != nil {
		minPos := map[string]float64{}
		hasMin := map[string]bool{}
		for ci, c := range cards {
			lid, ok := w.ids[c.ListKey]
			if !ok {
				lid = w.first
			}
			pos := c.Pos
			if pos == 0 {
				pos = float64(ci+1) * db.PositionGap
			}
			if !hasMin[lid] || pos < minPos[lid] {
				minPos[lid], hasMin[lid] = pos, true
			}
		}
		shift = map[string]float64{}
		for lid, mp := range minPos {
			if mp <= 0 {
				shift[lid] = -mp + db.PositionGap
			}
		}
	}
	for ci, c := range cards {
		lid, ok := w.ids[c.ListKey]
		if !ok {
			if w.first == "" {
				w.first = db.NewID()
				if _, err := tx.ExecContext(ctx, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES(?,?,?,?,?,?)`, w.first, boardID, "active", db.PositionGap, "Imported", now); err != nil {
					return out, err
				}
			}
			lid = w.first
		}
		pos := c.Pos
		if pos == 0 {
			pos = float64(ci+1) * db.PositionGap
		}
		if base != nil {
			pos += shift[lid] + base[lid]
		}
		cid := db.NewID()
		var due any
		var done any
		if d, ok := dueFromOffset(nowT, c.DueOffsetDays); ok {
			due, done = d, 0
		} else if c.Due != "" {
			due = c.Due
			done = boolInt(c.DueDone)
		}
		var closedAt any
		if c.Closed {
			if c.ClosedAt != "" {
				closedAt = c.ClosedAt
			} else {
				closedAt = now
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cards(id,board_id,list_id,creator_user_id,position,name,description,due_date,is_due_completed,is_closed,closed_at,comments_total,list_changed_at,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, cid, boardID, lid, userOrNil(e), pos, c.Name, nullStr(c.Desc), due, done, boolInt(c.Closed), closedAt, len(c.Comments), now, now); err != nil {
			return out, err
		}
		out.lands = append(out.lands, Landing{CardID: cid, ListID: lid})
		if w.closed[lid] {
			out.closedLands = append(out.closedLands, Landing{CardID: cid, ListID: lid})
		}
		if c.Key != "" {
			out.ids[c.Key] = cid
			if len(c.Values) > 0 {
				out.values[c.Key] = c.Values
			}
		}
		seen := map[string]bool{}
		for _, k := range c.LabelKeys {
			if lab, ok := labels[k]; ok && !seen[lab] {
				seen[lab] = true
				if _, err := tx.ExecContext(ctx, `INSERT INTO card_labels(card_id,label_id) VALUES(?,?)`, cid, lab); err != nil {
					return out, err
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
					return out, err
				}
			}
		}
		for ti, tl := range c.TaskLists {
			tlid := db.NewID()
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_lists(id,card_id,position,name,show_on_front_of_card,hide_completed_tasks,created_at) VALUES(?,?,?,?,?,?,?)`,
				tlid, cid, float64(ti+1)*db.PositionGap, nonEmpty(tl.Name, "Tasks"), boolInt(tl.ShowOnFront), boolInt(tl.HideDone), now); err != nil {
				return out, err
			}
			for k, t := range tl.Tasks {
				if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,task_list_id,position,name,is_completed,created_at) VALUES(?,?,?,?,?,?)`, db.NewID(), tlid, float64(k+1)*db.PositionGap, t.Name, boolInt(t.Done), now); err != nil {
					return out, err
				}
			}
		}
		for _, cm := range c.Comments {
			at := nonEmpty(cm.CreatedAt, now)
			if _, err := tx.ExecContext(ctx, `INSERT INTO comments(id,card_id,user_id,text,created_at) VALUES(?,?,?,?,?)`, db.NewID(), cid, userOrNil(e), cm.Text, at); err != nil {
				return out, err
			}
		}
	}
	return out, nil
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
	return impFromDoc(&d), nil
}

// impFromDoc converts a weave-board document into the importer's representation. Everything in it is untrusted.
func impFromDoc(d *exportDoc) impBoard {
	ib := impBoard{FieldGroups: d.CustomFields}
	str := func(k string) string { s, _ := d.Board[k].(string); return s }
	flag := func(k string) bool { b, _ := d.Board[k].(bool); return b }
	// A file that does not say (templates, Trello/CSV, old exports) gets the default: card counters on.
	flagOn := func(k string) bool { b, ok := d.Board[k].(bool); return b || !ok }
	ib.Name = str("name")
	// Colours from a file are untrusted: anything that does not validate is dropped, never stored.
	ib.Color, _ = NormalizeColor(str("color"))
	ib.BackgroundPattern, _ = NormalizePattern(str("backgroundPattern"))
	if m := str("cardColorMode"); validCardColorMode(m) {
		ib.CardColorMode = m
	}
	// The board's agent prompt travels as text (cleaned and capped like a manual edit; too long = dropped).
	if t, err := agentprompt.Normalize(str("agentPrompt"), agentprompt.Default(agentprompt.ScopeBoard)); err == nil {
		ib.AgentPrompt = t
	}
	if v := str("defaultView"); validView(v) {
		ib.View = v
	}
	// Older exports also carry defaultCardType/limitCardTypesToDefault (and cards a "type"): card types were
	// removed, so those keys are read past and ignored.
	ib.Stated = map[string]bool{}
	for k := range d.Board {
		ib.Stated[k] = true
	}
	ib.Flags = impFlags{AlwaysCreator: flag("alwaysDisplayCardCreator"), ExpandTasks: flag("expandTaskListsByDefault"),
		CardCounter: flagOn("showCardCounter"), CardAges: flag("showCardAges"), Inbox: flag("hasInbox")}
	for _, l := range d.Lists {
		lc, _ := NormalizeColor(l.Color)
		ib.Lists = append(ib.Lists, impList{Key: l.ID, Name: l.Name, Type: l.Type, Color: lc, Desc: l.Description, Pos: l.Position, WIP: l.WIPLimit, ArchiveAfter: l.ArchiveAfter})
	}
	for _, g := range d.Groups {
		ib.Groups = append(ib.Groups, impGroup{Key: g.ID, Name: g.Name, SingleSelect: g.SingleSelect})
	}
	for _, l := range d.Labels {
		ib.Labels = append(ib.Labels, impLabel{Key: l.ID, Name: l.Name, Color: l.Color, Group: l.GroupID})
	}
	for _, c := range d.Cards {
		ic := impCard{Key: c.ID, ListKey: c.ListID, Name: c.Name, Desc: c.Description, Due: c.DueDate, DueDone: c.IsDueCompleted, Closed: c.IsClosed, ClosedAt: c.ClosedAt, DueOffsetDays: c.DueOffsetDays, Pos: c.Position,
			LabelKeys: c.LabelIDs, MemberEmails: c.MemberEmails, Values: c.CustomValues}
		for _, tl := range c.TaskLists {
			x := impTaskList{Name: tl.Name, ShowOnFront: tl.ShowOnFront, HideDone: tl.HideCompleted}
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
	return ib
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
	ib := impBoard{Name: t.Name, Flags: impFlags{CardCounter: true}}
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
	ib := impBoard{Flags: impFlags{CardCounter: true}}
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
	doc, err := buildExport(e.Ctx, e.DB, b, exportOpts{includeCards: true, includeActivity: true})
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

// exportOpts controls which card-level data buildExport loads. Templates skip activity (members/comments) and, when
// sample cards are not requested, skip cards/tasks/values altogether.
type exportOpts struct {
	includeCards    bool
	includeActivity bool
}

func buildExport(ctx context.Context, d querier, b Board, opts exportOpts) (*exportDoc, error) {
	doc := &exportDoc{Format: "weave-board", Version: 1, Board: map[string]any{"name": b.Name, "defaultView": b.DefaultView, "cardColorMode": b.CardColorMode,
		"alwaysDisplayCardCreator": b.AlwaysDisplayCardCreator, "expandTaskListsByDefault": b.ExpandTaskListsByDefault,
		"showCardCounter": b.ShowCardCounter, "showCardAges": b.ShowCardAges, "hasInbox": b.HasInbox}}
	if b.Color != nil {
		doc.Board["color"] = *b.Color
	}
	if b.BackgroundPattern != nil && ValidPattern(*b.BackgroundPattern) {
		doc.Board["backgroundPattern"] = *b.BackgroundPattern
	}
	if b.AgentPrompt != "" {
		doc.Board["agentPrompt"] = b.AgentPrompt
	}
	rows, err := d.QueryContext(ctx, `SELECT id,type,COALESCE(position,0),COALESCE(name,''),COALESCE(color,''),COALESCE(description,''),COALESCE(wip_limit,0),COALESCE(archive_after,0) FROM lists WHERE board_id=? ORDER BY position IS NULL, position`, b.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l exportList
		if err := rows.Scan(&l.ID, &l.Type, &l.Position, &l.Name, &l.Color, &l.Description, &l.WIPLimit, &l.ArchiveAfter); err != nil {
			rows.Close()
			return nil, err
		}
		doc.Lists = append(doc.Lists, l)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = d.QueryContext(ctx, `SELECT id,name,single_select FROM label_groups WHERE board_id=? ORDER BY position`, b.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g exportGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.SingleSelect); err != nil {
			rows.Close()
			return nil, err
		}
		doc.Groups = append(doc.Groups, g)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = d.QueryContext(ctx, `SELECT l.id,COALESCE(l.name,''),l.color,COALESCE(l.group_id,'') FROM labels l WHERE l.board_id=?1 OR (l.id IN
		(SELECT cl.label_id FROM card_labels cl JOIN card_placements p ON p.card_id=cl.card_id WHERE p.board_id=?1) AND `+db.LabelOnBoard("l", "?1")+`) ORDER BY l.position`, b.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l exportLabel
		if err := rows.Scan(&l.ID, &l.Name, &l.Color, &l.GroupID); err != nil {
			rows.Close()
			return nil, err
		}
		doc.Labels = append(doc.Labels, l)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	idx := map[string]int{}
	if opts.includeCards {
		rows, err = d.QueryContext(ctx, `SELECT c.id,p.list_id,c.name,COALESCE(c.description,''),COALESCE(c.due_date,''),COALESCE(c.is_due_completed,0),c.is_closed,COALESCE(c.closed_at,''),COALESCE(p.position,0)
			FROM card_placements p JOIN cards c ON c.id=p.card_id WHERE p.board_id=? AND c.is_draft=0 AND c.is_template=0 ORDER BY p.position, c.id`, b.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c exportCard
			if err := rows.Scan(&c.ID, &c.ListID, &c.Name, &c.Description, &c.DueDate, &c.IsDueCompleted, &c.IsClosed, &c.ClosedAt, &c.Position); err != nil {
				rows.Close()
				return nil, err
			}
			idx[c.ID] = len(doc.Cards)
			doc.Cards = append(doc.Cards, c)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		each := func(q string, fn func(id, a, b string, c bool)) error {
			rs, err := d.QueryContext(ctx, q, b.ID)
			if err != nil {
				return err
			}
			defer rs.Close()
			for rs.Next() {
				var id, a, bb string
				var c bool
				if err := rs.Scan(&id, &a, &bb, &c); err != nil {
					return err
				}
				fn(id, a, bb, c)
			}
			return rs.Err()
		}
		if err := each(`SELECT cl.card_id,cl.label_id,'',0 FROM card_labels cl JOIN labels l ON l.id=cl.label_id JOIN card_placements p ON p.card_id=cl.card_id
			WHERE p.board_id=?1 AND `+db.LabelOnBoard("l", "?1")+` ORDER BY cl.card_id, cl.label_id`, func(id, a, _ string, _ bool) {
			if i, ok := idx[id]; ok {
				doc.Cards[i].LabelIDs = append(doc.Cards[i].LabelIDs, a)
			}
		}); err != nil {
			return nil, err
		}
		if opts.includeActivity {
			if err := each(`SELECT cm.card_id,u.email,'',0 FROM card_memberships cm JOIN users u ON u.id=cm.user_id JOIN card_placements p ON p.card_id=cm.card_id WHERE p.board_id=?`, func(id, a, _ string, _ bool) {
				if i, ok := idx[id]; ok {
					doc.Cards[i].MemberEmails = append(doc.Cards[i].MemberEmails, a)
				}
			}); err != nil {
				return nil, err
			}
		}
		tlIdx := map[string][2]int{}
		tlRows, err := d.QueryContext(ctx, `SELECT tl.card_id,tl.id,tl.name,tl.show_on_front_of_card,tl.hide_completed_tasks FROM task_lists tl JOIN card_placements p ON p.card_id=tl.card_id WHERE p.board_id=? ORDER BY tl.position`, b.ID)
		if err != nil {
			return nil, err
		}
		for tlRows.Next() {
			var cardID, tlid, name string
			var front, hide bool
			if err := tlRows.Scan(&cardID, &tlid, &name, &front, &hide); err != nil {
				tlRows.Close()
				return nil, err
			}
			if i, ok := idx[cardID]; ok {
				doc.Cards[i].TaskLists = append(doc.Cards[i].TaskLists, exportTaskList{Name: name, ShowOnFront: front, HideCompleted: hide})
				tlIdx[tlid] = [2]int{i, len(doc.Cards[i].TaskLists) - 1}
			}
		}
		if err := tlRows.Close(); err != nil {
			return nil, err
		}
		if err := tlRows.Err(); err != nil {
			return nil, err
		}
		if err := each(`SELECT t.task_list_id,t.name,'',t.is_completed FROM tasks t JOIN task_lists tl ON tl.id=t.task_list_id JOIN card_placements p ON p.card_id=tl.card_id WHERE p.board_id=? ORDER BY t.position`, func(tlid, name, _ string, done bool) {
			if p, ok := tlIdx[tlid]; ok {
				tl := &doc.Cards[p[0]].TaskLists[p[1]]
				tl.Tasks = append(tl.Tasks, exportTask{name, done})
			}
		}); err != nil {
			return nil, err
		}
		if opts.includeActivity {
			if err := each(`SELECT cm.card_id,cm.text,cm.created_at,0 FROM comments cm JOIN card_placements p ON p.card_id=cm.card_id WHERE p.board_id=? ORDER BY cm.created_at`, func(id, text, at string, _ bool) {
				if i, ok := idx[id]; ok {
					doc.Cards[i].Comments = append(doc.Cards[i].Comments, exportComment{Text: text, CreatedAt: at})
				}
			}); err != nil {
				return nil, err
			}
		}
	}
	if ExportCustomFields != nil {
		groups, values, err := ExportCustomFields(ctx, d, b.ID, opts.includeCards)
		if err != nil {
			return nil, err
		}
		doc.CustomFields = groups
		for id, vs := range values {
			if i, ok := idx[id]; ok {
				doc.Cards[i].CustomValues = vs
			}
		}
	}
	return doc, nil
}

func exportCSV(doc *exportDoc) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	listName := map[string]string{}
	for _, l := range doc.Lists {
		n := l.Name
		if n == "" && len(l.Type) > 0 {
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
	return buf.Bytes()
}

// importWIP validates a limit read from a file (untrusted): out-of-range values are dropped. It serves the WIP limit
// and the auto-archive setting (both 1-10000; MaxWIPLimit == MaxArchiveAfter).
func importWIP(n int) any {
	if n < 1 || n > MaxWIPLimit {
		return nil
	}
	return n
}
