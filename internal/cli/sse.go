package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sseFrame is one parsed Server-Sent-Events frame.
type sseFrame struct {
	ID, Event, Data string
	Comment         bool // ": ping" style heartbeat/comment line
}

// readSSE parses an SSE stream, calling activity() for every line read (heartbeat watchdog)
// and emit() for each frame; emit returning false stops reading.
func readSSE(r io.Reader, activity func(), emit func(sseFrame) bool) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var f sseFrame
	var data []string
	have := false
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			activity()
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if have {
					f.Data = strings.Join(data, "\n")
					if !emit(f) {
						return nil
					}
				}
				f, data, have = sseFrame{}, nil, false
			case strings.HasPrefix(line, ":"):
				if !emit(sseFrame{Comment: true, Data: strings.TrimSpace(line[1:])}) {
					return nil
				}
			default:
				k, v, _ := strings.Cut(line, ":")
				v = strings.TrimPrefix(v, " ")
				switch k {
				case "id":
					f.ID, have = v, true
				case "event":
					f.Event, have = v, true
				case "data":
					data, have = append(data, v), true
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// Event is one change event (envelope) from /events.
type Event struct {
	ID   string
	Type string
	M    map[string]any
	Raw  []byte
}

func (e Event) S(k string) string { return str(e.M, k) }

// CardID, ListID, etc. read the envelope.
func (e Event) CardID() string { return firstStr(e.M, "cardId") }
func (e Event) BoardID() string {
	return e.S("boardId")
}
func (e Event) ListID() string {
	if v := e.S("toListId"); v != "" {
		return v
	}
	if v := e.S("listId"); v != "" {
		return v
	}
	if c, ok := e.M["card"].(map[string]any); ok {
		return str(c, "listId")
	}
	return ""
}
func (e Event) ActorID() string {
	if a, ok := e.M["actor"].(map[string]any); ok {
		return str(a, "id")
	}
	return ""
}
func (e Event) Card() map[string]any { c, _ := e.M["card"].(map[string]any); return c }

func newEvent(id, typ string, raw []byte) Event {
	ev := Event{ID: id, Type: typ, Raw: raw}
	_ = json.Unmarshal(raw, &ev.M)
	if ev.M == nil {
		ev.M = map[string]any{}
	}
	if ev.ID == "" {
		ev.ID = str(ev.M, "id")
	}
	if ev.Type == "" {
		ev.Type = str(ev.M, "type")
	}
	return ev
}

// marshal returns the envelope as one JSON line.
func (e Event) marshal() []byte {
	if e.Raw != nil {
		var buf strings.Builder
		if json.Valid(e.Raw) {
			// compact
			var m json.RawMessage = e.Raw
			b, _ := json.Marshal(m)
			buf.Write(b)
			return []byte(buf.String())
		}
	}
	b, _ := json.Marshal(e.M)
	return b
}

// ---- cursor tracking ----

// cursor persists the last fully-handled event id atomically. With concurrent handlers it only
// advances to the highest id for which every earlier event has also finished, so a restart
// never skips work that was still in flight (at-least-once).
type cursor struct {
	path string
	mu   sync.Mutex
	q    []*cursorEntry
	done string
}

type cursorEntry struct {
	id   string
	done bool
}

func loadCursor(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (c *cursor) begin(id string) *cursorEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := &cursorEntry{id: id}
	c.q = append(c.q, e)
	return e
}

func (c *cursor) finish(e *cursorEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.done = true
	var last string
	for len(c.q) > 0 && c.q[0].done {
		if c.q[0].id != "" {
			last = c.q[0].id
		}
		c.q = c.q[1:]
	}
	if last != "" && last != c.done {
		c.done = last
		if c.path != "" {
			_ = atomicWrite(c.path, []byte(last+"\n"), 0o600)
		}
	}
}

// ---- listener ----

// Listener keeps ONE SSE connection to /events open, reconnecting with backoff and resuming
// from the last event id, and delivers every matching event exactly once per process.
type Listener struct {
	App        *App
	Boards     []string
	Lists      []string
	Types      []string
	Since      string
	CursorFile string
	Heartbeat  time.Duration
	AgentLabel string
	Rooms      string // legacy ?rooms= mode (weave watch --rooms)

	// Skip drops events before dispatch (e.g. caused by this listener itself).
	Skip func(Event) bool
	// Existing, when set, is called once after the first `ready` of a fresh start and returns
	// synthetic events for the backlog; they are delivered before live events.
	Existing func(ctx context.Context) ([]Event, error)
	// Handle receives events in arrival order; it must call done exactly once when finished
	// (immediately for synchronous handlers).
	Handle func(ctx context.Context, ev Event, done func())

	BackoffMin, BackoffMax time.Duration
	WatchdogFactor         int
	Log                    func(format string, args ...any)
	Rand                   *rand.Rand

	cur      *cursor
	lastRecv string
	seen     map[string]bool
	seenQ    []string
	Resumed  bool // true when started from a saved cursor
	Connects atomic.Int64
	existing bool
}

func (l *Listener) logf(f string, a ...any) {
	if l.Log != nil {
		l.Log(f, a...)
	}
}

func (l *Listener) defaults() {
	if l.Heartbeat <= 0 {
		l.Heartbeat = 15 * time.Second
	}
	if l.BackoffMin <= 0 {
		l.BackoffMin = time.Second
	}
	if l.BackoffMax <= 0 {
		l.BackoffMax = 30 * time.Second
	}
	if l.WatchdogFactor <= 0 {
		l.WatchdogFactor = 3
	}
	if l.Rand == nil {
		l.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	l.seen = map[string]bool{}
	l.cur = &cursor{path: l.CursorFile}
	if l.Since == "" {
		l.Since = loadCursor(l.CursorFile)
		l.Resumed = l.Since != ""
	}
	l.cur.done = l.Since
	l.lastRecv = l.Since
}

func (l *Listener) streamURL() string {
	q := url.Values{}
	if l.Rooms != "" {
		q.Set("rooms", l.Rooms)
	}
	if len(l.Boards) > 0 {
		q.Set("boards", strings.Join(l.Boards, ","))
	}
	if len(l.Lists) > 0 {
		q.Set("lists", strings.Join(l.Lists, ","))
	}
	if len(l.Types) > 0 {
		q.Set("types", strings.Join(l.Types, ","))
	}
	if l.lastRecv != "" {
		q.Set("since", l.lastRecv)
	}
	if l.Rooms == "" {
		q.Set("heartbeat", strconv.Itoa(int(l.Heartbeat/time.Second)))
	}
	if l.AgentLabel != "" {
		q.Set("agent", l.AgentLabel)
	}
	return strings.TrimRight(l.App.Cfg.URL, "/") + "/events?" + q.Encode()
}

// Run blocks until ctx is cancelled (returns nil) or a fatal error (auth, not found).
func (l *Listener) Run(ctx context.Context) error {
	l.defaults()
	backoff := l.BackoffMin
	for {
		if ctx.Err() != nil {
			return nil
		}
		got, err := l.connectOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.fatalStream() {
			return err
		}
		if got {
			backoff = l.BackoffMin
		}
		d := backoff
		if ae != nil && ae.retryAfter > 0 {
			d = ae.retryAfter
		}
		d = time.Duration(float64(d) * (0.8 + 0.4*l.Rand.Float64()))
		if err != nil {
			l.logf("listen: %v; reconnecting in %s", err, d.Round(time.Millisecond))
		} else {
			l.logf("listen: stream closed; reconnecting in %s", d.Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d):
		}
		if backoff *= 2; backoff > l.BackoffMax {
			backoff = l.BackoffMax
		}
	}
}

func (e *APIError) fatalStream() bool {
	switch e.Status {
	case 400, 401, 403, 404, 422:
		return true
	}
	return false
}

func (l *Listener) connectOnce(ctx context.Context) (bool, error) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", l.streamURL(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if l.lastRecv != "" {
		req.Header.Set("Last-Event-ID", l.lastRecv)
	}
	if l.App.Cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+l.App.Cfg.Token)
	}
	l.App.warnInsecure()
	hc := l.App.HTTP
	if hc == nil {
		hc = &http.Client{CheckRedirect: redirectPolicy(l.App.Cfg.Token != "")}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false, &APIError{Code: "network", Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		ae := errorFromResponse(&Response{Status: resp.StatusCode, Header: resp.Header, Body: b})
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if n, e := strconv.Atoi(ra); e == nil {
				ae.retryAfter = time.Duration(n) * time.Second
			}
		}
		return false, ae
	}
	l.Connects.Add(1)
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	stalled := atomic.Bool{}
	hb := l.Heartbeat
	if l.Rooms != "" {
		hb = 0
	}
	if hb > 0 {
		limit := time.Duration(l.WatchdogFactor) * hb
		go func() {
			t := time.NewTicker(limit / 6)
			defer t.Stop()
			for {
				select {
				case <-cctx.Done():
					return
				case <-t.C:
					if time.Since(time.Unix(0, last.Load())) > limit {
						stalled.Store(true)
						cancel()
						return
					}
				}
			}
		}()
	}
	frames := make(chan sseFrame, 1024)
	var rerr error
	go func() {
		defer close(frames)
		rerr = readSSE(resp.Body, func() { last.Store(time.Now().UnixNano()) }, func(f sseFrame) bool {
			select {
			case frames <- f:
				return true
			case <-cctx.Done():
				return false
			}
		})
	}()
	got := false
	for f := range frames {
		if f.Comment {
			continue
		}
		switch f.Event {
		case "ready":
			got = true
			var rd struct {
				ResumeFrom string `json:"resumeFrom"`
			}
			_ = json.Unmarshal([]byte(f.Data), &rd)
			fresh := l.lastRecv == ""
			if l.Existing != nil && !l.existing {
				l.existing = true
				if l.Resumed {
					l.logf("listen: resuming from saved cursor; skipping --include-existing")
				} else if err := l.deliverExisting(ctx); err != nil && ctx.Err() == nil {
					return got, err
				}
			}
			if fresh && rd.ResumeFrom != "" {
				// the stream position at connect time becomes the resume point; it commits only after
				// the backlog above is fully handled, so a restart neither replays it nor misses events
				l.lastRecv = rd.ResumeFrom
				l.remember(rd.ResumeFrom)
				l.cur.finish(l.cur.begin(rd.ResumeFrom))
			}
			continue
		case "ping", "heartbeat":
			continue
		}
		ev := newEvent(f.ID, f.Event, []byte(f.Data))
		if !json.Valid([]byte(f.Data)) {
			continue
		}
		got = true
		if ev.ID != "" {
			if l.seen[ev.ID] {
				continue
			}
			l.remember(ev.ID)
			l.lastRecv = ev.ID
		}
		if l.Skip != nil && l.Skip(ev) {
			l.commitSkipped(ev)
			continue
		}
		l.dispatch(ctx, ev)
	}
	cancel()
	if stalled.Load() {
		return got, &APIError{Code: "network", Message: fmt.Sprintf("no heartbeat for %s", time.Duration(l.WatchdogFactor)*hb)}
	}
	if rerr != nil && ctx.Err() == nil {
		return got, &APIError{Code: "network", Message: rerr.Error()}
	}
	return got, nil
}

func (l *Listener) commitSkipped(ev Event) {
	l.cur.finish(l.cur.begin(ev.ID))
}

func (l *Listener) dispatch(ctx context.Context, ev Event) {
	ent := l.cur.begin(ev.ID)
	var once sync.Once
	l.Handle(ctx, ev, func() { once.Do(func() { l.cur.finish(ent) }) })
}

func (l *Listener) deliverExisting(ctx context.Context) error {
	evs, err := l.Existing(ctx)
	if err != nil {
		return err
	}
	for _, ev := range evs {
		if ctx.Err() != nil {
			return nil
		}
		l.dispatch(ctx, ev)
	}
	return nil
}

func (l *Listener) remember(id string) {
	l.seen[id] = true
	l.seenQ = append(l.seenQ, id)
	if len(l.seenQ) > 8192 {
		delete(l.seen, l.seenQ[0])
		l.seenQ = l.seenQ[1:]
	}
}
