package cardextras

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// querier is satisfied by *sql.Tx and *db.DB.
type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// DeletedOut is returned by delete ops; CardID lets the UI refresh the right fragment.
type DeletedOut struct {
	ID     string `json:"id"`
	CardID string `json:"cardId"`
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strp(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

// dataDir resolves the data directory: Svc["dataDir"], else the process config, else ./data.
func dataDir(e *ops.Env) string {
	if s, ok := e.Svc["dataDir"].(string); ok && s != "" {
		return s
	}
	if c, ok := e.Svc["config"].(config.Config); ok && c.DataDir != "" {
		return c.DataDir
	}
	return "./data"
}

func uid(e *ops.Env) string {
	if e.User == nil {
		return ""
	}
	return e.User.UserID
}

// emit records an activity row (through the notify hook when installed, else directly)
// and publishes the realtime event.
func emit(e *ops.Env, typ, boardID, cardID, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if cardID != "" {
		data["cardId"] = cardID
	}
	ev := ops.Event{Type: typ, BoardID: boardID, CardID: cardID, Name: name, Data: data}
	if ops.EmitHook != nil {
		ops.Emit(e, ev)
		return
	}
	_, _ = RecordAction(e, ev)
	if e.Hub != nil && boardID != "" {
		e.Hub.Publish(realtime.Event{Name: name, Room: "board:" + boardID, Data: data})
	}
}

// RecordAction writes one activity (actions) row for ev and returns its id. It is
// called by the notify domain's ops.EmitHook for every domain event.
func RecordAction(e *ops.Env, ev ops.Event) (string, error) {
	if ev.BoardID == "" {
		return "", nil
	}
	data := "{}"
	if ev.Data != nil {
		if b, err := json.Marshal(ev.Data); err == nil {
			data = string(b)
		}
	}
	id := db.NewID()
	_, err := e.DB.ExecContext(e.Ctx, `INSERT INTO actions(id,board_id,card_id,user_id,type,data,created_at) VALUES(?,?,?,?,?,?,?)`,
		id, ev.BoardID, nz(ev.CardID), nz(uid(e)), ev.Type, data, db.Now())
	return id, err
}

// place computes the REAL position for an item dropped between afterID and beforeID
// within a scope (table + where clause). ok=false means "no repositioning requested".
func place(ctx context.Context, q querier, table, scope string, scopeArgs []any, afterID, beforeID string, explicit *float64) (pos float64, ok bool, err error) {
	if explicit != nil {
		return *explicit, true, nil
	}
	if afterID == "" && beforeID == "" {
		return 0, false, nil
	}
	get := func(id string) (float64, error) {
		var p float64
		args := append([]any{id}, scopeArgs...)
		if err := q.QueryRowContext(ctx, "SELECT position FROM "+table+" WHERE id=? AND "+scope, args...).Scan(&p); err != nil {
			return 0, ops.Invalid("neighbour " + id + " not found in this container")
		}
		return p, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		var p, n float64
		if afterID != "" {
			if p, err = get(afterID); err != nil {
				return
			}
		}
		if beforeID != "" {
			if n, err = get(beforeID); err != nil {
				return
			}
		}
		switch {
		case afterID != "" && beforeID != "":
			if d := n - p; d < 1 && d > -1 {
				if err = rebalance(ctx, q, table, scope, scopeArgs); err != nil {
					return
				}
				continue
			}
			return (p + n) / 2, true, nil
		case afterID != "":
			return p + db.PositionGap, true, nil
		default:
			return n - db.PositionGap, true, nil
		}
	}
	return 0, false, fmt.Errorf("reposition failed")
}

func rebalance(ctx context.Context, q querier, table, scope string, scopeArgs []any) error {
	rows, err := q.QueryContext(ctx, "SELECT id FROM "+table+" WHERE "+scope+" ORDER BY position", scopeArgs...)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		if _, err := q.ExecContext(ctx, "UPDATE "+table+" SET position=? WHERE id=?", float64(i+1)*db.PositionGap, id); err != nil {
			return err
		}
	}
	return nil
}

func endPosition(ctx context.Context, q querier, table, scope string, args ...any) float64 {
	var m sql.NullFloat64
	_ = q.QueryRowContext(ctx, "SELECT MAX(position) FROM "+table+" WHERE "+scope, args...).Scan(&m)
	return m.Float64 + db.PositionGap
}

func query[T any](ctx context.Context, q querier, scan func(*sql.Rows) (T, error), sqlStr string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanString(r *sql.Rows) (string, error) { var s string; return s, r.Scan(&s) }

func humanize(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) && i > 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
