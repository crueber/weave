package cardextras

import (
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

const badgeChunkSize = 400

func init() { events.TaskCounts = taskCounts }

// taskCounts is events.TaskCounts: {total, completed} tasks per card in chunked aggregate queries, so card
// snapshots (board.tree, card.list, card.next, event payloads) show checklist progress without N+1 reads.
func taskCounts(e *ops.Env, cardIDs []string) map[string][2]int {
	out := map[string][2]int{}
	for start := 0; start < len(cardIDs); start += badgeChunkSize {
		chunk := cardIDs[start:min(start+badgeChunkSize, len(cardIDs))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT tl.card_id,COUNT(t.id),COALESCE(SUM(t.is_completed),0)
			FROM task_lists tl JOIN tasks t ON t.task_list_id=tl.id
			WHERE tl.card_id IN (`+inPlaceholders(len(chunk))+`) GROUP BY tl.card_id`, args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id string
			var n [2]int
			if rows.Scan(&id, &n[0], &n[1]) == nil {
				out[id] = n
			}
		}
		rows.Close()
	}
	return out
}

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

// Badges summarises a card's extras for rendering on the card face (board/list/grid).
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

// CardBadges returns face-of-card summaries for many cards in a handful of queries, showing each card's own
// custom-field groups plus its home board's board-wide groups.
// It performs no permission check: callers pass cards the user may already see.
func CardBadges(e *ops.Env, cardIDs []string) (map[string]Badges, error) {
	return cardBadges(e, "", cardIDs)
}

// CardBadgesOn returns face-of-card summaries for many cards as seen on boardID (when boardID is non-empty,
// only card-level custom-field groups and boardID's board-wide groups are included; empty falls back to home board).
// It performs no permission check: callers pass cards the user may already see on boardID.
func CardBadgesOn(e *ops.Env, boardID string, cardIDs []string) (map[string]Badges, error) {
	return cardBadges(e, boardID, cardIDs)
}

func cardBadges(e *ops.Env, boardID string, cardIDs []string) (map[string]Badges, error) {
	out := map[string]Badges{}
	if len(cardIDs) == 0 {
		return out, nil
	}
	get := func(id string) Badges { return out[id] }

	for start := 0; start < len(cardIDs); start += badgeChunkSize {
		chunk := cardIDs[start:min(start+badgeChunkSize, len(cardIDs))]
		in := "(" + inPlaceholders(len(chunk)) + ")"
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

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
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
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
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()

		fieldFilter := ` AND (g.board_id IS NULL OR g.board_id=(SELECT board_id FROM cards WHERE id=v.card_id))`
		fieldArgs := args
		if boardID != "" {
			fieldFilter = ` AND (g.board_id IS NULL OR g.board_id=?)`
			fieldArgs = append(append([]any{}, args...), boardID)
		}
		rows, err = e.DB.QueryContext(e.Ctx, `SELECT v.card_id,COALESCE(g.name,bg.name,''),f.name,f.type,COALESCE(v.content,'')
			FROM custom_field_values v JOIN custom_fields f ON f.id=v.field_id JOIN custom_field_groups g ON g.id=v.group_id
			LEFT JOIN base_custom_field_groups bg ON bg.id=g.base_group_id
			WHERE f.show_on_front_of_card=1 AND v.card_id IN `+in+fieldFilter+` AND v.content IS NOT NULL AND v.content<>''
			ORDER BY g.position,f.position,v.row_id`, fieldArgs...)
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
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}
