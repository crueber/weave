// Package events is the durable, replayable event stream behind GET /events.
//
// Every domain event that passes through notify.Record is stored on its `actions`
// row together with a monotonic sequence (seq = the public event id), routing
// columns (list_id, from_list_id, to_list_id) and the JSON envelope. SSE clients
// tail that table, so replay and live delivery are the same code path: same ids,
// strictly ordered, no gaps and no duplicates.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// Ref is a compact user/label reference inside a snapshot.
type Ref struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Color   string `json:"color,omitempty"`
	IsAgent bool   `json:"isAgent,omitempty"`
}

// CardSnap is the compact card snapshot shared by events, board.tree, card.list and card.next.
type CardSnap struct {
	ID                   string   `json:"id"`
	BoardID              string   `json:"boardId"`
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	DescriptionTruncated bool     `json:"descriptionTruncated,omitempty"`
	ListID               string   `json:"listId"`
	Position             *float64 `json:"position"`
	Labels               []Ref    `json:"labels"`
	Members              []Ref    `json:"members"`
	DueDate              *string  `json:"dueDate"`
	IsClosed             bool     `json:"isClosed"`
	ClaimedBy            *string  `json:"claimedBy"`
	ClaimedByName        *string  `json:"claimedByName,omitempty"`
	ClaimedUntil         *string  `json:"claimedUntil"`
	TasksTotal           int      `json:"tasksTotal"`
	TasksCompleted       int      `json:"tasksCompleted"`
	// AlsoOnBoards is per viewer (board.tree only, never in event envelopes): how many OTHER boards the viewer can
	// see this card on.
	AlsoOnBoards int `json:"alsoOnBoards,omitempty"`
}

// Actor is who caused an event.
type Actor struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsAgent bool   `json:"isAgent"`
}

// Envelope is the event as seen by SSE clients.
type Envelope struct {
	ID string `json:"id,omitempty"`
	// Origin identifies the logical event: an event about a card on several boards is stored once per board (one
	// envelope per board, each with its own id), and every copy carries the same origin. Consumers streaming several
	// boards dedupe by it. Envelopes stored before placements existed have none.
	Origin     string         `json:"origin,omitempty"`
	Type       string         `json:"type"`
	BoardID    string         `json:"boardId"`
	ListID     string         `json:"listId,omitempty"`
	FromListID string         `json:"fromListId,omitempty"`
	ToListID   string         `json:"toListId,omitempty"`
	CardID     string         `json:"cardId,omitempty"`
	Actor      Actor          `json:"actor"`
	Card       *CardSnap      `json:"card,omitempty"`
	Data       map[string]any `json:"data"`
	At         string         `json:"at"`
}

// Snapshot loads the compact snapshot of a card; installed by the cards domain.
// Returns nil when the card does not exist (deleted).
var Snapshot func(e *ops.Env, cardID string) *CardSnap

// Snapshots is the batch form of Snapshot (same order as ids, missing cards skipped).
var Snapshots func(e *ops.Env, ids []string) []CardSnap

// SnapshotOn is the snapshot of a card as seen on one board (its placement there: boardId, listId, position, and
// only the labels shown on that board); nil when the card is not placed on boardID. Snapshot and Snapshots keep
// meaning the home board. Installed by the cards domain.
var SnapshotOn func(e *ops.Env, cardID, boardID string) *CardSnap

// SnapshotsOn is the batch form of SnapshotOn (same order as ids, cards not placed on boardID skipped).
var SnapshotsOn func(e *ops.Env, boardID string, ids []string) []CardSnap

// Placement is one board a card is on, as the event fan-out needs it.
type Placement struct {
	BoardID string
	ListID  string
	IsHome  bool
}

// Placements lists the placements of a card (home first, then by placed_at; empty when the card does not exist);
// installed by the cards domain. notify.Record uses it to find the boards a card-level event concerns.
var Placements func(e *ops.Env, cardID string) []Placement

// CardOn is the full card payload (data.card of card events: the cards.Card shape) as seen on one board: that
// placement's column and position, only that board's labels, and none of the per-viewer fields (watch state,
// placements, share token). nil when the card is not placed on boardID. No access check: the payload goes to that
// board's room, activity row and webhooks. Installed by the cards domain.
var CardOn func(e *ops.Env, cardID, boardID string) any

// Event types of placements (phase 4 ops `card.place` / `card.unplace` emit them; defined here so the fan-out and
// webhook scope rules exist before any op can create a second placement).
const (
	TypePlace        = "cardPlace"              // activity type: the card was placed on ev.BoardID
	TypeUnplace      = "cardUnplace"            // activity type: the card was removed from ev.BoardID
	StreamPlaced     = "card.placed"            // stream type of TypePlace (agents treat it as an arrival on that board)
	StreamUnplaced   = "card.unplaced"          // stream type of TypeUnplace
	PlacementChanged = "card.placement.changed" // unstored realtime nudge to the card's OTHER boards: {cardId} only
)

// CardLevel is the routing key (ops.Event.Data) of a card event that moves EVERY placement of the card: card.archive
// (archive or trash) and card.restore set it to true. Routing only: notify strips it from the stored data, the room
// publishes and webhook payloads, and the envelope never carries it.
const CardLevel = "cardLevel"

// PlacementLists is the routing key (ops.Event.Data, map[string][2]string: board id -> [from, to] column of that
// board's placement) of a card-level move (card.archive, card.restore): the fan-out gives each board's copy its own
// fromListId/toListId, so a lists= filter or the column activity of the column a placement LEFT on another board
// sees the move. Never stored, published or sent (notify strips it like CardLevel).
const PlacementLists = "placementLists"

// PlacementScoped reports whether a card event concerns ONE placement (scope = ev.BoardID only) rather than the card
// (scope = every board the card is on). Placement events: a card created in a column; a card.moved that moved one
// placement (card.move within a board or to another board, a column moved, archived or deleted with its cards, a
// column's archive-after rule); a card placed on or removed from a board. A card.moved that carries CardLevel
// (card.archive, card.restore: they move every placement) is card-level, and so is everything else (fields, closed
// state, comments, members, labels' card rows, tasks, claims, delete). An event that lists its boards explicitly
// (ops.Event.Boards) is fanned out to that list, whatever its type; this still decides whether the card's other
// boards get the card.placement.changed nudge and who the notification planner considers.
func PlacementScoped(ev ops.Event) bool {
	switch ev.Type {
	case "cardCreate", TypePlace, TypeUnplace:
		return true
	case "cardMove":
		if fb := sval(ev.Data, "fromBoardId"); fb != "" && fb != ev.BoardID {
			return true // a cross-board relocation of one placement
		}
		all, _ := ev.Data[CardLevel].(bool)
		return !all
	}
	return false
}

// TaskCounts returns {total, completed} checklist task counts per card id (cards without tasks
// may be absent) in one aggregate query; installed by the cardextras domain, which owns tasks.
var TaskCounts func(e *ops.Env, cardIDs []string) map[string][2]int

// FormatID renders an event sequence as the public id (zero padded so string and
// numeric order agree). ParseID accepts padded and plain decimals.
func FormatID(seq int64) string { return fmt.Sprintf("%012d", seq) }

func ParseID(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid event id %q", s)
	}
	return n, nil
}

// Types whose realtime name is not already the stream type.
var typeOf = map[string]string{
	"card.member.added":   "member.added",
	"card.member.removed": "member.removed",
	"card.label.added":    "label.added",
	"card.label.removed":  "label.removed",
	"list.moved":          "list.updated",
}

// TypeFor maps an op event to the public stream type.
func TypeFor(ev ops.Event) string {
	if ev.Kind != "" {
		return ev.Kind
	}
	if t, ok := typeOf[ev.Name]; ok {
		return t
	}
	if ev.Name != "" {
		return ev.Name
	}
	return ev.Type
}

func sval(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

var noise = []string{"card", "actionId", "originId", "actionAt", CardLevel, PlacementLists, "cardId", "boardId", "fromListId", "toListId", "listId"}

// Build assembles the envelope for an op event (call after the mutation committed). For a card event the card is
// the snapshot on ev.BoardID (notify.Record calls Build once per board of the event, with that board's event).
// Origin is ev.Data["originId"] (a fan-out copy) or else ev.Data["actionId"] (the origin itself).
func Build(e *ops.Env, ev ops.Event) Envelope {
	env := Envelope{Type: TypeFor(ev), BoardID: ev.BoardID, CardID: ev.CardID, At: db.Now(), Actor: Actor{Name: "system"}}
	if env.Origin = sval(ev.Data, "originId"); env.Origin == "" {
		env.Origin = sval(ev.Data, "actionId")
	}
	if at := sval(ev.Data, "actionAt"); at != "" {
		env.At = at // every copy of one logical event has the same time
	}
	if e.User != nil {
		env.Actor = Actor{ID: e.User.UserID, Name: e.User.Name, IsAgent: e.User.IsAgent}
	}
	d := ev.Data
	env.FromListID, env.ToListID = sval(d, "fromListId"), sval(d, "toListId")
	env.ListID = sval(d, "listId")
	switch {
	case ev.CardID != "" && ev.BoardID != "" && SnapshotOn != nil:
		// the card as seen on the event's board; nil when it is not placed there (e.g. the source-board "deleted"
		// half of a cross-board move): never expose another board's placement
		env.Card = SnapshotOn(e, ev.CardID, ev.BoardID)
	case ev.CardID != "" && Snapshot != nil:
		env.Card = Snapshot(e, ev.CardID)
		if env.Card != nil && ev.BoardID != "" && env.Card.BoardID != ev.BoardID {
			env.Card = nil
		}
	}
	if env.ListID == "" && env.Card != nil {
		env.ListID = env.Card.ListID
	}
	if env.ToListID != "" {
		env.ListID = env.ToListID
	}
	if strings.HasPrefix(env.Type, "list.") && ev.CardID == "" {
		env.ListID = sval(d, "id")
	}
	if env.Card != nil && env.BoardID == "" {
		env.BoardID = env.Card.BoardID
	}
	env.Data = map[string]any{}
	for k, v := range d {
		skip := false
		for _, n := range noise {
			if k == n {
				skip = true
			}
		}
		if k == "id" && ev.CardID != "" && v == ev.CardID {
			skip = true // card events: the card id is already cardId
		}
		if !skip {
			env.Data[k] = v
		}
	}
	return env
}

// Marshal renders the stored form: the envelope without id (the id is the row's seq).
func (en Envelope) Marshal() string {
	en.ID = ""
	b, _ := json.Marshal(en)
	return string(b)
}

// Wire renders a stored envelope for the wire, injecting the id.
func Wire(seq int64, stored string) string {
	if !strings.HasPrefix(stored, "{") {
		return stored
	}
	return `{"id":"` + FormatID(seq) + `",` + stored[1:]
}

// Row is one stored event.
type Row struct {
	Seq     int64
	BoardID string
	Type    string
	Raw     string // stored envelope (no id)
}

func (r Row) JSON() string { return Wire(r.Seq, r.Raw) }

// Filter selects events. Empty sets mean "no restriction".
type Filter struct {
	Since  int64
	Boards []string
	Lists  []string
	Types  []string
	Limit  int
}

func in(col string, vals []string, args *[]any) string {
	ph := make([]string, len(vals))
	for i, v := range vals {
		ph[i] = "?"
		*args = append(*args, v)
	}
	return col + " IN (" + strings.Join(ph, ",") + ")"
}

// Query returns events with seq > f.Since in order.
func Query(ctx context.Context, d *db.DB, f Filter) ([]Row, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 500
	}
	args := []any{f.Since}
	q := `SELECT seq,board_id,etype,envelope FROM actions WHERE seq > ? AND envelope IS NOT NULL `
	if len(f.Boards) > 0 {
		q += `AND ` + in("board_id", f.Boards, &args) + ` `
	}
	if len(f.Lists) > 0 {
		q += `AND (` + in("list_id", f.Lists, &args) + ` OR ` + in("from_list_id", f.Lists, &args) + ` OR ` + in("to_list_id", f.Lists, &args) + `) `
	}
	if len(f.Types) > 0 {
		q += `AND ` + in("etype", f.Types, &args) + ` `
	}
	q += `ORDER BY seq LIMIT ?`
	args = append(args, f.Limit)
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Seq, &r.BoardID, &r.Type, &r.Raw); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Recent returns the newest stored events that touch one list (as the list, the source or the
// target of a move), newest first. before > 0 pages: only events with seq < before. Only the
// given event types are returned (empty = all). Events past the retention window (see Purge)
// have no envelope any more and are not returned.
//
// Quiet per-card events (data.quiet = true: bulk side effects such as a column's auto-archive pass) are left out; each
// bulk change records its own summary event on the column (list.updated), which is what the feed shows.
func Recent(ctx context.Context, d *db.DB, boardID, listID string, types []string, before int64, limit int) ([]Row, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args := []any{boardID, listID, listID, listID}
	q := `SELECT seq,board_id,etype,envelope FROM actions WHERE board_id=? AND seq IS NOT NULL AND envelope IS NOT NULL AND (list_id=? OR from_list_id=? OR to_list_id=?)
		AND (card_id IS NULL OR json_extract(envelope,'$.data.quiet') IS NOT 1) `
	if len(types) > 0 {
		q += `AND ` + in("etype", types, &args) + ` `
	}
	if before > 0 {
		q += `AND seq < ? `
		args = append(args, before)
	}
	q += `ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Seq, &r.BoardID, &r.Type, &r.Raw); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastSeq is the id of the newest stored event (0 when none).
func LastSeq(ctx context.Context, d *db.DB) int64 {
	var n *int64
	_ = d.QueryRowContext(ctx, `SELECT MAX(seq) FROM actions WHERE seq IS NOT NULL`).Scan(&n)
	if n == nil {
		return 0
	}
	return *n
}

// ListBoards maps list ids to their board ids (unknown ids are absent).
func ListBoards(ctx context.Context, d *db.DB, listIDs []string) map[string]string {
	out := map[string]string{}
	if len(listIDs) == 0 {
		return out
	}
	var args []any
	rows, err := d.QueryContext(ctx, `SELECT id,board_id FROM lists WHERE `+in("id", listIDs, &args), args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var l, b string
		if rows.Scan(&l, &b) == nil {
			out[l] = b
		}
	}
	return out
}

// Purge drops stream payloads older than the retention window. The activity row and
// its seq stay so ids are never reused.
func Purge(ctx context.Context, d *db.DB, keep time.Duration) (int64, error) {
	cut := time.Now().UTC().Add(-keep).Format(time.RFC3339)
	res, err := d.ExecContext(ctx, `UPDATE actions SET envelope=NULL WHERE envelope IS NOT NULL AND created_at < ?`, cut)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
