package cardextras

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
)

type ActionItem struct {
	ID        string          `json:"id"`
	BoardID   string          `json:"boardId"`
	CardID    *string         `json:"cardId"`
	CardName  *string         `json:"cardName"`
	UserID    *string         `json:"userId"`
	UserName  *string         `json:"userName"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	CreatedAt string          `json:"createdAt"`
}

type ActionsIn struct {
	BoardID string `path:"boardId"`
	Before  string `query:"before" desc:"Action id cursor: return actions older than this one"`
	Limit   int    `query:"limit" desc:"Page size (default 50, max 200)"`
}
type CardActionsIn struct {
	CardID  string `path:"cardId"`
	BoardID string `query:"boardId" desc:"Context board when the card is on several boards: an event recorded on several boards is listed once, preferably as recorded on this board"`
	Before  string `query:"before" desc:"Action id cursor: return actions older than this one"`
	Limit   int    `query:"limit" desc:"Page size (default 50, max 200)"`
}

const actionSelect = `SELECT a.id,a.board_id,a.card_id,c.name,a.user_id,u.name,a.type,a.data,a.created_at FROM actions a
	LEFT JOIN users u ON u.id=a.user_id LEFT JOIN cards c ON c.id=a.card_id`

func scanAction(r *sql.Rows) (ActionItem, error) {
	var a ActionItem
	var card, cn, user, un sql.NullString
	var data string
	err := r.Scan(&a.ID, &a.BoardID, &card, &cn, &user, &un, &a.Type, &data, &a.CreatedAt)
	a.CardID, a.CardName, a.UserID, a.UserName = strp(card), strp(cn), strp(user), strp(un)
	if !json.Valid([]byte(data)) {
		data = "{}"
	}
	a.Data = json.RawMessage(data)
	return a, err
}

func listActions(e *ops.Env, where string, args []any, before string, limit int) ([]ActionItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := actionSelect + ` WHERE ` + where
	if before != "" {
		q += ` AND (a.created_at,a.id) < (SELECT created_at,id FROM actions WHERE id=?)`
		args = append(args, before)
	}
	q += ` ORDER BY a.created_at DESC, a.id DESC LIMIT ?`
	args = append(args, limit)
	return query(e.Ctx, e.DB, scanAction, q, args...)
}

// ActionText renders a short human sentence fragment for an activity row.
func ActionText(a ActionItem) string {
	var d map[string]any
	_ = json.Unmarshal(a.Data, &d)
	name, _ := d["name"].(string)
	verbs := map[string]string{
		"taskListCreate": "added a task list", "taskListUpdate": "updated a task list", "taskListDelete": "deleted a task list",
		"taskCreate": "added task", "taskUpdate": "updated task", "taskComplete": "completed task", "taskReopen": "reopened task", "taskDelete": "deleted a task",
		"commentCreate": "commented", "commentUpdate": "edited a comment", "commentDelete": "deleted a comment",
		"attachmentCreate": "attached", "attachmentUpdate": "updated attachment", "attachmentDelete": "removed attachment", "attachmentCover": "changed the cover",
		"customFieldGroupCreate": "added a field group", "customFieldGroupUpdate": "updated a field group", "customFieldGroupDelete": "removed a field group",
		"customFieldCreate": "added a field", "customFieldUpdate": "updated a field", "customFieldDelete": "removed a field",
		"customFieldValueUpdate": "set a field value", "customFieldValueDelete": "cleared a field value",
		"stopwatchStart": "started the stopwatch", "stopwatchStop": "stopped the stopwatch", "stopwatchReset": "reset time tracking",
		"timeEntryCreate": "logged time", "timeEntryUpdate": "edited a time entry", "timeEntryDelete": "deleted a time entry",
		"createCard": "created the card", "cardClaim": "claimed the card", "cardRelease": "released the card", "moveCard": "moved the card",
	}
	s, ok := verbs[a.Type]
	if !ok {
		s = humanize(a.Type)
	}
	if auto, _ := d["auto"].(string); auto == "archiveAfter" && a.Type == "cardMove" {
		s = "auto-archived the card"
		if k, _ := d["keep"].(float64); k > 0 {
			s += ": its column keeps the newest " + strconv.Itoa(int(k)) + " cards"
		}
		return s
	}
	if auto, _ := d["auto"].(string); auto == "applyTemplate" && a.Type == "cardMove" {
		return "moved the card into the first column of an applied board template"
	}
	if auto, _ := d["auto"].(string); auto == "bulkRestore" && a.Type == "boardUpdate" {
		restored, _ := d["restored"].(float64)
		skipped, _ := d["skipped"].(float64)
		return "bulk-restored " + countOf(int(restored), "card") + "; skipped " + strconv.Itoa(int(skipped))
	}
	if auto, _ := d["auto"].(string); auto == "bulkDelete" && a.Type == "boardUpdate" {
		deleted, _ := d["deleted"].(float64)
		skipped, _ := d["skipped"].(float64)
		return "permanently deleted " + countOf(int(deleted), "card") + "; skipped " + strconv.Itoa(int(skipped))
	}
	if tn, _ := d["appliedTemplate"].(string); tn != "" && a.Type == "boardUpdate" {
		cols, _ := d["columnsAdded"].(float64)
		moved, _ := d["cardsMoved"].(float64)
		s := "applied the template “" + clip(tn, 80) + "”: added " + countOf(int(cols), "column") + ", moved " + countOf(int(moved), "card")
		if rm, _ := d["columnsRemoved"].(float64); rm > 0 {
			s += ", removed " + countOf(int(rm), "old column")
		}
		return s
	}
	if name != "" {
		s += " “" + clip(name, 80) + "”"
	}
	if a.Type == "commentCreate" {
		if t, _ := d["text"].(string); t != "" {
			s += ": " + clip(strings.Join(strings.Fields(t), " "), 120)
		}
	}
	return s
}

func init() {
	ops.Register(ops.Op{Name: "activity.board", Summary: "Board activity feed (actions), newest first. Quiet per-card rows of bulk changes (auto-archive, applying a template) are left out; their summary row is kept and activity.card still lists them", Method: "GET", Path: "/boards/{boardId}/actions",
		Auth: ops.AuthPublic, UI: "GET /ui/boards/{id}/activity"}, func(e *ops.Env, in ActionsIn) ([]ActionItem, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return nil, err
		}
		// quiet per-card events (bulk side effects: auto-archive, applying a template) are left out of the board feed;
		// each bulk change records its own summary row. The card's own feed keeps them.
		return listActions(e, `a.board_id=? AND (a.card_id IS NULL OR json_extract(a.data,'$.quiet') IS NOT 1)`, []any{in.BoardID}, in.Before, in.Limit)
	})
	ops.Register(ops.Op{Name: "activity.card", Summary: "Card activity feed (actions), newest first. Only what was recorded on boards you can view; an event recorded on several boards (the card is on several) is listed once", Method: "GET", Path: "/cards/{cardId}/actions",
		Auth: ops.AuthPublic, UI: "GET /ui/cards/{id}/activity"}, func(e *ops.Env, in CardActionsIn) ([]ActionItem, error) {
		a, err := authz.CardAccessFor(e, in.CardID, in.BoardID)
		if err != nil {
			return nil, err
		}
		// rows recorded on boards the caller can view (a move on a board they cannot see names its columns; a board the
		// card has left keeps its rows for that board's viewers), and one row per logical event (COALESCE(origin_id,id)):
		// the context board's copy, else the oldest viewable copy. The filter runs before the dedupe, so a hidden
		// origin row never hides the event or names its board. Two queries plus one BoardLevels, whatever the history.
		var boards []string
		if rows, err := e.DB.QueryContext(e.Ctx, `SELECT DISTINCT board_id FROM actions WHERE card_id=?`, in.CardID); err == nil {
			var all []string
			for rows.Next() {
				var b string
				if rows.Scan(&b) == nil {
					all = append(all, b)
				}
			}
			rows.Close()
			for b, l := range authz.BoardLevels(e, all) {
				if l >= authz.View {
					boards = append(boards, b)
				}
			}
		}
		if len(boards) == 0 {
			return []ActionItem{}, nil
		}
		sort.Strings(boards)
		ph := "?" + strings.Repeat(",?", len(boards)-1)
		args := []any{in.CardID}
		for _, b := range boards {
			args = append(args, b)
		}
		args = append(args, in.CardID)
		for _, b := range boards {
			args = append(args, b)
		}
		args = append(args, a.BoardID, a.BoardID, a.BoardID, a.BoardID)
		return listActions(e, `a.card_id=? AND a.board_id IN (`+ph+`) AND NOT EXISTS (SELECT 1 FROM actions x WHERE x.card_id=? AND x.id<>a.id
			AND COALESCE(x.origin_id,x.id)=COALESCE(a.origin_id,a.id) AND x.board_id IN (`+ph+`)
			AND ((x.board_id=?) > (a.board_id=?) OR ((x.board_id=?) = (a.board_id=?) AND x.id < a.id)))`, args, in.Before, in.Limit)
	})
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
