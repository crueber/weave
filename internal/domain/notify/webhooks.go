package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/events"
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
	"cardPlacementCreate", "cardPlacementDelete",
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

// BoardWebhookEvents are the events a board-scoped webhook may subscribe to and receive: things that happen on the
// board itself. Notifications (they belong to one user), users, projects, configuration, notification services and
// webhooks are never sent to a board webhook, so a board's managers only ever see their own board's activity.
var BoardWebhookEvents = []string{
	"attachmentCreate", "attachmentUpdate", "attachmentDelete",
	"boardUpdate", "boardDelete",
	"boardMembershipCreate", "boardMembershipUpdate", "boardMembershipDelete",
	"cardCreate", "cardUpdate", "cardDelete", "cardMove", "cardClaim", "cardRelease",
	"cardPlacementCreate", "cardPlacementDelete",
	"cardLabelCreate", "cardLabelDelete",
	"cardMembershipCreate", "cardMembershipDelete",
	"commentCreate", "commentUpdate", "commentDelete",
	"customFieldCreate", "customFieldUpdate", "customFieldDelete",
	"customFieldGroupCreate", "customFieldGroupUpdate", "customFieldGroupDelete",
	"customFieldValueUpdate", "customFieldValueDelete",
	"labelCreate", "labelUpdate", "labelDelete",
	"listCreate", "listUpdate", "listClear", "listDelete",
	"taskCreate", "taskUpdate", "taskDelete",
	"taskListCreate", "taskListUpdate", "taskListDelete",
}

func setOf(l []string) map[string]bool {
	m := map[string]bool{}
	for _, e := range l {
		m[e] = true
	}
	return m
}

var (
	eventSet      = setOf(WebhookEvents)
	boardEventSet = setOf(BoardWebhookEvents)
)

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
	Scope          string   `json:"scope" desc:"global | project | board"`
	CreatedBy      string   `json:"createdBy,omitempty"`
	UserCreated    bool     `json:"userCreated" desc:"created or last changed by a non-admin: delivered under the strict destination policy"`
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
	ResponseBody  string `json:"responseBody,omitempty" desc:"start of the receiver's response (admins up to 2 KiB, board managers up to 512 bytes)"`
	Payload       string `json:"payload,omitempty"`
	CreatedAt     string `json:"createdAt"`
}

const whCols = `id,name,url,COALESCE(access_token,''),COALESCE(secret,''),COALESCE(event_types,''),COALESCE(excluded_event_types,''),COALESCE(project_id,''),COALESCE(board_id,''),COALESCE(user_id,''),COALESCE(created_by,''),strict_dest,created_at,COALESCE(updated_at,'')`

func scanWebhook(r interface{ Scan(...any) error }) (WebhookOut, error) {
	var w WebhookOut
	var ev, ex string
	err := r.Scan(&w.ID, &w.Name, &w.URL, &w.AccessToken, &w.Secret, &ev, &ex, &w.ProjectID, &w.BoardID, &w.UserID, &w.CreatedBy, &w.UserCreated, &w.CreatedAt, &w.UpdatedAt)
	w.Events, w.ExcludedEvents = []string{}, []string{}
	if ev != "" {
		_ = json.Unmarshal([]byte(ev), &w.Events)
	}
	if ex != "" {
		_ = json.Unmarshal([]byte(ex), &w.ExcludedEvents)
	}
	w.Scope = scopeOf(w)
	return w, err
}

func scopeOf(w WebhookOut) string {
	switch {
	case w.BoardID != "":
		return "board"
	case w.ProjectID != "":
		return "project"
	}
	return "global"
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
		"hasSecret": r.HasSecret, "scope": r.Scope, "createdAt": r.CreatedAt, "updatedAt": r.UpdatedAt}
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
	Name           string      `json:"name" required:"true" desc:"label (max 255 characters)"`
	URL            string      `json:"url" required:"true" desc:"http(s) endpoint receiving POSTs (max 2048 characters). Board webhooks created by non-admins may not point at loopback, link-local, cloud metadata or (unless an admin allows it) private-network addresses"`
	AccessToken    string      `json:"accessToken" desc:"sent as 'Authorization: Bearer <token>'; also the HMAC key when no secret is set (write-only, max 1024 characters)"`
	Secret         string      `json:"secret" desc:"HMAC-SHA256 signing key (headers X-Weave-Signature: sha256=<hex> and X-Weave-Signature-V2: t=<ts>,v1=<hex>; see docs/WEBHOOKS.md); write-only, max 1024 characters"`
	Events         *StringList `json:"events" desc:"only these event names (empty = all); see webhook.events (board webhooks: webhook.events scope=board)"`
	ExcludedEvents *StringList `json:"excludedEvents" desc:"never send these event names"`
	ProjectID      string      `json:"projectId" desc:"admins only: scope to events inside this project"`
	BoardID        string      `json:"boardId" desc:"scope to events inside this board; required unless you are an admin (needs Manage on the board: board owner or project manager)"`
	UserID         string      `json:"userId" desc:"admins only: only events caused by this user"`
}
type WebhookUpdateIn struct {
	ID             string      `path:"id"`
	Name           *string     `json:"name"`
	URL            *string     `json:"url"`
	AccessToken    *string     `json:"accessToken" desc:"write-only; empty clears it"`
	Secret         *string     `json:"secret" desc:"write-only; empty clears it"`
	Events         *StringList `json:"events"`
	ExcludedEvents *StringList `json:"excludedEvents"`
	ProjectID      *string     `json:"projectId" desc:"admins only"`
	BoardID        *string     `json:"boardId" desc:"move to another board you manage (non-admins cannot remove the board scope)"`
	UserID         *string     `json:"userId" desc:"admins only"`
}
type WebhookListIn struct {
	BoardID string `query:"boardId" desc:"only the webhooks of this board (needs Manage on it)"`
}
type WebhookListOut struct {
	Items []WebhookOut `json:"items"`
}
type EventsIn struct {
	Scope string `query:"scope" desc:"board: only the events a board-scoped webhook can subscribe to; empty: every event name"`
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

func validateEvents(l []string, board bool) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range l {
		if e == "" || seen[e] {
			continue
		}
		if !eventSet[e] {
			return nil, ops.Invalid("unknown event " + strconv.Quote(e))
		}
		if board && !boardEventSet[e] {
			return nil, ops.Invalid("event " + strconv.Quote(e) + " is not available to board webhooks (see webhook.events scope=board)")
		}
		seen[e] = true
		out = append(out, e)
	}
	return out, nil
}

const (
	maxWebhookName   = 255
	maxWebhookURL    = 2048
	maxWebhookSecret = 1024
	userBodyCap      = 512 // bytes of a response body shown to non-admins
	testSnippetCap   = 256 // bytes of the ping response returned to non-admins by webhook.test
)

// defaultWebhooksPerBoard caps the webhooks of one board (WEAVE_WEBHOOKS_PER_BOARD; 0 = no cap).
const defaultWebhooksPerBoard = 20

func webhooksPerBoard() int {
	n := defaultWebhooksPerBoard
	if v := strings.TrimSpace(os.Getenv("WEAVE_WEBHOOKS_PER_BOARD")); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x >= 0 {
			n = x
		} else {
			log.Println("WEAVE_WEBHOOKS_PER_BOARD: invalid value", v, "- using", n)
		}
	}
	return n
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateWebhook(e *ops.Env, w *WebhookOut) error {
	w.Name = strings.TrimSpace(w.Name)
	if w.Name == "" {
		return ops.Invalid("name required")
	}
	if len([]rune(w.Name)) > maxWebhookName {
		return ops.Invalid("name is too long (max 255 characters)")
	}
	if len(w.URL) > maxWebhookURL || hasControl(w.URL) || strings.ContainsAny(w.URL, " \t") {
		return ops.Invalid("url must be an http(s) URL of at most 2048 characters")
	}
	u, err := url.Parse(w.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ops.Invalid("url must be an http(s) URL")
	}
	for _, s := range []string{w.AccessToken, w.Secret} {
		if len(s) > maxWebhookSecret || hasControl(s) {
			return ops.Invalid("accessToken and secret must be at most 1024 printable characters")
		}
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
	if w.BoardID != "" {
		for _, l := range [][]string{w.Events, w.ExcludedEvents} {
			if _, err := validateEvents(l, true); err != nil {
				return err
			}
		}
	}
	if w.UserCreated {
		return checkStrictURL(e.Ctx, e.DB, w.URL)
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

// manageable loads a webhook the caller may see and manage (authz.RequireWebhook); anything else is not_found.
func manageable(e *ops.Env, id string) (WebhookOut, error) {
	w, err := getWebhook(e, id)
	if err != nil {
		return w, err
	}
	if err := authz.RequireWebhook(e, w.ProjectID, w.BoardID); err != nil {
		return WebhookOut{}, err
	}
	return w, nil
}

func saveWebhook(e *ops.Env, w WebhookOut, insert bool) error {
	if insert {
		_, err := e.DB.ExecContext(e.Ctx, `INSERT INTO webhooks(id,name,url,access_token,secret,event_types,excluded_event_types,project_id,board_id,user_id,created_by,strict_dest,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.ID, w.Name, w.URL, nullIf(w.AccessToken), nullIf(w.Secret), jsonList(w.Events), jsonList(w.ExcludedEvents), nullIf(w.ProjectID), nullIf(w.BoardID), nullIf(w.UserID), nullIf(w.CreatedBy), w.UserCreated, w.CreatedAt)
		return err
	}
	_, err := e.DB.ExecContext(e.Ctx, `UPDATE webhooks SET name=?,url=?,access_token=?,secret=?,event_types=?,excluded_event_types=?,project_id=?,board_id=?,user_id=?,strict_dest=?,updated_at=? WHERE id=?`,
		w.Name, w.URL, nullIf(w.AccessToken), nullIf(w.Secret), jsonList(w.Events), jsonList(w.ExcludedEvents), nullIf(w.ProjectID), nullIf(w.BoardID), nullIf(w.UserID), w.UserCreated, w.UpdatedAt, w.ID)
	return err
}

// recordWebhook emits a webhook settings change. Global and project webhooks go to the global room as before; a
// board webhook's change goes to that board's room only (realtime, no activity row). The item never carries the URL,
// access token or secret.
func recordWebhook(e *ops.Env, typ, name string, w WebhookOut) {
	room := "global"
	if w.BoardID != "" {
		room = "board:" + w.BoardID
	}
	Record(e, ops.Event{Type: typ, Name: name, Room: room, Data: map[string]any{"item": w.eventItem()}})
}

// boardWebhookCount counts the webhooks of a board (for WEAVE_WEBHOOKS_PER_BOARD).
func boardWebhookCount(e *ops.Env, boardID string) int {
	var n int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM webhooks WHERE board_id=?`, boardID).Scan(&n)
	return n
}

func checkBoardCap(e *ops.Env, boardID string) error {
	if limit := webhooksPerBoard(); boardID != "" && limit > 0 && boardWebhookCount(e, boardID) >= limit {
		return ops.Invalid(fmt.Sprintf("this board already has %d webhooks, the most allowed (WEAVE_WEBHOOKS_PER_BOARD); delete one first", limit))
	}
	return nil
}

func capBody(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}

// forViewer trims a delivery for the caller: non-admins see a shorter response body.
func forViewer(e *ops.Env, d DeliveryOut, n int) DeliveryOut {
	if !authz.SystemAdmin(e) {
		d.ResponseBody = capBody(d.ResponseBody, n)
	}
	return d
}

func init() {
	ops.Register(ops.Op{Name: "webhook.events", Summary: "List the event names webhooks can subscribe to (scope=board: those a board webhook may use)", Method: "GET", Path: "/webhooks/events", UI: "GET /ui/boards/{boardId}/webhooks"},
		func(e *ops.Env, in EventsIn) (EventsOut, error) {
			if e.User == nil || e.User.IsAgent {
				return EventsOut{}, ops.Forbidden()
			}
			if in.Scope == "board" || !authz.SystemAdmin(e) {
				return EventsOut{append([]string(nil), BoardWebhookEvents...)}, nil
			}
			return EventsOut{append([]string(nil), WebhookEvents...)}, nil
		})
	ops.Register(ops.Op{Name: "webhook.list", Summary: "List webhooks: the board webhooks of boards you manage (admins also: global and project webhooks)", Method: "GET", Path: "/webhooks", UI: "GET /ui/boards/{boardId}/webhooks"},
		func(e *ops.Env, in WebhookListIn) (WebhookListOut, error) {
			if e.User == nil || e.User.IsAgent {
				return WebhookListOut{}, ops.Forbidden()
			}
			admin := authz.SystemAdmin(e)
			q, args := `SELECT `+whCols+` FROM webhooks`, []any{}
			switch {
			case in.BoardID != "":
				if err := authz.RequireBoard(e, in.BoardID, authz.Manage); err != nil {
					return WebhookListOut{}, err
				}
				q, args = q+` WHERE board_id=?`, append(args, in.BoardID)
			case !admin:
				q += ` WHERE board_id IS NOT NULL`
			}
			rows, err := e.DB.QueryContext(e.Ctx, q+` ORDER BY created_at, id`, args...)
			if err != nil {
				return WebhookListOut{}, err
			}
			var all []WebhookOut
			for rows.Next() {
				w, _ := scanWebhook(rows)
				all = append(all, w)
			}
			rows.Close()
			out := WebhookListOut{Items: []WebhookOut{}}
			// board webhooks: only those of boards the caller manages (administrators included); global and project
			// webhooks (admins only, by the query above) are instance integrations
			ids := []string{}
			for _, w := range all {
				if w.BoardID != "" {
					ids = append(ids, w.BoardID)
				}
			}
			lv := authz.BoardLevels(e, ids)
			for _, w := range all {
				if w.BoardID == "" || lv[w.BoardID] >= authz.Manage {
					out.Items = append(out.Items, w.redacted())
				}
			}
			return out, nil
		})
	ops.Register(ops.Op{Name: "webhook.create", Summary: "Create a webhook (board scope: board owners and project managers; global and project scope: admins)", Method: "POST", Path: "/webhooks", UI: "POST /ui/boards/{boardId}/webhooks"},
		func(e *ops.Env, in WebhookCreateIn) (WebhookOut, error) {
			in.ProjectID, in.BoardID, in.UserID = strings.TrimSpace(in.ProjectID), strings.TrimSpace(in.BoardID), strings.TrimSpace(in.UserID)
			if err := authz.RequireWebhookScope(e, in.ProjectID, in.BoardID); err != nil {
				return WebhookOut{}, err
			}
			admin := authz.SystemAdmin(e)
			if !admin && in.UserID != "" {
				return WebhookOut{}, ops.Invalid("userId is an admin-only filter")
			}
			w := WebhookOut{ID: db.NewID(), Name: in.Name, URL: strings.TrimSpace(in.URL), AccessToken: in.AccessToken, Secret: in.Secret, ProjectID: in.ProjectID, BoardID: in.BoardID, UserID: in.UserID,
				CreatedBy: e.User.UserID, UserCreated: !admin, CreatedAt: db.Now()}
			var err error
			if in.Events != nil {
				if w.Events, err = validateEvents(*in.Events, false); err != nil {
					return w, err
				}
			}
			if in.ExcludedEvents != nil {
				if w.ExcludedEvents, err = validateEvents(*in.ExcludedEvents, false); err != nil {
					return w, err
				}
			}
			if w.Events == nil {
				w.Events = []string{}
			}
			if w.ExcludedEvents == nil {
				w.ExcludedEvents = []string{}
			}
			if err := checkBoardCap(e, w.BoardID); err != nil {
				return WebhookOut{}, err
			}
			if err := validateWebhook(e, &w); err != nil {
				return WebhookOut{}, err
			}
			w.Scope = scopeOf(w)
			if err := saveWebhook(e, w, true); err != nil {
				return WebhookOut{}, err
			}
			recordWebhook(e, "webhookCreate", "webhook.created", w)
			return w.redacted(), nil
		})
	ops.Register(ops.Op{Name: "webhook.update", Summary: "Update a webhook", Method: "PATCH", Path: "/webhooks/{id}", UI: "POST /ui/webhooks/{id}"},
		func(e *ops.Env, in WebhookUpdateIn) (WebhookOut, error) {
			w, err := manageable(e, in.ID)
			if err != nil {
				return w, err
			}
			admin := authz.SystemAdmin(e)
			set := func(dst *string, v *string) {
				if v != nil {
					*dst = strings.TrimSpace(*v)
				}
			}
			oldBoard := w.BoardID
			set(&w.Name, in.Name)
			set(&w.URL, in.URL)
			set(&w.AccessToken, in.AccessToken)
			set(&w.Secret, in.Secret)
			set(&w.ProjectID, in.ProjectID)
			set(&w.BoardID, in.BoardID)
			set(&w.UserID, in.UserID)
			if !admin && (in.ProjectID != nil && w.ProjectID != "" || in.UserID != nil && w.UserID != "") {
				return WebhookOut{}, ops.Invalid("projectId and userId are admin-only")
			}
			if err := authz.RequireWebhookScope(e, w.ProjectID, w.BoardID); err != nil {
				return WebhookOut{}, err // the new scope must be one the caller manages too
			}
			if w.BoardID != oldBoard {
				if err := checkBoardCap(e, w.BoardID); err != nil {
					return WebhookOut{}, err
				}
			}
			if !admin {
				w.UserCreated = true // a non-admin's change is always delivered under the strict policy
			}
			if in.Events != nil {
				if w.Events, err = validateEvents(*in.Events, false); err != nil {
					return WebhookOut{}, err
				}
			}
			if in.ExcludedEvents != nil {
				if w.ExcludedEvents, err = validateEvents(*in.ExcludedEvents, false); err != nil {
					return WebhookOut{}, err
				}
			}
			if err := validateWebhook(e, &w); err != nil {
				return WebhookOut{}, err
			}
			w.Scope = scopeOf(w)
			w.UpdatedAt = db.Now()
			if err := saveWebhook(e, w, false); err != nil {
				return WebhookOut{}, err
			}
			recordWebhook(e, "webhookUpdate", "webhook.updated", w)
			return w.redacted(), nil
		})
	ops.Register(ops.Op{Name: "webhook.delete", Summary: "Delete a webhook (and its delivery log)", Method: "DELETE", Path: "/webhooks/{id}", UI: "POST /ui/webhooks/{id}/delete"},
		func(e *ops.Env, in IDIn) (WebhookOut, error) {
			w, err := manageable(e, in.ID)
			if err != nil {
				return w, err
			}
			if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM webhooks WHERE id=?`, w.ID); err != nil {
				return WebhookOut{}, err
			}
			recordWebhook(e, "webhookDelete", "webhook.deleted", w)
			return w.redacted(), nil
		})
	ops.Register(ops.Op{Name: "webhook.test", Summary: "Send a synchronous 'ping' event to a webhook and report the result (non-admins get the status and a short response snippet)", Method: "POST", Path: "/webhooks/{id}/test", UI: "POST /ui/webhooks/{id}/test"},
		func(e *ops.Env, in IDIn) (DeliveryOut, error) {
			w, err := manageable(e, in.ID)
			if err != nil {
				return DeliveryOut{}, err
			}
			payload := buildPayload("ping", map[string]any{"item": map[string]any{"webhookId": w.ID, "message": "Hello from Weave"}}, e.User)
			id, err := insertDelivery(e.Ctx, e.DB, w.ID, "ping", payload)
			if err != nil {
				return DeliveryOut{}, err
			}
			runDelivery(e.DB, id, false)
			d, err := loadDelivery(e.Ctx, e.DB, id, true)
			return forViewer(e, d, testSnippetCap), err
		})
	ops.Register(ops.Op{Name: "webhook.deliveries", Summary: "Delivery log of a webhook (newest first)", Method: "GET", Path: "/webhooks/{id}/deliveries", UI: "GET /ui/webhooks/{id}/deliveries"},
		func(e *ops.Env, in DeliveriesIn) (DeliveriesOut, error) {
			if _, err := manageable(e, in.ID); err != nil {
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
				out.Items = append(out.Items, forViewer(e, d, userBodyCap))
			}
			return out, nil
		})
	ops.Register(ops.Op{Name: "webhook.retry", Summary: "Redeliver a logged webhook delivery (async, with backoff)", Method: "POST", Path: "/webhook-deliveries/{id}/retry", UI: "POST /ui/webhook-deliveries/{id}/retry"},
		func(e *ops.Env, in IDIn) (DeliveryOut, error) {
			d, err := loadDelivery(e.Ctx, e.DB, in.ID, false)
			if err != nil {
				return d, err
			}
			if _, err := manageable(e, d.WebhookID); err != nil {
				return DeliveryOut{}, ops.NotFound("delivery")
			}
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE webhook_deliveries SET attempts=0,error=NULL,delivered_at=NULL,next_attempt_at=? WHERE id=?`, db.Now(), in.ID); err != nil {
				return d, err
			}
			d2, dbh := in.ID, e.DB
			bg(func() { runDelivery(dbh, d2, true) })
			d, err = loadDelivery(e.Ctx, e.DB, in.ID, false)
			return forViewer(e, d, userBodyCap), err
		})
}

// ---- dispatch -------------------------------------------------------------

func buildPayload(event string, data map[string]any, user *ops.Principal) string {
	clean := map[string]any{}
	var prev any
	for k, v := range data {
		switch k {
		case "actionId", "originId", "actionAt", events.CardLevel, events.PlacementLists:
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

// webhookType maps an event type to its webhook event name where they differ (the placement events of phase 4).
func webhookType(t string) string {
	switch t {
	case "cardPlace":
		return "cardPlacementCreate"
	case "cardUnplace":
		return "cardPlacementDelete"
	}
	return t
}

// dispatchWebhooks queues a delivery for each webhook whose filters match the event (excluded events, included
// events, acting user, project/board scope), ONCE per webhook per logical event. evs are the event's per-board
// copies (perBoard; nil = just ev): a board webhook matches when its board is among them and receives that board's
// copy (that board's card view, never another board's columns or labels); a project webhook matches when one of
// those boards is in its project and receives the copy of the home board when it is in the project, else of the
// first such board; an unscoped webhook receives the home board's copy (else the origin's).
func dispatchWebhooks(e *ops.Env, ev ops.Event, actorID string, evs []ops.Event, home string) {
	typ := webhookType(ev.Type)
	if !eventSet[typ] {
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
	if len(evs) == 0 {
		evs = []ops.Event{ev}
	}
	boards := make([]string, len(evs))
	projectOf := map[string]string{}
	if len(evs) == 1 {
		project, board := resolveScope(e, evs[0])
		boards[0], projectOf[board] = board, project
	} else {
		args := make([]any, len(evs))
		for i, x := range evs {
			boards[i], args[i] = x.BoardID, x.BoardID
		}
		if r, err := e.DB.QueryContext(e.Ctx, `SELECT id,project_id FROM boards WHERE id IN (?`+strings.Repeat(",?", len(args)-1)+`)`, args...); err == nil {
			for r.Next() {
				var b, p string
				if r.Scan(&b, &p) == nil {
					projectOf[b] = p
				}
			}
			r.Close()
		}
	}
	// pick: the home board's copy when it matches, else the first matching copy (-1: none)
	pick := func(match func(b string) bool) int {
		first := -1
		for i, b := range boards {
			if !match(b) {
				continue
			}
			if b == home && home != "" {
				return i
			}
			if first < 0 {
				first = i
			}
		}
		return first
	}
	payloads := map[int]string{}
	for _, w := range all {
		if contains(w.ExcludedEvents, typ) || (len(w.Events) > 0 && !contains(w.Events, typ)) || (w.UserID != "" && w.UserID != actorID) {
			continue
		}
		var i int
		switch {
		case w.BoardID != "":
			if !boardEventSet[typ] {
				continue
			}
			i = pick(func(b string) bool { return b == w.BoardID })
		case w.ProjectID != "":
			i = pick(func(b string) bool { return projectOf[b] == w.ProjectID })
		default:
			i = pick(func(string) bool { return true })
		}
		if i < 0 {
			continue
		}
		payload, ok := payloads[i]
		if !ok {
			payload = buildPayload(typ, evs[i].Data, e.User)
			payloads[i] = payload
		}
		id, err := insertDelivery(e.Ctx, e.DB, w.ID, typ, payload)
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
		var strict bool
		if d.QueryRowContext(ctx, `SELECT url,COALESCE(access_token,''),COALESCE(secret,''),strict_dest FROM webhooks WHERE id=?`, whID).Scan(&url, &token, &secret, &strict) != nil {
			return
		}
		attempts++
		client := HTTPClient
		if strict {
			client = strictClient(strictPolicy(userAllowPrivate(ctx, d)))
		}
		status, body, derr, dur := post(ctx, client, url, token, secret, event, id, payload)
		if strict && derr != "" {
			// connection details (resolved addresses, refusals, timeouts) stay in the server log
			log.Printf("notify: webhook %s delivery %s: %s", whID, id, derr)
			derr = GenericDeliveryError
		}
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

func post(ctx context.Context, client *http.Client, url, token, secret, event, deliveryID, payload string) (status int, body, errMsg string, ms int) {
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
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", transportErr(err), 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return resp.StatusCode, string(b), "", 0
}

// transportErr is a failed request's error without the request URL (which may carry a token).
func transportErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
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
