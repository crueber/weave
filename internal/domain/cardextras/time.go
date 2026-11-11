package cardextras

import (
	"database/sql"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

type TimeEntryItem struct {
	ID        string  `json:"id"`
	CardID    string  `json:"cardId"`
	UserID    *string `json:"userId"`
	UserName  *string `json:"userName"`
	StartedAt string  `json:"startedAt"`
	Seconds   int     `json:"seconds"`
	Note      *string `json:"note"`
}

// TimeSummary is the card's time-tracking state. TotalSeconds includes a running stopwatch.
type TimeSummary struct {
	CardID          string          `json:"cardId"`
	RecordedSeconds int             `json:"recordedSeconds"`
	TotalSeconds    int             `json:"totalSeconds"`
	Running         bool            `json:"running"`
	StartedAt       *string         `json:"startedAt"`
	RunningUserID   *string         `json:"runningUserId"`
	RunningUserName *string         `json:"runningUserName"`
	Entries         []TimeEntryItem `json:"entries"`
}

type CardIDIn struct {
	CardID string `path:"cardId"`
}
type TimeAddIn struct {
	CardID    string `path:"cardId"`
	Hours     int    `json:"hours"`
	Minutes   int    `json:"minutes"`
	Seconds   int    `json:"seconds" desc:"Duration = hours*3600 + minutes*60 + seconds"`
	StartedAt string `json:"startedAt" desc:"RFC3339 time the work started (default now)"`
	Note      string `json:"note"`
}
type TimeUpdateIn struct {
	ID        string  `path:"id"`
	Hours     *int    `json:"hours"`
	Minutes   *int    `json:"minutes"`
	Seconds   *int    `json:"seconds" desc:"When any of hours/minutes/seconds is given the duration is recomputed from them"`
	StartedAt string  `json:"startedAt"`
	Note      *string `json:"note"`
}

func nzi(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func duration(h, m, s int) (int, error) {
	if h < 0 || m < 0 || s < 0 {
		return 0, ops.Invalid("duration cannot be negative")
	}
	d := h*3600 + m*60 + s
	if d <= 0 {
		return 0, ops.Invalid("duration must be greater than zero")
	}
	if d > 100000*3600 {
		return 0, ops.Invalid("duration too large")
	}
	return d, nil
}

func parseStart(s string) (string, error) {
	if s == "" {
		return db.Now(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", ops.Invalid("startedAt must be RFC3339")
	}
	return t.UTC().Format(time.RFC3339), nil
}

// syncTotal keeps cards.stopwatch_total_seconds equal to the sum of recorded entries.
func syncTotal(e *ops.Env, q querier, cardID string) error {
	_, err := q.ExecContext(e.Ctx, `UPDATE cards SET stopwatch_total_seconds=(SELECT COALESCE(SUM(seconds),0) FROM time_entries WHERE card_id=?) WHERE id=?`, cardID, cardID)
	return err
}

func timeSummary(e *ops.Env, cardID string) (TimeSummary, error) {
	s := TimeSummary{CardID: cardID}
	var started sql.NullString
	if err := e.DB.QueryRowContext(e.Ctx, `SELECT stopwatch_total_seconds,stopwatch_started_at FROM cards WHERE id=?`, cardID).Scan(&s.RecordedSeconds, &started); err != nil {
		return s, ops.NotFound("card")
	}
	s.TotalSeconds = s.RecordedSeconds
	if started.Valid {
		s.Running, s.StartedAt = true, &started.String
		if t, err := time.Parse(time.RFC3339, started.String); err == nil {
			if d := int(time.Since(t).Seconds()); d > 0 {
				s.TotalSeconds += d
			}
		}
		var ru, rn sql.NullString
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT w.user_id,u.name FROM stopwatch_state w LEFT JOIN users u ON u.id=w.user_id WHERE w.card_id=?`, cardID).Scan(&ru, &rn)
		s.RunningUserID, s.RunningUserName = strp(ru), strp(rn)
	}
	entries, err := query(e.Ctx, e.DB, func(r *sql.Rows) (TimeEntryItem, error) {
		var t TimeEntryItem
		var u, n, note sql.NullString
		err := r.Scan(&t.ID, &t.CardID, &u, &n, &t.StartedAt, &t.Seconds, &note)
		t.UserID, t.UserName, t.Note = strp(u), strp(n), strp(note)
		return t, err
	}, `SELECT t.id,t.card_id,t.user_id,u.name,t.started_at,t.seconds,t.note FROM time_entries t LEFT JOIN users u ON u.id=t.user_id WHERE t.card_id=? ORDER BY t.started_at DESC,t.id DESC`, cardID)
	s.Entries = entries
	return s, err
}

func entryCard(e *ops.Env, id string, min authz.Level) (cardID, board string, err error) {
	if e.DB.QueryRowContext(e.Ctx, `SELECT t.card_id,c.board_id FROM time_entries t JOIN cards c ON c.id=t.card_id WHERE t.id=?`, id).Scan(&cardID, &board) != nil {
		return "", "", ops.NotFound("time entry")
	}
	return cardID, board, authz.RequireBoard(e, board, min)
}

func init() {
	ops.Register(ops.Op{Name: "time.list", Summary: "Time tracking summary and entries for a card", Method: "GET", Path: "/cards/{cardId}/time",
		Auth: ops.AuthPublic, UI: "GET /ui/cards/{id}/time"}, func(e *ops.Env, in CardIDIn) (TimeSummary, error) {
		if _, err := authz.RequireCard(e, in.CardID, authz.View); err != nil {
			return TimeSummary{}, err
		}
		return timeSummary(e, in.CardID)
	})

	ops.Register(ops.Op{Name: "time.start", Summary: "Start the card stopwatch", Method: "POST", Path: "/cards/{cardId}/time/start",
		UI: "POST /ui/cards/{cardId}/time/start"}, func(e *ops.Env, in CardIDIn) (TimeSummary, error) {
		board, err := authz.RequireCard(e, in.CardID, authz.Edit)
		if err != nil {
			return TimeSummary{}, err
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			var started sql.NullString
			if err := tx.QueryRowContext(e.Ctx, `SELECT stopwatch_started_at FROM cards WHERE id=?`, in.CardID).Scan(&started); err != nil {
				return err
			}
			if started.Valid {
				return ops.Conflict("stopwatch already running")
			}
			if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET stopwatch_started_at=? WHERE id=?`, db.Now(), in.CardID); err != nil {
				return err
			}
			_, err := tx.ExecContext(e.Ctx, `INSERT INTO stopwatch_state(card_id,user_id) VALUES(?,?) ON CONFLICT(card_id) DO UPDATE SET user_id=excluded.user_id`, in.CardID, e.User.UserID)
			return err
		})
		if err != nil {
			return TimeSummary{}, err
		}
		emit(e, "stopwatchStart", board, in.CardID, "time.updated", nil)
		return timeSummary(e, in.CardID)
	})

	ops.Register(ops.Op{Name: "time.stop", Summary: "Stop the stopwatch and record the elapsed time as an entry", Method: "POST", Path: "/cards/{cardId}/time/stop",
		UI: "POST /ui/cards/{cardId}/time/stop"}, func(e *ops.Env, in CardIDIn) (TimeSummary, error) {
		board, err := authz.RequireCard(e, in.CardID, authz.Edit)
		if err != nil {
			return TimeSummary{}, err
		}
		elapsed := 0
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			var started string
			var starter sql.NullString
			if err := tx.QueryRowContext(e.Ctx, `SELECT COALESCE(stopwatch_started_at,'') FROM cards WHERE id=?`, in.CardID).Scan(&started); err != nil {
				return err
			}
			if started == "" {
				return ops.Conflict("stopwatch is not running")
			}
			_ = tx.QueryRowContext(e.Ctx, `SELECT user_id FROM stopwatch_state WHERE card_id=?`, in.CardID).Scan(&starter)
			if !starter.Valid {
				starter = sql.NullString{String: e.User.UserID, Valid: true}
			}
			if t, err := time.Parse(time.RFC3339, started); err == nil {
				elapsed = int(time.Since(t).Seconds())
			}
			if elapsed > 0 {
				if _, err := tx.ExecContext(e.Ctx, `INSERT INTO time_entries(id,card_id,user_id,started_at,seconds,note) VALUES(?,?,?,?,?,NULL)`,
					db.NewID(), in.CardID, starter.String, started, elapsed); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET stopwatch_started_at=NULL WHERE id=?`, in.CardID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM stopwatch_state WHERE card_id=?`, in.CardID); err != nil {
				return err
			}
			return syncTotal(e, tx, in.CardID)
		})
		if err != nil {
			return TimeSummary{}, err
		}
		emit(e, "stopwatchStop", board, in.CardID, "time.updated", map[string]any{"seconds": elapsed})
		return timeSummary(e, in.CardID)
	})

	ops.Register(ops.Op{Name: "time.add", Summary: "Add a manual time entry", Method: "POST", Path: "/cards/{cardId}/time-entries",
		UI: "POST /ui/cards/{cardId}/time-entries"}, func(e *ops.Env, in TimeAddIn) (TimeSummary, error) {
		board, err := authz.RequireCard(e, in.CardID, authz.Edit)
		if err != nil {
			return TimeSummary{}, err
		}
		d, err := duration(in.Hours, in.Minutes, in.Seconds)
		if err != nil {
			return TimeSummary{}, err
		}
		started, err := parseStart(in.StartedAt)
		if err != nil {
			return TimeSummary{}, err
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(e.Ctx, `INSERT INTO time_entries(id,card_id,user_id,started_at,seconds,note) VALUES(?,?,?,?,?,?)`,
				db.NewID(), in.CardID, e.User.UserID, started, d, nz(in.Note)); err != nil {
				return err
			}
			return syncTotal(e, tx, in.CardID)
		})
		if err != nil {
			return TimeSummary{}, err
		}
		emit(e, "timeEntryCreate", board, in.CardID, "time.updated", map[string]any{"seconds": d})
		return timeSummary(e, in.CardID)
	})

	ops.Register(ops.Op{Name: "time.update", Summary: "Edit a time entry", Method: "PATCH", Path: "/time-entries/{id}",
		UI: "POST /ui/time-entries/{id}"}, func(e *ops.Env, in TimeUpdateIn) (TimeSummary, error) {
		cardID, board, err := entryCard(e, in.ID, authz.Edit)
		if err != nil {
			return TimeSummary{}, err
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if in.Hours != nil || in.Minutes != nil || in.Seconds != nil {
				d, err := duration(nzi(in.Hours), nzi(in.Minutes), nzi(in.Seconds))
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(e.Ctx, `UPDATE time_entries SET seconds=? WHERE id=?`, d, in.ID); err != nil {
					return err
				}
			}
			if in.StartedAt != "" {
				s, err := parseStart(in.StartedAt)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(e.Ctx, `UPDATE time_entries SET started_at=? WHERE id=?`, s, in.ID); err != nil {
					return err
				}
			}
			if in.Note != nil {
				if _, err := tx.ExecContext(e.Ctx, `UPDATE time_entries SET note=? WHERE id=?`, nz(*in.Note), in.ID); err != nil {
					return err
				}
			}
			return syncTotal(e, tx, cardID)
		})
		if err != nil {
			return TimeSummary{}, err
		}
		emit(e, "timeEntryUpdate", board, cardID, "time.updated", map[string]any{"entryId": in.ID})
		return timeSummary(e, cardID)
	})

	ops.Register(ops.Op{Name: "time.delete", Summary: "Delete a time entry", Method: "DELETE", Path: "/time-entries/{id}",
		UI: "POST /ui/time-entries/{id}/delete"}, func(e *ops.Env, in IDIn) (DeletedOut, error) {
		cardID, board, err := entryCard(e, in.ID, authz.Edit)
		if err != nil {
			return DeletedOut{}, err
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM time_entries WHERE id=?`, in.ID); err != nil {
				return err
			}
			return syncTotal(e, tx, cardID)
		})
		if err != nil {
			return DeletedOut{}, err
		}
		emit(e, "timeEntryDelete", board, cardID, "time.updated", map[string]any{"entryId": in.ID})
		return DeletedOut{in.ID, cardID}, nil
	})

	ops.Register(ops.Op{Name: "time.reset", Summary: "Stop the stopwatch and delete all time entries of a card", Method: "POST", Path: "/cards/{cardId}/time/reset",
		UI: "POST /ui/cards/{cardId}/time/reset"}, func(e *ops.Env, in CardIDIn) (TimeSummary, error) {
		board, err := authz.RequireCard(e, in.CardID, authz.Edit)
		if err != nil {
			return TimeSummary{}, err
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM time_entries WHERE card_id=?`, in.CardID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM stopwatch_state WHERE card_id=?`, in.CardID); err != nil {
				return err
			}
			_, err := tx.ExecContext(e.Ctx, `UPDATE cards SET stopwatch_started_at=NULL,stopwatch_total_seconds=0 WHERE id=?`, in.CardID)
			return err
		})
		if err != nil {
			return TimeSummary{}, err
		}
		emit(e, "stopwatchReset", board, in.CardID, "time.updated", nil)
		return timeSummary(e, in.CardID)
	})
}
