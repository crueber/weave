package boards

import (
	"context"
	"database/sql"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// Colors are the label / list colour names accepted by Weave (Planka palette).
var Colors = []string{"muddy-grey", "autumn-leafs", "morning-sky", "antique-blue", "egg-yellow", "desert-sand", "dark-granite",
	"fresh-salad", "lagoon-blue", "midnight-blue", "light-orange", "pumpkin-orange", "light-concrete", "sunny-grass", "navy-blue",
	"lilac-eyes", "apricot-red", "orange-peel", "silver-glint", "bright-moss", "deep-ocean", "summer-sky", "berry-red", "light-cocoa",
	"grey-stone", "tank-green", "coral-green", "sugar-plum", "pink-tulip", "shady-rust", "wet-rock", "wet-moss", "turquoise-sea",
	"lavender-fields", "piggy-red", "light-mud", "gun-metal", "modern-green", "french-coast", "sweet-lilac", "red-burgundy", "pirate-gold"}

// validColor accepts exactly #rrggbb, a Planka palette name or a Weave preset name.
func validColor(c string) bool {
	_, ok := ResolveColor(c)
	return ok
}

// ui builds the browser route string for an op: the UI mirrors REST under /ui.
func ui(method, path string) string { return method + " /ui" + path }

// emit publishes a realtime event (and activity/notifications when the notify
// hook is installed). Never call inside a DB transaction.
func emit(e *ops.Env, typ, name, boardID string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if ops.EmitHook != nil {
		ops.Emit(e, ops.Event{Type: typ, BoardID: boardID, Name: name, Data: data})
		return
	}
	e.Hub.Publish(realtime.Event{Name: name, Room: "board:" + boardID, Data: data})
}

// emitCard publishes a card-scoped event raised by a list operation (bulk moves,
// clears). kind overrides the event-stream type (e.g. card.archived).
func emitCard(e *ops.Env, typ, name, kind, boardID, cardID string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["id"] = cardID
	if ops.EmitHook != nil {
		ops.Emit(e, ops.Event{Type: typ, Kind: kind, BoardID: boardID, CardID: cardID, Name: name, Data: data})
		return
	}
	e.Hub.Publish(realtime.Event{Name: name, Room: "board:" + boardID, Data: data})
}

func strs(e *ops.Env, q string, args ...any) []string {
	var out []string
	rows, err := e.DB.QueryContext(e.Ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			out = append(out, s)
		}
	}
	return out
}

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func maxPos(ctx context.Context, q querier, table, scope string, args ...any) float64 {
	var m sql.NullFloat64
	_ = q.QueryRowContext(ctx, `SELECT MAX(position) FROM `+table+` WHERE `+scope, args...).Scan(&m)
	if !m.Valid {
		return 0
	}
	return m.Float64
}

// appendPos returns the position for a new row at the end of a scope.
func appendPos(ctx context.Context, q querier, table, scope string, args ...any) float64 {
	return maxPos(ctx, q, table, scope, args...) + db.PositionGap
}

func posOf(ctx context.Context, q querier, table, id string) (float64, bool) {
	if id == "" {
		return 0, false
	}
	var p sql.NullFloat64
	if q.QueryRowContext(ctx, `SELECT position FROM `+table+` WHERE id=?`, id).Scan(&p) != nil || !p.Valid {
		return 0, false
	}
	return p.Float64, true
}

// PlaceBetween computes a position for a row dropped between afterID and beforeID
// (either may be empty) inside a scope (SQL predicate on table). It rebalances
// the scope when the gap collapses. Exported for the cards domain.
func PlaceBetween(ctx context.Context, q querier, table, scope string, scopeArgs []any, afterID, beforeID string) float64 {
	for attempt := 0; attempt < 2; attempt++ {
		a, hasA := posOf(ctx, q, table, afterID)
		b, hasB := posOf(ctx, q, table, beforeID)
		switch {
		case hasA && hasB:
			if b-a >= 1 {
				return (a + b) / 2
			}
		case hasA:
			return a + db.PositionGap
		case hasB:
			if b >= 2 {
				return b / 2
			}
			return b - db.PositionGap
		default:
			return appendPos(ctx, q, table, scope, scopeArgs...)
		}
		Rebalance(ctx, q, table, scope, scopeArgs)
	}
	return appendPos(ctx, q, table, scope, scopeArgs...)
}

// Rebalance renumbers all rows in a scope with PositionGap spacing, keeping order.
func Rebalance(ctx context.Context, q querier, table, scope string, args []any) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM `+table+` WHERE `+scope+` ORDER BY position, id`, args...)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		_, _ = q.ExecContext(ctx, `UPDATE `+table+` SET position=? WHERE id=?`, float64(i+1)*db.PositionGap, id)
	}
}

// SingleSelectConflicts returns the ids of labels already on the card that
// share a single-select label group with labelID; the cards domain must remove
// them when adding labelID.
func SingleSelectConflicts(ctx context.Context, d *db.DB, cardID, labelID string) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT cl.label_id FROM card_labels cl JOIN labels l ON l.id=cl.label_id
		WHERE cl.card_id=? AND cl.label_id<>? AND l.group_id IS NOT NULL AND l.group_id=(SELECT g.id FROM labels x JOIN label_groups g ON g.id=x.group_id
		WHERE x.id=? AND g.single_select=1)`, cardID, labelID, labelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out, rows.Err()
}

func levelName(l authz.Level) string {
	switch l {
	case authz.View:
		return "view"
	case authz.Comment:
		return "comment"
	case authz.Edit:
		return "edit"
	case authz.Manage:
		return "manage"
	}
	return "none"
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Deleted is the output of delete-style ops.
type Deleted struct {
	ID string `json:"id"`
}

func init() {
	registerBoardOps()
	registerMemberOps()
	registerLabelOps()
	registerListOps()
	registerFilterOps()
	registerImportExportOps()
	registerUI()
}
