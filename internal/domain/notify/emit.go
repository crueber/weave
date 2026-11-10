package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// ActionWriter persists the activity-log row of an event. Replaceable so the
// cardextras domain can own activity writing; the default inserts into `actions`.
// ev.Data["actionId"] holds the pre-generated action id.
var ActionWriter func(*ops.Env, ops.Event) = defaultActionWriter

var wg sync.WaitGroup

// Wait blocks until all background deliveries (webhooks, notification services,
// e-mail) started so far have finished. Used by tests and graceful shutdown.
func Wait() { wg.Wait() }

func bg(fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Println("notify: background panic:", r)
			}
		}()
		fn()
	}()
}

func init() { ops.EmitHook = Record }

func defaultActionWriter(e *ops.Env, ev ops.Event) {
	if ev.BoardID == "" {
		return
	}
	id, _ := ev.Data["actionId"].(string)
	if id == "" {
		id = db.NewID()
	}
	data := map[string]any{}
	for k, v := range ev.Data {
		if k != "actionId" {
			data[k] = v
		}
	}
	b, _ := json.Marshal(data)
	var card, user any
	if ev.CardID != "" {
		card = ev.CardID
	}
	if e.User != nil {
		user = e.User.UserID
	}
	env := events.Build(e, ev)
	for attempt := 0; attempt < 5; attempt++ {
		// seq is allocated inside the INSERT: SQLite's write lock makes seq order equal commit order.
		_, err := e.DB.ExecContext(e.Ctx, `INSERT INTO actions(id,board_id,card_id,user_id,type,data,created_at,seq,etype,list_id,from_list_id,to_list_id,envelope)
			VALUES(?,?,?,?,?,?,?,(SELECT COALESCE(MAX(seq),0)+1 FROM actions WHERE seq IS NOT NULL),?,?,?,?,?)`,
			id, ev.BoardID, card, user, ev.Type, string(b), env.At, env.Type, nz(env.ListID), nz(env.FromListID), nz(env.ToListID), env.Marshal())
		if err == nil {
			e.Hub.Tick()
			return
		}
		if !db.IsUnique(err) {
			log.Println("notify: action insert:", err)
			return
		}
	}
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

// Record is the installed ops.EmitHook: activity row, realtime publish,
// notifications (+services, e-mail) and webhooks. It never fails the caller.
func Record(e *ops.Env, ev ops.Event) {
	defer func() {
		if r := recover(); r != nil {
			log.Println("notify: emit panic:", r)
		}
	}()
	if ev.Room == "" && ev.BoardID != "" {
		ev.Room = "board:" + ev.BoardID
	}
	if ev.Data == nil {
		ev.Data = map[string]any{}
	}
	if str(ev.Data, "actionId") == "" {
		ev.Data["actionId"] = db.NewID()
	}
	if ActionWriter != nil {
		ActionWriter(e, ev)
	}
	if ev.Name != "" && e.Hub != nil {
		e.Hub.Publish(realtime.Event{Name: ev.Name, Room: ev.Room, Data: ev.Data})
	}
	createNotifications(e, ev)
	dispatchWebhooks(e, ev, "")
}

type planned struct {
	UserID, Type, CommentID string
	Data                    map[string]any
}

// NotificationOut is the API shape of a notification.
type NotificationOut struct {
	ID          string         `json:"id"`
	UserID      string         `json:"userId"`
	CreatorID   string         `json:"creatorUserId,omitempty"`
	CreatorName string         `json:"creatorName,omitempty"`
	BoardID     string         `json:"boardId,omitempty"`
	BoardName   string         `json:"boardName,omitempty"`
	CardID      string         `json:"cardId,omitempty"`
	CardName    string         `json:"cardName,omitempty"`
	CommentID   string         `json:"commentId,omitempty"`
	ActionID    string         `json:"actionId,omitempty"`
	Type        string         `json:"type"`
	Data        map[string]any `json:"data"`
	IsRead      bool           `json:"isRead"`
	CreatedAt   string         `json:"createdAt"`
	Summary     string         `json:"summary"`
}

var mentionRe = regexp.MustCompile(`@\[(.*?)\]\((.*?)\)`)

// MentionIDs extracts user ids from `@[name](userId)` markup.
func MentionIDs(text string) []string {
	var ids []string
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		ids = append(ids, m[2])
	}
	return ids
}

// MentionsToText turns mention markup into plain `@name`.
func MentionsToText(text string) string { return mentionRe.ReplaceAllString(text, "@$1") }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}

func userCanView(e *ops.Env, userID, boardID string) bool {
	var role string
	var deact bool
	if e.DB.QueryRowContext(e.Ctx, `SELECT role,is_deactivated FROM users WHERE id=? AND deleted_at IS NULL`, userID).Scan(&role, &deact) != nil || deact {
		return false
	}
	c := *e
	c.User = &ops.Principal{UserID: userID, Role: role}
	return authz.BoardLevel(&c, boardID) >= authz.View
}

func queryIDs(e *ops.Env, q string, args ...any) []string {
	var out []string
	rows, err := e.DB.QueryContext(e.Ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func listRef(v any) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case string:
		return map[string]any{"id": x}
	}
	return nil
}

func planNotifications(e *ops.Env, ev ops.Event, actor string) []planned {
	if ev.CardID == "" || ev.BoardID == "" {
		return nil
	}
	cardName := str(ev.Data, "cardName")
	if cardName == "" {
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT name FROM cards WHERE id=?`, ev.CardID).Scan(&cardName)
	}
	subs := func() []string {
		return queryIDs(e, `SELECT user_id FROM card_subscriptions WHERE card_id=? UNION SELECT user_id FROM board_subscriptions WHERE board_id=?`, ev.CardID, ev.BoardID)
	}
	var out []planned
	switch {
	case ev.Type == "commentCreate":
		text := str(ev.Data, "text")
		cid := str(ev.Data, "commentId")
		seen := map[string]bool{actor: true}
		d := map[string]any{"card": map[string]any{"name": cardName}, "text": text}
		for _, id := range MentionIDs(text) {
			if !seen[id] && userCanView(e, id, ev.BoardID) {
				seen[id] = true
				out = append(out, planned{id, "mentionInComment", cid, d})
			}
		}
		for _, id := range subs() {
			if !seen[id] {
				seen[id] = true
				out = append(out, planned{id, "commentCard", cid, d})
			}
		}
	case ev.Type == "cardMembershipCreate":
		if id := str(ev.Data, "userId"); id != "" && id != actor {
			out = append(out, planned{id, "addMemberToCard", "", map[string]any{"card": map[string]any{"name": cardName}}})
		}
	case ev.Type == "cardMove" || ev.Type == "cardUpdate":
		from, to := listRef(ev.Data["fromList"]), listRef(ev.Data["toList"])
		if from == nil || to == nil || str(from, "id") == str(to, "id") {
			return nil
		}
		d := map[string]any{"card": map[string]any{"name": cardName}, "fromList": from, "toList": to}
		for _, id := range subs() {
			if id != actor {
				out = append(out, planned{id, "moveCard", "", d})
			}
		}
	}
	return out
}

func listName(m map[string]any) string {
	if n := str(m, "name"); n != "" {
		return n
	}
	switch str(m, "type") {
	case "archive":
		return "Archive"
	case "trash":
		return "Trash"
	case "closed":
		return "Closed"
	}
	return "list"
}

func baseURL() string {
	if v := os.Getenv("WEAVE_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8080"
}

func mdEscape(s string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "`", "\\`", "#", "\\#").Replace(s)
}

// Message renders a notification in text, markdown or html.
func Message(n NotificationOut, format string) (title, body string) {
	actor, card, board := n.CreatorName, n.CardName, n.BoardName
	if actor == "" {
		actor = "Someone"
	}
	link := baseURL() + "/cards/" + n.CardID
	var sentence func(a, c, b string, extra ...string) string
	tail := ""
	switch n.Type {
	case "moveCard":
		title = "Card Moved"
		from, to := listName(listRef(n.Data["fromList"])), listName(listRef(n.Data["toList"]))
		sentence = func(a, c, b string, x ...string) string {
			if format == "markdown" {
				return fmt.Sprintf("%s moved %s from **%s** to **%s** on %s", a, c, mdEscape(from), mdEscape(to), b)
			} else if format == "html" {
				return fmt.Sprintf("%s moved %s from <b>%s</b> to <b>%s</b> on %s", a, c, html.EscapeString(from), html.EscapeString(to), b)
			}
			return fmt.Sprintf("%s moved %s from %s to %s on %s", a, c, from, to, b)
		}
	case "commentCard", "mentionInComment":
		verb := "left a new comment to"
		title = "New Comment"
		if n.Type == "mentionInComment" {
			verb, title = "mentioned you in", "You Were Mentioned in Comment"
		}
		tail = truncate(MentionsToText(fmt.Sprint(n.Data["text"])), 280)
		sentence = func(a, c, b string, x ...string) string { return fmt.Sprintf("%s %s %s on %s", a, verb, c, b) }
	case "addMemberToCard":
		title = "You Were Added to Card"
		sentence = func(a, c, b string, x ...string) string { return fmt.Sprintf("%s added you to %s on %s", a, c, b) }
	default:
		return n.Type, n.Type
	}
	switch format {
	case "markdown":
		body = sentence(mdEscape(actor), "["+mdEscape(card)+"]("+link+")", mdEscape(board))
		if tail != "" {
			body += ":\n\n*" + mdEscape(tail) + "*"
		}
	case "html":
		body = sentence(html.EscapeString(actor), `<a href="`+html.EscapeString(link)+`">`+html.EscapeString(card)+`</a>`, html.EscapeString(board))
		if tail != "" {
			body += ":\n\n<i>" + html.EscapeString(tail) + "</i>"
		}
	default:
		body = sentence(actor, card, board)
		if tail != "" {
			body += ":\n" + tail
		}
	}
	return
}

func createNotifications(e *ops.Env, ev ops.Event) {
	actor := ""
	actorName := ""
	if e.User != nil {
		actor, actorName = e.User.UserID, e.User.Name
	}
	plans := planNotifications(e, ev, actor)
	if len(plans) == 0 {
		return
	}
	var boardName, cardName string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT name FROM boards WHERE id=?`, ev.BoardID).Scan(&boardName)
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT name FROM cards WHERE id=?`, ev.CardID).Scan(&cardName)
	if actorName == "" && actor != "" {
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT name FROM users WHERE id=?`, actor).Scan(&actorName)
	}
	var created []NotificationOut
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		for _, p := range plans {
			n := NotificationOut{ID: db.NewID(), UserID: p.UserID, CreatorID: actor, CreatorName: actorName, BoardID: ev.BoardID, BoardName: boardName,
				CardID: ev.CardID, CardName: cardName, CommentID: p.CommentID, ActionID: str(ev.Data, "actionId"), Type: p.Type, Data: p.Data, CreatedAt: db.Now()}
			b, _ := json.Marshal(p.Data)
			var creator, comment, action any
			if actor != "" {
				creator = actor
			}
			if p.CommentID != "" {
				comment = p.CommentID
			}
			if n.ActionID != "" {
				action = n.ActionID
			}
			if _, err := tx.Exec(`INSERT INTO notifications(id,user_id,creator_user_id,board_id,card_id,comment_id,action_id,type,data,is_read,created_at) VALUES(?,?,?,?,?,?,?,?,?,0,?)`,
				n.ID, n.UserID, creator, n.BoardID, n.CardID, comment, action, n.Type, string(b), n.CreatedAt); err != nil {
				return err
			}
			n.Summary = summaryOf(n)
			created = append(created, n)
		}
		return nil
	})
	if err != nil {
		log.Println("notify: insert notifications:", err)
		return
	}
	for _, n := range created {
		n := n
		if e.Hub != nil {
			e.Hub.Publish(realtime.Event{Name: "notification", Room: "user:" + n.UserID, Data: map[string]any{"action": "create", "item": n}})
		}
		dispatchWebhooks(e, ops.Event{Type: "notificationCreate", BoardID: ev.BoardID, CardID: ev.CardID, Data: map[string]any{"item": n}}, actor)
		d, ctx := e.DB, context.Background()
		bg(func() { deliverToUser(ctx, d, n) })
	}
	// Board-level notification services only hear about new comments (Planka behaviour).
	if ev.Type == "commentCreate" {
		n := NotificationOut{CreatorName: actorName, BoardID: ev.BoardID, BoardName: boardName, CardID: ev.CardID, CardName: cardName, Type: "commentCard",
			Data: map[string]any{"text": str(ev.Data, "text")}}
		d, ctx := e.DB, context.Background()
		bg(func() { deliverToBoard(ctx, d, ev.BoardID, n) })
	}
}

func summaryOf(n NotificationOut) string { _, b := Message(n, "text"); return b }

func deliverToUser(ctx context.Context, d *db.DB, n NotificationOut) {
	var email string
	var deact bool
	_ = d.QueryRowContext(ctx, `SELECT email,is_deactivated FROM users WHERE id=?`, n.UserID).Scan(&email, &deact)
	if deact {
		return
	}
	rows, err := d.QueryContext(ctx, `SELECT url,format FROM notification_services WHERE user_id=?`, n.UserID)
	if err == nil {
		type svc struct{ url, format string }
		var svcs []svc
		for rows.Next() {
			var s svc
			_ = rows.Scan(&s.url, &s.format)
			svcs = append(svcs, s)
		}
		rows.Close()
		for _, s := range svcs {
			title, body := Message(n, s.format)
			_ = deliver(ctx, s.url, title, body) // failures are logged inside deliver
		}
	}
	env := &ops.Env{Ctx: ctx, DB: d}
	if email != "" && SMTPConfigured(env) && Config(env, "smtp.notifications") != "false" {
		title, text := Message(n, "text")
		_, htmlBody := Message(n, "html")
		if err := SendMail(env, []string{email}, title, text, "<p>"+strings.ReplaceAll(htmlBody, "\n", "<br>")+"</p>"); err != nil {
			log.Println("notify: email:", err)
		}
	}
}

func deliverToBoard(ctx context.Context, d *db.DB, boardID string, n NotificationOut) {
	rows, err := d.QueryContext(ctx, `SELECT url,format FROM notification_services WHERE board_id=?`, boardID)
	if err != nil {
		return
	}
	type svc struct{ url, format string }
	var svcs []svc
	for rows.Next() {
		var s svc
		_ = rows.Scan(&s.url, &s.format)
		svcs = append(svcs, s)
	}
	rows.Close()
	for _, s := range svcs {
		title, body := Message(n, s.format)
		_ = deliver(ctx, s.url, title, body)
	}
}
