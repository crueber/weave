package server_test

// Authorization regression sweep. It walks ops.All() and every registered web route, seeds one world
// owned by user A, and calls everything as anonymous callers, outsiders and read-only members.
// Nothing may succeed or leak the CANARY marker unless the op is on an explicit allowlist below
// (public views, self-service, directory reads). Add an op to an allowlist only with a reason.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

const canary = "CANARY"

type world struct {
	e    *env
	ids  map[string]string
	keys map[string]string // caller -> bearer key ("" = anonymous)
	uid  map[string]string
}

var sweepClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 15 * time.Second}

func (e *env) raw(key, method, path string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := sweepClient.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// mk seeds data as the given key and returns the created item ({} when seeding failed).
func (e *env) mk(key, method, path string, body any) map[string]any {
	code, b := e.raw(key, method, "/api/v1"+path, body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	if code != 200 {
		e.t.Errorf("seed %s %s -> %d %s", method, path, code, b)
		return map[string]any{}
	}
	it, _ := out["item"].(map[string]any)
	if it == nil {
		return map[string]any{}
	}
	return it
}

func (e *env) upload(key, path, fname string, data []byte) map[string]any {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, fname))
	h.Set("Content-Type", "application/octet-stream")
	w, _ := mw.CreatePart(h)
	_, _ = w.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1"+path, &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := sweepClient.Do(req)
	if err != nil {
		e.t.Errorf("seed upload %s: %v", path, err)
		return map[string]any{}
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	it, _ := out["item"].(map[string]any)
	if it == nil {
		e.t.Errorf("seed upload %s -> %d %v", path, res.StatusCode, out)
		return map[string]any{}
	}
	return it
}

func (e *env) userID(key string) string {
	_, o := e.api(key, "GET", "/auth/me", nil)
	it, _ := o["item"].(map[string]any)
	return id(it)
}

func pngBytes() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	return b.Bytes()
}

// newWorld seeds a board with data owned by user A, one member per read-only role, and a public board.
func newWorld(t *testing.T) *world {
	e := newEnv(t)
	w := &world{e: e, ids: map[string]string{}, keys: map[string]string{"anon": ""}, uid: map[string]string{}}
	A := e.newUser("a-owner@x.io", "projectOwner")
	w.keys["A"], w.keys["admin"] = A, e.admin
	w.keys["B"] = e.newUser("b-outsider@x.io", "boardUser")
	w.keys["V"] = e.newUser("v-viewer@x.io", "boardUser")
	w.keys["CM"] = e.newUser("cm-commenter@x.io", "boardUser")
	w.keys["W"] = e.newUser("w-worker@x.io", "worker")
	w.keys["G"] = e.newUser("g-guest@x.io", "guest")
	w.keys["WV"] = e.newUser("wv-worker-viewer@x.io", "worker")
	w.keys["GV"] = e.newUser("gv-guest-viewer@x.io", "guest")
	for k, v := range w.keys {
		if v != "" {
			w.uid[k] = e.userID(v)
		}
	}
	I := w.ids
	I["userA"] = w.uid["A"]
	I["project"] = id(e.mk(A, "POST", "/projects", map[string]any{"name": "ProjCANARY"}))
	I["board"] = id(e.mk(A, "POST", "/projects/"+I["project"]+"/boards", map[string]any{"name": "BoardCANARY"}))
	e.mk(A, "PATCH", "/boards/"+I["board"], map[string]any{"hasInbox": true})
	I["list"] = id(e.mk(A, "POST", "/boards/"+I["board"]+"/lists", map[string]any{"name": "L1 CANARY"}))
	I["list2"] = id(e.mk(A, "POST", "/boards/"+I["board"]+"/lists", map[string]any{"name": "L2 CANARY"}))
	I["card"] = id(e.mk(A, "POST", "/lists/"+I["list"]+"/cards", map[string]any{"name": "CardCANARY", "description": "DescCANARY"}))
	I["card2"] = id(e.mk(A, "POST", "/lists/"+I["list2"]+"/cards", map[string]any{"name": "Card2CANARY"}))
	I["comment"] = id(e.mk(A, "POST", "/cards/"+I["card"]+"/comments", map[string]any{"text": "CommentCANARY"}))
	I["tasklist"] = id(e.mk(A, "POST", "/cards/"+I["card"]+"/task-lists", map[string]any{"name": "TL CANARY"}))
	I["task"] = id(e.mk(A, "POST", "/task-lists/"+I["tasklist"]+"/tasks", map[string]any{"name": "TaskCANARY"}))
	I["labelgroup"] = id(e.mk(A, "POST", "/boards/"+I["board"]+"/label-groups", map[string]any{"name": "LG CANARY"}))
	I["label"] = id(e.mk(A, "POST", "/boards/"+I["board"]+"/labels", map[string]any{"name": "LabelCANARY", "color": "berry-red"}))
	I["cfgroup"] = id(e.mk(A, "POST", "/custom-field-groups", map[string]any{"cardId": I["card"], "name": "CFG CANARY", "repeatable": true}))
	I["cfield"] = id(e.mk(A, "POST", "/custom-fields", map[string]any{"groupId": I["cfgroup"], "name": "CF CANARY", "type": "text"}))
	I["cfrow"] = id(e.mk(A, "POST", "/cards/"+I["card"]+"/custom-field-groups/"+I["cfgroup"]+"/rows", nil))
	I["attachment"] = id(e.upload(A, "/cards/"+I["card"]+"/attachments", "secret.txt", []byte("ATTACHMENT-CANARY-BODY")))
	I["attname"] = "secret.txt"
	e.mk(A, "POST", "/cards/"+I["card"]+"/time-entries", map[string]any{"hours": 1, "note": "TimeCANARY"})
	_ = e.d.QueryRow(`SELECT id FROM time_entries LIMIT 1`).Scan(new(string))
	var tid string
	_ = e.d.QueryRow(`SELECT id FROM time_entries LIMIT 1`).Scan(&tid)
	I["time"] = tid
	I["filter"] = id(e.mk(A, "POST", "/boards/"+I["board"]+"/filters", map[string]any{"name": "FilterCANARY", "query": "x"}))
	if s := e.mk(A, "POST", "/cards/"+I["card"]+"/share", nil); s != nil {
		I["sharetoken"], _ = s["token"].(string)
	}
	// a second, public board: anonymous read is intended there
	I["pubboard"] = id(e.mk(A, "POST", "/projects/"+I["project"]+"/boards", map[string]any{"name": "PublicBoard"}))
	e.mk(A, "POST", "/boards/"+I["pubboard"]+"/lists", map[string]any{"name": "PL"})
	I["pubtoken"], _ = e.mk(A, "POST", "/boards/"+I["pubboard"]+"/public", map[string]any{"isPublic": true})["publicToken"].(string)
	for k, role := range map[string]string{"V": "viewer", "WV": "viewer", "GV": "viewer"} {
		m := e.mk(A, "POST", "/boards/"+I["board"]+"/members", map[string]any{"userId": w.uid[k], "role": role})
		if k == "V" {
			I["member"] = id(m)
		}
	}
	e.mk(A, "POST", "/boards/"+I["board"]+"/members", map[string]any{"userId": w.uid["CM"], "role": "viewer", "canComment": true})
	I["draft"] = id(e.mk(A, "POST", "/boards/"+I["board"]+"/drafts", map[string]any{"name": "DraftCANARY"}))
	I["cardtpl"] = id(e.mk(A, "POST", "/cards/"+I["card2"]+"/template", map[string]any{"name": "TplCANARY"}))
	I["tplboard"] = id(e.mk(A, "POST", "/projects/"+I["project"]+"/boards", map[string]any{"name": "TemplateBoardCANARY"}))
	e.mk(A, "POST", "/boards/"+I["tplboard"]+"/template", map[string]any{"isTemplate": true})
	I["ns"] = id(e.mk(A, "POST", "/notification-services", map[string]any{"url": "generic://example.com/hook?token=CANARY", "boardId": I["board"]}))
	I["webhook"] = "wh-fake"
	I["group"] = id(e.mk(e.admin, "POST", "/user-groups", map[string]any{"name": "GroupCANARY"}))
	I["agent"] = id(e.mk(e.admin, "POST", "/agents", map[string]any{"name": "AgentCANARY", "boardIds": []string{I["board"]}}))
	I["basegroup"] = id(e.mk(A, "POST", "/projects/"+I["project"]+"/base-custom-field-groups", map[string]any{"name": "BFG CANARY"}))
	I["basefield"] = id(e.mk(A, "POST", "/base-custom-field-groups/"+I["basegroup"]+"/custom-fields", map[string]any{"name": "BF CANARY", "type": "text"}))
	I["image"] = id(e.upload(A, "/projects/"+I["project"]+"/background-images", "bg.png", pngBytes()))
	h := sha256.Sum256([]byte("sweep-session-token-A"))
	if _, err := e.d.Exec(`INSERT INTO sessions(id,user_id,token_hash,pending_totp,pending_terms,created_at,expires_at) VALUES(?,?,?,0,0,?,?)`,
		"sess-A", w.uid["A"], hex.EncodeToString(h[:]), db.Now(), time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Errorf("session seed: %v", err)
	}
	I["session"] = "sess-A"
	I["notification"] = "notif-fake"
	var nid string
	_ = e.d.QueryRow(`SELECT id FROM notifications LIMIT 1`).Scan(&nid)
	if nid != "" {
		I["notification"] = nid
	}
	return w
}

// val resolves a value for an input field of an op (seeded ids; self-grant attempts use the caller's id).
func (w *world) val(o *ops.Op, f ops.Field, caller string) (string, bool) {
	I := w.ids
	pre, _, _ := strings.Cut(o.Name, ".")
	switch f.Name {
	case "cardId":
		return I["card"], true
	case "boardId":
		return I["board"], true
	case "projectId":
		return I["project"], true
	case "listId":
		if o.Name == "card.create" || o.Name == "card.next" {
			return I["list"], true
		}
		return I["list2"], true
	case "labelId":
		return I["label"], true
	case "templateId":
		return I["tplboard"], true
	case "attachmentId":
		return I["attachment"], true
	case "imageId":
		return I["image"], true
	case "managerId":
		return I["userA"], true
	case "memberId":
		if o.Name == "board.member.remove" || o.Name == "board.member.update" {
			return I["member"], true
		}
		return I["userA"], true
	case "taskListId":
		return I["tasklist"], true
	case "userId":
		switch o.Name {
		case "board.member.add", "project.manager.add", "card.member.add":
			return w.uid[caller], true // self-grant attempt
		}
		return I["userA"], true
	case "groupId":
		switch o.Name {
		case "customfield.create", "customfieldvalue.set", "customfieldvalue.clear":
			return I["cfgroup"], true
		case "label.create", "label.update", "label.project.create":
			return I["labelgroup"], true
		case "board.member.add":
			return "", false
		}
		return I["cfgroup"], true
	case "fieldId":
		return I["cfield"], true
	case "token":
		if o.Name == "card.share.view" {
			return I["sharetoken"], true
		}
		return I["pubtoken"], true
	case "name":
		if f.In == "path" {
			return I["attname"], true
		}
		return "pwn", true
	case "variant":
		return "original", true
	case "id":
		switch {
		case o.Name == "card.fromTemplate":
			return I["cardtpl"], true
		case o.Name == "card.draft.publish":
			return I["draft"], true
		case strings.HasPrefix(o.Name, "card."), o.Name == "export.card":
			return I["card"], true
		case o.Name == "export.board" || strings.HasPrefix(o.Name, "board."):
			return I["board"], true
		}
		m := map[string]string{"list": "list", "comment": "comment", "attachment": "attachment", "task": "task", "tasklist": "tasklist",
			"label": "label", "labelgroup": "labelgroup", "customfield": "cfield", "customfieldgroup": "cfgroup", "customfieldrow": "cfrow",
			"filter": "filter", "time": "time", "notification_service": "ns", "notification": "notification", "session": "session",
			"user": "userA", "apikey": "userA", "agent": "agent", "group": "group", "webhook": "webhook"}
		if k, ok := m[pre]; ok {
			return I[k], true
		}
		switch o.Name {
		case "project.field.update", "project.field.delete":
			return I["basefield"], true
		case "project.fieldgroup.update", "project.fieldgroup.delete", "project.field.create":
			return I["basegroup"], true
		}
		if pre == "project" {
			return I["project"], true
		}
		return I["card"], true
	}
	return "", false
}

var bodyDefaults = map[string]any{
	"text": "pwn", "color": "berry-red", "url": "generic://example.com/x?token=pwn", "format": "json", "state": "online", "field": "name",
	"hours": 1, "to": "x@x.io", "signature": "x", "type": "text", "role": "editor", "isPublic": true, "enabled": true,
	"description": "pwn", "isClosed": true, "closed": true, "trash": false, "completed": true, "query": "x", "month": "2026-01",
	"recurrence": "", "dueDate": "2030-01-01", "isDeactivated": true, "lat": 1.5, "lng": 2.5, "unset": false, "isTemplate": true,
	"data": "{}", "canComment": true,
}

type result struct {
	caller, op, method, path string
	status                   int
	leak                     bool
	body                     string
}

func (w *world) call(o *ops.Op, caller string) result {
	path := "/api/v1" + o.Path
	s := o.Schema()
	body := map[string]any{}
	var q []string
	for _, f := range s.Fields {
		if f.In == "file" {
			continue
		}
		v, ok := w.val(o, f, caller)
		if f.In == "query" && f.Name == "q" {
			v, ok = canary, true
		}
		if !ok {
			switch {
			case f.Required && f.In != "path":
				if d, ok2 := bodyDefaults[f.Name]; ok2 {
					v, ok = fmt.Sprint(d), true
				} else {
					v, ok = "x", true
				}
			case f.In == "body":
				if d, ok2 := bodyDefaults[f.Name]; ok2 {
					body[f.Name] = d
				}
				continue
			default:
				continue
			}
		}
		switch f.In {
		case "path":
			path = strings.ReplaceAll(path, "{"+f.Name+"}", v)
		case "query":
			q = append(q, f.Name+"="+v)
		default:
			if d, ok2 := bodyDefaults[f.Name]; ok2 && (!f.Required || f.Type != "string") {
				body[f.Name] = d
			} else {
				body[f.Name] = v
			}
		}
	}
	switch {
	case strings.HasPrefix(o.Name, "card.") && o.Method != "GET":
		if _, ok := body["type"]; ok {
			body["type"] = "project"
		}
	case strings.HasPrefix(o.Name, "list."):
		body["type"] = "active"
	}
	if strings.HasPrefix(o.Name, "notification_service") {
		body["format"] = "text"
	}
	switch o.Name {
	case "card.create", "card.update":
		body["type"] = "project"
	case "customfieldgroup.create":
		body["boardId"] = w.ids["board"]
	case "board.import":
		body["format"], body["data"] = "json", `{"lists":[{"name":"x","cards":[{"name":"y"}]}]}`
	case "card.fromTemplate":
		body["listId"] = w.ids["list"]
	}
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	var bd any
	if o.Method != "GET" && o.Method != "DELETE" {
		bd = body
	}
	code, b := w.e.raw(w.keys[caller], o.Method, path, bd)
	// search echoes the query back; only its results can leak
	leak := strings.Contains(strings.ReplaceAll(string(b), `"query":"`+canary+`"`, ""), canary)
	return result{caller: caller, op: o.Name, method: o.Method, path: path, status: code, leak: leak, body: string(b)}
}

// ---- allowlists -----------------------------------------------------------------------------

// publicOps are meant to work without authentication (and so for everyone).
var publicOps = map[string]string{
	"auth.oidc.info": "login page", "auth.oidc.authorize": "login flow", "auth.oidc.callback": "login flow", "auth.logout": "ends own session",
	"auth.terms.get": "login page", "auth.terms.accept": "login flow (pending token)", "config.public": "branding", "config.logo.get": "branding",
	"system.health": "health check", "system.version": "version",
	"card.share.view": "anonymous read via the share token", "board.public.get": "anonymous read via the public token",
}

// selfOps may succeed for any authenticated caller: they act on the caller's own data or return
// data every signed-in user may see. They must still never leak another user's seeded data
// (see directoryOps for the documented exceptions).
var selfOps = map[string]string{
	"auth.me": "self", "auth.whoami": "self", "dashboard.get": "self", "dashboard.update": "self", "notification.list": "self",
	"notification.read_all": "self", "notification.unread_count": "self", "presence.list": "directory presence", "presence.set": "self",
	"project.list": "only visible projects", "board.list": "only visible boards", "board.template.list": "templates visible to the caller",
	"search.query": "search is permission-filtered", "session.list": "own sessions", "session.revoke_others": "own sessions",
	"user.get": "directory entry", "user.list": "directory of active users", "group.get": "directory", "group.list": "directory",
	"notification_service.list": "own services", "apikey.get": "own key status", "whoami": "self",
}

// directoryOps are allowed to show the seeded CANARY names because Weave exposes a user and group
// directory to signed-in users. Guests and workers without any shared board (callers "G" and "W")
// get no such exception: for them the directory must stay empty of seeded names.
var directoryOps = map[string]bool{"user.list": true, "group.get": true, "group.list": true, "card.share.view": true, "board.public.get": true}

func (w *world) allowed(r result) bool {
	if _, ok := publicOps[r.op]; ok {
		return true
	}
	if r.caller != "anon" {
		if _, ok := selfOps[r.op]; ok {
			return true
		}
	}
	return false
}

func skipOp(o *ops.Op) bool { return o.Name == "auth.oidc.settings.update" || o.Name == "admin.backup" }

// sweep calls every op as caller (in a stable order) against one shared world and returns violations.
func sweep(t *testing.T, caller string, writesOnly bool, extraOK func(result) bool) []string {
	w := newWorld(t)
	var bad []string
	for _, o := range ops.All() {
		if skipOp(o) || (writesOnly && o.Method == "GET") {
			continue
		}
		r := w.call(o, caller)
		ok2xx := r.status >= 200 && r.status < 300
		switch {
		case r.status == 404 && strings.HasPrefix(r.body, "404 page not found"):
			bad = append(bad, fmt.Sprintf("%s %s %s is unroutable (sweep builds a wrong path)", caller, r.op, r.path))
		case r.status == 0 || r.status >= 500:
			bad = append(bad, fmt.Sprintf("%s %s -> %d %.100s", caller, r.op, r.status, r.body))
		case ok2xx && !w.allowed(r) && (extraOK == nil || !extraOK(r)):
			bad = append(bad, fmt.Sprintf("%s %s %s succeeded (%d): %.100s", caller, r.op, r.path, r.status, strings.ReplaceAll(r.body, "\n", " ")))
		case r.leak && !(directoryOps[r.op] && (caller != "G" && caller != "W" || publicOps[r.op] != "")) && !(extraOK != nil && extraOK(r)):
			bad = append(bad, fmt.Sprintf("%s %s %s leaked seeded data: %.100s", caller, r.op, r.path, r.body))
		}
	}
	return bad
}

// Anonymous callers and authenticated outsiders (including worker/guest roles with no membership).
func TestAuthzSweepAnonymousAndOutsiders(t *testing.T) {
	var mu sync.Mutex
	var all []string
	for _, caller := range []string{"anon", "B", "W", "G"} {
		caller := caller
		t.Run(caller, func(t *testing.T) {
			t.Parallel()
			bad := sweep(t, caller, false, nil)
			mu.Lock()
			all = append(all, bad...)
			mu.Unlock()
		})
	}
	t.Cleanup(func() {
		sort.Strings(all)
		for _, b := range all {
			t.Error(b)
		}
	})
}

// Read-only members (viewer, commenter, worker/guest with a viewer membership) must not write anything.
func TestAuthzSweepReadOnlyMembersCannotWrite(t *testing.T) {
	var mu sync.Mutex
	var all []string
	for _, caller := range []string{"V", "CM", "WV", "GV"} {
		caller := caller
		t.Run(caller, func(t *testing.T) {
			t.Parallel()
			bad := sweep(t, caller, true, func(r result) bool {
				switch r.op {
				case "card.edit.release", "card.notifications.read", "card.subscribe", "card.unsubscribe", "project.favorite", "project.unfavorite":
					return true // per-user state (own edit lock, own subscription, own favourites)
				case "card.next":
					return true // peek without claim is a read; claiming needs edit access (covered by card tests)
				case "filter.create":
					return true // saved filters are private to the user who creates them
				case "board.member.remove":
					return caller == "V" // the member removing their own membership ("leave board")
				case "comment.create":
					return caller == "CM" // canComment members may comment
				}
				return false
			})
			mu.Lock()
			all = append(all, bad...)
			mu.Unlock()
		})
	}
	t.Cleanup(func() {
		sort.Strings(all)
		for _, b := range all {
			t.Error(b)
		}
	})
}

// The owner must be able to do what outsiders are denied, otherwise a "denied" proves nothing for that op.
// Ops whose seeded request is not valid for the owner either are listed in ownerUnverified.
func TestAuthzSweepOwnerBaseline(t *testing.T) {
	w := newWorld(t)
	var failed []string
	for _, o := range ops.All() {
		if o.Auth == ops.AuthAdmin || skipOp(o) {
			continue
		}
		r := w.call(o, "A")
		if r.status < 200 || r.status >= 300 {
			failed = append(failed, o.Name)
		}
		if o.Method != "GET" && r.status >= 200 && r.status < 300 {
			w = newWorld(t) // the mutation may have destroyed seeded objects
		}
	}
	sort.Strings(failed)
	var unexpected []string
	for _, n := range failed {
		if !ownerUnverified[n] {
			unexpected = append(unexpected, n)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("ops that did not succeed for the owner (fix the sweep's request, or list in ownerUnverified): %v", unexpected)
	}
	t.Logf("owner-unverified ops: %v", failed)
}

// ownerUnverified lists ops the generic request builder cannot satisfy even for the owner (they need
// special bodies, tokens or state); their authorization is covered by the domain tests.
var ownerUnverified = map[string]bool{
	"attachment.cover": true, "attachment.create": true, "attachment.thumbnail": true, "auth.oidc.authorize": true, "auth.oidc.callback": true,
	"board.member.add": true, "card.cover.set": true, "card.delete": true, "card.due.complete": true, "card.recurrence.run": true,
	"card.renew": true, "card.restore": true, "config.logo.get": true, "customfieldgroup.create": true, "customfieldvalue.clear": true,
	"customfieldvalue.set": true, "label.project.create": true, "list.clear": true, "notification.read": true, "notification_service.test": true,
	"project.background.upload": true, "project.manager.add": true, "project.manager.remove": true, "time.stop": true,
	"user.avatar.get": true, "user.avatar.update": true,
}

// Every registered web route: anonymous callers get nothing from non-public pages; no caller outside
// the board sees the seeded CANARY data except through the intentionally public share/public pages.
func TestAuthzSweepWebRoutes(t *testing.T) {
	w := newWorld(t)
	var ids []string
	for _, k := range []string{"card", "board", "list", "project", "attachment", "comment", "task", "pubboard", "image", "cfgroup", "userA", "session", "ns", "member"} {
		ids = append(ids, w.ids[k])
	}
	for _, rp := range web.RoutePatterns() {
		meth, pat, _ := strings.Cut(rp[0], " ")
		if meth != "GET" {
			continue
		}
		public := rp[1] == "1"
		for _, caller := range []string{"anon", "B"} {
			for _, cand := range ids {
				p := strings.ReplaceAll(pat, "{$}", "")
				for _, ph := range []string{"id", "boardId", "cardId", "listId", "projectId", "imageId"} {
					p = strings.ReplaceAll(p, "{"+ph+"}", cand)
				}
				p = strings.ReplaceAll(p, "{token}", w.ids["sharetoken"])
				p = strings.ReplaceAll(p, "{name}", w.ids["attname"])
				p = strings.ReplaceAll(p, "{variant}", "original")
				p = strings.ReplaceAll(p, "{tab}", "general")
				p = strings.ReplaceAll(p, "{userId}", w.ids["userA"])
				if strings.Contains(p, "{") || (strings.HasSuffix(p, "/") && strings.Contains(p, "/ui/cards/")) {
					continue
				}
				code, b := w.e.raw(w.keys[caller], "GET", p, nil)
				isShare := strings.HasPrefix(p, "/share/")
				if code == 200 && !public && caller == "anon" {
					t.Errorf("anon GET %s -> 200 on a non-public route", p)
				}
				// public boards are readable by everyone (their project name is part of the page), and the
				// user/group directory is visible to every signed-in user: both are documented exceptions
				if code == 200 && strings.Contains(string(b), canary) && !isShare && cand != w.ids["pubboard"] && !strings.HasPrefix(p, "/admin/groups") {
					// public routes keyed by the share/public token are the only intended exceptions; every
					// other public route (attachments, media, activity fragments) must authorize by board access
					if !strings.HasPrefix(p, "/public/") {
						t.Errorf("%s GET %s leaked seeded data (route %s)", caller, p, rp[0])
					}
				}
				if !strings.Contains(rp[0], "{") {
					break
				}
			}
		}
	}
	for _, p := range []string{"/attachments/" + w.ids["attachment"] + "/download/secret.txt", "/attachments/" + w.ids["attachment"] + "/thumbnail",
		"/boards/" + w.ids["board"] + "/media", "/ui/boards/" + w.ids["board"] + "/activity"} {
		for _, caller := range []string{"anon", "B"} {
			code, b := w.e.raw(w.keys[caller], "GET", p, nil)
			if code == 200 || strings.Contains(string(b), "ATTACHMENT-CANARY-BODY") || strings.Contains(string(b), canary) {
				t.Errorf("%s GET %s -> %d (must be denied)", caller, p, code)
			}
		}
	}
	for _, sec := range []string{"tasks", "comments", "attachments", "fields", "time", "activity"} {
		for _, caller := range []string{"anon", "B"} {
			code, b := w.e.raw(w.keys[caller], "GET", "/ui/cards/"+w.ids["card"]+"/"+sec, nil)
			if strings.Contains(string(b), canary) {
				t.Errorf("%s GET /ui/cards/<id>/%s -> %d leaked seeded data", caller, sec, code)
			}
		}
	}
}
