package cardextras

import (
	"database/sql"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

type TaskItem struct {
	ID               string  `json:"id"`
	TaskListID       string  `json:"taskListId"`
	CardID           string  `json:"cardId"`
	Name             string  `json:"name"`
	IsCompleted      bool    `json:"isCompleted"`
	Position         float64 `json:"position"`
	AssigneeUserID   *string `json:"assigneeUserId"`
	AssigneeName     *string `json:"assigneeName"`
	LinkedCardID     *string `json:"linkedCardId"`
	LinkedCardName   *string `json:"linkedCardName"`
	LinkedCardClosed bool    `json:"linkedCardIsClosed"`
	CreatedAt        string  `json:"createdAt"`
}

type TaskListItem struct {
	ID                 string     `json:"id"`
	CardID             string     `json:"cardId"`
	Name               string     `json:"name"`
	Position           float64    `json:"position"`
	ShowOnFrontOfCard  bool       `json:"showOnFrontOfCard"`
	HideCompletedTasks bool       `json:"hideCompletedTasks"`
	TasksTotal         int        `json:"tasksTotal"`
	TasksCompleted     int        `json:"tasksCompleted"`
	Tasks              []TaskItem `json:"tasks"`
	CreatedAt          string     `json:"createdAt"`
}

const taskSelect = `SELECT t.id,t.task_list_id,tl.card_id,t.name,t.is_completed,t.position,t.assignee_user_id,u.name,
	t.linked_card_id,lc.name,COALESCE(lc.is_closed,0),t.created_at
	FROM tasks t JOIN task_lists tl ON tl.id=t.task_list_id LEFT JOIN users u ON u.id=t.assignee_user_id LEFT JOIN cards lc ON lc.id=t.linked_card_id`

func scanTask(r *sql.Rows) (TaskItem, error) {
	var t TaskItem
	var as, an, lc, ln sql.NullString
	var done, closed int
	err := r.Scan(&t.ID, &t.TaskListID, &t.CardID, &t.Name, &done, &t.Position, &as, &an, &lc, &ln, &closed, &t.CreatedAt)
	t.IsCompleted, t.LinkedCardClosed = done != 0, closed != 0
	t.AssigneeUserID, t.AssigneeName, t.LinkedCardID, t.LinkedCardName = strp(as), strp(an), strp(lc), strp(ln)
	return t, err
}

func getTask(e *ops.Env, q querier, id string) (TaskItem, error) {
	ts, err := query(e.Ctx, q, scanTask, taskSelect+` WHERE t.id=?`, id)
	if err != nil {
		return TaskItem{}, err
	}
	if len(ts) == 0 {
		return TaskItem{}, ops.NotFound("task")
	}
	return ts[0], nil
}

// taskListCard resolves a task list to (cardID, boardID) and checks the level.
func taskListCard(e *ops.Env, listID string, min authz.Level) (cardID, boardID string, err error) {
	if e.DB.QueryRowContext(e.Ctx, `SELECT tl.card_id,c.board_id FROM task_lists tl JOIN cards c ON c.id=tl.card_id WHERE tl.id=?`, listID).Scan(&cardID, &boardID) != nil {
		return "", "", ops.NotFound("task list")
	}
	return cardID, boardID, authz.RequireBoard(e, boardID, min)
}

func taskCard(e *ops.Env, taskID string, min authz.Level) (listID, cardID, boardID string, err error) {
	if e.DB.QueryRowContext(e.Ctx, `SELECT t.task_list_id,tl.card_id,c.board_id FROM tasks t JOIN task_lists tl ON tl.id=t.task_list_id JOIN cards c ON c.id=tl.card_id WHERE t.id=?`, taskID).Scan(&listID, &cardID, &boardID) != nil {
		return "", "", "", ops.NotFound("task")
	}
	return listID, cardID, boardID, authz.RequireBoard(e, boardID, min)
}

func getTaskList(e *ops.Env, q querier, id string) (TaskListItem, error) {
	var l TaskListItem
	var front, hide int
	if q.QueryRowContext(e.Ctx, `SELECT id,card_id,name,position,show_on_front_of_card,hide_completed_tasks,created_at FROM task_lists WHERE id=?`, id).
		Scan(&l.ID, &l.CardID, &l.Name, &l.Position, &front, &hide, &l.CreatedAt) != nil {
		return l, ops.NotFound("task list")
	}
	l.ShowOnFrontOfCard, l.HideCompletedTasks = front != 0, hide != 0
	ts, err := query(e.Ctx, q, scanTask, taskSelect+` WHERE t.task_list_id=? ORDER BY t.position`, id)
	if err != nil {
		return l, err
	}
	l.Tasks = ts
	for _, t := range ts {
		l.TasksTotal++
		if t.IsCompleted {
			l.TasksCompleted++
		}
	}
	return l, nil
}

func checkAssignee(e *ops.Env, userID string) error {
	var n int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM users WHERE id=? AND is_deactivated=0 AND deleted_at IS NULL`, userID).Scan(&n)
	if n == 0 {
		return ops.Invalid("assignee not found")
	}
	return nil
}

func checkLinkedCard(e *ops.Env, linkedID, ownCardID string) error {
	if linkedID == ownCardID {
		return ops.Invalid("a task cannot link to its own card")
	}
	if _, err := authz.RequireCard(e, linkedID, authz.View); err != nil {
		return ops.Invalid("linked card not found")
	}
	return nil
}

// ---- inputs ----

type TaskListsIn struct {
	CardID string `path:"cardId" desc:"Card id"`
}
type TaskListCreateIn struct {
	CardID             string `path:"cardId" desc:"Card id"`
	Name               string `json:"name" required:"true" desc:"Task list name"`
	ShowOnFrontOfCard  bool   `json:"showOnFrontOfCard" desc:"Show progress on the card face"`
	HideCompletedTasks bool   `json:"hideCompletedTasks" desc:"Hide completed tasks"`
}
type TaskListUpdateIn struct {
	ID                 string   `path:"id" desc:"Task list id"`
	Name               *string  `json:"name"`
	ShowOnFrontOfCard  *bool    `json:"showOnFrontOfCard"`
	HideCompletedTasks *bool    `json:"hideCompletedTasks"`
	Position           *float64 `json:"position" desc:"Explicit position"`
	AfterID            string   `json:"afterId" desc:"Place after this task list"`
	BeforeID           string   `json:"beforeId" desc:"Place before this task list"`
}
type IDIn struct {
	ID string `path:"id"`
}
type TaskCreateIn struct {
	TaskListID     string  `path:"taskListId" desc:"Task list id"`
	Name           string  `json:"name" desc:"Task text (optional when linkedCardId is given: defaults to the card name)"`
	AssigneeUserID *string `json:"assigneeUserId"`
	LinkedCardID   *string `json:"linkedCardId" desc:"Link this task to another card"`
	IsCompleted    bool    `json:"isCompleted"`
}
type TaskUpdateIn struct {
	ID             string   `path:"id" desc:"Task id"`
	Name           *string  `json:"name"`
	IsCompleted    *bool    `json:"isCompleted"`
	AssigneeUserID *string  `json:"assigneeUserId" desc:"User id; empty string clears"`
	LinkedCardID   *string  `json:"linkedCardId" desc:"Card id; empty string unlinks"`
	TaskListID     string   `json:"taskListId" desc:"Move the task to another task list of the same card"`
	Position       *float64 `json:"position"`
	AfterID        string   `json:"afterId"`
	BeforeID       string   `json:"beforeId"`
}

func init() {
	ops.Register(ops.Op{Name: "tasklist.list", Summary: "List a card's task lists with their tasks", Method: "GET", Path: "/cards/{cardId}/task-lists",
		Auth: ops.AuthPublic, UI: "GET /ui/cards/{id}/tasks"}, func(e *ops.Env, in TaskListsIn) ([]TaskListItem, error) {
		if _, err := authz.RequireCard(e, in.CardID, authz.View); err != nil {
			return nil, err
		}
		ids, err := query(e.Ctx, e.DB, scanString, `SELECT id FROM task_lists WHERE card_id=? ORDER BY position`, in.CardID)
		if err != nil {
			return nil, err
		}
		out := []TaskListItem{}
		for _, id := range ids {
			l, err := getTaskList(e, e.DB, id)
			if err != nil {
				return nil, err
			}
			out = append(out, l)
		}
		return out, nil
	})

	ops.Register(ops.Op{Name: "tasklist.create", Summary: "Add a task list to a card", Method: "POST", Path: "/cards/{cardId}/task-lists",
		UI: "POST /ui/cards/{cardId}/task-lists"}, func(e *ops.Env, in TaskListCreateIn) (TaskListItem, error) {
		board, err := authz.RequireCard(e, in.CardID, authz.Edit)
		if err != nil {
			return TaskListItem{}, err
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			return TaskListItem{}, ops.Invalid("name is required")
		}
		id := db.NewID()
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(e.Ctx, `INSERT INTO task_lists(id,card_id,position,name,show_on_front_of_card,hide_completed_tasks,created_at) VALUES(?,?,?,?,?,?,?)`,
				id, in.CardID, endPosition(e.Ctx, tx, "task_lists", "card_id=?", in.CardID), name, b2i(in.ShowOnFrontOfCard), b2i(in.HideCompletedTasks), db.Now())
			return err
		})
		if err != nil {
			return TaskListItem{}, err
		}
		l, err := getTaskList(e, e.DB, id)
		if err == nil {
			emit(e, "taskListCreate", board, in.CardID, "tasklist.created", map[string]any{"taskListId": id, "name": name})
		}
		return l, err
	})

	ops.Register(ops.Op{Name: "tasklist.update", Summary: "Rename, reposition or change display flags of a task list", Method: "PATCH", Path: "/task-lists/{id}",
		UI: "POST /ui/task-lists/{id}"}, func(e *ops.Env, in TaskListUpdateIn) (TaskListItem, error) {
		cardID, board, err := taskListCard(e, in.ID, authz.Edit)
		if err != nil {
			return TaskListItem{}, err
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if in.Name != nil {
				n := strings.TrimSpace(*in.Name)
				if n == "" {
					return ops.Invalid("name cannot be empty")
				}
				if _, err := tx.ExecContext(e.Ctx, `UPDATE task_lists SET name=? WHERE id=?`, n, in.ID); err != nil {
					return err
				}
			}
			if in.ShowOnFrontOfCard != nil {
				if _, err := tx.ExecContext(e.Ctx, `UPDATE task_lists SET show_on_front_of_card=? WHERE id=?`, b2i(*in.ShowOnFrontOfCard), in.ID); err != nil {
					return err
				}
			}
			if in.HideCompletedTasks != nil {
				if _, err := tx.ExecContext(e.Ctx, `UPDATE task_lists SET hide_completed_tasks=? WHERE id=?`, b2i(*in.HideCompletedTasks), in.ID); err != nil {
					return err
				}
			}
			pos, ok, err := place(e.Ctx, tx, "task_lists", "card_id=?", []any{cardID}, in.AfterID, in.BeforeID, in.Position)
			if err != nil {
				return err
			}
			if ok {
				_, err = tx.ExecContext(e.Ctx, `UPDATE task_lists SET position=? WHERE id=?`, pos, in.ID)
			}
			return err
		})
		if err != nil {
			return TaskListItem{}, err
		}
		emit(e, "taskListUpdate", board, cardID, "tasklist.updated", map[string]any{"taskListId": in.ID})
		return getTaskList(e, e.DB, in.ID)
	})

	ops.Register(ops.Op{Name: "tasklist.delete", Summary: "Delete a task list and its tasks", Method: "DELETE", Path: "/task-lists/{id}",
		UI: "POST /ui/task-lists/{id}/delete"}, func(e *ops.Env, in IDIn) (DeletedOut, error) {
		cardID, board, err := taskListCard(e, in.ID, authz.Edit)
		if err != nil {
			return DeletedOut{}, err
		}
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM task_lists WHERE id=?`, in.ID); err != nil {
			return DeletedOut{}, err
		}
		emit(e, "taskListDelete", board, cardID, "tasklist.deleted", map[string]any{"taskListId": in.ID})
		return DeletedOut{in.ID, cardID}, nil
	})

	ops.Register(ops.Op{Name: "task.create", Summary: "Add a task (optionally linked to another card) to a task list", Method: "POST", Path: "/task-lists/{taskListId}/tasks",
		UI: "POST /ui/task-lists/{taskListId}/tasks"}, func(e *ops.Env, in TaskCreateIn) (TaskItem, error) {
		cardID, board, err := taskListCard(e, in.TaskListID, authz.Edit)
		if err != nil {
			return TaskItem{}, err
		}
		name := strings.TrimSpace(in.Name)
		var linked any
		if in.LinkedCardID != nil && *in.LinkedCardID != "" {
			if err := checkLinkedCard(e, *in.LinkedCardID, cardID); err != nil {
				return TaskItem{}, err
			}
			linked = *in.LinkedCardID
			if name == "" {
				_ = e.DB.QueryRowContext(e.Ctx, `SELECT name FROM cards WHERE id=?`, *in.LinkedCardID).Scan(&name)
			}
		}
		if name == "" {
			return TaskItem{}, ops.Invalid("name or linkedCardId is required")
		}
		var assignee any
		if in.AssigneeUserID != nil && *in.AssigneeUserID != "" {
			if err := checkAssignee(e, *in.AssigneeUserID); err != nil {
				return TaskItem{}, err
			}
			assignee = *in.AssigneeUserID
		}
		id := db.NewID()
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(e.Ctx, `INSERT INTO tasks(id,task_list_id,linked_card_id,assignee_user_id,position,name,is_completed,created_at) VALUES(?,?,?,?,?,?,?,?)`,
				id, in.TaskListID, linked, assignee, endPosition(e.Ctx, tx, "tasks", "task_list_id=?", in.TaskListID), name, b2i(in.IsCompleted), db.Now())
			return err
		})
		if err != nil {
			return TaskItem{}, err
		}
		emit(e, "taskCreate", board, cardID, "task.created", map[string]any{"taskId": id, "taskListId": in.TaskListID, "name": name})
		return getTask(e, e.DB, id)
	})

	ops.Register(ops.Op{Name: "task.update", Summary: "Edit, complete, assign, link, move or reorder a task", Method: "PATCH", Path: "/tasks/{id}",
		UI: "POST /ui/tasks/{id}"}, func(e *ops.Env, in TaskUpdateIn) (TaskItem, error) {
		listID, cardID, board, err := taskCard(e, in.ID, authz.Edit)
		if err != nil {
			return TaskItem{}, err
		}
		if in.AssigneeUserID != nil && *in.AssigneeUserID != "" {
			if err := checkAssignee(e, *in.AssigneeUserID); err != nil {
				return TaskItem{}, err
			}
		}
		if in.LinkedCardID != nil && *in.LinkedCardID != "" {
			if err := checkLinkedCard(e, *in.LinkedCardID, cardID); err != nil {
				return TaskItem{}, err
			}
		}
		if in.TaskListID != "" && in.TaskListID != listID {
			var c string
			if e.DB.QueryRowContext(e.Ctx, `SELECT card_id FROM task_lists WHERE id=?`, in.TaskListID).Scan(&c) != nil || c != cardID {
				return TaskItem{}, ops.Invalid("target task list must belong to the same card")
			}
		}
		wasDone := false
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT is_completed FROM tasks WHERE id=?`, in.ID).Scan(&wasDone)
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			set := func(col string, v any) error {
				_, err := tx.ExecContext(e.Ctx, `UPDATE tasks SET `+col+`=? WHERE id=?`, v, in.ID)
				return err
			}
			if in.Name != nil {
				n := strings.TrimSpace(*in.Name)
				if n == "" {
					return ops.Invalid("name cannot be empty")
				}
				if err := set("name", n); err != nil {
					return err
				}
			}
			if in.IsCompleted != nil {
				if err := set("is_completed", b2i(*in.IsCompleted)); err != nil {
					return err
				}
			}
			if in.AssigneeUserID != nil {
				if err := set("assignee_user_id", nz(*in.AssigneeUserID)); err != nil {
					return err
				}
			}
			if in.LinkedCardID != nil {
				if err := set("linked_card_id", nz(*in.LinkedCardID)); err != nil {
					return err
				}
			}
			target := listID
			moved := in.TaskListID != "" && in.TaskListID != listID
			if moved {
				target = in.TaskListID
				if err := set("task_list_id", target); err != nil {
					return err
				}
			}
			pos, ok, err := place(e.Ctx, tx, "tasks", "task_list_id=?", []any{target}, in.AfterID, in.BeforeID, in.Position)
			if err != nil {
				return err
			}
			if !ok && moved {
				pos, ok = endPosition(e.Ctx, tx, "tasks", "task_list_id=?", target), true
			}
			if ok {
				return set("position", pos)
			}
			return nil
		})
		if err != nil {
			return TaskItem{}, err
		}
		t, err := getTask(e, e.DB, in.ID)
		if err != nil {
			return t, err
		}
		typ := "taskUpdate"
		if in.IsCompleted != nil && *in.IsCompleted != wasDone {
			typ = "taskComplete"
			if !*in.IsCompleted {
				typ = "taskReopen"
			}
		}
		emit(e, typ, board, cardID, "task.updated", map[string]any{"taskId": in.ID, "name": t.Name, "isCompleted": t.IsCompleted})
		return t, nil
	})

	ops.Register(ops.Op{Name: "task.delete", Summary: "Delete a task", Method: "DELETE", Path: "/tasks/{id}",
		UI: "POST /ui/tasks/{id}/delete"}, func(e *ops.Env, in IDIn) (DeletedOut, error) {
		_, cardID, board, err := taskCard(e, in.ID, authz.Edit)
		if err != nil {
			return DeletedOut{}, err
		}
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM tasks WHERE id=?`, in.ID); err != nil {
			return DeletedOut{}, err
		}
		emit(e, "taskDelete", board, cardID, "task.deleted", map[string]any{"taskId": in.ID})
		return DeletedOut{in.ID, cardID}, nil
	})
}
