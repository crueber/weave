package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// Persona and SSRF tests for board-owned webhooks (authz/webhooks.go, notify/webhook_ssrf.go).

const pubIP = "203.0.113.9" // documentation address: what the fake resolver answers for "public" names

type hookNet struct {
	mu      sync.Mutex
	dialed  []string
	answers map[string][]string
}

// fakeNet installs a fake resolver and a dialer that sends connections for pubIP to the local test server (so a
// "public" host can be exercised without leaving the machine) and refuses every other address.
func fakeNet(t *testing.T, srv *httptest.Server) *hookNet {
	t.Helper()
	n := &hookNet{answers: map[string][]string{
		"good.test": {pubIP}, "evil.test": {"10.0.0.1"}, "mixed.test": {pubIP, "127.0.0.1"}, "meta.test": {"169.254.169.254"},
		"ula.test": {"fd00::1"}, "loop6.test": {"::1"},
	}}
	oldL, oldD, oldB := notify.LookupIP, notify.WebhookDialer, notify.RetryBackoff
	notify.LookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		var out []net.IP
		for _, a := range n.answers[host] {
			out = append(out, net.ParseIP(a))
		}
		if len(out) == 0 {
			return nil, errors.New("no such host")
		}
		return out, nil
	}
	local := ""
	if srv != nil {
		u, _ := url.Parse(srv.URL)
		local = u.Host
	}
	notify.WebhookDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		n.mu.Lock()
		n.dialed = append(n.dialed, addr)
		n.mu.Unlock()
		host, _, _ := net.SplitHostPort(addr)
		if host == pubIP && local != "" {
			var d net.Dialer
			return d.DialContext(ctx, network, local)
		}
		return nil, errors.New("test dialer: refused " + addr)
	}
	notify.RetryBackoff = nil
	t.Cleanup(func() { notify.Wait(); notify.LookupIP, notify.WebhookDialer, notify.RetryBackoff = oldL, oldD, oldB })
	return n
}

func (n *hookNet) set(host string, ips ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.answers[host] = ips
}

func (n *hookNet) dialedAny(prefix string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, a := range n.dialed {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

type hookWorld struct {
	*world
	olga, vic, cora, agent, anon *ops.Env
}

func hookSeed(t *testing.T) *hookWorld {
	w := &hookWorld{world: seed(t)}
	mk := func(id, role string) *ops.Env {
		x(t, w.admin, `INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,?,?)`, id, id+"@x.io", id, role, db.Now())
		return testutil.As(w.admin, &ops.Principal{UserID: id, Role: role, Name: id})
	}
	w.olga, w.vic, w.cora = mk("olga", "boardUser"), mk("vic", "boardUser"), mk("cora", "boardUser")
	x(t, w.admin, `INSERT INTO users(id,email,name,role,is_agent,created_at) VALUES('bot','bot@agents.invalid','Bot','boardUser',1,?)`, db.Now())
	w.agent = testutil.As(w.admin, &ops.Principal{UserID: "bot", Role: "boardUser", Name: "Bot", IsAgent: true})
	w.anon = testutil.As(w.admin, nil)
	x(t, w.admin, `INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES('pm1','p1','olga',?)`, db.Now())
	x(t, w.admin, `INSERT INTO projects(id,name,created_at) VALUES('p2','Other',?)`, db.Now())
	x(t, w.admin, `INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b3','p2',1,'Not hers',?)`, db.Now())
	x(t, w.admin, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m-v','b1','vic','viewer',?)`, db.Now())
	x(t, w.admin, `INSERT INTO board_memberships(id,board_id,user_id,role,can_comment,created_at) VALUES('m-c','b1','cora','viewer',1,?)`, db.Now())
	x(t, w.admin, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m-b','b1','bot','editor',?)`, db.Now())
	return w
}

func status(err error) int {
	if oe, ok := err.(*ops.Error); ok {
		return oe.Status
	}
	if err == nil {
		return 200
	}
	return 500
}

func TestBoardWebhookPersonas(t *testing.T) {
	w := hookSeed(t)
	fakeNet(t, nil)
	in := func(board string) *notify.WebhookCreateIn {
		return &notify.WebhookCreateIn{Name: "h", URL: "https://good.test/hook", BoardID: board, Secret: "s3cret-value", AccessToken: "tok-value"}
	}
	// create: board scope needs Manage on the board; global/project scopes are admin-only
	cases := []struct {
		who  string
		env  *ops.Env
		in   *notify.WebhookCreateIn
		want int
	}{
		{"owner board", w.olga, in("b1"), 200},
		{"owner other board", w.olga, in("b3"), 404},
		{"owner global", w.olga, &notify.WebhookCreateIn{Name: "g", URL: "https://good.test/"}, 404},
		{"owner project", w.olga, &notify.WebhookCreateIn{Name: "p", URL: "https://good.test/", ProjectID: "p1", BoardID: "b1"}, 403},
		{"owner user filter", w.olga, &notify.WebhookCreateIn{Name: "u", URL: "https://good.test/", BoardID: "b1", UserID: "alice"}, 422},
		{"owner board-only events", w.olga, &notify.WebhookCreateIn{Name: "n", URL: "https://good.test/", BoardID: "b1", Events: &notify.StringList{"notificationCreate"}}, 422},
		{"editor", w.alice, in("b1"), 403},
		{"viewer", w.vic, in("b1"), 403},
		{"commenter", w.cora, in("b1"), 403},
		{"outsider", w.dave, in("b1"), 404},
		{"agent", w.agent, in("b1"), 404},
		{"anonymous", w.anon, in("b1"), 401},
		{"admin global", w.admin, &notify.WebhookCreateIn{Name: "g", URL: "http://127.0.0.1:1/x"}, 200}, // admin policy unchanged
		{"admin project", w.admin, &notify.WebhookCreateIn{Name: "p", URL: "https://good.test/", ProjectID: "p1"}, 200},
		{"admin board", w.admin, in("b1"), 200},              // the admin persona manages p1 as a project manager
		{"admin board not a member", w.admin, in("b3"), 404}, // the admin role alone manages no board
	}
	var mine notify.WebhookOut
	for _, c := range cases {
		out, err := ops.Call(c.env, "webhook.create", c.in)
		if got := status(err); got != c.want {
			t.Fatalf("%s: status %d (%v), want %d", c.who, got, err, c.want)
		}
		if c.who == "owner board" {
			mine = out.(notify.WebhookOut)
			if !mine.UserCreated || mine.Scope != "board" || mine.Secret != "" || mine.AccessToken != "" || !mine.HasSecret || !mine.HasAccessToken {
				t.Fatalf("owner webhook output: %+v", mine)
			}
		}
	}
	// list: the owner sees every webhook of the boards she manages (whoever created it); secrets never travel
	l, err := ops.Typed[notify.WebhookListOut](w.olga, "webhook.list", notify.WebhookListIn{})
	if err != nil || len(l.Items) != 2 || l.Items[0].ID != mine.ID {
		t.Fatalf("owner list: %+v %v", l, err)
	}
	b, _ := json.Marshal(l)
	if strings.Contains(string(b), "s3cret-value") || strings.Contains(string(b), "tok-value") {
		t.Fatal("secret in webhook.list")
	}
	for name, env := range map[string]*ops.Env{"editor": w.alice, "viewer": w.vic, "outsider": w.dave} {
		if l, err := ops.Typed[notify.WebhookListOut](env, "webhook.list", notify.WebhookListIn{}); err != nil || len(l.Items) != 0 {
			t.Fatalf("%s list: %+v %v", name, l, err)
		}
		if _, err := ops.Typed[notify.WebhookListOut](env, "webhook.list", notify.WebhookListIn{BoardID: "b1"}); err == nil {
			t.Fatalf("%s may list board b1's webhooks", name)
		}
	}
	if l, _ := ops.Typed[notify.WebhookListOut](w.admin, "webhook.list", notify.WebhookListIn{}); len(l.Items) != 4 {
		t.Fatalf("admin list: %d", len(l.Items))
	}
	// manage: owner yes; members/outsiders/agents not_found; a board webhook an admin created is managed by the
	// board's managers too (and a non-admin's change puts it under the strict destination policy)
	var adminBoard string
	for _, x := range func() []notify.WebhookOut {
		l, _ := ops.Typed[notify.WebhookListOut](w.admin, "webhook.list", notify.WebhookListIn{BoardID: "b1"})
		return l.Items
	}() {
		if !x.UserCreated {
			adminBoard = x.ID
		}
	}
	for name, env := range map[string]*ops.Env{"editor": w.alice, "viewer": w.vic, "commenter": w.cora, "outsider": w.dave, "agent": w.agent} {
		for _, op := range []string{"webhook.test", "webhook.delete", "webhook.deliveries"} {
			var arg any = &notify.IDIn{ID: mine.ID}
			if op == "webhook.deliveries" {
				arg = &notify.DeliveriesIn{ID: mine.ID}
			}
			if _, err := ops.Call(env, op, arg); status(err) != 404 {
				t.Fatalf("%s %s: %v", name, op, err)
			}
		}
	}
	if out, err := ops.Typed[notify.WebhookOut](w.olga, "webhook.update", &notify.WebhookUpdateIn{ID: adminBoard, Name: sp("x")}); err != nil || !out.UserCreated {
		t.Fatalf("owner editing an admin-created board webhook: %+v %v", out, err)
	}
	// moving to a board she does not manage, or dropping the board scope
	if _, err := ops.Call(w.olga, "webhook.update", &notify.WebhookUpdateIn{ID: mine.ID, BoardID: sp("b3")}); status(err) != 404 {
		t.Fatalf("move to b3: %v", err)
	}
	if _, err := ops.Call(w.olga, "webhook.update", &notify.WebhookUpdateIn{ID: mine.ID, BoardID: sp("")}); status(err) != 404 {
		t.Fatalf("drop board scope: %v", err)
	}
	if _, err := ops.Call(w.olga, "webhook.update", &notify.WebhookUpdateIn{ID: mine.ID, BoardID: sp("b2"), Name: sp("moved")}); err != nil {
		t.Fatalf("move to b2 (also hers): %v", err)
	}
	// events visible to a board webhook
	ev, _ := ops.Typed[notify.EventsOut](w.olga, "webhook.events", notify.EventsIn{})
	for _, e := range ev.Items {
		if strings.HasPrefix(e, "notification") || strings.HasPrefix(e, "user") || strings.HasPrefix(e, "project") || strings.HasPrefix(e, "webhook") || e == "configUpdate" {
			t.Fatalf("board webhook events include %s", e)
		}
	}
	// delete
	if _, err := ops.Call(w.olga, "webhook.delete", &notify.IDIn{ID: mine.ID}); err != nil {
		t.Fatal(err)
	}
}

func sp(s string) *string { return &s }

func TestBoardWebhookStrictDestinations(t *testing.T) {
	w := hookSeed(t)
	n := fakeNet(t, nil)
	refused := []string{
		"http://127.0.0.1/x", "http://127.1.2.3:8080/", "http://localhost:8080/", "http://app.localhost/", "http://[::1]/", "http://loop6.test/",
		"http://10.0.0.5/", "http://172.16.0.1/", "http://192.168.1.10/", "http://100.64.0.1/", "http://[fd00::1]/", "http://ula.test/",
		"http://169.254.169.254/latest/meta-data", "http://[fd00:ec2::254]/", "http://metadata.google.internal/", "http://meta.test/",
		"http://0.0.0.0/", "http://0.1.2.3/", "http://224.0.0.1/", "http://[::ffff:127.0.0.1]/", "http://[64:ff9b::a00:1]/", "http://[2002:a00:1::]/",
		"http://evil.test/", "http://mixed.test/", "http://nxdomain.test/", "ftp://good.test/", "javascript:alert(1)", "https://good.test/" + strings.Repeat("a", 2100),
	}
	for _, u := range refused {
		if _, err := ops.Call(w.olga, "webhook.create", &notify.WebhookCreateIn{Name: "h", URL: u, BoardID: "b1"}); status(err) != 422 {
			t.Errorf("owner webhook to %s: %v (want refused)", u, err)
		}
	}
	if _, err := ops.Call(w.olga, "webhook.create", &notify.WebhookCreateIn{Name: "h", URL: "https://good.test/x", BoardID: "b1", AccessToken: "a\r\nX-Injected: 1"}); status(err) != 422 {
		t.Fatalf("header injection through the token: %v", err)
	}
	// the homelab switch allows private networks, never loopback or metadata
	t.Setenv("WEAVE_WEBHOOK_USER_ALLOW_PRIVATE", "true")
	if _, err := ops.Call(w.olga, "webhook.create", &notify.WebhookCreateIn{Name: "lan", URL: "http://192.168.1.10/hook", BoardID: "b1"}); err != nil {
		t.Fatalf("private destination with the switch on: %v", err)
	}
	for _, u := range []string{"http://127.0.0.1/", "http://169.254.169.254/", "http://[::1]/"} {
		if _, err := ops.Call(w.olga, "webhook.create", &notify.WebhookCreateIn{Name: "h", URL: u, BoardID: "b1"}); status(err) != 422 {
			t.Errorf("switch on, %s: %v (want refused)", u, err)
		}
	}
	t.Setenv("WEAVE_WEBHOOK_USER_ALLOW_PRIVATE", "")
	// DNS rebinding: the name resolved publicly when saved, now answers loopback; the dialer re-resolves and refuses
	h, err := ops.Typed[notify.WebhookOut](w.olga, "webhook.create", notify.WebhookCreateIn{Name: "rebind", URL: "http://good.test/hook", BoardID: "b1"})
	if err != nil {
		t.Fatal(err)
	}
	n.set("good.test", "127.0.0.1")
	d, err := ops.Typed[notify.DeliveryOut](w.olga, "webhook.test", notify.IDIn{ID: h.ID})
	if err != nil || d.State != "failed" || d.Error != notify.GenericDeliveryError {
		t.Fatalf("rebound delivery: %+v %v", d, err)
	}
	if n.dialedAny("127.") {
		t.Fatal("the dialer connected to loopback after rebinding")
	}
	// admin-created webhooks keep today's policy (no destination restriction)
	if _, err := ops.Call(w.admin, "webhook.create", &notify.WebhookCreateIn{Name: "lan", URL: "http://10.1.1.1/", BoardID: "b1"}); err != nil {
		t.Fatalf("admin board webhook to a private address: %v", err)
	}
}

func TestBoardWebhookRedirectsAndScoping(t *testing.T) {
	w := hookSeed(t)
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits = append(hits, r.URL.Path+" "+r.Header.Get("X-Weave-Event")+" "+string(b))
		mu.Unlock()
		switch r.URL.Path {
		case "/to-private":
			http.Redirect(rw, r, "http://evil.test/stolen", http.StatusFound)
		case "/to-loopback":
			http.Redirect(rw, r, "http://127.0.0.1:"+strings.Split(r.Host, ":")[1]+"/stolen", http.StatusFound)
		default:
			rw.WriteHeader(200)
			_, _ = rw.Write([]byte(strings.Repeat("R", 4000)))
		}
	}))
	defer srv.Close()
	n := fakeNet(t, srv)
	port := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[1]
	mk := func(env *ops.Env, path string, events ...string) notify.WebhookOut {
		t.Helper()
		in := notify.WebhookCreateIn{Name: path, URL: "http://good.test:" + port + path, BoardID: "b1"}
		if len(events) > 0 {
			l := notify.StringList(events)
			in.Events = &l
		}
		o, err := ops.Typed[notify.WebhookOut](env, "webhook.create", in)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	// redirects to blocked targets are not followed
	for _, p := range []string{"/to-private", "/to-loopback"} {
		h := mk(w.olga, p)
		d, err := ops.Typed[notify.DeliveryOut](w.olga, "webhook.test", notify.IDIn{ID: h.ID})
		if err != nil || d.State == "delivered" {
			t.Fatalf("%s: %+v %v", p, d, err)
		}
	}
	mu.Lock()
	for _, h := range hits {
		if strings.HasPrefix(h, "/stolen") {
			t.Fatal("a redirect to a blocked destination was followed")
		}
	}
	hits = nil
	mu.Unlock()
	if n.dialedAny("10.") || n.dialedAny("127.") {
		t.Fatal("the dialer connected to a blocked address")
	}

	// a delivered ping: the owner gets a short snippet, the admin the stored 2 KiB
	good := mk(w.olga, "/board")
	d, err := ops.Typed[notify.DeliveryOut](w.olga, "webhook.test", notify.IDIn{ID: good.ID})
	if err != nil || d.State != "delivered" || len(d.ResponseBody) > 260 {
		t.Fatalf("owner ping: state=%s len=%d %v", d.State, len(d.ResponseBody), err)
	}
	da, _ := ops.Typed[notify.DeliveryOut](w.admin, "webhook.test", notify.IDIn{ID: good.ID})
	if len(da.ResponseBody) != 2048 {
		t.Fatalf("admin ping body %d", len(da.ResponseBody))
	}
	dl, err := ops.Typed[notify.DeliveriesOut](w.olga, "webhook.deliveries", notify.DeliveriesIn{ID: good.ID})
	if err != nil || len(dl.Items) == 0 || len(dl.Items[0].ResponseBody) > 520 {
		t.Fatalf("owner deliveries: %v", err)
	}
	if _, err := ops.Call(w.olga, "webhook.retry", &notify.IDIn{ID: dl.Items[0].ID}); err != nil {
		t.Fatalf("owner retry: %v", err)
	}
	if _, err := ops.Call(w.alice, "webhook.retry", &notify.IDIn{ID: dl.Items[0].ID}); status(err) != 404 {
		t.Fatalf("editor retry: %v", err)
	}
	notify.Wait()
	mu.Lock()
	hits = nil
	mu.Unlock()

	// only this board's events, and never another user's notification or account events
	hub := w.admin.Hub
	ch, cancel := hub.Subscribe(func(room string) bool { return room == "board:b1" || room == "global" })
	defer cancel()
	ops.Emit(w.alice, ops.Event{Type: "cardCreate", BoardID: "b1", CardID: "c1", Data: map[string]any{"item": map[string]any{"id": "c1", "name": "Fuel the rocket"}}})
	ops.Emit(w.alice, ops.Event{Type: "cardCreate", BoardID: "b2", CardID: "c2", Data: map[string]any{"item": map[string]any{"id": "c2", "name": "Classified plan"}}})
	notify.Record(w.alice, ops.Event{Type: "notificationCreate", Name: "notification.created", Room: "user:bob",
		Data: map[string]any{"item": map[string]any{"id": "n1", "userId": "bob", "boardId": "b1", "cardId": "c1", "summary": "for bob only"}}})
	notify.Record(w.admin, ops.Event{Type: "userCreate", Name: "user.created", Room: "global", Data: map[string]any{"item": map[string]any{"id": "zed", "boardId": "b1"}}})
	notify.Record(w.admin, ops.Event{Type: "projectUpdate", Name: "project.updated", Room: "global", Data: map[string]any{"item": map[string]any{"id": "p1"}, "boardId": "b1"}})
	notify.Wait()
	mu.Lock()
	var got []string
	for _, h := range hits {
		if strings.HasPrefix(h, "/board ") {
			got = append(got, strings.SplitN(h, " ", 3)[1])
		}
		if strings.Contains(h, "Classified") || strings.Contains(h, "for bob only") {
			t.Fatalf("board webhook received another board's or user's data: %s", h)
		}
	}
	mu.Unlock()
	if len(got) != 1 || got[0] != "cardCreate" {
		t.Fatalf("board webhook received %v, want exactly the b1 cardCreate", got)
	}
	// settings changes never carry the URL or the secrets
	if _, err := ops.Call(w.olga, "webhook.update", &notify.WebhookUpdateIn{ID: good.ID, Secret: sp("TOPSECRET"), AccessToken: sp("TOKSECRET")}); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(time.Second)
	for done := false; !done; {
		select {
		case e := <-ch:
			b, _ := json.Marshal(e.Data)
			if strings.Contains(string(b), "TOPSECRET") || strings.Contains(string(b), "TOKSECRET") || strings.Contains(string(b), "good.test") {
				t.Fatalf("event %s leaked webhook secrets or URL: %s", e.Name, b)
			}
			if e.Name == "webhook.updated" && e.Room != "board:b1" {
				t.Fatalf("board webhook change went to %s", e.Room)
			}
		case <-timeout:
			done = true
		}
	}
	var leaks int
	_ = w.admin.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE data LIKE '%TOPSECRET%' OR data LIKE '%TOKSECRET%'`).Scan(&leaks)
	if leaks != 0 {
		t.Fatal("webhook secret in the activity log")
	}
}

func TestBoardWebhookCap(t *testing.T) {
	w := hookSeed(t)
	fakeNet(t, nil)
	t.Setenv("WEAVE_WEBHOOKS_PER_BOARD", "1")
	if _, err := ops.Call(w.olga, "webhook.create", &notify.WebhookCreateIn{Name: "1", URL: "https://good.test/1", BoardID: "b1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Call(w.olga, "webhook.create", &notify.WebhookCreateIn{Name: "2", URL: "https://good.test/2", BoardID: "b1"}); status(err) != 422 {
		t.Fatalf("cap: %v", err)
	}
	t.Setenv("WEAVE_WEBHOOKS_PER_BOARD", "0")
	if _, err := ops.Call(w.olga, "webhook.create", &notify.WebhookCreateIn{Name: "2", URL: "https://good.test/2", BoardID: "b1"}); err != nil {
		t.Fatalf("cap off: %v", err)
	}
}
