package cards

import (
	"database/sql"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// DuplicateHooks let other domains (cardextras: tasks, attachments, custom fields)
// copy their data when a card is cloned (duplicate, from-template, recurrence).
// They run inside the clone transaction.
var DuplicateHooks []func(e *ops.Env, tx *sql.Tx, srcID, dstID string) error

type cloneOpts struct {
	Name       string
	ListID     string
	Position   float64
	DueDate    *string
	Recurrence *string
	IsTemplate bool
	IsDraft    bool
	TemplateOf *string // template_project_id
	SkipExtras bool
}

// cloneCard copies a card into a list inside tx and returns the new id.
func cloneCard(e *ops.Env, tx *sql.Tx, src Card, o cloneOpts) (string, error) {
	boardID, ltype, err := listRef(e.Ctx, tx, o.ListID)
	if err != nil {
		return "", err
	}
	id, now := db.NewID(), db.Now()
	var creator any
	if e.User != nil {
		creator = e.User.UserID
	} else if src.CreatorUserID != nil {
		creator = *src.CreatorUserID
	}
	_, err = tx.ExecContext(e.Ctx, `INSERT INTO cards(id,board_id,list_id,creator_user_id,type,position,name,description,due_date,is_closed,is_draft,is_template,
		lat,lng,location_name,recurrence,list_changed_at,created_at,template_project_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, boardID, o.ListID, creator, src.Type, o.Position, o.Name, src.Description, o.DueDate, ltype == "closed", o.IsDraft, o.IsTemplate,
		src.Lat, src.Lng, src.LocationName, o.Recurrence, now, now, o.TemplateOf)
	if err != nil {
		return "", err
	}
	for _, l := range src.LabelIDs {
		if labelAllowed(e.Ctx, tx, boardID, l) {
			_, _ = tx.ExecContext(e.Ctx, `INSERT OR IGNORE INTO card_labels(card_id,label_id) VALUES(?,?)`, id, l)
		}
	}
	members := boardMemberIDs(e.Ctx, tx, boardID)
	for _, u := range src.MemberIDs {
		if members[u] {
			_, _ = tx.ExecContext(e.Ctx, `INSERT OR IGNORE INTO card_memberships(card_id,user_id) VALUES(?,?)`, id, u)
		}
	}
	if !o.SkipExtras {
		for _, h := range DuplicateHooks {
			if err := h(e, tx, src.ID, id); err != nil {
				return "", err
			}
		}
	}
	return id, nil
}

func cardDuplicate(e *ops.Env, in DuplicateIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	src, err := loadCard(e, e.DB, in.ID)
	if err != nil {
		return src, err
	}
	if src.IsTemplate {
		return src, ops.Invalid("use card.fromTemplate for templates")
	}
	name := in.Name
	if name == "" {
		name = src.Name + " (copy)"
	}
	var newID string
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		after := ""
		if !src.IsDraft {
			after = src.ID
		}
		pos := positionFor(e.Ctx, tx, src.ListID, "", after, "", nil)
		var err error
		newID, err = cloneCard(e, tx, src, cloneOpts{Name: name, ListID: src.ListID, Position: pos, DueDate: src.DueDate, Recurrence: src.Recurrence, IsDraft: src.IsDraft})
		return err
	})
	if err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, newID)
	if err == nil {
		emit(e, "cardCreate", "card.created", boardID, newID, map[string]any{"card": c, "listId": c.ListID, "duplicatedFrom": in.ID})
	}
	return c, err
}

// ---- templates (project-wide)

type TemplateSaveIn struct {
	ID   string `path:"id" desc:"card to snapshot"`
	Name string `json:"name" desc:"template name (default: card name)"`
}

type TemplatesIn struct {
	ProjectID string `path:"projectId"`
}

type FromTemplateIn struct {
	ID       string   `path:"id" desc:"template card id"`
	ListID   string   `json:"listId" required:"true" desc:"list to create the new card in"`
	Name     string   `json:"name" desc:"default: template name"`
	Position *float64 `json:"position"`
}

type TemplatesOut struct {
	Items []Card `json:"items"`
}

func init() {
	ops.Register(ops.Op{Name: "card.template.save", Summary: "Save a card as a project-wide card template", Method: "POST", Path: "/cards/{id}/template",
		UI: "POST /ui/cards/{id}/template"}, templateSave)
	ops.Register(ops.Op{Name: "card.template.list", Summary: "List the card templates of a project", Method: "GET", Path: "/projects/{projectId}/card-templates",
		UI: "GET /ui/projects/{projectId}/card-templates"}, templateList)
	ops.Register(ops.Op{Name: "card.fromTemplate", Summary: "Create a card from a card template", Method: "POST", Path: "/card-templates/{id}/cards",
		UI: "POST /ui/cards/{id}/from-template"}, fromTemplate)
}

func templateSave(e *ops.Env, in TemplateSaveIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	src, err := loadCard(e, e.DB, in.ID)
	if err != nil {
		return src, err
	}
	if src.IsTemplate {
		return src, ops.Invalid("card is already a template")
	}
	name := in.Name
	if name == "" {
		name = src.Name
	}
	proj := boardProject(e.Ctx, e.DB, boardID)
	var newID string
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		var err error
		newID, err = cloneCard(e, tx, src, cloneOpts{Name: name, ListID: src.ListID, Position: 0, IsTemplate: true, TemplateOf: &proj})
		return err
	})
	if err != nil {
		return Card{}, err
	}
	return loadCard(e, e.DB, newID)
}

func templateList(e *ops.Env, in TemplatesIn) (TemplatesOut, error) {
	if err := authz.RequireProject(e, in.ProjectID, authz.View); err != nil {
		return TemplatesOut{}, err
	}
	out := TemplatesOut{Items: []Card{}}
	for _, id := range strs(e, e.DB, `SELECT id FROM cards WHERE is_template=1 AND template_project_id=? ORDER BY name`, in.ProjectID) {
		if c, err := loadCard(e, e.DB, id); err == nil {
			c.ShareToken = nil
			out.Items = append(out.Items, c)
		}
	}
	return out, nil
}

func fromTemplate(e *ops.Env, in FromTemplateIn) (Card, error) {
	tpl, err := loadCard(e, e.DB, in.ID)
	if err != nil || !tpl.IsTemplate || tpl.TemplateProjectID == nil {
		return Card{}, ops.NotFound("card template")
	}
	if err := authz.RequireProject(e, *tpl.TemplateProjectID, authz.View); err != nil {
		return Card{}, err
	}
	boardID, ltype, err := listRef(e.Ctx, e.DB, in.ListID)
	if err != nil {
		return Card{}, err
	}
	if err := authz.RequireBoard(e, boardID, authz.Edit); err != nil {
		return Card{}, err
	}
	if boardProject(e.Ctx, e.DB, boardID) != *tpl.TemplateProjectID {
		return Card{}, ops.Invalid("template belongs to a different project")
	}
	if ltype != "active" && ltype != "closed" {
		return Card{}, ops.Invalid("cards can only be created in active or closed lists")
	}
	name := in.Name
	if name == "" {
		name = tpl.Name
	}
	var newID string
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		pos := positionFor(e.Ctx, tx, in.ListID, "", "", "", in.Position)
		var err error
		newID, err = cloneCard(e, tx, tpl, cloneOpts{Name: name, ListID: in.ListID, Position: pos})
		return err
	})
	if err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, newID)
	if err == nil {
		emit(e, "cardCreate", "card.created", boardID, newID, map[string]any{"card": c, "listId": in.ListID, "templateId": in.ID})
	}
	return c, err
}

// ---- drafts + board inbox

type DraftCreateIn struct {
	BoardID     string `path:"boardId"`
	Name        string `json:"name" required:"true"`
	Description string `json:"description"`
}

type InboxSetIn struct {
	BoardID string `path:"boardId"`
	Enabled bool   `json:"enabled"`
}

type InboxIn struct {
	BoardID string `path:"boardId"`
}

type InboxOut struct {
	Enabled bool   `json:"enabled"`
	Items   []Card `json:"items"`
}

type PublishIn struct {
	ID       string   `path:"id" desc:"draft card id"`
	ListID   string   `json:"listId" required:"true"`
	Position *float64 `json:"position"`
}

func init() {
	ops.Register(ops.Op{Name: "card.inbox.set", Summary: "Enable or disable the board inbox (drafts)", Method: "PUT", Path: "/boards/{boardId}/inbox",
		UI: "POST /ui/boards/{boardId}/inbox/settings"}, func(e *ops.Env, in InboxSetIn) (InboxOut, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.Manage); err != nil {
			return InboxOut{}, err
		}
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE boards SET has_inbox=?,updated_at=? WHERE id=?`, in.Enabled, db.Now(), in.BoardID); err != nil {
			return InboxOut{}, err
		}
		return inboxList(e, InboxIn{in.BoardID})
	})
	ops.Register(ops.Op{Name: "card.inbox.list", Summary: "List the drafts in the board inbox", Method: "GET", Path: "/boards/{boardId}/inbox",
		UI: "GET /ui/boards/{boardId}/inbox"}, inboxList)
	ops.Register(ops.Op{Name: "card.draft.create", Summary: "Capture a draft card into the board inbox", Method: "POST", Path: "/boards/{boardId}/drafts",
		UI: "POST /ui/boards/{boardId}/inbox"}, draftCreate)
	ops.Register(ops.Op{Name: "card.draft.publish", Summary: "Publish an inbox draft into a list", Method: "POST", Path: "/cards/{id}/publish",
		UI: "POST /ui/cards/{id}/publish"}, draftPublish)
}

func inboxList(e *ops.Env, in InboxIn) (InboxOut, error) {
	if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
		return InboxOut{}, err
	}
	out := InboxOut{Items: []Card{}}
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT has_inbox FROM boards WHERE id=?`, in.BoardID).Scan(&out.Enabled)
	for _, id := range strs(e, e.DB, `SELECT id FROM cards WHERE board_id=? AND is_draft=1 ORDER BY created_at`, in.BoardID) {
		if c, err := loadCard(e, e.DB, id); err == nil {
			c.ShareToken = nil
			out.Items = append(out.Items, c)
		}
	}
	return out, nil
}

func draftCreate(e *ops.Env, in DraftCreateIn) (Card, error) {
	if err := authz.RequireBoard(e, in.BoardID, authz.Edit); err != nil {
		return Card{}, err
	}
	var inbox bool
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT has_inbox FROM boards WHERE id=?`, in.BoardID).Scan(&inbox)
	if !inbox {
		return Card{}, ops.Invalid("the board inbox is not enabled")
	}
	name := in.Name
	if name == "" {
		return Card{}, ops.Invalid("name is required")
	}
	if err := ops.CheckLen("name", name, ops.MaxCardNameLen); err != nil {
		return Card{}, err
	}
	// A draft still needs a home list (schema); boards must hide is_draft cards from list rendering.
	list := firstList(e, in.BoardID, "active")
	if list == "" {
		return Card{}, ops.Invalid("board has no active list")
	}
	typ := "project"
	var desc any
	if in.Description != "" {
		desc = in.Description
	}
	id, now := db.NewID(), db.Now()
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(e.Ctx, `INSERT INTO cards(id,board_id,list_id,creator_user_id,type,position,name,description,is_draft,list_changed_at,created_at)
			VALUES(?,?,?,?,?,?,?,?,1,?,?)`, id, in.BoardID, list, e.User.UserID, typ, endPos(e.Ctx, tx, list), name, desc, now, now)
		return err
	})
	if err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, id)
	if err == nil {
		emit(e, "cardCreate", "card.created", in.BoardID, id, map[string]any{"card": c, "draft": true})
	}
	return c, err
}

func draftPublish(e *ops.Env, in PublishIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, in.ID)
	if err != nil {
		return c, err
	}
	if !c.IsDraft {
		return c, ops.Invalid("card is not a draft")
	}
	lb, lt, err := listRef(e.Ctx, e.DB, in.ListID)
	if err != nil {
		return c, err
	}
	if err := authz.RequireBoard(e, lb, authz.Edit); err != nil {
		return c, err
	}
	if lt != "active" && lt != "closed" {
		return c, ops.Invalid("drafts can only be published into active or closed lists")
	}
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		pos := positionFor(e.Ctx, tx, in.ListID, in.ID, "", "", in.Position)
		now := db.Now()
		_, err := tx.ExecContext(e.Ctx, `UPDATE cards SET is_draft=0,list_id=?,board_id=?,position=?,is_closed=?,list_changed_at=?,updated_at=? WHERE id=?`,
			in.ListID, lb, pos, lt == "closed", now, now, in.ID)
		return err
	})
	if err != nil {
		return c, err
	}
	c, err = loadCard(e, e.DB, in.ID)
	if err == nil {
		if lb != boardID {
			emit(e, "cardDelete", "card.deleted", boardID, in.ID, nil)
		}
		emit(e, "cardCreate", "card.created", lb, in.ID, map[string]any{"card": c, "listId": in.ListID, "published": true})
	}
	return c, err
}

// ---- share links

type ShareOut struct {
	Token string `json:"token"`
	URL   string `json:"url" desc:"public read-only path"`
}

type ShareViewIn struct {
	Token string `path:"token"`
}

type SharedLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type SharedCard struct {
	Name           string        `json:"name"`
	Description    string        `json:"description"`
	Type           string        `json:"type"`
	BoardName      string        `json:"boardName"`
	ListName       string        `json:"listName"`
	DueDate        *string       `json:"dueDate"`
	IsDueCompleted bool          `json:"isDueCompleted"`
	Labels         []SharedLabel `json:"labels"`
	Members        []string      `json:"members"`
	LocationName   string        `json:"locationName,omitempty"`
	Lat            *float64      `json:"lat"`
	Lng            *float64      `json:"lng"`
}

func init() {
	ops.Register(ops.Op{Name: "card.share.create", Summary: "Create (or return) the anonymous read-only share link", Method: "POST", Path: "/cards/{id}/share",
		UI: "POST /ui/cards/{id}/share"}, func(e *ops.Env, in IDIn) (ShareOut, error) {
		if _, err := editCard(e, in.ID); err != nil {
			return ShareOut{}, err
		}
		var tok *string
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT share_token FROM cards WHERE id=?`, in.ID).Scan(&tok)
		if tok == nil {
			t := db.Token(16)
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET share_token=?,updated_at=? WHERE id=?`, t, db.Now(), in.ID); err != nil {
				return ShareOut{}, err
			}
			tok = &t
		}
		return ShareOut{*tok, "/share/" + *tok}, nil
	})
	ops.Register(ops.Op{Name: "card.share.revoke", Summary: "Revoke the card's share link", Method: "DELETE", Path: "/cards/{id}/share",
		UI: "POST /ui/cards/{id}/share/revoke"}, func(e *ops.Env, in IDIn) (Deleted, error) {
		if _, err := editCard(e, in.ID); err != nil {
			return Deleted{}, err
		}
		_, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET share_token=NULL,updated_at=? WHERE id=?`, db.Now(), in.ID)
		return Deleted{in.ID}, err
	})
	ops.Register(ops.Op{Name: "card.share.view", Summary: "Read a shared card anonymously", Method: "GET", Path: "/share/{token}", Auth: ops.AuthPublic,
		UI: "GET /share/{token}"}, shareView)
}

func shareView(e *ops.Env, in ShareViewIn) (SharedCard, error) {
	var out SharedCard
	var id string
	var desc *string
	var done *bool
	var err error
	err = e.DB.QueryRowContext(e.Ctx, `SELECT c.id,c.name,c.description,c.type,b.name,COALESCE(l.name,l.type),c.due_date,c.is_due_completed,c.lat,c.lng,COALESCE(c.location_name,'')
		FROM cards c JOIN boards b ON b.id=c.board_id JOIN lists l ON l.id=c.list_id WHERE c.share_token=? AND c.is_template=0`, in.Token).
		Scan(&id, &out.Name, &desc, &out.Type, &out.BoardName, &out.ListName, &out.DueDate, &done, &out.Lat, &out.Lng, &out.LocationName)
	if err == sql.ErrNoRows || in.Token == "" {
		return out, ops.NotFound("shared card")
	}
	if err != nil {
		return out, err
	}
	if desc != nil {
		out.Description = *desc
	}
	out.IsDueCompleted = done != nil && *done
	out.Labels, out.Members = []SharedLabel{}, []string{}
	if rows, err := e.DB.QueryContext(e.Ctx, `SELECT COALESCE(l.name,''),l.color FROM card_labels cl JOIN labels l ON l.id=cl.label_id WHERE cl.card_id=?`, id); err == nil {
		for rows.Next() {
			var l SharedLabel
			_ = rows.Scan(&l.Name, &l.Color)
			out.Labels = append(out.Labels, l)
		}
		rows.Close()
	}
	out.Members = strs(e, e.DB, `SELECT u.name FROM card_memberships m JOIN users u ON u.id=m.user_id WHERE m.card_id=?`, id)
	return out, nil
}
