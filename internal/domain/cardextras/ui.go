package cardextras

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// Member is an assignable user in the task forms.
type Member struct{ ID, Name string }

// CardOpt is a linkable card in the task forms.
type CardOpt struct{ ID, Name string }

type sectionData struct {
	CardID     string
	BoardID    string
	CanEdit    bool
	CanComment bool
	CanManage  bool
	Limit      int
	NextLimit  int
	HasMore    bool
	Lists      []TaskListItem
	Members    []Member
	Cards      []CardOpt
	Comments   CommentsOut
	Files      []AttachmentItem
	Groups     []GroupItem
	Time       TimeSummary
	Actions    []ActionItem
	Types      []string
	// checklist totals across all task lists (header count + overall progress)
	TasksDone  int
	TasksTotal int
}

func fmtDur(sec int) string {
	if sec < 0 {
		sec = 0
	}
	h, m, s := sec/3600, sec%3600/60, sec%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func fmtDate(s any) string {
	var str string
	switch v := s.(type) {
	case string:
		str = v
	case *string:
		if v != nil {
			str = *v
		}
	}
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return str
	}
	return t.Local().Format("Jan 2, 2006 15:04")
}

// fmtAgo renders a compact relative time ("just now", "5m ago", "3d ago"); older than ~a month falls back to the date.
func fmtAgo(s any) string {
	var str string
	switch v := s.(type) {
	case string:
		str = v
	case *string:
		if v != nil {
			str = *v
		}
	}
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return str
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format("Jan 2, 2006")
}

func pct(done, total int) int {
	if total <= 0 {
		return 0
	}
	return done * 100 / total
}

func fmtBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for m := n / u; m >= u; m /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func init() {
	web.AddFuncs(template.FuncMap{
		"xDur":    fmtDur,
		"xDate":   fmtDate,
		"xBytes":  fmtBytes,
		"xAgo":    fmtAgo,
		"xPct":    pct,
		"xAction": ActionText,
		"xInitial": func(s *string) string {
			if s == nil || *s == "" {
				return "?"
			}
			return strings.ToUpper(string([]rune(*s)[:1]))
		},
		"xDateInput": func(s string) string {
			if len(s) >= 10 {
				return s[:10]
			}
			return s
		},
		"xJoin":      func(a []string) string { return strings.Join(a, ", ") },
		"xInt":       func(i int) string { return strconv.Itoa(i) },
		"xCellValue": func(r RowItem, f FieldItem) string { return r.Values[f.ID] },
		"xDict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
	})
	registerUI()
}

func base(c *web.Ctx, cardID string) (*sectionData, error) {
	board, err := authz.RequireCard(c.Env, cardID, authz.View)
	if err != nil {
		return nil, err
	}
	l := authz.BoardLevel(c.Env, board)
	return &sectionData{CardID: cardID, BoardID: board, CanEdit: l >= authz.Edit && c.Env.User != nil,
		CanComment: l >= authz.Comment && c.Env.User != nil, CanManage: l >= authz.Manage}, nil
}

func limitParam(c *web.Ctx, def int) int {
	n, _ := strconv.Atoi(c.R.URL.Query().Get("limit"))
	if n < def || n > 1000 {
		return def
	}
	return n
}

func members(c *web.Ctx, d *sectionData) []Member {
	ms, _ := query(c.Env.Ctx, c.Env.DB, func(r *sql.Rows) (Member, error) { var m Member; return m, r.Scan(&m.ID, &m.Name) },
		`SELECT u.id,u.name FROM users u WHERE u.is_deactivated=0 AND u.deleted_at IS NULL AND u.id IN (
			SELECT user_id FROM board_memberships WHERE board_id=? AND user_id IS NOT NULL
			UNION SELECT user_id FROM card_memberships WHERE card_id=?) ORDER BY u.name`, d.BoardID, d.CardID)
	return ms
}

func linkable(c *web.Ctx, d *sectionData) []CardOpt {
	cs, _ := query(c.Env.Ctx, c.Env.DB, func(r *sql.Rows) (CardOpt, error) { var m CardOpt; return m, r.Scan(&m.ID, &m.Name) },
		`SELECT id,name FROM cards WHERE board_id=? AND id<>? AND is_draft=0 AND is_template=0 ORDER BY name LIMIT 300`, d.BoardID, d.CardID)
	return cs
}

// renderSection renders one card-modal fragment (read via ops, so it matches REST/CLI).
func renderSection(c *web.Ctx, section, cardID string) error {
	d, err := base(c, cardID)
	if err != nil {
		return err
	}
	switch section {
	case "tasks":
		if d.Lists, err = ops.Typed[[]TaskListItem](c.Env, "tasklist.list", TaskListsIn{CardID: cardID}); err != nil {
			return err
		}
		for _, l := range d.Lists {
			d.TasksDone += l.TasksCompleted
			d.TasksTotal += l.TasksTotal
		}
		if d.CanEdit {
			d.Members, d.Cards = members(c, d), linkable(c, d)
		}
	case "comments":
		d.Limit = limitParam(c, 50)
		if d.Comments, err = ops.Typed[CommentsOut](c.Env, "comment.list", CommentsIn{CardID: cardID, Limit: d.Limit}); err != nil {
			return err
		}
		d.HasMore, d.NextLimit = d.Comments.Total > len(d.Comments.Items), d.Limit+50
	case "attachments":
		if d.Files, err = ops.Typed[[]AttachmentItem](c.Env, "attachment.list", CardIDIn{CardID: cardID}); err != nil {
			return err
		}
	case "fields":
		if d.Groups, err = ops.Typed[[]GroupItem](c.Env, "customfieldgroup.list", GroupListIn{CardID: cardID}); err != nil {
			return err
		}
		d.Types = []string{"text", "number", "date", "checkbox", "select", "url"}
	case "time":
		if d.Time, err = ops.Typed[TimeSummary](c.Env, "time.list", CardIDIn{CardID: cardID}); err != nil {
			return err
		}
	case "activity":
		d.Limit = limitParam(c, 50)
		if d.Actions, err = ops.Typed[[]ActionItem](c.Env, "activity.card", CardActionsIn{CardID: cardID, Limit: d.Limit}); err != nil {
			return err
		}
		d.HasMore, d.NextLimit = len(d.Actions) >= d.Limit, d.Limit+50
	}
	return c.RenderBlock("extras_card.html", section, d)
}

// cardOf extracts the card id from an op result (CardID string or *string).
func cardOf(out any) string {
	v := reflect.ValueOf(out)
	for v.Kind() == reflect.Ptr && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	f := v.FieldByName("CardID")
	if !f.IsValid() {
		return ""
	}
	if f.Kind() == reflect.Ptr {
		if f.IsNil() {
			return ""
		}
		f = f.Elem()
	}
	return f.String()
}

func afterMutation(c *web.Ctx, section string, out any) error {
	card := c.R.FormValue("_card")
	if card == "" {
		card = cardOf(out)
	}
	if card == "" {
		return ops.Invalid("card context missing")
	}
	return renderSection(c, section, card)
}

func retarget(c *web.Ctx, section string) {
	c.W.Header().Set("HX-Retarget", "#x-"+section)
	c.W.Header().Set("HX-Reswap", "outerHTML")
}

func formBool(s string) bool { return s == "on" || s == "true" || s == "1" }

func splitOptions(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
}

func registerUI() {
	// Fragments (public routes: the ops enforce board access, so public boards render read-only).
	for _, s := range []struct{ path, section string }{
		{"tasks", "tasks"}, {"comments", "comments"}, {"attachments", "attachments"},
		{"fields", "fields"}, {"time", "time"}, {"activity", "activity"},
	} {
		s := s
		web.RoutePublic("GET /ui/cards/{id}/"+s.path, func(c *web.Ctx) error { return renderSection(c, s.section, c.R.PathValue("id")) })
	}

	// Plain mutations: bind the op's own input from the form, run it, re-render the fragment.
	for _, m := range []struct{ pattern, op, section string }{
		{"POST /ui/cards/{cardId}/task-lists", "tasklist.create", "tasks"},
		{"POST /ui/task-lists/{id}", "tasklist.update", "tasks"},
		{"POST /ui/task-lists/{id}/delete", "tasklist.delete", "tasks"},
		{"POST /ui/task-lists/{taskListId}/tasks", "task.create", "tasks"},
		{"POST /ui/tasks/{id}", "task.update", "tasks"},
		{"POST /ui/tasks/{id}/delete", "task.delete", "tasks"},
		{"POST /ui/cards/{cardId}/comments", "comment.create", "comments"},
		{"POST /ui/comments/{id}", "comment.update", "comments"},
		{"POST /ui/comments/{id}/delete", "comment.delete", "comments"},
		{"POST /ui/cards/{cardId}/attachments", "attachment.create", "attachments"},
		{"POST /ui/attachments/{id}", "attachment.update", "attachments"},
		{"POST /ui/attachments/{id}/cover", "attachment.cover", "attachments"},
		{"POST /ui/attachments/{id}/delete", "attachment.delete", "attachments"},
		{"POST /ui/custom-field-groups", "customfieldgroup.create", "fields"},
		{"POST /ui/custom-field-groups/{id}", "customfieldgroup.update", "fields"},
		{"POST /ui/custom-field-groups/{id}/delete", "customfieldgroup.delete", "fields"},
		{"POST /ui/custom-fields/{id}/delete", "customfield.delete", "fields"},
		{"POST /ui/cards/{cardId}/custom-field-values", "customfieldvalue.set", "fields"},
		{"POST /ui/cards/{cardId}/custom-field-values/clear", "customfieldvalue.clear", "fields"},
		{"POST /ui/cards/{cardId}/custom-field-groups/{groupId}/rows", "customfieldrow.create", "fields"},
		{"POST /ui/custom-field-rows/{id}", "customfieldrow.update", "fields"},
		{"POST /ui/custom-field-rows/{id}/delete", "customfieldrow.delete", "fields"},
		{"POST /ui/cards/{cardId}/time/start", "time.start", "time"},
		{"POST /ui/cards/{cardId}/time/stop", "time.stop", "time"},
		{"POST /ui/cards/{cardId}/time/reset", "time.reset", "time"},
		{"POST /ui/cards/{cardId}/time-entries", "time.add", "time"},
		{"POST /ui/time-entries/{id}", "time.update", "time"},
		{"POST /ui/time-entries/{id}/delete", "time.delete", "time"},
	} {
		m := m
		web.Route(m.pattern, func(c *web.Ctx) error {
			out, err := c.Do(m.op)
			if err != nil {
				return err
			}
			return afterMutation(c, m.section, out)
		})
	}

	// Custom fields take a textarea of options, so bind them by hand.
	web.Route("POST /ui/custom-fields", func(c *web.Ctx) error {
		_ = c.R.ParseForm()
		f := c.R.Form
		out, err := ops.Call(c.Env, "customfield.create", &FieldCreateIn{GroupID: f.Get("groupId"), BaseGroupID: f.Get("baseGroupId"),
			Name: f.Get("name"), Type: f.Get("type"), Options: splitOptions(f.Get("options")), ShowOnFrontOfCard: formBool(f.Get("showOnFrontOfCard"))})
		if err != nil {
			return err
		}
		return afterMutation(c, "fields", out)
	})
	web.Route("POST /ui/custom-fields/{id}", func(c *web.Ctx) error {
		_ = c.R.ParseForm()
		f := c.R.Form
		in := &FieldUpdateIn{ID: c.R.PathValue("id")}
		if f.Has("name") {
			v := f.Get("name")
			in.Name = &v
		}
		if f.Has("type") {
			v := f.Get("type")
			in.Type = &v
		}
		if f.Has("options") {
			v := splitOptions(f.Get("options"))
			in.Options = &v
		}
		if f.Has("showOnFrontOfCard") {
			v := formBool(f.Get("showOnFrontOfCard"))
			in.ShowOnFrontOfCard = &v
		}
		out, err := ops.Call(c.Env, "customfield.update", in)
		if err != nil {
			return err
		}
		return afterMutation(c, "fields", out)
	})

	// Drag-and-drop reorder endpoints (weave.js posts id, container, afterId, beforeId; the sortable URL carries ?_card=<cardId>).
	reorder := func(pattern, section string, run func(c *web.Ctx, id, container, after, before string) (any, error)) {
		web.Route(pattern, func(c *web.Ctx) error {
			_ = c.R.ParseForm()
			f := c.R.Form
			out, err := run(c, f.Get("id"), f.Get("container"), f.Get("afterId"), f.Get("beforeId"))
			if err != nil {
				return err
			}
			retarget(c, section)
			return afterMutation(c, section, out)
		})
	}
	reorder("POST /ui/tasks/reorder", "tasks", func(c *web.Ctx, id, list, a, b string) (any, error) {
		return ops.Call(c.Env, "task.update", &TaskUpdateIn{ID: id, TaskListID: list, AfterID: a, BeforeID: b})
	})
	reorder("POST /ui/task-lists/reorder", "tasks", func(c *web.Ctx, id, _, a, b string) (any, error) {
		return ops.Call(c.Env, "tasklist.update", &TaskListUpdateIn{ID: id, AfterID: a, BeforeID: b})
	})
	reorder("POST /ui/custom-field-groups/reorder", "fields", func(c *web.Ctx, id, _, a, b string) (any, error) {
		return ops.Call(c.Env, "customfieldgroup.update", &GroupUpdateIn{ID: id, AfterID: a, BeforeID: b})
	})
	reorder("POST /ui/custom-field-rows/reorder", "fields", func(c *web.Ctx, id, _, a, b string) (any, error) {
		return ops.Call(c.Env, "customfieldrow.update", &RowUpdateIn{ID: id, AfterID: a, BeforeID: b})
	})

	// Attachment file routes (authorized by the ops; public boards work anonymously).
	web.RoutePublic("GET /attachments/{id}/download/{name}", func(c *web.Ctx) error {
		out, err := c.Do("attachment.download")
		if err != nil {
			return err
		}
		return writeStream(c.W, out.(*ops.Stream), true)
	})
	web.RoutePublic("GET /attachments/{id}/thumbnail", func(c *web.Ctx) error {
		out, err := c.Do("attachment.thumbnail")
		if err != nil {
			return err
		}
		return writeStream(c.W, out.(*ops.Stream), true)
	})

	// Board media view.
	web.RoutePublic("GET /boards/{boardId}/media", func(c *web.Ctx) error {
		out, err := c.Do("attachment.media")
		if err != nil {
			return err
		}
		typ := c.R.URL.Query().Get("type")
		if typ == "" {
			typ = "all"
		}
		return c.Render("extras_media.html", map[string]any{"BoardID": c.R.PathValue("boardId"), "Type": typ, "Items": out})
	})

	// Board activity fragment.
	web.RoutePublic("GET /ui/boards/{id}/activity", func(c *web.Ctx) error {
		limit := limitParam(c, 50)
		items, err := ops.Typed[[]ActionItem](c.Env, "activity.board", ActionsIn{BoardID: c.R.PathValue("id"), Limit: limit})
		if err != nil {
			return err
		}
		return c.RenderBlock("extras_card.html", "board-activity", map[string]any{
			"BoardID": c.R.PathValue("id"), "Actions": items, "HasMore": len(items) >= limit, "NextLimit": limit + 50})
	})
}

// writeStream sends a file inline only for safe raster/video/audio types; everything else downloads.
func writeStream(w http.ResponseWriter, st *ops.Stream, allowInline bool) error {
	disp := "attachment"
	ct := st.ContentType
	if allowInline && (strings.HasPrefix(ct, "image/") && ct != "image/svg+xml" || strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/") || ct == "application/pdf") {
		disp = "inline"
	}
	if disp == "attachment" {
		ct = "application/octet-stream"
		if st.ContentType != "" && !strings.Contains(st.ContentType, "html") && !strings.Contains(st.ContentType, "svg") && !strings.Contains(st.ContentType, "javascript") {
			ct = st.ContentType
		}
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename*=UTF-8''%s`, disp, urlEscape(st.Name)))
	_, err := w.Write(st.Data)
	return err
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
