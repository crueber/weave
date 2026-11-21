package boards

import (
	"encoding/json"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// Watching a board or a column (board_subscriptions, list_subscriptions; migration 0308). Watching is explicit: nothing
// watches a board or column automatically. Anyone who can view the board may watch it; the notify planner re-checks
// access at delivery time, so a watch outlives nothing it should not. Watch toggles emit no event (an activity row would
// tell everyone on the board who watches what), so other open tabs do not update live.

type BoardWatchOut struct {
	BoardID    string `json:"boardId"`
	Subscribed bool   `json:"subscribed"`
}

type ListWatchOut struct {
	ListID     string `json:"listId"`
	Subscribed bool   `json:"subscribed"`
}

func registerWatchOps() {
	ops.Register(ops.Op{Name: "board.subscribe", Summary: "Watch a board: get notified about every card on it (created, moved, closed or re-opened, commented on, due date changed)", Method: "PUT", Path: "/boards/{id}/subscription",
		UI: "POST /ui/boards/{id}/subscribe"}, func(e *ops.Env, in BoardGetIn) (BoardWatchOut, error) {
		if err := authz.RequireBoard(e, in.ID, authz.View); err != nil {
			return BoardWatchOut{}, err
		}
		_, err := e.DB.ExecContext(e.Ctx, `INSERT OR IGNORE INTO board_subscriptions(board_id,user_id) VALUES(?,?)`, in.ID, e.User.UserID)
		return BoardWatchOut{in.ID, true}, err
	})
	ops.Register(ops.Op{Name: "board.unsubscribe", Summary: "Stop watching a board (card and column watches stay)", Method: "DELETE", Path: "/boards/{id}/subscription",
		UI: "POST /ui/boards/{id}/unsubscribe"}, func(e *ops.Env, in BoardGetIn) (BoardWatchOut, error) {
		if err := authz.RequireBoard(e, in.ID, authz.View); err != nil {
			return BoardWatchOut{}, err
		}
		_, err := e.DB.ExecContext(e.Ctx, `DELETE FROM board_subscriptions WHERE board_id=? AND user_id=?`, in.ID, e.User.UserID)
		return BoardWatchOut{in.ID, false}, err
	})
	ops.Register(ops.Op{Name: "list.subscribe", Summary: "Watch a column: get notified about cards created in it or moved in or out, and comments, close/re-open and due-date changes on its cards", Method: "PUT", Path: "/lists/{id}/subscription",
		UI: "POST /ui/lists/{id}/subscribe"}, func(e *ops.Env, in ListRefIn) (ListWatchOut, error) {
		if _, err := watchableList(e, in.ID); err != nil {
			return ListWatchOut{}, err
		}
		_, err := e.DB.ExecContext(e.Ctx, `INSERT OR IGNORE INTO list_subscriptions(list_id,user_id,created_at) VALUES(?,?,?)`, in.ID, e.User.UserID, db.Now())
		return ListWatchOut{in.ID, true}, err
	})
	ops.Register(ops.Op{Name: "list.unsubscribe", Summary: "Stop watching a column", Method: "DELETE", Path: "/lists/{id}/subscription",
		UI: "POST /ui/lists/{id}/unsubscribe"}, func(e *ops.Env, in ListRefIn) (ListWatchOut, error) {
		l, err := requireList(e, in.ID, authz.View) // archive/trash allowed here so a stale watch can always be removed
		if err != nil {
			return ListWatchOut{}, err
		}
		_, err = e.DB.ExecContext(e.Ctx, `DELETE FROM list_subscriptions WHERE list_id=? AND user_id=?`, l.ID, e.User.UserID)
		return ListWatchOut{in.ID, false}, err
	})
}

// watchableList: View on the board, and a real column (archive and trash are system lists).
func watchableList(e *ops.Env, id string) (List, error) {
	l, err := requireList(e, id, authz.View)
	if err != nil {
		return l, err
	}
	if !isFinite(l.Type) {
		return List{}, ops.NotFound("list")
	}
	return l, nil
}

// boardWatched reports whether the caller watches the board.
func boardWatched(e *ops.Env, boardID string) bool {
	if e.User == nil {
		return false
	}
	var n int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM board_subscriptions WHERE board_id=? AND user_id=?`, boardID, e.User.UserID).Scan(&n)
	return n > 0
}

// listWatched reports whether the caller watches the column.
func listWatched(e *ops.Env, listID string) bool {
	if e.User == nil {
		return false
	}
	var n int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM list_subscriptions WHERE list_id=? AND user_id=?`, listID, e.User.UserID).Scan(&n)
	return n > 0
}

// watchedLists returns the ids of the board's columns the caller watches (one query, for the board page).
func watchedLists(e *ops.Env, boardID string) map[string]bool {
	out := map[string]bool{}
	if e.User == nil {
		return out
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT s.list_id FROM list_subscriptions s JOIN lists l ON l.id=s.list_id WHERE l.board_id=? AND s.user_id=?`, boardID, e.User.UserID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// WatchBtn feeds the watch toggles (board header bell, column sheet button).
type WatchBtn struct {
	ID, Name string
	On       bool
}

// watchTrigger sets HX-Trigger to a toast plus, for columns, a board refresh (the header bell of the column).
func watchTrigger(c *web.Ctx, msg string, refresh bool) {
	m := map[string]any{"toast": msg}
	if refresh {
		m["boards:refresh"] = true
	}
	b, _ := json.Marshal(m)
	c.W.Header().Set("HX-Trigger", string(b))
}

func registerWatchUI() {
	for _, on := range []bool{true, false} {
		on := on
		op, msg := "board.unsubscribe", "Stopped watching this board"
		if on {
			op, msg = "board.subscribe", "Watching this board"
		}
		web.Route(ops.Get(op).UI, func(c *web.Ctx) error {
			if _, err := c.Do(op); err != nil {
				return err
			}
			b, err := GetBoard(c.R.Context(), c.Env.DB, c.R.PathValue("id"))
			if err != nil {
				return err
			}
			watchTrigger(c, msg, false)
			return c.RenderBlock("boards_board.html", "boards_watch_board", WatchBtn{ID: b.ID, Name: b.Name, On: on})
		})
		lop, lmsg := "list.unsubscribe", "Stopped watching this column"
		if on {
			lop, lmsg = "list.subscribe", "Watching this column"
		}
		web.Route(ops.Get(lop).UI, func(c *web.Ctx) error {
			if _, err := c.Do(lop); err != nil {
				return err
			}
			l, err := GetList(c.R.Context(), c.Env.DB, c.R.PathValue("id"))
			if err != nil {
				return err
			}
			watchTrigger(c, lmsg, true)
			// the column sheet swaps its button in place; the column menu ignores the body (hx-swap="none")
			return c.RenderBlock("boards_listsheet.html", "boards_watch_list", WatchBtn{ID: l.ID, Name: deref(l.Name), On: on})
		})
	}
}
