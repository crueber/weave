package notify_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
	"github.com/crueber/weave/internal/web"
)

type world struct {
	admin, alice, bob, carol, dave *ops.Env
	proj, board, board2, list      string
	card                           string
}

func x(t *testing.T, e *ops.Env, q string, args ...any) {
	t.Helper()
	if _, err := e.DB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func seed(t *testing.T) *world {
	t.Helper()
	root := testutil.Env(t, nil)
	w := &world{proj: "p1", board: "b1", board2: "b2", list: "l1", card: "c1"}
	mk := func(id, role string) *ops.Env {
		x(t, root, `INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,?,?)`, id, id+"@x.io", strings.ToUpper(id[:1])+id[1:], role, db.Now())
		return testutil.As(root, &ops.Principal{UserID: id, Role: role, Name: strings.ToUpper(id[:1]) + id[1:]})
	}
	w.admin, w.alice, w.bob, w.carol, w.dave = mk("admin", "admin"), mk("alice", "boardUser"), mk("bob", "boardUser"), mk("carol", "boardUser"), mk("dave", "boardUser")
	x(t, root, `INSERT INTO projects(id,name,created_at) VALUES('p1','Apollo',?)`, db.Now())
	x(t, root, `INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'Launch board',?),('b2','p1',2,'Secret board',?)`, db.Now(), db.Now())
	x(t, root, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('l1','b1','active',1,'Todo',?),('l2','b1','active',2,'Done',?),('l3','b2','active',1,'Hidden',?)`, db.Now(), db.Now(), db.Now())
	for _, u := range []string{"alice", "bob", "carol"} {
		x(t, root, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`, db.NewID(), "b1", u, "editor", db.Now())
	}
	x(t, root, `INSERT INTO cards(id,board_id,list_id,position,name,description,list_changed_at,created_at) VALUES('c1','b1','l1',1,'Fuel the rocket','Use liquid oxygen',?,?)`, db.Now(), db.Now())
	x(t, root, `INSERT INTO cards(id,board_id,list_id,position,name,description,list_changed_at,created_at) VALUES('c2','b2','l3',1,'Classified plan','Zebra protocol',?,?)`, db.Now(), db.Now())
	x(t, root, `INSERT INTO card_subscriptions(card_id,user_id) VALUES('c1','bob'),('c1','alice')`)
	return w
}

func rows(t *testing.T, e *ops.Env, q string, args ...any) map[string]string {
	t.Helper()
	r, err := e.DB.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[string]string{}
	for r.Next() {
		var k, v string
		_ = r.Scan(&k, &v)
		out[k] = v
	}
	return out
}

func TestCommentNotificationsAndActions(t *testing.T) {
	w := seed(t)
	hub := w.alice.Hub
	ch, cancel := hub.Subscribe(func(room string) bool { return room == "user:bob" || room == "board:b1" })
	defer cancel()

	ops.Emit(w.alice, ops.Event{Type: "commentCreate", BoardID: "b1", CardID: "c1", Name: "comment.created",
		Data: map[string]any{"commentId": "m1", "text": "hi @[Carol](carol) and @[Dave](dave) and @[Alice](alice)"}})
	notify.Wait()

	got := rows(t, w.alice, `SELECT user_id,type FROM notifications`)
	want := map[string]string{"carol": "mentionInComment", "bob": "commentCard"}
	if len(got) != 2 || got["carol"] != want["carol"] || got["bob"] != want["bob"] {
		t.Fatalf("notifications = %v, want %v (dave has no access, alice is the author)", got, want)
	}
	if a := rows(t, w.alice, `SELECT type,board_id FROM actions`); a["commentCreate"] != "b1" {
		t.Fatalf("action not written: %v", a)
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-ch:
			seen[ev.Name+"@"+ev.Room] = true
		case <-time.After(time.Second):
			t.Fatalf("missing realtime events, got %v", seen)
		}
	}
	if !seen["comment.created@board:b1"] || !seen["notification@user:bob"] {
		t.Fatalf("realtime = %v", seen)
	}

	list, err := ops.Typed[notify.ListOut](w.bob, "notification.list", notify.ListIn{})
	if err != nil || len(list.Items) != 1 || list.UnreadCount != 1 {
		t.Fatalf("list = %+v %v", list, err)
	}
	n := list.Items[0]
	if !strings.Contains(n.Summary, "Alice left a new comment to Fuel the rocket on Launch board") {
		t.Fatalf("summary = %q", n.Summary)
	}
	cnt, _ := ops.Typed[notify.CountOut](w.bob, "notification.unread_count", notify.NoInput{})
	if cnt.Count != 1 {
		t.Fatal("badge count")
	}
	// someone else's notification is not found
	if _, err := ops.Call(w.carol, "notification.read", &notify.IDIn{ID: n.ID}); err == nil {
		t.Fatal("carol must not read bob's notification")
	}
	if _, err := ops.Call(w.bob, "notification.read", &notify.IDIn{ID: n.ID}); err != nil {
		t.Fatal(err)
	}
	cnt, _ = ops.Typed[notify.CountOut](w.bob, "notification.unread_count", notify.NoInput{})
	if cnt.Count != 0 {
		t.Fatal("read did not clear badge")
	}
	if c, _ := ops.Typed[notify.CountOut](w.carol, "notification.read_all", notify.NoInput{}); c.Count != 1 {
		t.Fatalf("read_all = %+v", c)
	}
}

func TestMoveAndMembershipNotifications(t *testing.T) {
	w := seed(t)
	ops.Emit(w.alice, ops.Event{Type: "cardUpdate", BoardID: "b1", CardID: "c1", Data: map[string]any{
		"fromList": map[string]any{"id": "l1", "name": "Todo"}, "toList": map[string]any{"id": "l2", "name": "Done"}}})
	ops.Emit(w.alice, ops.Event{Type: "cardMembershipCreate", BoardID: "b1", CardID: "c1", Data: map[string]any{"userId": "carol"}})
	notify.Wait()
	got := rows(t, w.alice, `SELECT user_id||':'||type,'x' FROM notifications`)
	for _, k := range []string{"bob:moveCard", "carol:addMemberToCard"} {
		if got[k] == "" {
			t.Errorf("missing %s in %v", k, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("alice acted; expected 2 notifications, got %v", got)
	}
	n := notify.NotificationOut{Type: "moveCard", CreatorName: "A*", CardName: "C", BoardName: "B", CardID: "c1",
		Data: map[string]any{"fromList": map[string]any{"name": "X"}, "toList": map[string]any{"type": "archive"}}}
	if _, b := notify.Message(n, "markdown"); !strings.Contains(b, `A\*`) || !strings.Contains(b, "**Archive**") {
		t.Errorf("markdown = %q", b)
	}
	if _, b := notify.Message(n, "html"); !strings.Contains(b, "<a href=") || !strings.Contains(b, "<b>X</b>") {
		t.Errorf("html = %q", b)
	}
}

func TestWebhookDeliveryFlow(t *testing.T) {
	w := seed(t)
	var mu sync.Mutex
	type hit struct {
		path, sig, auth, event, body string
	}
	var hits []hit
	failFirst := true
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		hits = append(hits, hit{r.URL.Path, r.Header.Get("X-Weave-Signature"), r.Header.Get("Authorization"), r.Header.Get("X-Weave-Event"), string(b)})
		if failFirst && r.URL.Path == "/flaky" {
			failFirst = false
			rw.WriteHeader(500)
			return
		}
		rw.WriteHeader(204)
	}))
	defer srv.Close()
	old := notify.RetryBackoff
	notify.RetryBackoff = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond}
	defer func() { notify.RetryBackoff = old }()
	mk := func(in notify.WebhookCreateIn) notify.WebhookOut {
		t.Helper()
		o, err := ops.Typed[notify.WebhookOut](w.admin, "webhook.create", in)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	signed := mk(notify.WebhookCreateIn{Name: "signed", URL: srv.URL + "/a", AccessToken: "tok", Secret: "s3cret", Events: &notify.StringList{"cardCreate", "cardUpdate"}, ExcludedEvents: &notify.StringList{"cardUpdate"}})
	mk(notify.WebhookCreateIn{Name: "other-board", URL: srv.URL + "/other", BoardID: "b2"})
	mk(notify.WebhookCreateIn{Name: "flaky", URL: srv.URL + "/flaky", Events: &notify.StringList{"commentCreate"}})
	mk(notify.WebhookCreateIn{Name: "proj", URL: srv.URL + "/proj", ProjectID: "p1", Events: &notify.StringList{"cardCreate"}})
	notify.Wait()
	mu.Lock()
	hits = nil // ignore webhookCreate events fired while setting up
	mu.Unlock()

	ops.Emit(w.alice, ops.Event{Type: "cardCreate", BoardID: "b1", CardID: "c1", Data: map[string]any{"item": map[string]any{"id": "c1", "name": "Fuel the rocket"}, "prevData": map[string]any{"x": 1}}})
	ops.Emit(w.alice, ops.Event{Type: "cardUpdate", BoardID: "b1", CardID: "c1", Data: map[string]any{"item": map[string]any{"id": "c1"}}})
	ops.Emit(w.alice, ops.Event{Type: "commentCreate", BoardID: "b1", CardID: "c1", Data: map[string]any{"commentId": "m1", "text": "hello"}})
	notify.Wait()

	mu.Lock()
	defer mu.Unlock()
	byPath := map[string][]hit{}
	for _, h := range hits {
		byPath[h.path] = append(byPath[h.path], h)
	}
	if len(byPath["/a"]) != 1 {
		t.Fatalf("signed hook should get only cardCreate (cardUpdate excluded): %v", byPath["/a"])
	}
	h := byPath["/a"][0]
	if h.event != "cardCreate" || h.auth != "Bearer tok" || h.sig != notify.Sign("s3cret", h.body) || !strings.HasPrefix(h.sig, "sha256=") {
		t.Fatalf("bad headers: %+v", h)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(h.body), &p)
	if p["event"] != "cardCreate" || p["prevData"] == nil || p["user"].(map[string]any)["id"] != "alice" {
		t.Fatalf("payload = %s", h.body)
	}
	if strings.Contains(h.body, "actionId") {
		t.Fatal("actionId leaked into payload")
	}
	if len(byPath["/other"]) != 0 {
		t.Fatal("board-scoped webhook fired for another board")
	}
	if len(byPath["/proj"]) != 1 {
		t.Fatalf("project-scoped hook: %v", byPath["/proj"])
	}
	if len(byPath["/flaky"]) != 2 {
		t.Fatalf("flaky hook should be retried once: %v", byPath["/flaky"])
	}
	// delivery log
	flaky := rows(t, w.admin, `SELECT w.name, d.attempts||'/'||COALESCE(d.delivered_at IS NOT NULL,0) FROM webhook_deliveries d JOIN webhooks w ON w.id=d.webhook_id WHERE w.name='flaky'`)
	if flaky["flaky"] != "2/1" {
		t.Fatalf("flaky delivery row = %v", flaky)
	}
	dl, err := ops.Typed[notify.DeliveriesOut](w.admin, "webhook.deliveries", notify.DeliveriesIn{ID: signed.ID})
	if err != nil || len(dl.Items) == 0 || dl.Items[0].State != "delivered" {
		t.Fatalf("deliveries = %+v %v", dl, err)
	}
	mu.Unlock()
	// ping + redelivery ops
	ping, err := ops.Typed[notify.DeliveryOut](w.admin, "webhook.test", notify.IDIn{ID: signed.ID})
	mu.Lock()
	if err != nil || ping.State != "delivered" || ping.Event != "ping" {
		t.Fatalf("ping = %+v %v", ping, err)
	}
	mu.Unlock()
	if _, err := ops.Call(w.admin, "webhook.retry", &notify.IDIn{ID: ping.ID}); err != nil {
		t.Fatal(err)
	}
	notify.Wait()
	mu.Lock()
	// delete cascades the log
	if _, err := ops.Call(w.admin, "webhook.delete", &notify.IDIn{ID: signed.ID}); err != nil {
		t.Fatal(err)
	}
	if n := rows(t, w.admin, `SELECT webhook_id,'x' FROM webhook_deliveries WHERE webhook_id=?`, signed.ID); len(n) != 0 {
		t.Fatal("delivery log not cascaded")
	}
}

func TestNotificationServices(t *testing.T) {
	t.Setenv("WEAVE_NOTIFY_ALLOW_HOSTS", "127.0.0.1") // the receiving test server is on loopback
	w := seed(t)
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	url := "generic://" + strings.TrimPrefix(srv.URL, "http://") + "/hook?disabletls=yes"

	if _, err := ops.Call(w.bob, "notification_service.create", &notify.ServiceCreateIn{URL: "not a url"}); err == nil {
		t.Fatal("invalid URL accepted")
	}
	if _, err := ops.Call(w.bob, "notification_service.create", &notify.ServiceCreateIn{URL: url, Format: "rtf"}); err == nil {
		t.Fatal("invalid format accepted")
	}
	svc, err := ops.Typed[notify.ServiceOut](w.bob, "notification_service.create", notify.ServiceCreateIn{URL: url, Format: "markdown"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Call(w.carol, "notification_service.update", &notify.ServiceUpdateIn{ID: svc.ID}); err == nil {
		t.Fatal("other user could update a service")
	}
	if _, err := ops.Call(w.bob, "notification_service.test", &notify.IDIn{ID: svc.ID}); err != nil {
		t.Fatal("test send:", err)
	}
	// board service needs manage rights
	if _, err := ops.Call(w.alice, "notification_service.create", &notify.ServiceCreateIn{URL: url, BoardID: "b1"}); err == nil {
		t.Fatal("board editor must not add board services")
	}
	if _, err := ops.Call(w.admin, "notification_service.create", &notify.ServiceCreateIn{URL: url, BoardID: "b1"}); err != nil {
		t.Fatal(err)
	}
	ops.Emit(w.alice, ops.Event{Type: "commentCreate", BoardID: "b1", CardID: "c1", Data: map[string]any{"commentId": "m1", "text": "ship it"}})
	notify.Wait()
	mu.Lock()
	defer mu.Unlock()
	// test send + bob's own service (comment notification) + board service
	if len(bodies) != 3 {
		t.Fatalf("expected 3 service deliveries, got %d: %v", len(bodies), bodies)
	}
	joined := strings.Join(bodies, "\n")
	if !strings.Contains(joined, "ship it") || !strings.Contains(joined, "Fuel the rocket") {
		t.Fatalf("bodies = %s", joined)
	}
	l, _ := ops.Typed[notify.ServiceListOut](w.bob, "notification_service.list", notify.ServiceListIn{})
	if len(l.Items) != 1 {
		t.Fatal("list should only show my services")
	}
	if _, err := ops.Call(w.bob, "notification_service.delete", &notify.IDIn{ID: svc.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigAndMail(t *testing.T) {
	w := seed(t)
	s := func(v string) *string { return &v }
	if _, err := ops.Call(w.alice, "config.get", &notify.NoInput{}); err == nil {
		t.Fatal("config.get must be admin only")
	}
	_, err := ops.Call(w.admin, "config.update", &notify.Settings{AppName: s("Acme Tasks"), SMTPHost: s("smtp.invalid"), SMTPPassword: s("hunter2"), OIDCIssuer: s("https://id.example"), OIDCClientSecret: s("shh")})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := ops.Typed[notify.SettingsView](w.admin, "config.get", notify.NoInput{})
	if v.Values["branding.appName"] != "Acme Tasks" || v.Values["smtp.password"] != "" || v.Values["oidc.clientSecret"] != "" || len(v.SecretsSet) != 2 {
		t.Fatalf("view = %+v", v)
	}
	if notify.Config(w.admin, "smtp.password") != "hunter2" {
		t.Fatal("secret must be stored")
	}
	// blank secret keeps the stored one
	_, _ = ops.Call(w.admin, "config.update", &notify.Settings{SMTPPassword: s("")})
	if notify.Config(w.admin, "smtp.password") != "hunter2" {
		t.Fatal("blank secret erased the stored one")
	}
	if _, err := ops.Call(w.admin, "config.update", &notify.Settings{SMTPSecure: s("ssl3")}); err == nil {
		t.Fatal("bad smtp.secure accepted")
	}
	pub, _ := ops.Typed[notify.PublicConfig](testutil.As(w.admin, nil), "config.public", notify.NoInput{})
	if pub.AppName != "Acme Tasks" {
		t.Fatalf("public = %+v", pub)
	}
	t.Setenv("WEAVE_BRANDING_APPNAME", "FromEnv")
	if notify.Config(w.admin, "branding.appName") != "FromEnv" {
		t.Fatal("env override ignored")
	}
	v, _ = ops.Typed[notify.SettingsView](w.admin, "config.get", notify.NoInput{})
	if len(v.EnvLocked) != 1 || v.EnvLocked[0] != "branding.appName" {
		t.Fatalf("envLocked = %v", v.EnvLocked)
	}
	if all := notify.ConfigAll(w.admin, "oidc."); all["oidc.issuer"] != "https://id.example" {
		t.Fatalf("ConfigAll = %v", all)
	}

	// logo
	png := []byte("\x89PNG\r\n\x1a\n0000000000")
	if _, err := ops.Call(w.admin, "config.logo.upload", &notify.LogoIn{Logo: &ops.File{Name: "l.png", ContentType: "application/octet-stream", Data: png}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Call(w.admin, "config.logo.upload", &notify.LogoIn{Logo: &ops.File{Name: "l.exe", Data: []byte("MZ....")}}); err == nil {
		t.Fatal("non-image accepted as logo")
	}
	st, err := ops.Typed[*ops.Stream](testutil.As(w.admin, nil), "config.logo.get", notify.NoInput{})
	if err != nil || st.ContentType != "image/png" || string(st.Data) != string(png) {
		t.Fatalf("logo = %+v %v", st, err)
	}

	// mail: unconfigured then stubbed then real go-mail against a fake SMTP server
	root := testutil.Env(t, nil)
	if err := notify.SendMail(root, []string{"a@b.c"}, "s", "t", ""); err == nil {
		t.Fatal("SendMail without config must fail")
	}
	got := fakeSMTP(t)
	_, err = ops.Call(w.admin, "config.update", &notify.Settings{SMTPHost: s("127.0.0.1"), SMTPPort: s(got.port), SMTPSecure: s("none"), SMTPFrom: s("Weave <w@example.com>")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Call(w.admin, "smtp.test", &notify.SMTPTestIn{To: "admin@x.io"}); err != nil {
		t.Fatal("smtp.test:", err)
	}
	if m := got.wait(); !strings.Contains(m, "RCPT TO:<admin@x.io>") || !strings.Contains(m, "Subject: Weave SMTP test") {
		t.Fatalf("smtp transcript = %q", m)
	}
}

type smtpCapture struct {
	port string
	ch   chan string
}

func (s *smtpCapture) wait() string {
	select {
	case m := <-s.ch:
		return m
	case <-time.After(3 * time.Second):
		return ""
	}
}

func fakeSMTP(t *testing.T) *smtpCapture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	c := &smtpCapture{port: port, ch: make(chan string, 4)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				var tr strings.Builder
				write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
				write("220 fake ESMTP")
				data := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if data {
						tr.WriteString(line)
						if line == ".\r\n" {
							data = false
							write("250 queued")
						}
						continue
					}
					tr.WriteString(line)
					up := strings.ToUpper(line)
					switch {
					case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
						write("250 fake")
					case strings.HasPrefix(up, "DATA"):
						data = true
						write("354 go")
					case strings.HasPrefix(up, "QUIT"):
						write("221 bye")
						c.ch <- tr.String()
						return
					default:
						write("250 ok")
					}
				}
			}()
		}
	}()
	return c
}

func TestSearchRespectsAccess(t *testing.T) {
	w := seed(t)
	x(t, w.admin, `INSERT INTO comments(id,card_id,user_id,text,created_at) VALUES('m1','c1','alice','The oxygen tank leaks',?)`, db.Now())
	res, err := ops.Typed[notify.SearchOut](w.carol, "search.query", notify.SearchIn{Q: "oxyg"})
	if err != nil || len(res.Items) != 2 {
		t.Fatalf("carol oxygen = %+v %v", res, err)
	}
	for _, h := range res.Items {
		if h.CardID != "c1" || h.BoardName != "Launch board" || h.ProjectName != "Apollo" {
			t.Fatalf("hit = %+v", h)
		}
	}
	if !strings.Contains(res.Items[0].SnippetHTML, "<mark>") && !strings.Contains(res.Items[1].SnippetHTML, "<mark>") {
		t.Fatal("no highlighted snippet")
	}
	// zebra is only on the secret board: dave and carol must not see it, admin must
	for _, e := range []*ops.Env{w.carol, w.dave} {
		r, _ := ops.Typed[notify.SearchOut](e, "search.query", notify.SearchIn{Q: "zebra"})
		if len(r.Items) != 0 {
			t.Fatalf("%s sees hidden card: %+v", e.User.UserID, r.Items)
		}
		r, _ = ops.Typed[notify.SearchOut](e, "search.query", notify.SearchIn{Q: "secret"})
		if len(r.Items) != 0 {
			t.Fatalf("%s sees hidden board", e.User.UserID)
		}
	}
	if r, _ := ops.Typed[notify.SearchOut](w.admin, "search.query", notify.SearchIn{Q: "zebra"}); len(r.Items) != 1 {
		t.Fatal("admin should see the secret card")
	}
	// project search + hostile input does not blow up
	if r, _ := ops.Typed[notify.SearchOut](w.carol, "search.query", notify.SearchIn{Q: "apollo"}); len(r.Items) != 1 || r.Items[0].Kind != "project" {
		t.Fatalf("project search = %+v", r.Items)
	}
	for _, q := range []string{`"`, `a:b -c`, `foo*"bar`, `AND OR NOT`, `(((`} {
		if _, err := ops.Typed[notify.SearchOut](w.carol, "search.query", notify.SearchIn{Q: q}); err != nil {
			if e, ok := err.(*ops.Error); !ok || e.Code != "validation" {
				t.Fatalf("query %q: %v", q, err)
			}
		}
	}
	// triggers follow edits and deletes
	x(t, w.admin, `UPDATE cards SET name='Renamed capsule' WHERE id='c1'`)
	if r, _ := ops.Typed[notify.SearchOut](w.carol, "search.query", notify.SearchIn{Q: "capsule", Kind: "card"}); len(r.Items) != 1 {
		t.Fatal("update not indexed")
	}
	if r, _ := ops.Typed[notify.SearchOut](w.carol, "search.query", notify.SearchIn{Q: "rocket", Kind: "card"}); len(r.Items) != 0 {
		t.Fatal("old name still indexed")
	}
	x(t, w.admin, `DELETE FROM comments WHERE id='m1'`)
	if r, _ := ops.Typed[notify.SearchOut](w.carol, "search.query", notify.SearchIn{Q: "leaks"}); len(r.Items) != 0 {
		t.Fatal("deleted comment still indexed")
	}
	// reindex rebuilds from scratch and is admin only
	if _, err := ops.Call(w.carol, "search.reindex", &notify.NoInput{}); err == nil {
		t.Fatal("reindex must be admin only")
	}
	x(t, w.admin, `DELETE FROM search_idx`)
	o, err := ops.Typed[notify.ReindexOut](w.admin, "search.reindex", notify.NoInput{})
	if err != nil || o.Indexed < 5 {
		t.Fatalf("reindex = %+v %v", o, err)
	}
	if r, _ := ops.Typed[notify.SearchOut](w.carol, "search.query", notify.SearchIn{Q: "capsule"}); len(r.Items) != 1 {
		t.Fatal("reindex lost cards")
	}
	if err := notify.IndexCard(w.admin, "c1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := ops.Typed[notify.SearchOut](w.carol, "search.query", notify.SearchIn{Q: "capsule", Kind: "card"}); len(r.Items) != 1 {
		t.Fatalf("IndexCard duplicated or lost the card: %+v", r.Items)
	}
}

func TestDashboard(t *testing.T) {
	w := seed(t)
	soon := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	late := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	x(t, w.admin, `UPDATE cards SET due_date=? WHERE id='c1'`, soon)
	x(t, w.admin, `INSERT INTO cards(id,board_id,list_id,position,name,due_date,list_changed_at,created_at) VALUES('c3','b1','l1',2,'Late card',?,?,?)`, late, db.Now(), db.Now())
	x(t, w.admin, `INSERT INTO card_memberships(card_id,user_id) VALUES('c1','carol'),('c3','carol'),('c2','carol')`)
	ops.Emit(w.alice, ops.Event{Type: "cardCreate", BoardID: "b1", CardID: "c1"})
	ops.Emit(w.admin, ops.Event{Type: "cardCreate", BoardID: "b2", CardID: "c2"})

	d, err := ops.Typed[notify.DashboardOut](w.carol, "dashboard.get", notify.NoInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.MyCards) != 2 { // c2 is on a board carol cannot see
		t.Fatalf("myCards = %+v", d.MyCards)
	}
	if len(d.DueSoon) != 1 || d.DueSoon[0].ID != "c1" || len(d.Overdue) != 1 || d.Overdue[0].ID != "c3" {
		t.Fatalf("due = %+v overdue = %+v", d.DueSoon, d.Overdue)
	}
	if len(d.Activity) != 1 || !strings.Contains(d.Activity[0].Summary, "Alice created Fuel the rocket") {
		t.Fatalf("activity = %+v", d.Activity)
	}
	d, err = ops.Typed[notify.DashboardOut](w.carol, "dashboard.update", notify.DashboardUpdateIn{Panels: "overdue,myCards", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if d.Panels[0].ID != "overdue" || !d.Panels[1].Enabled || d.Panels[2].Enabled || len(d.MyCards) != 1 || len(d.DueSoon) != 0 {
		t.Fatalf("customised dashboard = %+v", d)
	}
	if _, err := ops.Call(w.carol, "dashboard.update", &notify.DashboardUpdateIn{Panels: "bogus"}); err == nil {
		t.Fatal("unknown panel accepted")
	}
	var saved string
	_ = w.carol.DB.QueryRow(`SELECT dashboard FROM users WHERE id='carol'`).Scan(&saved)
	if !strings.Contains(saved, "overdue") {
		t.Fatalf("not persisted in users.dashboard: %s", saved)
	}
}

func TestExport(t *testing.T) {
	w := seed(t)
	x(t, w.admin, `INSERT INTO comments(id,card_id,user_id,text,created_at) VALUES('m1','c1','alice','<script>alert(1)</script> nice',?)`, db.Now())
	x(t, w.admin, `INSERT INTO task_lists(id,card_id,position,name,created_at) VALUES('tl1','c1',1,'Checklist',?)`, db.Now())
	x(t, w.admin, `INSERT INTO tasks(id,task_list_id,position,name,is_completed,created_at) VALUES('t1','tl1',1,'Buy fuel',1,?),('t2','tl1',2,'Light fuse',0,?)`, db.Now(), db.Now())
	x(t, w.admin, `INSERT INTO card_memberships(card_id,user_id) VALUES('c1','bob')`)

	st, err := ops.Typed[*ops.Stream](w.carol, "export.board", notify.ExportBoardIn{ID: "b1"})
	if err != nil {
		t.Fatal(err)
	}
	h := string(st.Data)
	for _, want := range []string{"Launch board", "Apollo", "Fuel the rocket", "Use liquid oxygen", "Buy fuel", "Light fuse", "Bob", "&lt;script&gt;"} {
		if !strings.Contains(h, want) {
			t.Errorf("export missing %q", want)
		}
	}
	if strings.Contains(h, "<script>") || strings.Contains(h, "Classified") || st.ContentType != "text/html; charset=utf-8" || !strings.HasSuffix(st.Name, ".html") || strings.Contains(st.Name, ".print.") {
		t.Fatalf("bad export: ct=%s name=%s", st.ContentType, st.Name)
	}
	pdf, err := ops.Typed[*ops.Stream](w.carol, "export.board", notify.ExportBoardIn{ID: "b1", Format: "pdf"})
	if err != nil || !strings.HasSuffix(pdf.Name, ".print.html") || !strings.Contains(string(pdf.Data), "window.print()") || !strings.Contains(string(pdf.Data), "@page") {
		t.Fatalf("pdf fallback = %+v %v", pdf, err)
	}
	if _, err := ops.Call(w.dave, "export.board", &notify.ExportBoardIn{ID: "b1"}); err == nil {
		t.Fatal("dave exported a board he cannot see")
	}
	if _, err := ops.Call(w.carol, "export.card", &notify.ExportCardIn{ID: "c2"}); err == nil {
		t.Fatal("card on hidden board exported")
	}
	if _, err := ops.Call(w.carol, "export.board", &notify.ExportBoardIn{ID: "b1", Format: "docx"}); err == nil {
		t.Fatal("bad format accepted")
	}
	c, err := ops.Typed[*ops.Stream](w.carol, "export.card", notify.ExportCardIn{ID: "c1"})
	if err != nil || !strings.Contains(string(c.Data), "Light fuse") || c.Name != "Fuel-the-rocket.html" {
		t.Fatalf("card export = %+v %v", c, err)
	}
}

func TestUIPagesRender(t *testing.T) {
	w := seed(t)
	old := notify.RetryBackoff
	notify.RetryBackoff = nil
	defer func() { notify.RetryBackoff = old }()
	x(t, w.admin, `INSERT INTO notification_services(id,user_id,url,format,created_at) VALUES('s1','admin','generic://example.com/x','text',?)`, db.Now())
	if _, err := ops.Call(w.admin, "webhook.create", &notify.WebhookCreateIn{Name: "hook", URL: "http://127.0.0.1:1/x"}); err != nil {
		t.Fatal(err)
	}
	notify.Wait()
	mux := http.NewServeMux()
	var cur *ops.Env = w.admin
	web.Mount(mux, func(r *http.Request) *ops.Env { c := *cur; c.Ctx = r.Context(); c.Req = r; return &c })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for path, want := range map[string]string{
		"/notifications":                         "Notifications",
		"/ui/notifications/badge":                "Inbox",
		"/notification-services":                 "Notification services",
		"/ui/boards/b1/notification-services":    "New service URL",
		"/admin/webhooks":                        "New webhook",
		"/admin/config":                          "SMTP",
		"/search?q=rocket":                       "Fuel the rocket",
		"/ui/search?q=rocket":                    "Fuel the rocket",
		"/search":                                "Search",
		"/dashboard":                             "Customize panels",
		"/export/boards/b1":                      "Launch board",
		"/export/cards/c1?format=pdf":            "Print / Save as PDF",
		"/branding/logo":                         "",
		"/admin/webhooks/nonexistent/deliveries": "",
	} {
		code, body := get(path)
		if path == "/branding/logo" || strings.Contains(path, "nonexistent") {
			if code != 404 {
				t.Errorf("%s: want 404, got %d", path, code)
			}
			continue
		}
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("%s: code %d, missing %q in %.200s", path, code, want, body)
		}
	}
	var whID string
	_ = w.admin.DB.QueryRow(`SELECT id FROM webhooks LIMIT 1`).Scan(&whID)
	if code, body := get("/admin/webhooks/" + whID + "/deliveries"); code != 200 || !strings.Contains(body, "Deliveries") {
		t.Errorf("deliveries page: %d", code)
	}
	// non-admins cannot open admin pages
	cur = w.bob
	if code, _ := get("/admin/config"); code != 403 {
		t.Errorf("bob on /admin/config: %d", code)
	}
}
