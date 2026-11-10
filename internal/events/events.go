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
}

// Actor is who caused an event.
type Actor struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsAgent bool   `json:"isAgent"`
}

// Envelope is the event as seen by SSE clients.
type Envelope struct {
	ID         string         `json:"id,omitempty"`
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

var noise = []string{"card", "actionId", "cardId", "boardId", "fromListId", "toListId", "listId"}

// Build assembles the envelope for an op event (call after the mutation committed).
func Build(e *ops.Env, ev ops.Event) Envelope {
	env := Envelope{Type: TypeFor(ev), BoardID: ev.BoardID, CardID: ev.CardID, At: db.Now(), Actor: Actor{Name: "system"}}
	if e.User != nil {
		env.Actor = Actor{ID: e.User.UserID, Name: e.User.Name, IsAgent: e.User.IsAgent}
	}
	d := ev.Data
	env.FromListID, env.ToListID = sval(d, "fromListId"), sval(d, "toListId")
	env.ListID = sval(d, "listId")
	if ev.CardID != "" && Snapshot != nil {
		env.Card = Snapshot(e, ev.CardID)
		if env.Card != nil && ev.BoardID != "" && env.Card.BoardID != ev.BoardID {
			env.Card = nil // e.g. the source-board "deleted" half of a cross-board move: never expose the other board's card
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
