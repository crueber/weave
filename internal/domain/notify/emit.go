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

// ActionWriter persists the activity-log row of an event (one call per board of a fanned-out event; see
// fanout.go) and reports whether it was written. Replaceable so the cardextras domain could own activity writing;
// the default inserts into `actions`. ev.Data["actionId"] holds the pre-generated action id of this row,
// ev.Data["originId"] the origin row's id on a fan-out copy, ev.Data["actionAt"] the shared timestamp.
var ActionWriter func(*ops.Env, ops.Event) bool = defaultActionWriter

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

func defaultActionWriter(e *ops.Env, ev ops.Event) bool {
	if ev.BoardID == "" {
		return false
	}
	id, _ := ev.Data["actionId"].(string)
	if id == "" {
		id = db.NewID()
	}
	data := map[string]any{}
	for k, v := range ev.Data {
		hidden := false
		for _, h := range hiddenKeys {
			hidden = hidden || k == h
		}
		if !hidden {
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
		_, err := e.DB.ExecContext(e.Ctx, `INSERT INTO actions(id,board_id,card_id,user_id,type,data,created_at,seq,etype,list_id,from_list_id,to_list_id,envelope,origin_id)
			VALUES(?,?,?,?,?,?,?,(SELECT COALESCE(MAX(seq),0)+1 FROM actions WHERE seq IS NOT NULL),?,?,?,?,?,?)`,
			id, ev.BoardID, card, user, ev.Type, string(b), env.At, env.Type, nz(env.ListID), nz(env.FromListID), nz(env.ToListID), env.Marshal(), nz(str(ev.Data, "originId")))
		if err == nil {
			e.Hub.Tick()
			return true
		}
		if !db.IsUnique(err) {
			log.Println("notify: action insert:", err)
			return false
		}
	}
	return false
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

// Record is the installed ops.EmitHook: activity rows, realtime publish, notifications (+services, e-mail) and
// webhooks. It never fails the caller. An event about a card on several boards is recorded once per board it
// concerns (eventScopeOf, perBoard: one actions row and one room publish per board, each with that board's view);
// notifications and webhooks still run once for the logical event.
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
	s := eventScopeOf(e, ev)
	evs := perBoard(e, ev, s, db.Now())
	if ActionWriter != nil {
		for i, x := range evs {
			if !ActionWriter(e, x) && i == 0 {
				break // no fan-out copies of an event whose origin row was not written
			}
		}
	}
	if ev.Name != "" && e.Hub != nil {
		for _, x := range evs {
			e.Hub.Publish(realtime.Event{Name: ev.Name, Room: x.Room, Data: publicData(x.Data)})
			if x.Room != "board:"+x.BoardID {
				break // a custom room (global, a user room) hears the event once
			}
		}
		// a placement event is recorded on its board only; the card's other boards get an unstored nudge (card id
		// only: never this board's column names) so their "also on" markers and open card sheets can refresh
		for _, b := range s.others {
			e.Hub.Publish(realtime.Event{Name: events.PlacementChanged, Room: "board:" + b, Data: map[string]any{"cardId": ev.CardID}})
		}
	}
	createNotifications(e, ev)
	dispatchWebhooks(e, ev, "", evs, s.home)
}

type planned struct {
	UserID, Type, CommentID string
	BoardID                 string // the board the notification names (see planNotifications)
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

// Quiet reports whether an event is marked quiet (Data["quiet"] == true): it is recorded and streamed but never notifies.
func Quiet(ev ops.Event) bool {
	q, _ := ev.Data["quiet"].(bool)
	return q
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
// constant number of queries whatever the number of watchers and placements (the card with all its placements and
// their columns, the source column of a move, all watchers in one UNION, one bulk access check).
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
// (card, column, board) but not a direct mention or being added; users who cannot view the board (authz.CardViewers:
// deactivated, deleted, lost access) are skipped; drafts and templates never notify. data.via records the most specific
// watch (card > list > board) and data.viaList the watched column.
//
// Placements (one card on several boards): the event's board (ev.BoardID) must hold a placement. A PLACEMENT event
// (a card created in or moved between columns of ev.BoardID: events.PlacementScoped) concerns that board only: its
// column and board watchers, the card's watchers, and every recipient must view that board, so its column names never
// reach someone who only sees another board. A CARD-level event (comment, due date, closed (also when a move into or
// out of a closing column causes it: closed is per card), card.archive/card.restore (events.CardLevel), membership) concerns every board
// the card is on (ev.Boards when the writer listed them): the card's watchers, each in-scope placement's CURRENT
// column (for card.archive: also the column each placement left, its previous column) and each in-scope board; a column or board watch counts only when its watcher can view that board. Each
// notification names ONE board (planned.BoardID): the watched board for a column or board watch, else the home when
// the recipient can view it, else the first board they can view. The columns of a move (fromList/toList) go only to
// notifications that name ev.BoardID.
//
// Quiet events: an event whose data carries "quiet": true never notifies anyone (no watch, mention or member rule
// applies). Bulk side effects use it (a column's closing setting switched on or off flips every card in it: one
// card.updated per card with auto: "listType"), so a watcher is not sent one notification per card. Everything else
// about the event is unchanged: the actions row (activity), SSE, the durable event stream and webhooks.
func planNotifications(e *ops.Env, ev ops.Event, actor string) ([]planned, []string) {
	if Quiet(ev) || ev.CardID == "" || ev.BoardID == "" {
		return nil, nil
	}
	// the card and ALL its placements with their current column, in one query (home first, then by placed_at)
	type place struct {
		board     string
		cur, prev listInfo // prev: the column an archived or trashed placement came from (its prev_list_id)
	}
	var name, dueDate string
	var draft, tmpl, closed bool
	var pls []place
	origin := -1
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT c.name,c.is_draft,c.is_template,c.is_closed,COALESCE(c.due_date,''),p.board_id,p.list_id,COALESCE(l.name,''),l.type,
		COALESCE(pl.id,''),COALESCE(pl.name,''),COALESCE(pl.type,'')
		FROM cards c JOIN card_placements p ON p.card_id=c.id JOIN lists l ON l.id=p.list_id LEFT JOIN lists pl ON pl.id=p.prev_list_id AND pl.board_id=p.board_id
		WHERE c.id=? ORDER BY p.is_home DESC, p.placed_at, p.board_id`, ev.CardID)
	if err != nil {
		return nil, nil
	}
	for rows.Next() {
		var p place
		if rows.Scan(&name, &draft, &tmpl, &closed, &dueDate, &p.board, &p.cur.ID, &p.cur.Name, &p.cur.Type, &p.prev.ID, &p.prev.Name, &p.prev.Type) != nil {
			continue
		}
		if p.board == ev.BoardID {
			origin = len(pls)
		}
		pls = append(pls, p)
	}
	rows.Close()
	if origin < 0 || draft || tmpl {
		return nil, nil // the event's board holds no placement (stale event, or the card is gone), or a draft/template
	}
	cur := pls[origin].cur
	card := map[string]any{"name": name}
	typ, commentID := "", ""
	originLists := []listInfo{cur}
	d := map[string]any{"card": card}
	var moveLists map[string]any // fromList/toList: only in notifications that name ev.BoardID
	var mentions []string
	placementOnly := events.PlacementScoped(ev)
	switch ev.Type {
	case "commentCreate":
		typ, commentID = "commentCard", str(ev.Data, "commentId")
		d["text"] = str(ev.Data, "text")
		mentions = MentionIDs(str(ev.Data, "text"))
	case "cardMembershipCreate":
		id := str(ev.Data, "userId")
		if id == "" || id == actor {
			return nil, nil
		}
		v := authz.CardViewers(e, ev.CardID, []string{id})[id] // home first when viewable
		if len(v) == 0 {
			return nil, nil
		}
		return []planned{{id, "addMemberToCard", "", v[0], d}}, nil
	case "cardCreate":
		if ev.Data["draft"] == true {
			return nil, nil
		}
		typ = "createCard"
		d["list"] = cur.ref()
	case "cardMove":
		fromID, toID := str(ev.Data, "fromListId"), str(ev.Data, "toListId")
		if fromID == "" || toID == "" || fromID == toID || toID != cur.ID {
			return nil, nil // a reorder inside one column, or a stale event
		}
		from := listInfo{ID: fromID}
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT COALESCE(name,''),type FROM lists WHERE id=?`, fromID).Scan(&from.Name, &from.Type)
		originLists = []listInfo{cur, from} // destination first: it names the reason when both are watched
		switch {
		case cur.Type == "closed" && from.Type != "closed":
			typ, placementOnly = "closeCard", false // closed is per card: everyone following the card hears it
		case from.Type == "closed" && cur.Type == "active":
			// (an archive or trash list keeps the closed state: archiving a closed card is a move, not a re-open)
			typ, placementOnly = "reopenCard", false
		default:
			typ = "moveCard"
		}
		moveLists = map[string]any{"fromList": from.ref(), "toList": cur.ref()}
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
			return nil, nil
		}
	default:
		return nil, nil // includes cardPlace/cardUnplace until their notification type exists
	}

	// the boards in scope: the event's board first, then the card's other boards for a card-level event
	scope := []place{pls[origin]}
	if !placementOnly {
		var allowed map[string]bool
		if ev.Boards != nil {
			allowed = map[string]bool{}
			for _, b := range ev.Boards {
				allowed[b] = true
			}
		}
		for i, p := range pls {
			if i != origin && (allowed == nil || allowed[p.board]) {
				scope = append(scope, p)
			}
		}
	}
	type wlist struct {
		info  listInfo
		board string
	}
	lists := make([]wlist, 0, len(scope)+1)
	for _, l := range originLists {
		lists = append(lists, wlist{l, ev.BoardID})
	}
	boards := make([]string, 0, len(scope))
	inScope := map[string]bool{}
	for i, p := range scope {
		boards = append(boards, p.board)
		inScope[p.board] = true
		if i > 0 {
			lists = append(lists, wlist{p.cur, p.board})
			// a card-level archive or trash (card.archive moves every placement): on the other boards the column the
			// card just left is its placement's previous column, and that column's watchers hear it too (as the
			// origin board's source column does)
			if all, _ := ev.Data[events.CardLevel].(bool); all && ev.Type == "cardMove" && (p.cur.Type == "archive" || p.cur.Type == "trash") && p.prev.ID != "" {
				lists = append(lists, wlist{p.prev, p.board})
			}
		}
	}

	// every watcher of the card, the in-scope columns and boards in ONE query
	type watchRow struct {
		via, list, board string
	}
	args := []any{ev.CardID}
	q := `SELECT user_id,'card',muted,'','' FROM card_subscriptions WHERE card_id=?`
	ph := make([]string, len(lists))
	for i, l := range lists {
		ph[i] = "?"
		args = append(args, l.info.ID)
	}
	q += ` UNION ALL SELECT user_id,'list',0,list_id,'' FROM list_subscriptions WHERE list_id IN (` + strings.Join(ph, ",") + `)`
	ph = ph[:0]
	for _, b := range boards {
		ph = append(ph, "?")
		args = append(args, b)
	}
	q += ` UNION ALL SELECT user_id,'board',0,'',board_id FROM board_subscriptions WHERE board_id IN (` + strings.Join(ph, ",") + `)`
	watches := map[string][]watchRow{}
	muted := map[string]bool{}
	var order []string
	if rows, err := e.DB.QueryContext(e.Ctx, q, args...); err == nil {
		for rows.Next() {
			var uid string
			var w watchRow
			var m bool
			if rows.Scan(&uid, &w.via, &m, &w.list, &w.board) != nil {
				continue
			}
			if _, ok := watches[uid]; !ok {
				watches[uid] = nil
				order = append(order, uid)
			}
			if m {
				muted[uid] = true
				continue
			}
			watches[uid] = append(watches[uid], w)
		}
		rows.Close()
	}

	// one access check for everyone who might hear about it: user -> the boards of the card they can view
	cand := append([]string{}, mentions...)
	cand = append(cand, order...)
	viewers := authz.CardViewers(e, ev.CardID, cand)
	canOn := func(u, b string) bool {
		for _, x := range viewers[u] {
			if x == b {
				return true
			}
		}
		return false
	}
	// cardBoard is the board a notification about the card itself names: the event's board for a placement event
	// (the recipient must view it), else the first in-scope board the recipient can view (home first)
	cardBoard := func(u string) string {
		if placementOnly {
			if canOn(u, ev.BoardID) {
				return ev.BoardID
			}
			return ""
		}
		for _, b := range viewers[u] {
			if inScope[b] {
				return b
			}
		}
		return ""
	}
	// dataFor is the data of a notification that names board: the columns of a move only when it names the event's
	// board (a notification naming another board must not carry this board's column names, even to someone who can
	// view both: it travels under that board's name, to that board's project webhooks and services)
	dataFor := func(board string) map[string]any {
		nd := make(map[string]any, len(d)+4)
		for k, v := range d {
			nd[k] = v
		}
		if moveLists != nil && board == ev.BoardID {
			for k, v := range moveLists {
				nd[k] = v
			}
		}
		return nd
	}
	seen := map[string]bool{actor: true}
	if actor == "" {
		seen = map[string]bool{}
	}
	var out []planned
	for _, id := range mentions {
		if seen[id] {
			continue
		}
		if b := cardBoard(id); b != "" {
			seen[id] = true
			out = append(out, planned{id, "mentionInComment", commentID, b, dataFor(b)})
		}
	}
	listAt := func(id string) int {
		for i, l := range lists {
			if l.info.ID == id {
				return i
			}
		}
		return len(lists)
	}
	boardAt := func(id string) int {
		for i, b := range boards {
			if b == id {
				return i
			}
		}
		return len(boards)
	}
	for _, id := range order {
		if seen[id] || muted[id] {
			continue
		}
		// the most specific watch the user can still see: card > column (destination first, then the event's board)
		// > board (the event's board first)
		rank, at, via, board, list := 0, 0, "", "", -1
		for _, w := range watches[id] {
			r, a, b, li := viaRank[w.via], 0, "", -1
			switch w.via {
			case "card":
				b = cardBoard(id)
			case "list":
				li = listAt(w.list)
				if li < len(lists) && canOn(id, lists[li].board) {
					b, a = lists[li].board, li
				}
			case "board":
				if canOn(id, w.board) {
					b, a = w.board, boardAt(w.board)
				}
			}
			if b == "" {
				continue
			}
			if r > rank || (r == rank && a < at) {
				rank, at, via, board, list = r, a, w.via, b, li
			}
		}
		if rank == 0 {
			continue
		}
		seen[id] = true
		nd := dataFor(board)
		nd["via"] = via
		if via == "list" {
			nd["viaList"] = lists[list].info.ref()
		}
		out = append(out, planned{id, typ, commentID, board, nd})
	}
	return out, boards
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
		if n.Data["toList"] == nil { // a card-level move (archive, restore) told to someone who cannot see its columns
			sentence = func(a, c, b string, x ...string) string { return fmt.Sprintf("%s moved %s on %s", a, c, b) }
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
	plans, scope := planNotifications(e, ev, actor)
	if len(plans) == 0 {
		return
	}
	// the names of every board the notifications (and the board services) name, in one query
	boardName := map[string]string{}
	{
		var ids []any
		seen := map[string]bool{}
		for _, b := range append(append([]string{}, scope...), ev.BoardID) {
			if !seen[b] {
				seen[b] = true
				ids = append(ids, b)
			}
		}
		for _, p := range plans {
			if !seen[p.BoardID] {
				seen[p.BoardID] = true
				ids = append(ids, p.BoardID)
			}
		}
		if rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,name FROM boards WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, ids...); err == nil {
			for rows.Next() {
				var id, n string
				if rows.Scan(&id, &n) == nil {
					boardName[id] = n
				}
			}
			rows.Close()
		}
	}
	var cardName string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT name FROM cards WHERE id=?`, ev.CardID).Scan(&cardName)
	if actorName == "" && actor != "" {
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT name FROM users WHERE id=?`, actor).Scan(&actorName)
	}
	var created []NotificationOut
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		for _, p := range plans {
			n := NotificationOut{ID: db.NewID(), UserID: p.UserID, CreatorID: actor, CreatorName: actorName, BoardID: p.BoardID, BoardName: boardName[p.BoardID],
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
		// one recipient's notification: scoped to the board it names, never fanned out by card
		dispatchWebhooks(e, ops.Event{Type: "notificationCreate", BoardID: n.BoardID, CardID: ev.CardID, Data: map[string]any{"item": n}}, actor, nil, "")
		d, ctx := e.DB, context.Background()
		bg(func() { deliverToUser(ctx, d, n) })
	}
	// Board-level notification services only hear about new comments (Planka behaviour): a comment is card-level, so
	// each board the card is on tells its own channel once, under its own name.
	if ev.Type == "commentCreate" {
		for _, b := range scope {
			n := NotificationOut{CreatorName: actorName, BoardID: b, BoardName: boardName[b], CardID: ev.CardID, CardName: cardName, Type: "commentCard",
				Data: map[string]any{"text": str(ev.Data, "text")}}
			d, ctx, b := e.DB, context.Background(), b
			bg(func() { deliverToBoard(ctx, d, b, n) })
		}
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
