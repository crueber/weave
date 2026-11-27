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

var actionVerbs = map[string]string{
	"taskListCreate": "added a task list", "taskListUpdate": "updated a task list", "taskListDelete": "deleted a task list",
	"taskCreate": "added task", "taskUpdate": "updated task", "taskComplete": "completed task", "taskReopen": "reopened task", "taskDelete": "deleted a task",
	"commentCreate": "commented", "commentUpdate": "edited a comment", "commentDelete": "deleted a comment",
	"attachmentCreate": "attached", "attachmentUpdate": "updated attachment", "attachmentDelete": "removed attachment", "attachmentCover": "changed the cover",
	"customFieldGroupCreate": "added a field group", "customFieldGroupUpdate": "updated a field group", "customFieldGroupDelete": "removed a field group",
	"customFieldCreate": "added a field", "customFieldUpdate": "updated a field", "customFieldDelete": "removed a field",
	"customFieldValueUpdate": "set a field value", "customFieldValueDelete": "cleared a field value",
	"customFieldRowCreate": "added a field row", "customFieldRowUpdate": "reordered a field row", "customFieldRowDelete": "removed a field row",
	"stopwatchStart": "started the stopwatch", "stopwatchStop": "stopped the stopwatch", "stopwatchReset": "reset time tracking",
	"timeEntryCreate": "logged time", "timeEntryUpdate": "edited a time entry", "timeEntryDelete": "deleted a time entry",
	"cardCreate": "created the card", "cardMove": "moved the card", "cardUpdate": "updated the card", "cardDelete": "deleted the card",
	"cardPlace": "placed the card on a board", "cardUnplace": "removed the card from a board",
	"cardMembershipCreate": "added a member", "cardMembershipDelete": "removed a member",
	"cardLabelCreate": "added a label", "cardLabelDelete": "removed a label",
	"cardClaim": "claimed the card", "cardRelease": "released the card",
}

func actionLimit(limit int) int {
	if limit <= 0 || limit > 200 {
		return 50
	}
	return limit
}

// ActionText renders a short human sentence fragment for an activity row.
func ActionText(a ActionItem) string {
	var d map[string]any
	_ = json.Unmarshal(a.Data, &d)
	name, _ := d["name"].(string)
	s, ok := actionVerbs[a.Type]
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
		q := actionSelect + ` WHERE a.board_id=? AND (a.card_id IS NULL OR json_extract(a.data,'$.quiet') IS NOT 1)`
		args := []any{in.BoardID}
		if in.Before != "" {
			q += ` AND (a.created_at,a.id) < (SELECT created_at,id FROM actions WHERE id=? AND board_id=?)`
			args = append(args, in.Before, in.BoardID)
		}
		q += ` ORDER BY a.created_at DESC, a.id DESC LIMIT ?`
		args = append(args, actionLimit(in.Limit))
		return query(e.Ctx, e.DB, scanAction, q, args...)
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
		all, err := query(e.Ctx, e.DB, scanString, `SELECT DISTINCT board_id FROM actions WHERE card_id=?`, in.CardID)
		if err != nil {
			return nil, err
		}
		var boards []string
		for b, l := range authz.BoardLevels(e, all) {
			if l >= authz.View {
				boards = append(boards, b)
			}
		}
		if len(boards) == 0 {
			return []ActionItem{}, nil
		}
		sort.Strings(boards)
		ph := inPlaceholders(len(boards))
		args := make([]any, 0, 2+len(boards)*2+3)
		args = append(args, a.BoardID, in.CardID)
		for _, b := range boards {
			args = append(args, b)
		}
		q := `WITH ranked AS (
			SELECT id,board_id,card_id,user_id,type,data,created_at,
				ROW_NUMBER() OVER (PARTITION BY COALESCE(origin_id,id) ORDER BY (board_id=?) DESC, id ASC) AS rn
			FROM actions WHERE card_id=? AND board_id IN (` + ph + `)
		)
		SELECT a.id,a.board_id,a.card_id,c.name,a.user_id,u.name,a.type,a.data,a.created_at
		FROM ranked a LEFT JOIN users u ON u.id=a.user_id LEFT JOIN cards c ON c.id=a.card_id
		WHERE a.rn=1`
		if in.Before != "" {
			q += ` AND (a.created_at,a.id) < (SELECT created_at,id FROM actions WHERE id=? AND card_id=? AND board_id IN (` + ph + `))`
			args = append(args, in.Before, in.CardID)
			for _, b := range boards {
				args = append(args, b)
			}
		}
		q += ` ORDER BY a.created_at DESC, a.id DESC LIMIT ?`
		args = append(args, actionLimit(in.Limit))
		return query(e.Ctx, e.DB, scanAction, q, args...)
	})
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
