package notify

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/types"
)

// ---- notifications -------------------------------------------------------

type ListIn struct {
	Unread bool   `query:"unread" desc:"only unread notifications"`
	Limit  int    `query:"limit" desc:"max items (default 50, max 200)"`
	Before string `query:"before" desc:"cursor: return notifications older than this id"`
}
type ListOut struct {
	Items       []NotificationOut `json:"items"`
	UnreadCount int               `json:"unreadCount"`
	NextBefore  string            `json:"nextBefore,omitempty"`
}
type IDIn struct {
	ID string `path:"id" desc:"id"`
}
type CountOut struct {
	Count int `json:"count"`
}

const notifSelect = `SELECT n.id,n.user_id,COALESCE(n.creator_user_id,''),COALESCE(u.name,''),COALESCE(n.board_id,''),COALESCE(b.name,''),
 COALESCE(n.card_id,''),COALESCE(c.name,''),COALESCE(n.comment_id,''),COALESCE(n.action_id,''),n.type,n.data,n.is_read,n.created_at
 FROM notifications n LEFT JOIN users u ON u.id=n.creator_user_id LEFT JOIN boards b ON b.id=n.board_id LEFT JOIN cards c ON c.id=n.card_id`

func scanNotif(r interface{ Scan(...any) error }) (NotificationOut, error) {
	var n NotificationOut
	var data string
	err := r.Scan(&n.ID, &n.UserID, &n.CreatorID, &n.CreatorName, &n.BoardID, &n.BoardName, &n.CardID, &n.CardName, &n.CommentID, &n.ActionID, &n.Type, &data, &n.IsRead, &n.CreatedAt)
	n.Data = map[string]any{}
	_ = json.Unmarshal([]byte(data), &n.Data)
	if n.CardName == "" {
		if c, ok := n.Data["card"].(map[string]any); ok {
			n.CardName = str(c, "name")
		}
	}
	n.Summary = summaryOf(n)
	return n, err
}

func unreadCount(e *ops.Env) int {
	var c int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM notifications WHERE user_id=? AND is_read=0`, e.User.UserID).Scan(&c)
	return c
}

func pushUpdate(e *ops.Env, action string, data any) {
	if e.Hub != nil {
		e.Hub.Publish(realtime.Event{Name: "notification", Room: "user:" + e.User.UserID, Data: map[string]any{"action": action, "item": data}})
	}
}

// ReadCardNotifications marks all of the caller's notifications for a card as read
// (exported for the cards domain `card.read-notifications`). Returns the number changed.
func ReadCardNotifications(e *ops.Env, cardID string) (int, error) {
	if e.User == nil {
		return 0, ops.Unauthorized()
	}
	res, err := e.DB.ExecContext(e.Ctx, `UPDATE notifications SET is_read=1 WHERE user_id=? AND card_id=? AND is_read=0`, e.User.UserID, cardID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		pushUpdate(e, "read", map[string]any{"cardId": cardID})
	}
	return int(n), nil
}

func init() {
	ops.Register(ops.Op{Name: "notification.list", Summary: "List my notifications", Method: "GET", Path: "/notifications", UI: "GET /notifications"},
		func(e *ops.Env, in ListIn) (ListOut, error) {
			limit := in.Limit
			if limit <= 0 || limit > 200 {
				limit = 50
			}
			q := notifSelect + ` WHERE n.user_id=?`
			args := []any{e.User.UserID}
			if in.Unread {
				q += ` AND n.is_read=0`
			}
			if in.Before != "" {
				q += ` AND n.id<?`
				args = append(args, in.Before)
			}
			q += ` ORDER BY n.id DESC LIMIT ?`
			args = append(args, limit+1)
			rows, err := e.DB.QueryContext(e.Ctx, q, args...)
			if err != nil {
				return ListOut{}, err
			}
			defer rows.Close()
			out := ListOut{Items: []NotificationOut{}}
			for rows.Next() {
				n, err := scanNotif(rows)
				if err != nil {
					return out, err
				}
				out.Items = append(out.Items, n)
			}
			if len(out.Items) > limit {
				out.Items = out.Items[:limit]
				out.NextBefore = out.Items[limit-1].ID
			}
			out.UnreadCount = unreadCount(e)
			return out, nil
		})
	ops.Register(ops.Op{Name: "notification.unread_count", Summary: "Number of my unread notifications", Method: "GET", Path: "/notifications/unread-count", UI: "GET /ui/notifications/badge"},
		func(e *ops.Env, _ NoInput) (CountOut, error) { return CountOut{unreadCount(e)}, nil })
	ops.Register(ops.Op{Name: "notification.read", Summary: "Mark one notification as read", Method: "POST", Path: "/notifications/{id}/read", UI: "POST /ui/notifications/{id}/read"},
		func(e *ops.Env, in IDIn) (NotificationOut, error) {
			res, err := e.DB.ExecContext(e.Ctx, `UPDATE notifications SET is_read=1 WHERE id=? AND user_id=?`, in.ID, e.User.UserID)
			if err != nil {
				return NotificationOut{}, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				var x string
				if e.DB.QueryRowContext(e.Ctx, `SELECT id FROM notifications WHERE id=? AND user_id=?`, in.ID, e.User.UserID).Scan(&x) != nil {
					return NotificationOut{}, ops.NotFound("notification")
				}
			}
			n, err := scanNotif(e.DB.QueryRowContext(e.Ctx, notifSelect+` WHERE n.id=?`, in.ID))
			if err == nil {
				pushUpdate(e, "update", n)
			}
			return n, err
		})
	ops.Register(ops.Op{Name: "notification.read_all", Summary: "Mark all my notifications as read", Method: "POST", Path: "/notifications/read-all", UI: "POST /ui/notifications/read-all"},
		func(e *ops.Env, _ NoInput) (CountOut, error) {
			res, err := e.DB.ExecContext(e.Ctx, `UPDATE notifications SET is_read=1 WHERE user_id=? AND is_read=0`, e.User.UserID)
			if err != nil {
				return CountOut{}, err
			}
			n, _ := res.RowsAffected()
			pushUpdate(e, "read_all", nil)
			return CountOut{int(n)}, nil
		})
}

// ---- notification services ----------------------------------------------

// Sender delivers one message to a shoutrrr URL. Replaceable in tests.
var Sender = shoutrrrSend

func shoutrrrSend(url, title, body string) error {
	s, err := shoutrrr.CreateSender(url)
	if err != nil {
		return err
	}
	p := types.Params{"title": title}
	for _, e := range s.Send(body, &p) {
		if e != nil {
			return e
		}
	}
	return nil
}

type ServiceOut struct {
	ID        string `json:"id"`
	UserID    string `json:"userId,omitempty"`
	BoardID   string `json:"boardId,omitempty"`
	URL       string `json:"url"`
	Format    string `json:"format"`
	CreatedAt string `json:"createdAt"`
}
type ServiceListIn struct {
	BoardID string `query:"boardId" desc:"board whose services to list (needs board manage rights); omit for my own"`
}
type ServiceListOut struct {
	Items []ServiceOut `json:"items"`
}
type ServiceCreateIn struct {
	URL     string `json:"url" required:"true" desc:"shoutrrr service URL, e.g. discord://token@id or generic://host/path"`
	Format  string `json:"format" desc:"text (default) | markdown | html"`
	BoardID string `json:"boardId" desc:"attach to a board instead of myself (needs board manage rights)"`
	UserID  string `json:"userId" desc:"admin only: attach to another user"`
}
type ServiceUpdateIn struct {
	ID     string  `path:"id"`
	URL    *string `json:"url" desc:"new shoutrrr URL"`
	Format *string `json:"format" desc:"text | markdown | html"`
}

func validService(ctx context.Context, url, format string) error {
	if _, err := shoutrrr.CreateSender(url); err != nil {
		return ops.Invalid("invalid notification service URL: " + err.Error())
	}
	switch format {
	case "text", "markdown", "html":
	default:
		return ops.Invalid("format must be text, markdown or html")
	}
	if err := checkDestination(ctx, url); err != nil {
		return ops.Invalid("destination not allowed: loopback, link-local/cloud-metadata and unspecified addresses are blocked (see WEAVE_NOTIFY_ALLOW_HOSTS)")
	}
	return nil
}

func loadService(e *ops.Env, id string) (ServiceOut, error) {
	var s ServiceOut
	var u, b sql.NullString
	err := e.DB.QueryRowContext(e.Ctx, `SELECT id,user_id,board_id,url,format,created_at FROM notification_services WHERE id=?`, id).Scan(&s.ID, &u, &b, &s.URL, &s.Format, &s.CreatedAt)
	if err != nil {
		return s, ops.NotFound("notification service")
	}
	s.UserID, s.BoardID = u.String, b.String
	return s, nil
}

func authService(e *ops.Env, s ServiceOut) error {
	if s.BoardID != "" {
		return authz.RequireBoard(e, s.BoardID, authz.Manage)
	}
	if s.UserID != e.User.UserID && e.User.Role != "admin" {
		return ops.NotFound("notification service")
	}
	return nil
}

func serviceEvent(e *ops.Env, typ, name string, s ServiceOut) {
	// the service URL usually carries credentials (bot tokens): never put it in events,
	// which reach realtime rooms, the activity log and webhooks. Managers read it via the REST list.
	item := map[string]any{"id": s.ID, "userId": s.UserID, "boardId": s.BoardID, "format": s.Format, "createdAt": s.CreatedAt}
	ev := ops.Event{Type: typ, BoardID: s.BoardID, Name: name, Data: map[string]any{"item": item}}
	if s.BoardID == "" {
		ev.Room = "user:" + s.UserID
	}
	Record(e, ev)
}

func init() {
	ops.Register(ops.Op{Name: "notification_service.list", Summary: "List my (or a board's) notification services", Method: "GET", Path: "/notification-services", UI: "GET /notification-services"},
		func(e *ops.Env, in ServiceListIn) (ServiceListOut, error) {
			q, arg := `WHERE user_id=?`, e.User.UserID
			if in.BoardID != "" {
				if err := authz.RequireBoard(e, in.BoardID, authz.Manage); err != nil {
					return ServiceListOut{}, err
				}
				q, arg = `WHERE board_id=?`, in.BoardID
			}
			rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,COALESCE(user_id,''),COALESCE(board_id,''),url,format,created_at FROM notification_services `+q+` ORDER BY created_at`, arg)
			if err != nil {
				return ServiceListOut{}, err
			}
			defer rows.Close()
			out := ServiceListOut{Items: []ServiceOut{}}
			for rows.Next() {
				var s ServiceOut
				_ = rows.Scan(&s.ID, &s.UserID, &s.BoardID, &s.URL, &s.Format, &s.CreatedAt)
				out.Items = append(out.Items, s)
			}
			return out, nil
		})
	ops.Register(ops.Op{Name: "notification_service.create", Summary: "Add a notification service (shoutrrr URL) to me or a board", Method: "POST", Path: "/notification-services", UI: "POST /ui/notification-services"},
		func(e *ops.Env, in ServiceCreateIn) (ServiceOut, error) {
			if in.Format == "" {
				in.Format = "text"
			}
			if err := validService(e.Ctx, in.URL, in.Format); err != nil {
				return ServiceOut{}, err
			}
			s := ServiceOut{ID: db.NewID(), URL: in.URL, Format: in.Format, CreatedAt: db.Now()}
			var user, board any
			if in.BoardID != "" {
				if err := authz.RequireBoard(e, in.BoardID, authz.Manage); err != nil {
					return s, err
				}
				s.BoardID, board = in.BoardID, in.BoardID
			} else {
				s.UserID = e.User.UserID
				if in.UserID != "" && e.User.Role == "admin" {
					s.UserID = in.UserID
				}
				user = s.UserID
			}
			if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO notification_services(id,user_id,board_id,url,format,created_at) VALUES(?,?,?,?,?,?)`, s.ID, user, board, s.URL, s.Format, s.CreatedAt); err != nil {
				return s, err
			}
			serviceEvent(e, "notificationServiceCreate", "notification_service.created", s)
			return s, nil
		})
	ops.Register(ops.Op{Name: "notification_service.update", Summary: "Change a notification service", Method: "PATCH", Path: "/notification-services/{id}", UI: "POST /ui/notification-services/{id}"},
		func(e *ops.Env, in ServiceUpdateIn) (ServiceOut, error) {
			s, err := loadService(e, in.ID)
			if err != nil {
				return s, err
			}
			if err := authService(e, s); err != nil {
				return s, err
			}
			if in.URL != nil {
				s.URL = *in.URL
			}
			if in.Format != nil {
				s.Format = *in.Format
			}
			if err := validService(e.Ctx, s.URL, s.Format); err != nil {
				return s, err
			}
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE notification_services SET url=?,format=? WHERE id=?`, s.URL, s.Format, s.ID); err != nil {
				return s, err
			}
			serviceEvent(e, "notificationServiceUpdate", "notification_service.updated", s)
			return s, nil
		})
	ops.Register(ops.Op{Name: "notification_service.delete", Summary: "Remove a notification service", Method: "DELETE", Path: "/notification-services/{id}", UI: "POST /ui/notification-services/{id}/delete"},
		func(e *ops.Env, in IDIn) (ServiceOut, error) {
			s, err := loadService(e, in.ID)
			if err != nil {
				return s, err
			}
			if err := authService(e, s); err != nil {
				return s, err
			}
			if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM notification_services WHERE id=?`, s.ID); err != nil {
				return s, err
			}
			serviceEvent(e, "notificationServiceDelete", "notification_service.deleted", s)
			return s, nil
		})
	ops.Register(ops.Op{Name: "notification_service.test", Summary: "Send a test message through a notification service", Method: "POST", Path: "/notification-services/{id}/test", UI: "POST /ui/notification-services/{id}/test"},
		func(e *ops.Env, in IDIn) (OKOut, error) {
			s, err := loadService(e, in.ID)
			if err != nil {
				return OKOut{}, err
			}
			if err := authService(e, s); err != nil {
				return OKOut{}, err
			}
			n := NotificationOut{CreatorName: e.User.Name, Type: "commentCard", BoardName: "Test board", CardName: "Test card", CardID: "test", Data: map[string]any{"text": "This is a test notification from Weave."}}
			title, body := Message(n, s.Format)
			if err := deliver(e.Ctx, s.URL, "Test: "+title, body); err != nil {
				return OKOut{}, err
			}
			return OKOut{true}, nil
		})
}
