package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/server"
)

// WebhookEvents is the allowlist of event names a webhook can subscribe to
// (Planka action/event names, camelCase). ops.Event.Type must be one of these
// for outgoing webhooks to fire.
var WebhookEvents = []string{
	"actionCreate",
	"attachmentCreate", "attachmentUpdate", "attachmentDelete",
	"backgroundImageCreate", "backgroundImageDelete",
	"baseCustomFieldGroupCreate", "baseCustomFieldGroupUpdate", "baseCustomFieldGroupDelete",
	"boardCreate", "boardUpdate", "boardDelete",
	"boardMembershipCreate", "boardMembershipUpdate", "boardMembershipDelete",
	"cardCreate", "cardUpdate", "cardDelete", "cardMove", "cardClaim", "cardRelease",
	"cardLabelCreate", "cardLabelDelete",
	"cardMembershipCreate", "cardMembershipDelete",
	"commentCreate", "commentUpdate", "commentDelete",
	"configUpdate",
	"customFieldCreate", "customFieldUpdate", "customFieldDelete",
	"customFieldGroupCreate", "customFieldGroupUpdate", "customFieldGroupDelete",
	"customFieldValueUpdate", "customFieldValueDelete",
	"labelCreate", "labelUpdate", "labelDelete",
	"listCreate", "listUpdate", "listClear", "listDelete",
	"notificationCreate", "notificationUpdate",
	"notificationServiceCreate", "notificationServiceUpdate", "notificationServiceDelete",
	"projectCreate", "projectUpdate", "projectDelete",
	"projectManagerCreate", "projectManagerDelete",
	"taskCreate", "taskUpdate", "taskDelete",
	"taskListCreate", "taskListUpdate", "taskListDelete",
	"userCreate", "userUpdate", "userDelete",
	"webhookCreate", "webhookUpdate", "webhookDelete",
}

var eventSet = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range WebhookEvents {
		m[e] = true
	}
	return m
}()

// RetryBackoff is the wait before each retry of a failed delivery (so 1+len attempts in total).
var RetryBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}

// HTTPClient performs webhook deliveries.
var HTTPClient = &http.Client{Timeout: 15 * time.Second}

type WebhookOut struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	URL            string   `json:"url"`
	AccessToken    string   `json:"accessToken,omitempty"` // write-only: never serialised by ops (see redacted)
	Secret         string   `json:"secret,omitempty"`      // write-only: never serialised by ops (see redacted)
	HasAccessToken bool     `json:"hasAccessToken"`
	HasSecret      bool     `json:"hasSecret"`
	Events         []string `json:"events"`
	ExcludedEvents []string `json:"excludedEvents"`
	ProjectID      string   `json:"projectId,omitempty"`
	BoardID        string   `json:"boardId,omitempty"`
	UserID         string   `json:"userId,omitempty"`
	CreatedAt      string   `json:"createdAt"`
	UpdatedAt      string   `json:"updatedAt,omitempty"`
}

type DeliveryOut struct {
	ID            string `json:"id"`
	WebhookID     string `json:"webhookId"`
	Event         string `json:"event"`
	Status        int    `json:"status"`
	Error         string `json:"error,omitempty"`
	Attempts      int    `json:"attempts"`
	State         string `json:"state"` // pending | delivered | failed
	NextAttemptAt string `json:"nextAttemptAt,omitempty"`
	DeliveredAt   string `json:"deliveredAt,omitempty"`
	DurationMs    int    `json:"durationMs"`
	ResponseBody  string `json:"responseBody,omitempty"`
	Payload       string `json:"payload,omitempty"`
	CreatedAt     string `json:"createdAt"`
}

const whCols = `id,name,url,COALESCE(access_token,''),COALESCE(secret,''),COALESCE(event_types,''),COALESCE(excluded_event_types,''),COALESCE(project_id,''),COALESCE(board_id,''),COALESCE(user_id,''),created_at,COALESCE(updated_at,'')`

func scanWebhook(r interface{ Scan(...any) error }) (WebhookOut, error) {
	var w WebhookOut
	var ev, ex string
	err := r.Scan(&w.ID, &w.Name, &w.URL, &w.AccessToken, &w.Secret, &ev, &ex, &w.ProjectID, &w.BoardID, &w.UserID, &w.CreatedAt, &w.UpdatedAt)
	w.Events, w.ExcludedEvents = []string{}, []string{}
	if ev != "" {
		_ = json.Unmarshal([]byte(ev), &w.Events)
	}
	if ex != "" {
		_ = json.Unmarshal([]byte(ex), &w.ExcludedEvents)
	}
	return w, err
}

// redacted is the API/UI shape of a webhook: the access token and signing secret are
// write-only, so only hasAccessToken / hasSecret flags are returned.
func (w WebhookOut) redacted() WebhookOut {
	w.HasAccessToken, w.HasSecret = w.AccessToken != "", w.Secret != ""
	w.AccessToken, w.Secret = "", ""
	return w
}

// eventItem is what realtime rooms and webhook forwarding see: no URL (it may carry a
// token), no access token, no secret.
func (w WebhookOut) eventItem() map[string]any {
	r := w.redacted()
	return map[string]any{"id": r.ID, "name": r.Name, "events": r.Events, "excludedEvents": r.ExcludedEvents,
		"projectId": r.ProjectID, "boardId": r.BoardID, "userId": r.UserID, "hasAccessToken": r.HasAccessToken,
		"hasSecret": r.HasSecret, "createdAt": r.CreatedAt, "updatedAt": r.UpdatedAt}
}

// StringList accepts a JSON array or a comma/space separated string.
type StringList []string

func (s *StringList) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		*s = arr
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}
	*s = strings.FieldsFunc(one, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
	return nil
}

type WebhookCreateIn struct {
	Name           string      `json:"name" required:"true" desc:"label"`
	URL            string      `json:"url" required:"true" desc:"http(s) endpoint receiving POSTs"`
	AccessToken    string      `json:"accessToken" desc:"sent as 'Authorization: Bearer <token>'; also the HMAC key when no secret is set"`
	Secret         string      `json:"secret" desc:"HMAC-SHA256 signing key (headers X-Weave-Signature: sha256=<hex> and X-Weave-Signature-V2: t=<ts>,v1=<hex>; see docs/WEBHOOKS.md)"`
	Events         *StringList `json:"events" desc:"only these event names (empty = all); see webhook.events"`
	ExcludedEvents *StringList `json:"excludedEvents" desc:"never send these event names"`
	ProjectID      string      `json:"projectId" desc:"scope: only events inside this project"`
	BoardID        string      `json:"boardId" desc:"scope: only events inside this board"`
	UserID         string      `json:"userId" desc:"scope: only events caused by this user"`
}
type WebhookUpdateIn struct {
	ID             string      `path:"id"`
	Name           *string     `json:"name"`
	URL            *string     `json:"url"`
	AccessToken    *string     `json:"accessToken"`
	Secret         *string     `json:"secret"`
	Events         *StringList `json:"events"`
	ExcludedEvents *StringList `json:"excludedEvents"`
	ProjectID      *string     `json:"projectId"`
	BoardID        *string     `json:"boardId"`
	UserID         *string     `json:"userId"`
}
type WebhookListOut struct {
	Items []WebhookOut `json:"items"`
}
type EventsOut struct {
	Items []string `json:"items"`
}
type DeliveriesIn struct {
	ID    string `path:"id"`
	Limit int    `query:"limit" desc:"max rows (default 50, max 500)"`
}
type DeliveriesOut struct {
	Items []DeliveryOut `json:"items"`
}

func validateEvents(l []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range l {
		if e == "" || seen[e] {
			continue
		}
		if !eventSet[e] {
			return nil, ops.Invalid("unknown event " + strconv.Quote(e))
		}
		seen[e] = true
		out = append(out, e)
	}
	return out, nil
}

func validateWebhook(e *ops.Env, w *WebhookOut) error {
	if strings.TrimSpace(w.Name) == "" {
		return ops.Invalid("name required")
	}
	u, err := url.Parse(w.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ops.Invalid("url must be an http(s) URL")
	}
	var x string
	if w.ProjectID != "" && e.DB.QueryRowContext(e.Ctx, `SELECT id FROM projects WHERE id=?`, w.ProjectID).Scan(&x) != nil {
		return ops.Invalid("unknown projectId")
	}
	if w.BoardID != "" && e.DB.QueryRowContext(e.Ctx, `SELECT id FROM boards WHERE id=?`, w.BoardID).Scan(&x) != nil {
		return ops.Invalid("unknown boardId")
	}
	if w.UserID != "" && e.DB.QueryRowContext(e.Ctx, `SELECT id FROM users WHERE id=?`, w.UserID).Scan(&x) != nil {
		return ops.Invalid("unknown userId")
	}
	return nil
}

func nullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func jsonList(l []string) any {
	if len(l) == 0 {
		return nil
	}
	b, _ := json.Marshal(l)
	return string(b)
}

func getWebhook(e *ops.Env, id string) (WebhookOut, error) {
	w, err := scanWebhook(e.DB.QueryRowContext(e.Ctx, `SELECT `+whCols+` FROM webhooks WHERE id=?`, id))
	if err != nil {
		return w, ops.NotFound("webhook")
	}
	return w, nil
}

func saveWebhook(e *ops.Env, w WebhookOut, insert bool) error {
	if insert {
		_, err := e.DB.ExecContext(e.Ctx, `INSERT INTO webhooks(id,name,url,access_token,secret,event_types,excluded_event_types,project_id,board_id,user_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			w.ID, w.Name, w.URL, nullIf(w.AccessToken), nullIf(w.Secret), jsonList(w.Events), jsonList(w.ExcludedEvents), nullIf(w.ProjectID), nullIf(w.BoardID), nullIf(w.UserID), w.CreatedAt)
		return err
	}
	_, err := e.DB.ExecContext(e.Ctx, `UPDATE webhooks SET name=?,url=?,access_token=?,secret=?,event_types=?,excluded_event_types=?,project_id=?,board_id=?,user_id=?,updated_at=? WHERE id=?`,
		w.Name, w.URL, nullIf(w.AccessToken), nullIf(w.Secret), jsonList(w.Events), jsonList(w.ExcludedEvents), nullIf(w.ProjectID), nullIf(w.BoardID), nullIf(w.UserID), w.UpdatedAt, w.ID)
	return err
}

func init() {
	ops.Register(ops.Op{Name: "webhook.events", Summary: "List the event names webhooks can subscribe to", Method: "GET", Path: "/webhooks/events", Auth: ops.AuthAdmin, UI: "GET /admin/webhooks"},
		func(e *ops.Env, _ NoInput) (EventsOut, error) {
			return EventsOut{append([]string(nil), WebhookEvents...)}, nil
		})
	ops.Register(ops.Op{Name: "webhook.list", Summary: "List webhooks", Method: "GET", Path: "/webhooks", Auth: ops.AuthAdmin, UI: "GET /admin/webhooks"},
		func(e *ops.Env, _ NoInput) (WebhookListOut, error) {
			rows, err := e.DB.QueryContext(e.Ctx, `SELECT `+whCols+` FROM webhooks ORDER BY created_at`)
			if err != nil {
				return WebhookListOut{}, err
			}
			defer rows.Close()
			out := WebhookListOut{Items: []WebhookOut{}}
			for rows.Next() {
				w, _ := scanWebhook(rows)
				out.Items = append(out.Items, w.redacted())
			}
			return out, nil
		})
	ops.Register(ops.Op{Name: "webhook.create", Summary: "Create a webhook", Method: "POST", Path: "/webhooks", Auth: ops.AuthAdmin, UI: "POST /ui/admin/webhooks"},
		func(e *ops.Env, in WebhookCreateIn) (WebhookOut, error) {
			w := WebhookOut{ID: db.NewID(), Name: in.Name, URL: in.URL, AccessToken: in.AccessToken, Secret: in.Secret, ProjectID: in.ProjectID, BoardID: in.BoardID, UserID: in.UserID, CreatedAt: db.Now()}
			var err error
			if in.Events != nil {
				if w.Events, err = validateEvents(*in.Events); err != nil {
					return w, err
				}
			}
			if in.ExcludedEvents != nil {
				if w.ExcludedEvents, err = validateEvents(*in.ExcludedEvents); err != nil {
					return w, err
				}
			}
			if w.Events == nil {
				w.Events = []string{}
			}
			if w.ExcludedEvents == nil {
				w.ExcludedEvents = []string{}
			}
			if err := validateWebhook(e, &w); err != nil {
				return w, err
			}
			if err := saveWebhook(e, w, true); err != nil {
				return w, err
			}
			Record(e, ops.Event{Type: "webhookCreate", Name: "webhook.created", Room: "global", Data: map[string]any{"item": w.eventItem()}})
			return w.redacted(), nil
		})
	ops.Register(ops.Op{Name: "webhook.update", Summary: "Update a webhook", Method: "PATCH", Path: "/webhooks/{id}", Auth: ops.AuthAdmin, UI: "POST /ui/admin/webhooks/{id}"},
		func(e *ops.Env, in WebhookUpdateIn) (WebhookOut, error) {
			w, err := getWebhook(e, in.ID)
			if err != nil {
				return w, err
			}
			set := func(dst *string, v *string) {
				if v != nil {
					*dst = strings.TrimSpace(*v)
				}
			}
			set(&w.Name, in.Name)
			set(&w.URL, in.URL)
			set(&w.AccessToken, in.AccessToken)
			set(&w.Secret, in.Secret)
			set(&w.ProjectID, in.ProjectID)
			set(&w.BoardID, in.BoardID)
			set(&w.UserID, in.UserID)
			if in.Events != nil {
				if w.Events, err = validateEvents(*in.Events); err != nil {
					return w, err
				}
			}
			if in.ExcludedEvents != nil {
				if w.ExcludedEvents, err = validateEvents(*in.ExcludedEvents); err != nil {
					return w, err
				}
			}
			if err := validateWebhook(e, &w); err != nil {
				return w, err
			}
			w.UpdatedAt = db.Now()
			if err := saveWebhook(e, w, false); err != nil {
				return w, err
			}
			Record(e, ops.Event{Type: "webhookUpdate", Name: "webhook.updated", Room: "global", Data: map[string]any{"item": w.eventItem()}})
			return w.redacted(), nil
		})
	ops.Register(ops.Op{Name: "webhook.delete", Summary: "Delete a webhook (and its delivery log)", Method: "DELETE", Path: "/webhooks/{id}", Auth: ops.AuthAdmin, UI: "POST /ui/admin/webhooks/{id}/delete"},
		func(e *ops.Env, in IDIn) (WebhookOut, error) {
			w, err := getWebhook(e, in.ID)
			if err != nil {
				return w, err
			}
			if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM webhooks WHERE id=?`, w.ID); err != nil {
				return w, err
			}
			Record(e, ops.Event{Type: "webhookDelete", Name: "webhook.deleted", Room: "global", Data: map[string]any{"item": w.eventItem()}})
			return w.redacted(), nil
		})
	ops.Register(ops.Op{Name: "webhook.test", Summary: "Send a synchronous 'ping' event to a webhook and report the result", Method: "POST", Path: "/webhooks/{id}/test", Auth: ops.AuthAdmin, UI: "POST /ui/admin/webhooks/{id}/test"},
		func(e *ops.Env, in IDIn) (DeliveryOut, error) {
			w, err := getWebhook(e, in.ID)
			if err != nil {
				return DeliveryOut{}, err
			}
			payload := buildPayload("ping", map[string]any{"item": map[string]any{"webhookId": w.ID, "message": "Hello from Weave"}}, e.User)
			id, err := insertDelivery(e.Ctx, e.DB, w.ID, "ping", payload)
			if err != nil {
				return DeliveryOut{}, err
			}
			runDelivery(e.DB, id, false)
			return loadDelivery(e.Ctx, e.DB, id, true)
		})
	ops.Register(ops.Op{Name: "webhook.deliveries", Summary: "Delivery log of a webhook (newest first)", Method: "GET", Path: "/webhooks/{id}/deliveries", Auth: ops.AuthAdmin, UI: "GET /admin/webhooks/{id}/deliveries"},
		func(e *ops.Env, in DeliveriesIn) (DeliveriesOut, error) {
			if _, err := getWebhook(e, in.ID); err != nil {
				return DeliveriesOut{}, err
			}
			limit := in.Limit
			if limit <= 0 || limit > 500 {
				limit = 50
			}
			rows, err := e.DB.QueryContext(e.Ctx, deliverySelect+` WHERE webhook_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, in.ID, limit)
			if err != nil {
				return DeliveriesOut{}, err
			}
			defer rows.Close()
			out := DeliveriesOut{Items: []DeliveryOut{}}
			for rows.Next() {
				d, _ := scanDelivery(rows, false)
				out.Items = append(out.Items, d)
			}
			return out, nil
		})
	ops.Register(ops.Op{Name: "webhook.retry", Summary: "Redeliver a logged webhook delivery (async, with backoff)", Method: "POST", Path: "/webhook-deliveries/{id}/retry", Auth: ops.AuthAdmin, UI: "POST /ui/admin/webhook-deliveries/{id}/retry"},
		func(e *ops.Env, in IDIn) (DeliveryOut, error) {
			d, err := loadDelivery(e.Ctx, e.DB, in.ID, false)
			if err != nil {
				return d, err
			}
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE webhook_deliveries SET attempts=0,error=NULL,delivered_at=NULL,next_attempt_at=? WHERE id=?`, db.Now(), in.ID); err != nil {
				return d, err
			}
			d2, dbh := in.ID, e.DB
			bg(func() { runDelivery(dbh, d2, true) })
			return loadDelivery(e.Ctx, e.DB, in.ID, false)
		})
}

// ---- dispatch -------------------------------------------------------------

func buildPayload(event string, data map[string]any, user *ops.Principal) string {
	clean := map[string]any{}
	var prev any
	for k, v := range data {
		switch k {
		case "actionId":
		case "prevData":
			prev = v
		default:
			clean[k] = v
		}
	}
	p := map[string]any{"event": event, "data": clean, "prevData": prev}
	if user != nil {
		p["user"] = map[string]any{"id": user.UserID, "name": user.Name, "role": user.Role}
	} else {
		p["user"] = nil
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func resolveScope(e *ops.Env, ev ops.Event) (project, board string) {
	board = ev.BoardID
	if board == "" {
		board = str(ev.Data, "boardId")
	}
	project = str(ev.Data, "projectId")
	item, _ := ev.Data["item"].(map[string]any)
	if strings.HasPrefix(ev.Type, "project") && project == "" {
		project = str(item, "id")
	}
	if strings.HasPrefix(ev.Type, "board") && !strings.HasPrefix(ev.Type, "boardMembership") && board == "" {
		board = str(item, "id")
	}
	if board == "" {
		board = str(item, "boardId")
	}
	if project == "" {
		project = str(item, "projectId")
	}
	if project == "" && board != "" {
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT project_id FROM boards WHERE id=?`, board).Scan(&project)
	}
	return
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// dispatchWebhooks queues a delivery for each webhook whose filters match the
// event (excluded events, included events, acting user, project/board scope).
func dispatchWebhooks(e *ops.Env, ev ops.Event, actorID string) {
	if !eventSet[ev.Type] {
		return
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT `+whCols+` FROM webhooks`)
	if err != nil {
		return
	}
	var all []WebhookOut
	for rows.Next() {
		w, _ := scanWebhook(rows)
		all = append(all, w)
	}
	rows.Close()
	if len(all) == 0 {
		return
	}
	if e.User != nil {
		actorID = e.User.UserID
	}
	project, board := resolveScope(e, ev)
	var payload string
	for _, w := range all {
		switch {
		case contains(w.ExcludedEvents, ev.Type),
			len(w.Events) > 0 && !contains(w.Events, ev.Type),
			w.UserID != "" && w.UserID != actorID,
			w.ProjectID != "" && w.ProjectID != project,
			w.BoardID != "" && w.BoardID != board:
			continue
		}
		if payload == "" {
			payload = buildPayload(ev.Type, ev.Data, e.User)
		}
		id, err := insertDelivery(e.Ctx, e.DB, w.ID, ev.Type, payload)
		if err != nil {
			log.Println("notify: queue webhook:", err)
			continue
		}
		d := e.DB
		bg(func() { runDelivery(d, id, true) })
	}
}

func insertDelivery(ctx context.Context, d *db.DB, webhookID, event, payload string) (string, error) {
	id := db.NewID()
	_, err := d.ExecContext(ctx, `INSERT INTO webhook_deliveries(id,webhook_id,event,attempts,payload,next_attempt_at,created_at) VALUES(?,?,?,0,?,?,?)`,
		id, webhookID, event, payload, db.Now(), db.Now())
	return id, err
}

// Sign returns the X-Weave-Signature header value for a body.
func Sign(key, body string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// SignV2 returns the X-Weave-Signature-V2 header value: "t=<unix ts>,v1=<hex HMAC-SHA256(key, ts + "." + body)>".
// Unlike the legacy header it binds the timestamp into the MAC, so receivers can reject replays
// (see docs/WEBHOOKS.md).
func SignV2(key string, ts int64, body string) string {
	t := strconv.FormatInt(ts, 10)
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(t + "." + body))
	return "t=" + t + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

// runDelivery attempts a delivery; with retry it follows RetryBackoff on failure,
// otherwise it makes a single attempt.
func runDelivery(d *db.DB, id string, retry bool) {
	ctx := context.Background()
	for {
		var whID, event, payload string
		var attempts int
		if d.QueryRowContext(ctx, `SELECT webhook_id,event,COALESCE(payload,''),attempts FROM webhook_deliveries WHERE id=?`, id).Scan(&whID, &event, &payload, &attempts) != nil {
			return
		}
		var url, token, secret string
		if d.QueryRowContext(ctx, `SELECT url,COALESCE(access_token,''),COALESCE(secret,'') FROM webhooks WHERE id=?`, whID).Scan(&url, &token, &secret) != nil {
			return
		}
		attempts++
		status, body, derr, dur := post(ctx, url, token, secret, event, id, payload)
		ok := derr == "" && status >= 200 && status < 300
		if !ok && derr == "" {
			derr = fmt.Sprintf("HTTP %d", status)
		}
		next := any(nil)
		var wait time.Duration
		if !ok && retry && attempts-1 < len(RetryBackoff) {
			wait = RetryBackoff[attempts-1]
			next = time.Now().UTC().Add(wait).Format(time.RFC3339)
		}
		var deliveredAt, errv any
		if ok {
			deliveredAt = db.Now()
		} else {
			errv = derr
		}
		_, err := d.ExecContext(ctx, `UPDATE webhook_deliveries SET attempts=?,status=?,error=?,response_body=?,duration_ms=?,delivered_at=?,next_attempt_at=? WHERE id=?`,
			attempts, status, errv, body, dur, deliveredAt, next, id)
		if err != nil {
			log.Println("notify: delivery update:", err)
			return
		}
		if ok || next == nil {
			return
		}
		time.Sleep(wait)
	}
}

func post(ctx context.Context, url, token, secret, event, deliveryID, payload string) (status int, body, errMsg string, ms int) {
	start := time.Now()
	defer func() { ms = int(time.Since(start) / time.Millisecond) }()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader([]byte(payload)))
	if err != nil {
		return 0, "", err.Error(), 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "weave-webhook/1.0 (+"+baseURL()+")")
	req.Header.Set("X-Weave-Event", event)
	req.Header.Set("X-Weave-Delivery", deliveryID)
	now := time.Now().Unix()
	req.Header.Set("X-Weave-Timestamp", strconv.FormatInt(now, 10))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	key := secret
	if key == "" {
		key = token
	}
	if key != "" {
		req.Header.Set("X-Weave-Signature", Sign(key, payload))
		req.Header.Set("X-Weave-Signature-V2", SignV2(key, now, payload))
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return 0, "", err.Error(), 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return resp.StatusCode, string(b), "", 0
}

const deliverySelect = `SELECT id,webhook_id,event,COALESCE(status,0),COALESCE(error,''),attempts,COALESCE(next_attempt_at,''),COALESCE(delivered_at,''),COALESCE(duration_ms,0),COALESCE(response_body,''),COALESCE(payload,''),created_at FROM webhook_deliveries`

func scanDelivery(r interface{ Scan(...any) error }, withPayload bool) (DeliveryOut, error) {
	var d DeliveryOut
	err := r.Scan(&d.ID, &d.WebhookID, &d.Event, &d.Status, &d.Error, &d.Attempts, &d.NextAttemptAt, &d.DeliveredAt, &d.DurationMs, &d.ResponseBody, &d.Payload, &d.CreatedAt)
	switch {
	case d.DeliveredAt != "":
		d.State = "delivered"
	case d.NextAttemptAt != "":
		d.State = "pending"
	default:
		d.State = "failed"
	}
	if !withPayload {
		d.Payload = ""
	}
	return d, err
}

func loadDelivery(ctx context.Context, d *db.DB, id string, withPayload bool) (DeliveryOut, error) {
	x, err := scanDelivery(d.QueryRowContext(ctx, deliverySelect+` WHERE id=?`, id), withPayload)
	if err == sql.ErrNoRows {
		return x, ops.NotFound("delivery")
	}
	return x, err
}

func init() {
	server.Hooks = append(server.Hooks, func(s *server.Server) error {
		ctx := context.Background()
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE created_at < ?`, time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339))
		rows, err := s.DB.QueryContext(ctx, `SELECT id FROM webhook_deliveries WHERE delivered_at IS NULL AND next_attempt_at IS NOT NULL`)
		if err != nil {
			return nil
		}
		var ids []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			id := id
			bg(func() { runDelivery(s.DB, id, true) })
		}
		return nil
	})
}
