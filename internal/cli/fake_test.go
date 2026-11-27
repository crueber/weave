package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fake is an httptest Weave server plus an in-process CLI runner.
type fake struct {
	t     *testing.T
	mux   *http.ServeMux
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string
	posts map[string][]string // "METHOD path" -> bodies
	hs    map[string]http.HandlerFunc
}

// handleFunc registers (or replaces) the handler of a ServeMux pattern.
func (f *fake) handleFunc(pattern string, h http.HandlerFunc) {
	f.mu.Lock()
	_, have := f.hs[pattern]
	f.hs[pattern] = h
	f.mu.Unlock()
	if !have {
		f.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			h := f.hs[pattern]
			f.mu.Unlock()
			h(w, r)
		})
	}
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t, mux: http.NewServeMux(), posts: map[string][]string{}, hs: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		key := r.Method + " " + r.URL.Path
		f.calls = append(f.calls, key+"?"+r.URL.RawQuery)
		f.posts[key] = append(f.posts[key], string(b))
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(b))
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	t.Setenv("WEAVE_URL", f.srv.URL)
	t.Setenv("WEAVE_TOKEN", "wk_test")
	t.Setenv("WEAVE_PROFILE", "")
	t.Setenv("WEAVE_BOARD", "")
	t.Setenv("WEAVE_PROJECT", "")
	t.Setenv("WEAVE_CONFIG_DIR", t.TempDir())
	t.Setenv("WEAVE_CACHE_DIR", t.TempDir())
	t.Setenv("WEAVE_NO_SCHEMA_FETCH", "1")
	t.Setenv("NO_COLOR", "1")
	// defaults most tests need
	f.json("GET /api/v1/auth/whoami", map[string]any{"item": map[string]any{
		"user": map[string]any{"id": "u1", "name": "Agent Smith"}, "role": "boardUser", "isAgent": true,
		"boards": []any{map[string]any{"id": "b1", "name": "Product"}, map[string]any{"id": "b2", "name": "Platform"}},
	}})
	f.json("GET /api/v1/boards/{boardId}/lists", map[string]any{"items": []any{
		map[string]any{"id": "l1", "name": "Ready"}, map[string]any{"id": "l2", "name": "In progress"},
		map[string]any{"id": "l3", "name": "Review"}, map[string]any{"id": "l4", "name": "Needs info"},
		map[string]any{"id": "l5", "name": "Backlog"}, map[string]any{"id": "l6", "name": "Backlog archive"},
	}})
	// a card that is still in Ready, open and unclaimed (what the claim preflight reads)
	f.handleFunc("GET /api/v1/cards/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"item": map[string]any{"id": r.PathValue("id"), "boardId": "b1", "listId": "l1", "name": "Card", "isClosed": false, "claimedBy": nil}})
	})
	f.json("GET /api/v1/cards/{cardId}/comments", map[string]any{"items": []any{}})
	return f
}

func (f *fake) json(pattern string, v any) {
	f.handleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	})
}

func (f *fake) status(pattern string, code int, errCode, msg string) {
	f.handleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": errCode, "message": msg}})
	})
}

func (f *fake) called(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

func (f *fake) body(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b := f.posts[key]; len(b) > 0 {
		return b[len(b)-1]
	}
	return ""
}

// run executes the CLI in-process.
func (f *fake) run(args ...string) (int, string, string) {
	return f.runIn("", args...)
}

func (f *fake) runIn(stdin string, args ...string) (int, string, string) {
	var out, errb lockedBuf
	code := Run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// lockedBuf is a bytes.Buffer safe for concurrent writers (exec children, worker goroutines).
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// cardState is a tiny stateful card store: claim/move/release really move the card, so tests can
// exercise the "where is the card now" checks. Call it after registering any custom handlers.
type cardState struct {
	mu    sync.Mutex
	list  map[string]string // card -> list id
	claim map[string]string // card -> claimant
}

func (f *fake) trackCards(initial map[string]string) *cardState {
	cs := &cardState{list: initial, claim: map[string]string{}}
	if cs.list == nil {
		cs.list = map[string]string{}
	}
	get := func(id string) (string, string) {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		l := cs.list[id]
		if l == "" {
			l = "l1"
		}
		return l, cs.claim[id]
	}
	reply := func(w http.ResponseWriter, id string) {
		l, c := get(id)
		var cb any
		if c != "" {
			cb = c
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"item": map[string]any{"id": id, "boardId": "b1", "listId": l, "name": "Card", "isClosed": false, "claimedBy": cb}})
	}
	body := func(r *http.Request) map[string]any {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		return m
	}
	f.handleFunc("GET /api/v1/cards/{id}", func(w http.ResponseWriter, r *http.Request) { reply(w, r.PathValue("id")) })
	f.handleFunc("POST /api/v1/cards/{id}/claim", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		cs.mu.Lock()
		cs.claim[id] = "u1"
		if l, _ := body(r)["listId"].(string); l != "" {
			cs.list[id] = l
		}
		cs.mu.Unlock()
		reply(w, id)
	})
	f.handleFunc("POST /api/v1/cards/{id}/move", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		cs.mu.Lock()
		if l, _ := body(r)["listId"].(string); l != "" {
			cs.list[id] = l
		}
		cs.mu.Unlock()
		reply(w, id)
	})
	f.handleFunc("POST /api/v1/cards/{id}/release", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		cs.mu.Lock()
		delete(cs.claim, id)
		if l, _ := body(r)["listId"].(string); l != "" {
			cs.list[id] = l
		}
		cs.mu.Unlock()
		reply(w, id)
	})
	return cs
}

func (cs *cardState) listOf(id string) string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if l := cs.list[id]; l != "" {
		return l
	}
	return "l1"
}
func (cs *cardState) set(id, list string) { cs.mu.Lock(); cs.list[id] = list; cs.mu.Unlock() }
