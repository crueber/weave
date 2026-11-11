package cardextras

import (
	"database/sql"
	"encoding/json"
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
	CardID string `path:"cardId"`
	Before string `query:"before" desc:"Action id cursor: return actions older than this one"`
	Limit  int    `query:"limit" desc:"Page size (default 50, max 200)"`
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

func listActions(e *ops.Env, where string, id, before string, limit int) ([]ActionItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q, args := actionSelect+` WHERE `+where, []any{id}
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
	ops.Register(ops.Op{Name: "activity.board", Summary: "Board activity feed (actions), newest first", Method: "GET", Path: "/boards/{boardId}/actions",
		Auth: ops.AuthPublic, UI: "GET /ui/boards/{id}/activity"}, func(e *ops.Env, in ActionsIn) ([]ActionItem, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return nil, err
		}
		return listActions(e, `a.board_id=?`, in.BoardID, in.Before, in.Limit)
	})
	ops.Register(ops.Op{Name: "activity.card", Summary: "Card activity feed (actions), newest first", Method: "GET", Path: "/cards/{cardId}/actions",
		Auth: ops.AuthPublic, UI: "GET /ui/cards/{id}/activity"}, func(e *ops.Env, in CardActionsIn) ([]ActionItem, error) {
		if _, err := authz.RequireCard(e, in.CardID, authz.View); err != nil {
			return nil, err
		}
		return listActions(e, `a.card_id=?`, in.CardID, in.Before, in.Limit)
	})
}
