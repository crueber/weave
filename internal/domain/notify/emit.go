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
	"time"

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
	Reason      string         `json:"reason,omitempty" desc:"why you got it when it came from a watch, e.g. 'You get this because you watch the column Review.'"`
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

func listRef(v any) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case string:
		return map[string]any{"id": x}
	}
	return nil
}

func changed(ev ops.Event, field string) bool {
	switch v := ev.Data["changed"].(type) {
	case []string:
		for _, s := range v {
			if s == field {
				return true
			}
		}
	case []any:
		for _, s := range v {
			if s == field {
				return true
			}
		}
	}
	return false
}

// listInfo is a column as notifications describe it.
type listInfo struct{ ID, Name, Type string }

func (l listInfo) ref() map[string]any {
	return map[string]any{"id": l.ID, "name": l.Name, "type": l.Type}
}

// Watch reasons, most specific first (data.via of a notification).
var viaRank = map[string]int{"board": 1, "list": 2, "card": 3}

// planNotifications decides who hears about an event: at most ONE notification per user per event. The cost is a
// constant number of queries whatever the number of watchers (card row, the source column of a move, all watchers in
// one UNION, one bulk access check).
//
// What notifies (see docs/DESIGN.md "Watching and notifications"):
//
//	cardCreate (not drafts)                         -> createCard      card watchers (rare), column, board
//	cardMove between columns                        -> moveCard        card, source AND destination column, board
//	cardMove into / out of a closed-type column     -> closeCard / reopenCard (one event: never also moveCard)
//	cardUpdate with changed isClosed                -> closeCard / reopenCard
//	cardUpdate with changed dueDate (set, not cleared) -> setCardDueDate
//	commentCreate                                   -> mentionInComment for mentioned viewers, commentCard for watchers
//	cardMembershipCreate                            -> addMemberToCard for the added member only
//
// Rules: the actor never hears about their own action; a mention beats a watch; a muted card silences every watch
// (card, column, board) but not a direct mention or being added; users who cannot view the board (authz.ViewersAmong:
// deactivated, deleted, lost access) are skipped; drafts and templates never notify. data.via records the most specific
// watch (card > list > board) and data.viaList the watched column.
func planNotifications(e *ops.Env, ev ops.Event, actor string) []planned {
	if ev.CardID == "" || ev.BoardID == "" {
		return nil
	}
	var name, cardBoard, dueDate string
	var cur listInfo
	var draft, tmpl, closed bool
	if e.DB.QueryRowContext(e.Ctx, `SELECT c.name,c.board_id,c.list_id,COALESCE(l.name,''),l.type,c.is_draft,c.is_template,c.is_closed,COALESCE(c.due_date,'')
		FROM cards c JOIN lists l ON l.id=c.list_id WHERE c.id=?`, ev.CardID).
		Scan(&name, &cardBoard, &cur.ID, &cur.Name, &cur.Type, &draft, &tmpl, &closed, &dueDate) != nil || draft || tmpl || cardBoard != ev.BoardID {
		return nil
	}
	card := map[string]any{"name": name}
	typ, commentID := "", ""
	lists := []listInfo{cur}
	d := map[string]any{"card": card}
	var mentions []string
	switch ev.Type {
	case "commentCreate":
		typ, commentID = "commentCard", str(ev.Data, "commentId")
		d["text"] = str(ev.Data, "text")
		mentions = MentionIDs(str(ev.Data, "text"))
	case "cardMembershipCreate":
		id := str(ev.Data, "userId")
		if id == "" || id == actor || !authz.ViewersAmong(e, ev.BoardID, []string{id})[id] {
			return nil
		}
		return []planned{{id, "addMemberToCard", "", d}}
	case "cardCreate":
		if ev.Data["draft"] == true {
			return nil
		}
		typ = "createCard"
		d["list"] = cur.ref()
	case "cardMove":
		fromID, toID := str(ev.Data, "fromListId"), str(ev.Data, "toListId")
		if fromID == "" || toID == "" || fromID == toID || toID != cur.ID {
			return nil // a reorder inside one column, or a stale event
		}
		from := listInfo{ID: fromID}
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT COALESCE(name,''),type FROM lists WHERE id=?`, fromID).Scan(&from.Name, &from.Type)
		lists = []listInfo{cur, from} // destination first: it names the reason when both are watched
		switch {
		case cur.Type == "closed" && from.Type != "closed":
			typ = "closeCard"
		case from.Type == "closed" && cur.Type != "closed":
			typ = "reopenCard"
		default:
			typ = "moveCard"
		}
		d["fromList"], d["toList"] = from.ref(), cur.ref()
	case "cardUpdate":
		switch {
		case changed(ev, "isClosed"):
			typ = "reopenCard"
			if closed {
				typ = "closeCard"
			}
		case changed(ev, "dueDate") && dueDate != "":
			typ = "setCardDueDate"
			d["dueDate"] = dueDate
		default:
			return nil
		}
	default:
		return nil
	}

	// every watcher of the card, its column(s) and the board in one query
	type watch struct {
		rank   int
		via    string
		list   string
		muted  bool
		listAt int
	}
	args := []any{ev.CardID}
	q := `SELECT user_id,'card',muted,'' FROM card_subscriptions WHERE card_id=?`
	for _, l := range lists {
		q += ` UNION ALL SELECT user_id,'list',0,list_id FROM list_subscriptions WHERE list_id=?`
		args = append(args, l.ID)
	}
	q += ` UNION ALL SELECT user_id,'board',0,'' FROM board_subscriptions WHERE board_id=?`
	args = append(args, ev.BoardID)
	watchers := map[string]*watch{}
	var order []string
	if rows, err := e.DB.QueryContext(e.Ctx, q, args...); err == nil {
		for rows.Next() {
			var uid, via, lid string
			var muted bool
			if rows.Scan(&uid, &via, &muted, &lid) != nil {
				continue
			}
			w := watchers[uid]
			if w == nil {
				w = &watch{listAt: len(lists)}
				watchers[uid] = w
				order = append(order, uid)
			}
			if muted {
				w.muted = true
				continue
			}
			at := len(lists)
			for i, l := range lists {
				if l.ID == lid {
					at = i
				}
			}
			if r := viaRank[via]; r > w.rank || (r == w.rank && via == "list" && at < w.listAt) {
				w.rank, w.via, w.list, w.listAt = r, via, lid, at
			}
		}
		rows.Close()
	}

	// one access check for everyone who might hear about it
	cand := append([]string{}, mentions...)
	cand = append(cand, order...)
	can := authz.ViewersAmong(e, ev.BoardID, cand)
	seen := map[string]bool{actor: true}
	if actor == "" {
		seen = map[string]bool{}
	}
	var out []planned
	for _, id := range mentions {
		if !seen[id] && can[id] {
			seen[id] = true
			out = append(out, planned{id, "mentionInComment", commentID, d})
		}
	}
	for _, id := range order {
		w := watchers[id]
		if seen[id] || !can[id] || w.muted || w.rank == 0 {
			continue
		}
		seen[id] = true
		nd := make(map[string]any, len(d)+2)
		for k, v := range d {
			nd[k] = v
		}
		nd["via"] = w.via
		if w.via == "list" {
			nd["viaList"] = lists[w.listAt].ref()
		}
		out = append(out, planned{id, typ, commentID, nd})
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

// Reason is the "because you watch ..." line of a notification that came from a watch (data.via); "" otherwise.
func Reason(n NotificationOut) string {
	switch str(n.Data, "via") {
	case "card":
		return "You get this because you watch this card."
	case "list":
		return "You get this because you watch the column " + listName(listRef(n.Data["viaList"])) + "."
	case "board":
		return "You get this because you watch the board " + n.BoardName + "."
	}
	return ""
}

// dueText renders a stored due date (RFC3339 UTC) for a sentence: "3 Dec 2026", or with the time when it has one.
func dueText(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	t = t.UTC()
	if t.Hour() == 0 && t.Minute() == 0 {
		return t.Format("2 Jan 2006")
	}
	return t.Format("2 Jan 2006 15:04 UTC")
}

// Message renders a notification in text, markdown or html, ending with the watch reason (Reason) when there is one.
func Message(n NotificationOut, format string) (title, body string) {
	title, body = message(n, format)
	if r := Reason(n); r != "" && body != n.Type {
		switch format {
		case "markdown":
			body += "\n\n_" + mdEscape(r) + "_"
		case "html":
			body += "\n\n<small>" + html.EscapeString(r) + "</small>"
		default:
			body += "\n\n" + r
		}
	}
	return title, body
}

// message is Message without the reason line (the in-app summary shows the reason separately).
func message(n NotificationOut, format string) (title, body string) {
	actor, card, board := n.CreatorName, n.CardName, n.BoardName
	if actor == "" {
		actor = "Someone"
		if n.CreatorID == "" {
			actor = "Weave" // scheduler work (recurring cards, lease sweeps) has no acting user
		}
	}
	link := baseURL() + "/cards/" + n.CardID
	var sentence func(a, c, b string, extra ...string) string
	tail := ""
	// strong marks a list name or a date inside a sentence, escaped for the format
	strong := func(s string) string {
		switch format {
		case "markdown":
			return "**" + mdEscape(s) + "**"
		case "html":
			return "<b>" + html.EscapeString(s) + "</b>"
		}
		return s
	}
	switch n.Type {
	case "moveCard":
		title = "Card Moved"
		from, to := strong(listName(listRef(n.Data["fromList"]))), strong(listName(listRef(n.Data["toList"])))
		sentence = func(a, c, b string, x ...string) string {
			return fmt.Sprintf("%s moved %s from %s to %s on %s", a, c, from, to, b)
		}
	case "createCard":
		title = "Card Created"
		in := strong(listName(listRef(n.Data["list"])))
		sentence = func(a, c, b string, x ...string) string { return fmt.Sprintf("%s created %s in %s on %s", a, c, in, b) }
	case "closeCard":
		title = "Card Closed"
		sentence = func(a, c, b string, x ...string) string { return fmt.Sprintf("%s closed %s on %s", a, c, b) }
	case "reopenCard":
		title = "Card Re-opened"
		sentence = func(a, c, b string, x ...string) string { return fmt.Sprintf("%s re-opened %s on %s", a, c, b) }
	case "setCardDueDate":
		title = "Due Date Changed"
		due := strong(dueText(str(n.Data, "dueDate")))
		sentence = func(a, c, b string, x ...string) string {
			return fmt.Sprintf("%s set the due date of %s to %s on %s", a, c, due, b)
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
			n.Summary, n.Reason = summaryOf(n), Reason(n)
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

func summaryOf(n NotificationOut) string { _, b := message(n, "text"); return b }

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
