package cards

import (
	"database/sql"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// IdleAfter is how long an editing heartbeat may be silent before another user may take over.
const IdleAfter = 60 * time.Second

// EditHolder is a user currently (or recently) editing a card field.
type EditHolder struct {
	UserID    string `json:"userId"`
	Name      string `json:"name"`
	Field     string `json:"field"`
	TouchedAt string `json:"touchedAt"`
	Idle      bool   `json:"idle" desc:"no heartbeat for more than 60s: others may take over"`
	Me        bool   `json:"me"`
}

type EditTouchIn struct {
	ID       string `path:"id"`
	Field    string `json:"field" desc:"field being edited, default description"`
	Takeover bool   `json:"takeover" desc:"claim the lock from a holder who has been idle > 60s"`
}

type EditReleaseIn struct {
	ID    string `path:"id"`
	Field string `json:"field" desc:"default description"`
}

type EditTouchOut struct {
	Granted bool         `json:"granted"`
	Holder  *EditHolder  `json:"holder,omitempty" desc:"the user blocking you when not granted"`
	Editors []EditHolder `json:"editors"`
}

type EditListOut struct {
	Editors []EditHolder `json:"editors"`
}

func init() {
	ops.Register(ops.Op{Name: "card.edit.touch", Summary: "Claim/heartbeat the collaborative edit lock on a card field", Method: "POST", Path: "/cards/{id}/editing",
		UI: "POST /ui/cards/{id}/editing/touch"}, editTouch)
	ops.Register(ops.Op{Name: "card.edit.release", Summary: "Release your edit lock on a card field", Method: "DELETE", Path: "/cards/{id}/editing",
		UI: "POST /ui/cards/{id}/editing/release"}, func(e *ops.Env, in EditReleaseIn) (EditListOut, error) {
		boardID, err := viewCard(e, in.ID)
		if err != nil {
			return EditListOut{}, err
		}
		f := in.Field
		if f == "" {
			f = "description"
		}
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM card_editing WHERE card_id=? AND user_id=? AND field=?`, in.ID, e.User.UserID, f); err != nil {
			return EditListOut{}, err
		}
		publishEditing(e, boardID, in.ID)
		return EditListOut{editors(e, in.ID)}, nil
	})
	ops.Register(ops.Op{Name: "card.edit.list", Summary: "Who is editing which fields of a card", Method: "GET", Path: "/cards/{id}/editing",
		UI: "GET /ui/cards/{id}/editing"}, func(e *ops.Env, in IDIn) (EditListOut, error) {
		if _, err := viewCard(e, in.ID); err != nil {
			return EditListOut{}, err
		}
		return EditListOut{editors(e, in.ID)}, nil
	})
}

func publishEditing(e *ops.Env, boardID, cardID string) {
	e.Hub.Publish(rtEvent("card.editing", "board:"+boardID, map[string]any{"cardId": cardID, "editors": editors(e, cardID)}))
}

// editors lists lock holders (everything newer than 10 minutes), freshest first.
func editors(e *ops.Env, cardID string) []EditHolder {
	out := []EditHolder{}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT ce.user_id,COALESCE(u.name,''),ce.field,ce.touched_at FROM card_editing ce LEFT JOIN users u ON u.id=ce.user_id
		WHERE ce.card_id=? ORDER BY ce.touched_at DESC`, cardID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var h EditHolder
		_ = rows.Scan(&h.UserID, &h.Name, &h.Field, &h.TouchedAt)
		if t, err := time.Parse(time.RFC3339, h.TouchedAt); err == nil {
			h.Idle = time.Since(t) > IdleAfter
		}
		h.Me = e.User != nil && h.UserID == e.User.UserID
		out = append(out, h)
	}
	return out
}

func editTouch(e *ops.Env, in EditTouchIn) (EditTouchOut, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return EditTouchOut{}, err
	}
	field := in.Field
	if field == "" {
		field = "description"
	}
	var blocker *EditHolder
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		cutoff := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
		_, _ = tx.ExecContext(e.Ctx, `DELETE FROM card_editing WHERE touched_at<?`, cutoff)
		rows, err := tx.QueryContext(e.Ctx, `SELECT ce.user_id,COALESCE(u.name,''),ce.touched_at FROM card_editing ce LEFT JOIN users u ON u.id=ce.user_id
			WHERE ce.card_id=? AND ce.field=? AND ce.user_id<>?`, in.ID, field, e.User.UserID)
		if err != nil {
			return err
		}
		var others []EditHolder
		for rows.Next() {
			h := EditHolder{Field: field}
			_ = rows.Scan(&h.UserID, &h.Name, &h.TouchedAt)
			if t, err := time.Parse(time.RFC3339, h.TouchedAt); err == nil {
				h.Idle = time.Since(t) > IdleAfter
			}
			others = append(others, h)
		}
		rows.Close()
		for i := range others {
			if !others[i].Idle || !in.Takeover {
				blocker = &others[i]
				return nil
			}
		}
		for _, h := range others { // takeover of idle holders
			_, _ = tx.ExecContext(e.Ctx, `DELETE FROM card_editing WHERE card_id=? AND field=? AND user_id=?`, in.ID, field, h.UserID)
		}
		_, err = tx.ExecContext(e.Ctx, `INSERT INTO card_editing(card_id,user_id,field,touched_at) VALUES(?,?,?,?)
			ON CONFLICT(card_id,user_id,field) DO UPDATE SET touched_at=excluded.touched_at`, in.ID, e.User.UserID, field, db.Now())
		return err
	})
	if err != nil {
		return EditTouchOut{}, err
	}
	if blocker == nil {
		publishEditing(e, boardID, in.ID)
	}
	return EditTouchOut{Granted: blocker == nil, Holder: blocker, Editors: editors(e, in.ID)}, nil
}

// ---- calendar + map views

type CalendarIn struct {
	BoardID string `path:"boardId"`
	Month   string `query:"month" desc:"YYYY-MM, default current month"`
}

type CalendarCard struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	DueDate        string   `json:"dueDate"`
	IsDueCompleted bool     `json:"isDueCompleted"`
	ListID         string   `json:"listId"`
	Colors         []string `json:"labelColors"`
}

type CalendarOut struct {
	Month       string         `json:"month"`
	From        string         `json:"from" desc:"first grid day (Monday)"`
	To          string         `json:"to" desc:"last grid day (Sunday)"`
	Cards       []CalendarCard `json:"cards"`
	Unscheduled []CalendarCard `json:"unscheduled" desc:"active cards without due date (up to 50)"`
}

type MapPoint struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Lat          float64 `json:"lat"`
	Lng          float64 `json:"lng"`
	LocationName string  `json:"locationName"`
	ListID       string  `json:"listId"`
}

type MapOut struct {
	Points []MapPoint `json:"points"`
}

func init() {
	ops.Register(ops.Op{Name: "card.calendar", Summary: "Cards of a board by due date for a month grid", Method: "GET", Path: "/boards/{boardId}/calendar",
		UI: "GET /boards/{id}/calendar"}, calendar)
	ops.Register(ops.Op{Name: "card.map", Summary: "Cards of a board that have a map location", Method: "GET", Path: "/boards/{boardId}/map",
		UI: "GET /boards/{id}/map"}, func(e *ops.Env, in InboxIn) (MapOut, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return MapOut{}, err
		}
		out := MapOut{Points: []MapPoint{}}
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT c.id,c.name,c.lat,c.lng,COALESCE(c.location_name,''),c.list_id FROM cards c JOIN lists l ON l.id=c.list_id
			WHERE c.board_id=? AND c.lat IS NOT NULL AND c.lng IS NOT NULL AND c.is_draft=0 AND c.is_template=0 AND l.type IN ('active','closed') ORDER BY c.name`, in.BoardID)
		if err != nil {
			return out, err
		}
		defer rows.Close()
		for rows.Next() {
			var p MapPoint
			_ = rows.Scan(&p.ID, &p.Name, &p.Lat, &p.Lng, &p.LocationName, &p.ListID)
			out.Points = append(out.Points, p)
		}
		return out, nil
	})
}

func calendar(e *ops.Env, in CalendarIn) (CalendarOut, error) {
	if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
		return CalendarOut{}, err
	}
	m := time.Now().UTC()
	if in.Month != "" {
		t, err := time.Parse("2006-01", in.Month)
		if err != nil {
			return CalendarOut{}, ops.Invalid("month must be YYYY-MM")
		}
		m = t
	}
	first := time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, time.UTC)
	from := first.AddDate(0, 0, -((int(first.Weekday()) + 6) % 7)) // back to Monday
	last := first.AddDate(0, 1, -1)
	to := last.AddDate(0, 0, (7-int(last.Weekday()))%7) // forward to Sunday
	out := CalendarOut{Month: first.Format("2006-01"), From: from.Format("2006-01-02"), To: to.Format("2006-01-02"), Cards: []CalendarCard{}, Unscheduled: []CalendarCard{}}
	load := func(q string, args ...any) []CalendarCard {
		res := []CalendarCard{}
		rows, err := e.DB.QueryContext(e.Ctx, q, args...)
		if err != nil {
			return res
		}
		defer rows.Close()
		for rows.Next() {
			var c CalendarCard
			var due *string
			var done *bool
			_ = rows.Scan(&c.ID, &c.Name, &due, &done, &c.ListID)
			if due != nil {
				c.DueDate = *due
			}
			c.IsDueCompleted = done != nil && *done
			c.Colors = strs(e, e.DB, `SELECT l.color FROM card_labels cl JOIN labels l ON l.id=cl.label_id WHERE cl.card_id=? ORDER BY l.position`, c.ID)
			res = append(res, c)
		}
		return res
	}
	out.Cards = load(`SELECT c.id,c.name,c.due_date,c.is_due_completed,c.list_id FROM cards c JOIN lists l ON l.id=c.list_id
		WHERE c.board_id=? AND c.due_date>=? AND c.due_date<? AND c.is_draft=0 AND c.is_template=0 AND l.type IN ('active','closed') ORDER BY c.due_date`,
		in.BoardID, from.Format("2006-01-02"), to.AddDate(0, 0, 1).Format("2006-01-02"))
	out.Unscheduled = load(`SELECT c.id,c.name,c.due_date,c.is_due_completed,c.list_id FROM cards c JOIN lists l ON l.id=c.list_id
		WHERE c.board_id=? AND c.due_date IS NULL AND c.is_draft=0 AND c.is_template=0 AND l.type='active' ORDER BY c.updated_at DESC LIMIT 50`, in.BoardID)
	return out, nil
}
