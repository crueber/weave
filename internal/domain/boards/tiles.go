package boards

import (
	"database/sql"
	"strings"
	"time"

	"github.com/crueber/weave/internal/ops"
)

// loadTiles reads the compact card tiles of a board's kanban lists (read-only SQL
// over tables owned by the cards/cardextras domains).
func loadTiles(e *ops.Env, b Board, idx map[string]int, lists []List) ([]Tile, error) {
	return loadTilesOnly(e, b, idx, lists, nil)
}

// loadTilesOnly is loadTiles restricted to the given card ids (all cards when ids is empty).
func loadTilesOnly(e *ops.Env, b Board, idx map[string]int, lists []List, ids []string) ([]Tile, error) {
	names := map[string]string{}
	for _, l := range lists {
		if l.Name != nil {
			names[l.ID] = *l.Name
		}
	}
	only, args := "", []any{b.ID}
	if len(ids) > 0 {
		only = " AND c.id IN (?" + strings.Repeat(",?", len(ids)-1) + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT c.id,c.list_id,c.name,COALESCE(c.description,''),c.type,COALESCE(c.due_date,''),COALESCE(c.is_due_completed,0),
		c.comments_total,c.is_closed,COALESCE(c.updated_at,c.created_at),
		(SELECT COUNT(*) FROM tasks t JOIN task_lists tl ON tl.id=t.task_list_id WHERE tl.card_id=c.id),
		(SELECT COUNT(*) FROM tasks t JOIN task_lists tl ON tl.id=t.task_list_id WHERE tl.card_id=c.id AND t.is_completed=1),
		(SELECT COUNT(*) FROM attachments a WHERE a.card_id=c.id),
		COALESCE(c.cover_attachment_id,''),
		COALESCE((SELECT name FROM users WHERE id=c.claimed_by),''), COALESCE(c.claimed_until,'')
		FROM cards c JOIN lists l ON l.id=c.list_id
		WHERE c.board_id=? AND l.type IN ('active','closed') AND c.is_draft=0 AND c.is_template=0`+only+`
		ORDER BY l.position, c.position, c.id`, args...)
	if err != nil {
		return nil, err
	}
	var tiles []Tile
	byID := map[string]int{}
	now := time.Now()
	for rows.Next() {
		var t Tile
		var done bool
		var upd string
		if err := rows.Scan(&t.ID, &t.ListID, &t.Name, &t.desc, &t.Type, &t.DueRaw, &done, &t.Comments, &t.Closed, &upd,
			&t.TasksTotal, &t.TasksDone, &t.Attachments, &t.CoverID, &t.ClaimedBy, &t.ClaimedUntilRaw); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := idx[t.ListID]; !ok {
			continue
		}
		if cu, ok := parseTime(t.ClaimedUntilRaw); ok && t.ClaimedBy != "" && cu.After(now) {
			t.ClaimedUntil = cu.Local().Format("15:04")
		} else {
			t.ClaimedBy, t.ClaimedUntilRaw = "", ""
		}
		t.ListName = names[t.ListID]
		t.HasDescription = strings.TrimSpace(t.desc) != ""
		if d, ok := parseTime(t.DueRaw); ok {
			t.Due = d.Format("Jan 2")
			switch {
			case done:
				t.DueState = "done"
			case d.Before(now):
				t.DueState = "overdue"
			case d.Before(now.Add(24 * time.Hour)):
				t.DueState = "soon"
			}
		}
		if u, ok := parseTime(upd); ok {
			t.AgeDays = int(now.Sub(u).Hours() / 24)
		}
		byID[t.ID] = len(tiles)
		tiles = append(tiles, t)
	}
	rows.Close()
	if len(tiles) == 0 {
		return tiles, nil
	}
	if err := eachRow(e, `SELECT cl.card_id,l.id,COALESCE(l.name,''),l.color FROM card_labels cl JOIN labels l ON l.id=cl.label_id JOIN cards c ON c.id=cl.card_id
		WHERE c.board_id=? ORDER BY l.position, l.id`, b.ID, func(r *sql.Rows) {
		var cid string
		var l TileLabel
		_ = r.Scan(&cid, &l.ID, &l.Name, &l.Color)
		if i, ok := byID[cid]; ok {
			tiles[i].Labels = append(tiles[i].Labels, l)
		}
	}); err != nil {
		return nil, err
	}
	if err := eachRow(e, `SELECT cm.card_id,u.id,u.name,COALESCE(u.avatar,''),u.is_agent FROM card_memberships cm JOIN users u ON u.id=cm.user_id JOIN cards c ON c.id=cm.card_id
		WHERE c.board_id=? ORDER BY u.name`, b.ID, func(r *sql.Rows) {
		var cid string
		var m TileMember
		_ = r.Scan(&cid, &m.ID, &m.Name, &m.Avatar, &m.IsAgent)
		if i, ok := byID[cid]; ok {
			tiles[i].Members = append(tiles[i].Members, m)
		}
	}); err != nil {
		return nil, err
	}
	return tiles, nil
}

func eachRow(e *ops.Env, q, arg string, fn func(*sql.Rows)) error {
	rows, err := e.DB.QueryContext(e.Ctx, q, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		fn(rows)
	}
	return rows.Err()
}
