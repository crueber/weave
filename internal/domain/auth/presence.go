package auth

import (
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// A client heartbeat is expected at least every minute; silence for presenceTTL means offline.
const presenceTTL = 3 * time.Minute

type PresenceIn struct {
	State string `json:"state" required:"true" desc:"online|idle|away|offline"`
}
type PresenceEntry struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	State  string `json:"state"`
}
type PresenceList struct {
	Items []PresenceEntry `json:"items"`
}

func effectivePresence(state string, lastSeen *string) string {
	if state == "" || state == "offline" || lastSeen == nil {
		return "offline"
	}
	t, err := time.Parse(time.RFC3339, *lastSeen)
	if err != nil || time.Since(t) > presenceTTL {
		return "offline"
	}
	return state
}

func init() {
	ops.Register(ops.Op{Name: "presence.set", Summary: "Report your presence (heartbeat; call at least every minute while active)", Method: "POST", Path: "/presence", UI: "POST /ui/presence (static/auth_presence.js heartbeat)"},
		func(e *ops.Env, in PresenceIn) (OKOut, error) {
			switch in.State {
			case "online", "idle", "away", "offline":
			default:
				return OKOut{}, ops.Invalid("state must be online|idle|away|offline")
			}
			var prev string
			var seen *string
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT presence,last_seen_at FROM users WHERE id=?`, e.User.UserID).Scan(&prev, &seen)
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE users SET presence=?,last_seen_at=? WHERE id=?`, in.State, db.Now(), e.User.UserID); err != nil {
				return OKOut{}, err
			}
			if effectivePresence(prev, seen) != in.State {
				e.Hub.Publish(realtime.Event{Name: "presence.updated", Room: "global", Data: map[string]any{"userId": e.User.UserID, "state": in.State}})
			}
			return OKOut{true}, nil
		})

	ops.Register(ops.Op{Name: "presence.list", Summary: "Presence of every user who is not offline", Method: "GET", Path: "/presence", UI: "GET /admin/users (presence dot)"},
		func(e *ops.Env, _ NoInput) (PresenceList, error) {
			rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,name,presence,last_seen_at FROM users WHERE presence<>'offline' AND deleted_at IS NULL AND is_deactivated=0 AND last_seen_at>? ORDER BY name`,
				time.Now().UTC().Add(-presenceTTL).Format(time.RFC3339))
			if err != nil {
				return PresenceList{}, err
			}
			defer rows.Close()
			out := PresenceList{Items: []PresenceEntry{}}
			for rows.Next() {
				var p PresenceEntry
				var seen *string
				if err := rows.Scan(&p.UserID, &p.Name, &p.State, &seen); err != nil {
					return out, err
				}
				out.Items = append(out.Items, p)
			}
			return out, rows.Err()
		})
}
