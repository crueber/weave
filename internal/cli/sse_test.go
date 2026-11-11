package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadSSEParsing(t *testing.T) {
	in := ": hello\r\nid: 7\r\nevent: card.moved\r\ndata: {\"a\":1,\r\ndata: \"b\":2}\r\n\r\n: ping\n\nevent: ready\ndata: {}\n\n"
	var frames []sseFrame
	acts := 0
	err := readSSE(strings.NewReader(in), func() { acts++ }, func(f sseFrame) bool { frames = append(frames, f); return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 4 || !frames[0].Comment || frames[1].ID != "7" || frames[1].Event != "card.moved" || frames[1].Data != "{\"a\":1,\n\"b\":2}" || !frames[2].Comment || frames[3].Event != "ready" {
		t.Fatalf("frames %+v", frames)
	}
	if acts == 0 {
		t.Fatal("activity callback not called")
	}
}

// envelope builds the data payload of an event.
func envelope(id, typ string, extra map[string]any) string {
	m := map[string]any{"id": id, "type": typ, "boardId": "b1", "listId": "l1", "toListId": "l1", "cardId": "c-" + id,
		"actor": map[string]any{"id": "u9", "name": "Human"},
		"card":  map[string]any{"id": "c-" + id, "name": "Card " + id, "listId": "l1"}}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

type sseConn struct {
	n      int
	since  string
	lastID string
	query  string
	w      http.ResponseWriter
	r      *http.Request
}

func (c *sseConn) send(id, typ, data string) {
	if id != "" {
		fmt.Fprintf(c.w, "id: %s\n", id)
	}
	if typ != "" {
		fmt.Fprintf(c.w, "event: %s\n", typ)
	}
	fmt.Fprintf(c.w, "data: %s\n\n", data)
	c.w.(http.Flusher).Flush()
}
func (c *sseConn) ready() { c.send("", "ready", `{"serverTime":"now","resumeFrom":""}`) }
func (c *sseConn) ping()  { fmt.Fprint(c.w, ": ping\n\n"); c.w.(http.Flusher).Flush() }
func (c *sseConn) hold()  { <-c.r.Context().Done() }
func (c *sseConn) event(id, typ string, extra map[string]any) {
	c.send(id, typ, envelope(id, typ, extra))
}

// sse installs a scripted /events endpoint; script runs once per connection.
func (f *fake) sse(script func(c *sseConn)) *[]sseConn {
	var mu sync.Mutex
	var conns []sseConn
	f.handleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		c := sseConn{n: len(conns) + 1, since: r.URL.Query().Get("since"), lastID: r.Header.Get("Last-Event-ID"), query: r.URL.RawQuery, w: w, r: r}
		conns = append(conns, c)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		script(&c)
	})
	return &conns
}

func newTestListener(f *fake, ctxApp *App) *Listener {
	return &Listener{App: ctxApp, Heartbeat: 50 * time.Millisecond, BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond}
}

func testApp(f *fake) *App {
	a := &App{Cfg: Resolved{URL: f.srv.URL, Token: "wk_test", Profile: "default"}}
	return a
}

func collect(t *testing.T, l *Listener, want int, timeout time.Duration) []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct{})
	l.Handle = func(_ context.Context, ev Event, fin func()) {
		mu.Lock()
		got = append(got, ev.Type+":"+ev.ID)
		n := len(got)
		mu.Unlock()
		fin()
		if n >= want {
			cancel()
		}
	}
	go func() { _ = l.Run(ctx); close(done) }()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return append([]string{}, got...)
}

func TestListenerReconnectResumeDedupe(t *testing.T) {
	f := newFake(t)
	conns := f.sse(func(c *sseConn) {
		switch c.n {
		case 1:
			c.ready()
			c.event("e1", "card.created", nil)
			c.event("e2", "card.moved", nil)
			// drop the connection
		case 2:
			c.ready()
			c.event("e2", "card.moved", nil) // replayed: must be deduped
			c.event("e3", "card.moved", nil)
			c.hold()
		}
	})
	l := newTestListener(f, testApp(f))
	l.Boards, l.Lists, l.Types = []string{"b1"}, []string{"l1", "l2"}, []string{"card.moved", "card.created"}
	l.AgentLabel = "tester"
	got := collect(t, l, 3, 5*time.Second)
	if !reflect.DeepEqual(got, []string{"card.created:e1", "card.moved:e2", "card.moved:e3"}) {
		t.Fatalf("got %v", got)
	}
	cs := *conns
	if len(cs) < 2 {
		t.Fatalf("expected a reconnect, got %d connections", len(cs))
	}
	if cs[1].since != "e2" || cs[1].lastID != "e2" {
		t.Fatalf("resume: since=%q Last-Event-ID=%q", cs[1].since, cs[1].lastID)
	}
	q := cs[0].query
	for _, want := range []string{"boards=b1", "lists=l1%2Cl2", "types=card.moved%2Ccard.created", "agent=tester", "heartbeat="} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %s", q, want)
		}
	}
	if cs[0].since != "" {
		t.Errorf("first connection must not send since: %q", cs[0].since)
	}
}

func TestListenerWatchdogReconnectsOnSilence(t *testing.T) {
	f := newFake(t)
	f.sse(func(c *sseConn) {
		if c.n == 1 {
			c.ready()
			c.hold() // silent: no heartbeats
			return
		}
		c.ready()
		c.event("e1", "card.created", nil)
		c.hold()
	})
	l := newTestListener(f, testApp(f))
	l.Heartbeat = 40 * time.Millisecond
	got := collect(t, l, 1, 5*time.Second)
	if len(got) != 1 || l.Connects.Load() < 2 {
		t.Fatalf("got %v connects %d", got, l.Connects.Load())
	}
}

func TestListenerHeartbeatsKeepConnectionAlive(t *testing.T) {
	f := newFake(t)
	f.sse(func(c *sseConn) {
		c.ready()
		for i := 0; i < 8; i++ {
			time.Sleep(30 * time.Millisecond)
			c.ping()
		}
		c.event("e1", "card.created", nil)
		c.hold()
	})
	l := newTestListener(f, testApp(f))
	l.Heartbeat = 50 * time.Millisecond // watchdog limit 150ms > ping interval 30ms
	got := collect(t, l, 1, 5*time.Second)
	if len(got) != 1 || l.Connects.Load() != 1 {
		t.Fatalf("got %v connects %d (should not reconnect while pings arrive)", got, l.Connects.Load())
	}
}

func TestListenerFatalOnUnauthorized(t *testing.T) {
	f := newFake(t)
	n := 0
	f.handleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"code":"unauthorized","message":"bad key"}}`))
	})
	l := newTestListener(f, testApp(f))
	l.Handle = func(context.Context, Event, func()) {}
	err := l.Run(context.Background())
	ae, ok := err.(*APIError)
	if !ok || ae.Code != "unauthorized" || n != 1 {
		t.Fatalf("err %v after %d attempts", err, n)
	}
}

func TestListenerRetriesServerErrors(t *testing.T) {
	f := newFake(t)
	var n int
	f.handleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		c := &sseConn{w: w, r: r}
		c.ready()
		c.event("e1", "card.created", nil)
		c.hold()
	})
	l := newTestListener(f, testApp(f))
	got := collect(t, l, 1, 5*time.Second)
	if len(got) != 1 || n != 3 {
		t.Fatalf("got %v after %d attempts", got, n)
	}
}

func TestCursorContiguousAdvanceAndAtomicFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "x.cursor")
	c := &cursor{path: path}
	a, b, cc := c.begin("a"), c.begin("b"), c.begin("c")
	read := func() string { b, _ := os.ReadFile(path); return strings.TrimSpace(string(b)) }
	c.finish(cc)
	if read() != "" {
		t.Fatalf("c finished first: cursor must not advance, got %q", read())
	}
	c.finish(a)
	if read() != "a" {
		t.Fatalf("got %q want a", read())
	}
	c.finish(b)
	if read() != "c" {
		t.Fatalf("got %q want c", read())
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if loadCursor(path) != "c" || loadCursor(filepath.Join(dir, "none")) != "" {
		t.Fatal("loadCursor")
	}
}

func TestListenerPersistsAndResumesFromCursorFile(t *testing.T) {
	f := newFake(t)
	cf := filepath.Join(t.TempDir(), "c.cursor")
	conns := f.sse(func(c *sseConn) {
		c.ready()
		if c.n == 1 {
			c.event("e1", "card.created", nil)
			c.event("e2", "card.created", nil)
		} else {
			c.event("e3", "card.created", nil)
		}
		c.hold()
	})
	l := newTestListener(f, testApp(f))
	l.CursorFile = cf
	if got := collect(t, l, 2, 5*time.Second); len(got) != 2 {
		t.Fatalf("first run %v", got)
	}
	if loadCursor(cf) != "e2" {
		t.Fatalf("cursor %q", loadCursor(cf))
	}
	// "restart": a new listener resumes after e2
	l2 := newTestListener(f, testApp(f))
	l2.CursorFile = cf
	got := collect(t, l2, 1, 5*time.Second)
	cs := *conns
	if cs[len(cs)-1].since != "e2" || !reflect.DeepEqual(got, []string{"card.created:e3"}) || !l2.Resumed {
		t.Fatalf("since=%q got=%v", cs[len(cs)-1].since, got)
	}
}

func TestListenerIncludeExistingBeforeLiveAndSkippedOnResume(t *testing.T) {
	f := newFake(t)
	f.sse(func(c *sseConn) {
		c.ready()
		c.event("e1", "card.created", nil)
		c.hold()
	})
	existing := func(context.Context) ([]Event, error) {
		return []Event{{Type: "card.existing", M: map[string]any{"cardId": "x1"}, Raw: []byte(`{"type":"card.existing","cardId":"x1"}`)},
			{Type: "card.existing", M: map[string]any{"cardId": "x2"}, Raw: []byte(`{"type":"card.existing","cardId":"x2"}`)}}, nil
	}
	l := newTestListener(f, testApp(f))
	l.Existing = existing
	got := collect(t, l, 3, 5*time.Second)
	if !reflect.DeepEqual(got, []string{"card.existing:", "card.existing:", "card.created:e1"}) {
		t.Fatalf("order %v", got)
	}
	// with a saved cursor the backlog is not replayed
	cf := filepath.Join(t.TempDir(), "c")
	os.WriteFile(cf, []byte("e0\n"), 0o600)
	l2 := newTestListener(f, testApp(f))
	l2.Existing, l2.CursorFile = existing, cf
	got = collect(t, l2, 1, 5*time.Second)
	if !reflect.DeepEqual(got, []string{"card.created:e1"}) {
		t.Fatalf("resume must skip existing: %v", got)
	}
}

// ---- CLI level ----

func cliSSE(f *fake, events ...func(c *sseConn)) {
	f.sse(func(c *sseConn) {
		c.ready()
		for _, e := range events {
			e(c)
		}
		c.hold()
	})
}

func ev(id, typ string, extra map[string]any) func(c *sseConn) {
	return func(c *sseConn) { c.event(id, typ, extra) }
}

func TestListenOnceFiltersSelfAndPrintsNDJSON(t *testing.T) {
	f := newFake(t)
	self := map[string]any{"actor": map[string]any{"id": "u1", "name": "Agent Smith", "isAgent": true}}
	cliSSE(f, ev("e1", "card.moved", self), ev("e2", "listener.joined", nil), ev("e3", "card.moved", nil), ev("e4", "card.moved", nil))
	code, out, errs := f.run("listen", "--board", "Product", "--list", "Ready", "--list", "Review,Needs info", "--once", "--no-cursor")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one event (own and ephemeral skipped, --once), got %q", out)
	}
	var e map[string]any
	if json.Unmarshal([]byte(lines[0]), &e) != nil || e["id"] != "e3" {
		t.Fatalf("line %q", lines[0])
	}
}

func TestListenSendsResolvedIDsInOneConnection(t *testing.T) {
	f := newFake(t)
	conns := f.sse(func(c *sseConn) { c.ready(); c.event("e1", "card.created", nil); c.hold() })
	code, _, errs := f.run("listen", "--board", "Product", "--list", "Ready", "--list", "Review", "--list", "Needs info", "--type", "card.created", "--once", "--no-cursor", "--agent-label", "bot")
	if code != 0 {
		t.Fatal(errs)
	}
	cs := *conns
	if len(cs) != 1 || !strings.Contains(cs[0].query, "lists=l1%2Cl3%2Cl4") || !strings.Contains(cs[0].query, "boards=b1") || !strings.Contains(cs[0].query, "agent=bot") {
		t.Fatalf("%d conns, query %q", len(cs), cs[0].query)
	}
}

func TestListenTimeoutExit10AndOnceDelivery(t *testing.T) {
	f := newFake(t)
	cliSSE(f) // only ready
	start := time.Now()
	code, out, _ := f.run("listen", "--board", "Product", "--list", "Ready", "--timeout", "300ms", "--no-cursor")
	if code != ExitEmpty || out != "" {
		t.Fatalf("code %d out %q", code, out)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout did not stop the listener")
	}
	// with an event before the timeout: exit 0
	f2 := newFake(t)
	cliSSE(f2, ev("e1", "card.created", nil))
	code, out, _ = f2.run("listen", "--board", "Product", "--list", "Ready", "--timeout", "300ms", "--no-cursor")
	if code != 0 || !strings.Contains(out, "e1") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestListenExecEnvStdinAndFailureDoesNotStop(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs sh")
	}
	f := newFake(t)
	cliSSE(f, ev("e1", "card.moved", map[string]any{"fromListId": "l2"}), ev("e2", "card.created", nil))
	dir := t.TempDir()
	out := filepath.Join(dir, "log")
	script := fmt.Sprintf(`echo "$WEAVE_EVENT_TYPE|$WEAVE_CARD_ID|$WEAVE_LIST_ID|$WEAVE_BOARD_ID|$WEAVE_FROM_LIST_ID|$WEAVE_EVENT_ID" >> %s; cat >> %s; exit 3`, out, out)
	code, stdout, errs := f.run("listen", "--board", "Product", "--list", "Ready", "--exec", script, "--timeout", "700ms", "--no-cursor")
	if code != 0 {
		t.Fatalf("exec failures must not stop the listener: %d %s", code, errs)
	}
	if stdout != "" {
		t.Fatalf("with --exec envelopes are not printed: %q", stdout)
	}
	b, _ := os.ReadFile(out)
	log := string(b)
	for _, want := range []string{"card.moved|c-e1|l1|b1|l2|e1", "card.created|c-e2|l1|b1||e2", `"id":"e1"`, `"id":"e2"`} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Count(errs, "exec failed") != 2 {
		t.Errorf("expected two logged failures: %s", errs)
	}
}

func TestListenOnErrorComment(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs sh")
	}
	f := newFake(t)
	f.json("POST /api/v1/cards/{cardId}/comments", map[string]any{"item": map[string]any{"id": "m1"}})
	f.json("POST /api/v1/cards/{id}/release", map[string]any{"item": map[string]any{}})
	cliSSE(f, ev("e1", "card.created", nil))
	code, _, errs := f.run("listen", "--board", "Product", "--list", "Ready", "--exec", "echo boom >&2; exit 2", "--on-error", "comment,release", "--once", "--no-cursor")
	if code != 0 {
		t.Fatal(errs)
	}
	if !strings.Contains(f.body("POST /api/v1/cards/c-e1/comments"), "boom") || f.called("POST /api/v1/cards/c-e1/release") != 1 {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestListenClaimSkipsConflictAndMoves(t *testing.T) {
	f := newFake(t)
	f.handleFunc("POST /api/v1/cards/{id}/claim", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("id") == "c-e1" {
			w.WriteHeader(409)
			w.Write([]byte(`{"error":{"code":"conflict","message":"taken"}}`))
			return
		}
		w.Write([]byte(`{"item":{"cardId":"` + r.PathValue("id") + `","claimedBy":"u1","claimedUntil":"later"}}`))
	})
	f.json("POST /api/v1/cards/{id}/move", map[string]any{"item": map[string]any{}})
	cliSSE(f, ev("e1", "card.created", nil), ev("e2", "card.created", nil))
	code, out, errs := f.run("listen", "--board", "Product", "--list", "Ready", "--claim", "--claim-move-to", "In progress", "--ttl", "5m", "--once", "--no-cursor")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if !strings.Contains(out, `"id":"e2"`) || strings.Contains(out, `"id":"e1"`) || !strings.Contains(out, `"claim"`) {
		t.Fatalf("out %q", out)
	}
	if !strings.Contains(f.body("POST /api/v1/cards/c-e2/claim"), `"ttlSeconds":300`) || !strings.Contains(f.body("POST /api/v1/cards/c-e2/claim"), `"listId":"l1"`) {
		t.Fatalf("claim body %s", f.body("POST /api/v1/cards/c-e2/claim"))
	}
	if !strings.Contains(f.body("POST /api/v1/cards/c-e2/move"), `"listId":"l2"`) {
		t.Fatalf("move body %s", f.body("POST /api/v1/cards/c-e2/move"))
	}
}

func TestListenIncludeExistingCLI(t *testing.T) {
	f := newFake(t)
	f.json("GET /api/v1/boards/{boardId}/cards", map[string]any{"items": []any{
		map[string]any{"id": "k1", "name": "Old 1", "listId": "l1"}, map[string]any{"id": "k2", "name": "Old 2", "listId": "l1"}}})
	cliSSE(f, ev("e1", "card.created", nil))
	code, out, errs := f.run("listen", "--board", "Product", "--list", "Ready", "--include-existing", "--timeout", "500ms", "--no-cursor")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"card.existing"`) || !strings.Contains(lines[1], `"cardId":"k2"`) && !strings.Contains(lines[1], `"cardId":"k1"`) || !strings.Contains(lines[2], `"id":"e1"`) {
		t.Fatalf("lines %q", lines)
	}
	if f.called("listId=l1") == 0 {
		t.Fatalf("existing cards should be queried per list: %v", f.calls)
	}
}

func TestListenDefaultCursorFileUnderConfigDir(t *testing.T) {
	f := newFake(t)
	cliSSE(f, ev("e1", "card.created", nil))
	code, _, errs := f.run("listen", "--board", "Product", "--list", "Ready", "--once")
	if code != 0 {
		t.Fatal(errs)
	}
	m, _ := filepath.Glob(filepath.Join(os.Getenv("WEAVE_CONFIG_DIR"), "cursors", "*.cursor"))
	if len(m) != 1 || loadCursor(m[0]) != "e1" {
		t.Fatalf("cursor files %v", m)
	}
}

func TestListenNeedsTarget(t *testing.T) {
	f := newFake(t)
	if code, _, _ := f.run("listen"); code != ExitUsage {
		t.Fatalf("code %d", code)
	}
	if code, _, errs := f.run("listen", "--list", "Ready"); code != ExitUsage || !strings.Contains(errs, "--board") {
		t.Fatalf("code %d %s", code, errs)
	}
}

func TestListenerLegacyRoomsQuery(t *testing.T) {
	f := newFake(t)
	conns := f.sse(func(c *sseConn) {
		c.send("", "card.updated", `{"name":"card.updated"}`)
		c.hold()
	})
	l := newTestListener(f, testApp(f))
	l.Rooms = "board:b1"
	got := collect(t, l, 1, 5*time.Second)
	cs := *conns
	if len(got) != 1 || !strings.Contains(cs[0].query, "rooms=board%3Ab1") || strings.Contains(cs[0].query, "heartbeat") {
		t.Fatalf("got %v query %q", got, cs[0].query)
	}
}

func TestNextExitCodes(t *testing.T) {
	f := newFake(t)
	f.json("POST /api/v1/lists/{listId}/next", map[string]any{"item": nil})
	code, out, _ := f.run("next", "--board", "Product", "--list", "Ready")
	if code != ExitEmpty || out != "" {
		t.Fatalf("empty: %d %q", code, out)
	}
	f.handleFunc("POST /api/v1/lists/{listId}/next", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	if code, _, _ = f.run("next", "--board", "Product", "--list", "Ready"); code != ExitEmpty {
		t.Fatalf("204: %d", code)
	}
	f.json("POST /api/v1/lists/{listId}/next", map[string]any{"item": map[string]any{"id": "c9", "name": "Do it"}})
	code, out, _ = f.run("next", "--board", "Product", "--list", "Ready", "--claim", "--move-to", "In progress", "--ttl", "15m")
	if code != 0 || !strings.Contains(out, `"c9"`) {
		t.Fatalf("card: %d %q", code, out)
	}
	last := f.calls[len(f.calls)-1]
	if !strings.HasPrefix(last, "POST /api/v1/lists/l1/next?") || strings.Contains(last, "claim=") {
		t.Errorf("card.next must be a POST with no query parameters: %q", last)
	}
	bodies := f.posts["POST /api/v1/lists/l1/next"]
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &sent); err != nil || sent["claim"] != true || sent["ttlSeconds"] != float64(900) || sent["moveTo"] != "l2" {
		t.Errorf("JSON body %v (%v)", bodies, err)
	}
	// first non-empty list wins
	f.handleFunc("POST /api/v1/lists/{listId}/next", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("listId") == "l1" {
			w.Write([]byte(`{"item":null}`))
			return
		}
		w.Write([]byte(`{"item":{"id":"c-from-review"}}`))
	})
	code, out, _ = f.run("next", "--board", "Product", "--list", "Ready", "--list", "Review")
	if code != 0 || !strings.Contains(out, "c-from-review") {
		t.Fatalf("%d %q", code, out)
	}
	f.status("POST /api/v1/lists/{listId}/next", 403, "forbidden", "no")
	if code, _, _ = f.run("next", "--board", "Product", "--list", "Ready"); code != ExitForbidden {
		t.Fatalf("errors are not empty: %d", code)
	}
}

func TestAgentRunSuccessFailureAndDecline(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs sh")
	}
	for _, tc := range []struct {
		name                       string
		script                     string
		wantMove, wantCmt, wantRel bool
	}{
		{"success", "cat >/dev/null; echo result-text; exit 0", true, true, true},
		{"decline", "exit 11", false, false, true},
		{"failure", "echo oops >&2; exit 3", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.json("POST /api/v1/cards/{id}/claim", map[string]any{"item": map[string]any{"claimedBy": "u1"}})
			f.json("POST /api/v1/cards/{id}/release", map[string]any{"item": map[string]any{}})
			f.json("POST /api/v1/cards/{id}/move", map[string]any{"item": map[string]any{}})
			f.json("POST /api/v1/cards/{cardId}/comments", map[string]any{"item": map[string]any{"id": "m"}})
			cliSSE(f, ev("e1", "card.created", nil))
			code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--claim-move-to", "In progress", "--done-list", "Review",
				"--comment-output", "--once", "--no-cursor", "--", "sh", "-c", tc.script)
			if code != 0 {
				t.Fatalf("%d %s", code, errs)
			}
			moves := f.body("POST /api/v1/cards/c-e1/move")
			gotMoveDone := strings.Contains(moves, `"listId":"l3"`)
			if gotMoveDone != tc.wantMove {
				t.Errorf("done move: got %v want %v (%v)", gotMoveDone, tc.wantMove, f.calls)
			}
			if (f.called("POST /api/v1/cards/c-e1/comments") > 0) != tc.wantCmt {
				t.Errorf("comment: want %v: %v", tc.wantCmt, f.calls)
			}
			if (f.called("POST /api/v1/cards/c-e1/release") > 0) != tc.wantRel {
				t.Errorf("release: want %v: %v", tc.wantRel, f.calls)
			}
			if f.called("POST /api/v1/cards/c-e1/claim") != 1 || !strings.Contains(f.body("POST /api/v1/cards/c-e1/move"), "l2") && tc.name == "x" {
				t.Errorf("claim once: %v", f.calls)
			}
		})
	}
}

func TestAgentRunNeedsCommandAndList(t *testing.T) {
	f := newFake(t)
	if code, _, _ := f.run("agent", "run", "--board", "Product", "--listen", "Ready"); code != ExitUsage {
		t.Fatalf("no command: %d", code)
	}
	if code, _, _ := f.run("agent", "run", "--board", "Product", "--", "true"); code != ExitUsage {
		t.Fatalf("no list: %d", code)
	}
}
