package cards

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
	"github.com/crueber/weave/internal/server"
)

// Recurrence is the JSON stored in cards.recurrence.
type Recurrence struct {
	Freq     string `json:"freq" desc:"daily | weekly | monthly | yearly"`
	Interval int    `json:"interval,omitempty"`
	Trigger  string `json:"trigger,omitempty"`  // due (default) | completed
	Weekdays []int  `json:"weekdays,omitempty"` // 0=Sunday..6, weekly only
	Until    string `json:"until,omitempty"`    // no occurrences after this date
	Count    int    `json:"count,omitempty"`    // remaining occurrences to create; 0 = unlimited
	ListID   string `json:"listId,omitempty"`   // list for spawned cards (same board); default same list
}

type RecurrenceIn struct {
	ID         string `path:"id"`
	Recurrence string `json:"recurrence" desc:"JSON {freq,interval,trigger,weekdays,until,count,listId}; empty string clears"`
}

type RecurrenceRunOut struct {
	NextCardID string `json:"nextCardId"`
}

func init() {
	ops.Register(ops.Op{Name: "card.recurrence.set", Summary: "Set or clear the card's recurrence rule (requires a due date)", Method: "PUT", Path: "/cards/{id}/recurrence",
		UI: "POST /ui/cards/{id}/recurrence"}, recurrenceSet)
	ops.Register(ops.Op{Name: "card.recurrence.run", Summary: "Create the next occurrence of a recurring card now", Method: "POST", Path: "/cards/{id}/recurrence/run",
		UI: "POST /ui/cards/{id}/recurrence/run"}, func(e *ops.Env, in IDIn) (RecurrenceRunOut, error) {
		if _, err := editCard(e, in.ID); err != nil {
			return RecurrenceRunOut{}, err
		}
		id, err := spawnNext(e, in.ID)
		return RecurrenceRunOut{id}, err
	})

	server.Hooks = append(server.Hooks, func(s *server.Server) error {
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for range t.C {
				_, _ = RunRecurrence(context.Background(), s.DB, s.Hub, time.Now())
			}
		}()
		return nil
	})
}

const (
	maxRecurInterval = 1000
	maxRecurCount    = 10000
)

func parseRecurrence(s *string) *Recurrence {
	if s == nil || *s == "" {
		return nil
	}
	var r Recurrence
	if json.Unmarshal([]byte(*s), &r) != nil || r.Freq == "" {
		return nil
	}
	if r.Interval < 1 {
		r.Interval = 1
	}
	if r.Interval > maxRecurInterval { // rows predating the cap
		r.Interval = maxRecurInterval
	}
	if r.Trigger == "" {
		r.Trigger = "due"
	}
	return &r
}

func recurrenceSet(e *ops.Env, in RecurrenceIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	c, err := loadCard(e, e.DB, in.ID)
	if err != nil {
		return c, err
	}
	var v any
	if in.Recurrence != "" {
		var r Recurrence
		if err := json.Unmarshal([]byte(in.Recurrence), &r); err != nil {
			return c, ops.Invalid("recurrence must be valid JSON")
		}
		switch r.Freq {
		case "daily", "weekly", "monthly", "yearly":
		default:
			return c, ops.Invalid("freq must be daily, weekly, monthly or yearly")
		}
		if r.Interval < 0 || r.Count < 0 {
			return c, ops.Invalid("interval and count must be positive")
		}
		if r.Interval > maxRecurInterval || r.Count > maxRecurCount {
			return c, ops.Invalid("interval must be at most 1000 and count at most 10000")
		}
		if r.Trigger != "" && r.Trigger != "due" && r.Trigger != "completed" {
			return c, ops.Invalid("trigger must be due or completed")
		}
		for _, d := range r.Weekdays {
			if d < 0 || d > 6 {
				return c, ops.Invalid("weekdays must be 0..6")
			}
		}
		if r.Until != "" {
			u, err := normTime(r.Until)
			if err != nil {
				return c, err
			}
			r.Until = u
		}
		if r.ListID != "" {
			if b, t, err := listRef(e.Ctx, e.DB, r.ListID); err != nil || b != boardID || (t != "active" && t != "closed") {
				return c, ops.Invalid("listId must be an active list of the same board")
			}
		}
		if c.DueDate == nil {
			return c, ops.Invalid("set a due date before adding a recurrence")
		}
		b, _ := json.Marshal(r)
		v = string(b)
	}
	if _, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET recurrence=?,updated_at=? WHERE id=?`, v, db.Now(), in.ID); err != nil {
		return c, err
	}
	c, err = loadCard(e, e.DB, in.ID)
	if err == nil {
		emit(e, "cardUpdate", "card.updated", boardID, in.ID, map[string]any{"card": c})
	}
	return c, err
}

// nextDue computes the occurrence after prev.
func nextDue(prev time.Time, r *Recurrence) time.Time {
	n := min(max(r.Interval, 1), maxRecurInterval)
	switch r.Freq {
	case "daily":
		return prev.AddDate(0, 0, n)
	case "weekly":
		if len(r.Weekdays) == 0 {
			return prev.AddDate(0, 0, 7*n)
		}
		// Only the rest of prev's own week and the n-th following week qualify, so at most
		// 14 candidate days need checking (bounded regardless of the interval).
		wd := int(prev.Weekday())
		for _, rng := range [][2]int{{1, 6 - wd}, {7*n - wd, 7*n - wd + 6}} {
			for i := rng[0]; i <= rng[1]; i++ {
				d := prev.AddDate(0, 0, i)
				for _, w := range r.Weekdays {
					if int(d.Weekday()) == w {
						return d
					}
				}
			}
		}
		return prev.AddDate(0, 0, 7*n)
	case "monthly":
		return addMonths(prev, n)
	default:
		return addMonths(prev, 12*n)
	}
}

// addMonths adds months clamping the day to the target month's length (Jan 31 + 1m = Feb 28).
func addMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(n), 1, t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if d > last {
		d = last
	}
	return time.Date(first.Year(), first.Month(), d, t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

// spawnNext creates the next occurrence of a recurring card and moves the rule to it.
func spawnNext(e *ops.Env, cardID string) (string, error) { return spawnNextAt(e, cardID, time.Now()) }

func spawnNextAt(e *ops.Env, cardID string, now time.Time) (string, error) {
	src, err := loadCard(e, e.DB, cardID)
	if err != nil {
		return "", err
	}
	r := parseRecurrence(src.Recurrence)
	if r == nil || src.DueDate == nil {
		return "", ops.Invalid("card has no recurrence rule")
	}
	prev, err := parseTime(*src.DueDate)
	if err != nil {
		return "", err
	}
	now = now.UTC()
	next := nextDue(prev, r)
	for i := 0; i < 1000 && !next.After(now); i++ { // skip occurrences already in the past
		next = nextDue(next, r)
	}
	// the old card gives up its rule either way, so it never spawns twice
	var newID string
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET recurrence=NULL,updated_at=? WHERE id=? AND recurrence IS NOT NULL`, db.Now(), cardID); err != nil {
			return err
		}
		if r.Until != "" {
			if u, err := parseTime(r.Until); err == nil && next.After(u) {
				return nil
			}
		}
		nr := *r
		var rec *string
		if r.Count != 1 { // count = occurrences still to create; the last one carries no rule
			if r.Count > 1 {
				nr.Count = r.Count - 1
			}
			b, _ := json.Marshal(nr)
			s := string(b)
			rec = &s
		}
		list := src.ListID
		if r.ListID != "" {
			if b, t, err := listRef(e.Ctx, tx, r.ListID); err == nil && b == src.BoardID && (t == "active" || t == "closed") {
				list = r.ListID
			}
		} else if t := listType(e.Ctx, tx, src.ListID); t != "active" && t != "closed" {
			if l := firstActive(e.Ctx, tx, src.BoardID); l != "" {
				list = l
			}
		}
		due := next.Format(time.RFC3339)
		pos := positionFor(e.Ctx, tx, list, "", "", "", nil)
		newID, err = cloneCard(e, tx, src, cloneOpts{Name: src.Name, ListID: list, Position: pos, DueDate: &due, Recurrence: rec})
		return err
	})
	if err != nil {
		return "", err
	}
	if newID != "" {
		if c, err := loadCard(e, e.DB, newID); err == nil {
			emit(e, "cardCreate", "card.created", c.BoardID, newID, map[string]any{"card": c, "listId": c.ListID, "recurrenceOf": cardID})
		}
	}
	if cur, err := loadCard(e, e.DB, cardID); err == nil {
		emit(e, "cardUpdate", "card.updated", cur.BoardID, cardID, map[string]any{"card": cur})
	}
	return newID, nil
}

func listType(ctx context.Context, q querier, id string) (t string) {
	_ = q.QueryRowContext(ctx, `SELECT type FROM lists WHERE id=?`, id).Scan(&t)
	return
}

func firstActive(ctx context.Context, q querier, boardID string) (id string) {
	_ = q.QueryRowContext(ctx, `SELECT id FROM lists WHERE board_id=? AND type='active' ORDER BY position LIMIT 1`, boardID).Scan(&id)
	return
}

// RunRecurrence spawns the next occurrence of every recurring card whose trigger
// fired by `now` (due date reached, or due completed). It returns how many cards
// were created. The server scheduler calls it every minute; it is idempotent
// because the spawning card gives up its rule.
func RunRecurrence(ctx context.Context, d *db.DB, hub *realtime.Hub, now time.Time) (int, error) {
	rows, err := d.QueryContext(ctx, `SELECT c.id,c.recurrence,c.due_date,COALESCE(c.is_due_completed,0) FROM cards c JOIN lists l ON l.id=c.list_id
		WHERE c.recurrence IS NOT NULL AND c.is_template=0 AND c.is_draft=0 AND c.due_date IS NOT NULL AND l.type IN ('active','closed')`)
	if err != nil {
		return 0, err
	}
	var due []string
	for rows.Next() {
		var id, rec, dd string
		var done bool
		if rows.Scan(&id, &rec, &dd, &done) != nil {
			continue
		}
		r := parseRecurrence(&rec)
		t, err := parseTime(dd)
		if r == nil || err != nil {
			continue
		}
		if (r.Trigger == "completed" && done) || (r.Trigger == "due" && !t.After(now)) {
			due = append(due, id)
		}
	}
	rows.Close()
	e := &ops.Env{Ctx: ctx, DB: d, Hub: hub, Svc: map[string]any{}}
	n := 0
	for _, id := range due {
		if nid, err := spawnNextAt(e, id, now); err == nil && nid != "" {
			n++
		}
	}
	return n, nil
}
