package cardextras

import (
	"strings"

	"github.com/crueber/weave/internal/ops"
)

// FrontTaskList is a task list flagged show_on_front_of_card.
type FrontTaskList struct {
	Name  string `json:"name"`
	Total int    `json:"total"`
	Done  int    `json:"done"`
}

// FrontField is a custom-field value flagged show_on_front_of_card.
type FrontField struct {
	GroupName string `json:"groupName"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Content   string `json:"content"`
}

// Badges summarises a card's extras for rendering on the card face (kanban/list/grid).
type Badges struct {
	TasksTotal     int             `json:"tasksTotal"`
	TasksDone      int             `json:"tasksDone"`
	Attachments    int             `json:"attachments"`
	Comments       int             `json:"comments"`
	TrackedSeconds int             `json:"trackedSeconds"`
	Running        bool            `json:"running"`
	TaskLists      []FrontTaskList `json:"taskLists"`
	Fields         []FrontField    `json:"fields"`
}

// CardBadges returns face-of-card summaries for many cards in a handful of queries.
// It performs no permission check: callers pass cards the user may already see.
func CardBadges(e *ops.Env, cardIDs []string) (map[string]Badges, error) {
	out := map[string]Badges{}
	if len(cardIDs) == 0 {
		return out, nil
	}
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(cardIDs)), ",") + ")"
	args := make([]any, len(cardIDs))
	for i, id := range cardIDs {
		args[i] = id
	}
	get := func(id string) Badges { return out[id] }

	rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,comments_total,stopwatch_total_seconds,stopwatch_started_at IS NOT NULL,
		(SELECT COUNT(*) FROM attachments WHERE card_id=cards.id) FROM cards WHERE id IN `+in, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var b Badges
		var run bool
		if err := rows.Scan(&id, &b.Comments, &b.TrackedSeconds, &run, &b.Attachments); err != nil {
			rows.Close()
			return nil, err
		}
		b.Running = run
		out[id] = b
	}
	rows.Close()

	rows, err = e.DB.QueryContext(e.Ctx, `SELECT tl.card_id,tl.name,tl.show_on_front_of_card,COUNT(t.id),COALESCE(SUM(t.is_completed),0)
		FROM task_lists tl LEFT JOIN tasks t ON t.task_list_id=tl.id WHERE tl.card_id IN `+in+` GROUP BY tl.id ORDER BY tl.position`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, name string
		var front bool
		var total, done int
		if err := rows.Scan(&id, &name, &front, &total, &done); err != nil {
			rows.Close()
			return nil, err
		}
		b := get(id)
		b.TasksTotal += total
		b.TasksDone += done
		if front {
			b.TaskLists = append(b.TaskLists, FrontTaskList{name, total, done})
		}
		out[id] = b
	}
	rows.Close()

	rows, err = e.DB.QueryContext(e.Ctx, `SELECT v.card_id,COALESCE(g.name,bg.name,''),f.name,f.type,COALESCE(v.content,'')
		FROM custom_field_values v JOIN custom_fields f ON f.id=v.field_id JOIN custom_field_groups g ON g.id=v.group_id
		LEFT JOIN base_custom_field_groups bg ON bg.id=g.base_group_id
		WHERE f.show_on_front_of_card=1 AND v.card_id IN `+in+` AND v.content IS NOT NULL AND v.content<>''
		ORDER BY g.position,f.position,v.row_id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var ff FrontField
		if err := rows.Scan(&id, &ff.GroupName, &ff.Name, &ff.Type, &ff.Content); err != nil {
			rows.Close()
			return nil, err
		}
		b := get(id)
		b.Fields = append(b.Fields, ff)
		out[id] = b
	}
	rows.Close()
	return out, nil
}
