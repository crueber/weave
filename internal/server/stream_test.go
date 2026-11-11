package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/crueber/weave/internal/all"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/auth"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/realtime"
	"github.com/crueber/weave/internal/server"
)

type env struct {
	t     *testing.T
	ts    *httptest.Server
	d     *db.DB
	srv   *server.Server
	admin string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: t.TempDir(), BaseURL: "http://weave.test", SecretKey: "test-secret-key-test-secret-key-12"}
	s := &server.Server{Cfg: cfg, DB: d, Hub: realtime.New(), Svc: map[string]any{"config": cfg}}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(func() { ts.Close(); notify.Wait(); d.Close() })
	k, err := auth.MintKey(context.Background(), d, "root@x.io", "admin")
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ts: ts, d: d, srv: s, admin: k.Key}
}

// api calls the REST API and returns the decoded "item".
func (e *env) api(key, method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, e.ts.URL+"/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (e *env) must(key, method, path string, body any) map[string]any {
	e.t.Helper()
	code, out := e.api(key, method, path, body)
	if code != 200 {
		e.t.Fatalf("%s %s: %d %v", method, path, code, out)
	}
	item, _ := out["item"].(map[string]any)
	if item == nil {
		return map[string]any{"_": out["item"]}
	}
	return item
}

func id(m map[string]any) string { s, _ := m["id"].(string); return s }

type board struct {
	proj, id    string
	a, b, c     string
	cards       []string
	archiveList string
}

func (e *env) newBoard(name string) board {
	p := e.must(e.admin, "POST", "/projects", map[string]any{"name": name})
	b := e.must(e.admin, "POST", "/projects/"+id(p)+"/boards", map[string]any{"name": name})
	bd := board{proj: id(p), id: id(b)}
	bd.a = id(e.must(e.admin, "POST", "/boards/"+bd.id+"/lists", map[string]any{"name": "A"}))
	bd.b = id(e.must(e.admin, "POST", "/boards/"+bd.id+"/lists", map[string]any{"name": "B"}))
	bd.c = id(e.must(e.admin, "POST", "/boards/"+bd.id+"/lists", map[string]any{"name": "C"}))
	return bd
}

func (e *env) newUser(email, role string) string {
	k, err := auth.MintKey(context.Background(), e.d, email, role)
	if err != nil {
		e.t.Fatal(err)
	}
	return k.Key
}

func (e *env) card(listID, name string) string {
	return id(e.must(e.admin, "POST", "/lists/"+listID+"/cards", map[string]any{"name": name}))
}

// ---- SSE client

type frame struct {
	ID, Event string
	Data      string
}

type stream struct {
	t      *testing.T
	frames chan frame
	cancel context.CancelFunc
	status int
}

func (e *env) open(key, query string, hdr map[string]string) *stream {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/events?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	s := &stream{t: e.t, frames: make(chan frame, 1024), cancel: cancel, status: res.StatusCode}
	if res.StatusCode != 200 {
		res.Body.Close()
		cancel()
		return s
	}
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var f frame
		var any bool
		for sc.Scan() {
			l := sc.Text()
			switch {
			case l == "":
				if any {
					s.frames <- f
				}
				f, any = frame{}, false
			case strings.HasPrefix(l, ": "):
				s.frames <- frame{Event: "comment", Data: l[2:]}
			case strings.HasPrefix(l, "id: "):
				f.ID, any = l[4:], true
			case strings.HasPrefix(l, "event: "):
				f.Event, any = l[7:], true
			case strings.HasPrefix(l, "data: "):
				f.Data, any = l[6:], true
			}
		}
		close(s.frames)
	}()
	e.t.Cleanup(cancel)
	return s
}

func (s *stream) next(d time.Duration) (frame, bool) {
	select {
	case f, ok := <-s.frames:
		return f, ok
	case <-time.After(d):
		return frame{}, false
	}
}

// events returns the next n non-comment frames, skipping ready.
func (s *stream) events(n int) []map[string]any {
	s.t.Helper()
	var out []map[string]any
	for len(out) < n {
		f, ok := s.next(5 * time.Second)
		if !ok {
			s.t.Fatalf("timed out after %d/%d events", len(out), n)
		}
		if f.Event == "ready" || f.Event == "comment" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(f.Data), &m); err != nil {
			s.t.Fatalf("bad data %q", f.Data)
		}
		if m["id"] != f.ID || m["type"] != f.Event {
			s.t.Fatalf("frame/envelope mismatch: %+v vs %v", f, m)
		}
		out = append(out, m)
	}
	return out
}

func (s *stream) expectQuiet() {
	s.t.Helper()
	for {
		f, ok := s.next(300 * time.Millisecond)
		if !ok {
			return
		}
		if f.Event == "ready" || f.Event == "comment" {
			continue
		}
		s.t.Fatalf("unexpected event %s %s", f.Event, f.Data)
	}
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

// ---- tests

func TestStreamFiltersByListsAndTypes(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Filters")
	s := e.open(e.admin, "lists="+bd.a+","+bd.b+"&agent=tester", nil)
	f, ok := s.next(3 * time.Second)
	if !ok || f.Event != "ready" {
		t.Fatalf("first frame must be ready, got %+v", f)
	}
	var ready map[string]any
	_ = json.Unmarshal([]byte(f.Data), &ready)
	if ready["serverTime"] == nil || ready["resumeFrom"] == nil {
		t.Fatalf("ready payload %v", ready)
	}

	c1 := e.card(bd.a, "first")                                                   // arrives in A
	e.must(e.admin, "POST", "/cards/"+c1+"/move", map[string]any{"listId": bd.c}) // leaves A
	c2 := e.card(bd.c, "other")                                                   // C only: not visible
	e.must(e.admin, "POST", "/cards/"+c2+"/move", map[string]any{"listId": bd.b}) // arrives in B
	e.must(e.admin, "POST", "/cards/"+c1+"/move", map[string]any{"listId": bd.b}) // C -> B

	evs := s.events(4)
	want := []struct{ typ, list, from, to, card string }{
		{"card.created", bd.a, "", "", c1},
		{"card.moved", bd.c, bd.a, bd.c, c1},
		{"card.moved", bd.b, bd.c, bd.b, c2},
		{"card.moved", bd.b, bd.c, bd.b, c1},
	}
	var last string
	for i, w := range want {
		ev := evs[i]
		if str(ev, "type") != w.typ || str(ev, "listId") != w.list || str(ev, "fromListId") != w.from || str(ev, "toListId") != w.to || str(ev, "cardId") != w.card {
			t.Fatalf("event %d = %v, want %+v", i, ev, w)
		}
		if str(ev, "boardId") != bd.id || str(ev, "at") == "" {
			t.Fatalf("event %d envelope %v", i, ev)
		}
		act := ev["actor"].(map[string]any)
		if act["name"] == "" || act["isAgent"] != false {
			t.Fatalf("actor %v", act)
		}
		card := ev["card"].(map[string]any)
		if str(card, "id") != w.card || card["labels"] == nil || card["members"] == nil {
			t.Fatalf("card snapshot %v", card)
		}
		if str(ev, "id") <= last {
			t.Fatalf("ids must increase: %s after %s", str(ev, "id"), last)
		}
		last = str(ev, "id")
	}
	s.expectQuiet()

	// types filter
	st := e.open(e.admin, "lists="+bd.b+"&types=card.moved", nil)
	e.card(bd.b, "ignored created")
	e.must(e.admin, "POST", "/cards/"+c1+"/move", map[string]any{"listId": bd.a})
	ev := st.events(1)[0]
	if str(ev, "type") != "card.moved" || str(ev, "fromListId") != bd.b {
		t.Fatalf("types filter got %v", ev)
	}
}

func TestStreamReplaySinceNoGapNoDup(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Replay")
	live := e.open(e.admin, "boards="+bd.id, nil)
	var ids []string
	for i := 0; i < 5; i++ {
		e.card(bd.a, fmt.Sprintf("c%d", i))
	}
	first := live.events(5)
	for _, ev := range first {
		ids = append(ids, str(ev, "id"))
	}

	// reconnect from the third event: expect events 4 and 5 replayed, then live ones, exactly once
	r := e.open(e.admin, "boards="+bd.id+"&since="+ids[2], nil)
	more := make(chan struct{})
	go func() {
		<-more
		for i := 0; i < 3; i++ {
			e.card(bd.a, fmt.Sprintf("live%d", i))
		}
	}()
	close(more)
	got := r.events(5)
	if str(got[0], "id") != ids[3] || str(got[1], "id") != ids[4] {
		t.Fatalf("replay ids %v %v, want %v %v", str(got[0], "id"), str(got[1], "id"), ids[3], ids[4])
	}
	seen := map[string]bool{}
	prev := ""
	for _, ev := range got {
		i := str(ev, "id")
		if seen[i] || i <= prev {
			t.Fatalf("duplicate or out of order id %s", i)
		}
		seen[i], prev = true, i
	}
	r.expectQuiet()

	// Last-Event-ID header behaves like since
	h := e.open(e.admin, "boards="+bd.id, map[string]string{"Last-Event-ID": ids[3]})
	hv := h.events(4)
	if str(hv[0], "id") != ids[4] {
		t.Fatalf("Last-Event-ID replay started at %s, want %s", str(hv[0], "id"), ids[4])
	}
	// the live stream saw identical ids as the stored rows
	var stored []string
	rows, _ := e.d.Query(`SELECT seq FROM actions WHERE board_id=? AND seq IS NOT NULL AND type='cardCreate' ORDER BY seq`, bd.id)
	for rows.Next() {
		var n int64
		_ = rows.Scan(&n)
		stored = append(stored, fmt.Sprintf("%012d", n))
	}
	rows.Close()
	if len(stored) != 8 || stored[0] != ids[0] || stored[4] != ids[4] {
		t.Fatalf("stored ids %v vs live %v", stored, ids)
	}
	// since in the future / garbage
	if bad := e.open(e.admin, "since=abc", nil); bad.status != 422 {
		t.Fatalf("bad since status %d", bad.status)
	}
}

func TestStreamConcurrentWritersOrderedComplete(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Conc")
	s := e.open(e.admin, "lists="+bd.a, nil)
	const writers, per = 6, 8
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				e.card(bd.a, fmt.Sprintf("w%d-%d", w, i))
			}
		}(w)
	}
	evs := s.events(writers * per)
	wg.Wait()
	prev := ""
	seen := map[string]bool{}
	for _, ev := range evs {
		i := str(ev, "id")
		if i <= prev || seen[i] {
			t.Fatalf("out of order / duplicate id %s after %s", i, prev)
		}
		prev, seen[i] = i, true
	}
	s.expectQuiet()
}

func TestStreamPermissions(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Secret")
	other := e.newBoard("Open")
	outsider := e.newUser("out@x.io", "boardUser")
	viewer := e.newUser("view@x.io", "boardUser")
	uid := func(key string) string {
		_, o := e.api(key, "GET", "/auth/me", nil)
		return id(o["item"].(map[string]any))
	}
	e.must(e.admin, "POST", "/boards/"+bd.id+"/members", map[string]any{"userId": uid(viewer), "role": "viewer"})

	if s := e.open(outsider, "lists="+bd.a, nil); s.status != 404 {
		t.Fatalf("outsider lists= got %d, want 404", s.status)
	}
	if s := e.open(outsider, "boards="+bd.id, nil); s.status != 404 {
		t.Fatalf("outsider boards= got %d, want 404", s.status)
	}
	if s := e.open("", "boards="+bd.id, nil); s.status != 401 {
		t.Fatalf("anonymous got %d, want 401", s.status)
	}

	// viewer with no filter sees only boards it can view
	v := e.open(viewer, "types=card.created", nil)
	e.card(other.a, "hidden")
	e.card(bd.a, "visible")
	ev := v.events(1)[0]
	if str(ev, "boardId") != bd.id {
		t.Fatalf("viewer saw foreign event %v", ev)
	}
	v.expectQuiet()

	// asking for a mix: unviewable ids are dropped silently while a viewable one remains
	m := e.open(viewer, "boards="+bd.id+","+other.id, nil)
	e.card(other.b, "hidden2")
	e.card(bd.b, "visible2")
	if ev := m.events(1)[0]; str(ev, "boardId") != bd.id {
		t.Fatalf("mixed request leaked %v", ev)
	}
}

func TestStreamHeartbeat(t *testing.T) {
	e := newEnv(t)
	s := e.open(e.admin, "heartbeat=1", nil)
	deadline := time.After(4 * time.Second)
	for {
		select {
		case f := <-s.frames:
			if f.Event == "comment" && f.Data == "ping" {
				return
			}
		case <-deadline:
			t.Fatal("no heartbeat within 4s")
		}
	}
}

func TestLegacyRoomsStreamUnchanged(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Legacy")
	s := e.open(e.admin, "rooms=board:"+bd.id, nil)
	e.card(bd.a, "x")
	for {
		f, ok := s.next(3 * time.Second)
		if !ok {
			t.Fatal("no legacy event")
		}
		if f.Event == "card.created" {
			var m map[string]any
			_ = json.Unmarshal([]byte(f.Data), &m)
			if m["name"] != "card.created" || m["room"] != "board:"+bd.id || f.ID != "" {
				t.Fatalf("legacy frame changed: %+v", f)
			}
			return
		}
	}
}

func TestListenersRegistry(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Listen")
	k := e.must(e.admin, "POST", "/agents", map[string]any{"name": "Triage Bot", "boardIds": []string{bd.id}})
	key := str(k["key"].(map[string]any), "key")
	room := e.open(e.admin, "rooms=board:"+bd.id, nil) // browser watching for listener.joined
	s := e.open(key, "lists="+bd.a+","+bd.b+"&agent=triage-1", nil)
	s.events(0)

	waitListeners := func(path string, n int) []any {
		for i := 0; i < 50; i++ {
			_, out := e.api(e.admin, "GET", path, nil)
			if items, _ := out["items"].([]any); len(items) == n {
				return items
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s never reached %d listeners", path, n)
		return nil
	}
	items := waitListeners("/lists/"+bd.a+"/listeners", 1)
	l := items[0].(map[string]any)
	if l["name"] != "Triage Bot" || l["label"] != "triage-1" || l["isAgent"] != true || l["userId"] == "" || l["since"] == "" {
		t.Fatalf("listener %v", l)
	}
	waitListeners("/lists/"+bd.c+"/listeners", 0)
	waitListeners("/boards/"+bd.id+"/listeners", 1)

	joined := false
	for !joined {
		f, ok := room.next(3 * time.Second)
		if !ok {
			t.Fatal("no listener.joined on the board room")
		}
		joined = f.Event == "listener.joined"
	}
	s.cancel()
	waitListeners("/lists/"+bd.a+"/listeners", 0)
	for {
		f, ok := room.next(3 * time.Second)
		if !ok {
			t.Fatal("no listener.left on the board room")
		}
		if f.Event == "listener.left" {
			break
		}
	}
}

func TestAgentEventsCarryActor(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Actor")
	k := e.must(e.admin, "POST", "/agents", map[string]any{"name": "Coder", "boardIds": []string{bd.id}})
	key := str(k["key"].(map[string]any), "key")
	s := e.open(e.admin, "lists="+bd.a, nil)
	e.must(key, "POST", "/lists/"+bd.a+"/cards", map[string]any{"name": "by agent"})
	ev := s.events(1)[0]
	act := ev["actor"].(map[string]any)
	if act["isAgent"] != true || act["name"] != "Coder" {
		t.Fatalf("actor %v", act)
	}
}

func TestClaimEventsAndArchiveKinds(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Claims")
	k := e.must(e.admin, "POST", "/agents", map[string]any{"name": "Worker", "boardIds": []string{bd.id}})
	key := str(k["key"].(map[string]any), "key")
	c := e.card(bd.a, "job")
	s := e.open(key, "lists="+bd.a+","+bd.b, nil)
	e.must(key, "POST", "/cards/"+c+"/claim", map[string]any{"listId": bd.b, "ttlSeconds": 60, "note": "on it"})
	evs := s.events(3) // member.added, card.claimed, card.moved
	types := []string{str(evs[0], "type"), str(evs[1], "type"), str(evs[2], "type")}
	if strings.Join(types, ",") != "member.added,card.claimed,card.moved" {
		t.Fatalf("claim events %v", types)
	}
	snap := evs[1]["card"].(map[string]any)
	if snap["claimedBy"] == nil || snap["claimedUntil"] == nil {
		t.Fatalf("claim snapshot %v", snap)
	}
	e.must(e.admin, "POST", "/cards/"+c+"/archive", map[string]any{})
	e.must(e.admin, "POST", "/cards/"+c+"/restore", map[string]any{})
	got := s.events(2) // the claim survives archive; archived (B -> archive list), restored (archive list -> B)
	if str(got[0], "type") != "card.archived" || str(got[0], "fromListId") != bd.b || str(got[1], "type") != "card.restored" || str(got[1], "toListId") != bd.b {
		t.Fatalf("archive/restore events %v", got)
	}
}
