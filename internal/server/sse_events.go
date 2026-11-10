package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

func csv(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envelopeMode reports whether the request asks for the durable envelope stream.
func envelopeMode(r *http.Request) bool {
	q := r.URL.Query()
	for _, k := range []string{"boards", "lists", "types", "since", "heartbeat", "agent"} {
		if q.Has(k) {
			return true
		}
	}
	return r.Header.Get("Last-Event-ID") != ""
}

type accessEntry struct {
	ok bool
	at time.Time
}

// accessCache memoises board view checks for a short time, so permission changes
// reach open streams quickly without a query per event.
type accessCache struct {
	e   *ops.Env
	ttl time.Duration
	m   map[string]accessEntry
}

func (a *accessCache) can(board string) bool {
	if v, ok := a.m[board]; ok && time.Since(v.at) < a.ttl {
		return v.ok
	}
	ok := authz.BoardLevel(a.e, board) >= authz.View
	a.m[board] = accessEntry{ok, time.Now()}
	return ok
}

// sseEvents serves the envelope stream: replay from the durable store, then live
// tailing of the same store. See package events.
func (s *Server) sseEvents(w http.ResponseWriter, r *http.Request, e *ops.Env) {
	q := r.URL.Query()
	f := events.Filter{Types: csv(q.Get("types"))}
	ac := &accessCache{e: e, ttl: 10 * time.Second, m: map[string]accessEntry{}}

	// since: query param wins over the Last-Event-ID header; none = live only.
	sinceRaw := q.Get("since")
	if sinceRaw == "" {
		sinceRaw = r.Header.Get("Last-Event-ID")
	}
	last := int64(-1)
	if sinceRaw != "" {
		n, err := events.ParseID(sinceRaw)
		if err != nil {
			apiErr(w, ops.Invalid("since must be an event id"))
			return
		}
		last = n
	}
	hb := 15
	if v := q.Get("heartbeat"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3600 {
			apiErr(w, ops.Invalid("heartbeat must be 1..3600 seconds"))
			return
		}
		hb = n
	}

	// Scope: boards= and lists= are intersected with what the caller may view.
	// Unviewable ids behave exactly like unknown ones.
	listIDs := csv(q.Get("lists"))
	boardIDs := csv(q.Get("boards"))
	listBoard := events.ListBoards(r.Context(), s.DB, listIDs)
	var okLists, okListBoards, okBoards []string
	for _, l := range listIDs {
		if b, ok := listBoard[l]; ok && ac.can(b) {
			okLists = append(okLists, l)
			okListBoards = append(okListBoards, b)
		}
	}
	for _, b := range boardIDs {
		if ac.can(b) {
			okBoards = append(okBoards, b)
		}
	}
	if (len(listIDs) > 0 && len(okLists) == 0) || (len(boardIDs) > 0 && len(okBoards) == 0) {
		apiErr(w, ops.NotFound("list or board"))
		return
	}
	f.Lists, f.Boards = okLists, okBoards

	fl, ok := w.(http.Flusher)
	if !ok {
		apiErr(w, fmt.Errorf("streaming unsupported"))
		return
	}
	// SSE must outlive any server-wide write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	// Wake-ups first, so nothing written after the cursor read below is missed.
	ticks, cancelTicks := s.Hub.SubscribeTicks()
	defer cancelTicks()
	if last < 0 {
		last = events.LastSeq(r.Context(), s.DB)
	}
	if len(okLists) > 0 {
		label := strings.TrimSpace(q.Get("agent"))
		if label == "" {
			label = e.User.Name
		}
		unreg := s.Hub.Listeners.Register(realtime.Listener{UserID: e.User.UserID, Name: e.User.Name, IsAgent: e.User.IsAgent,
			Label: label, Lists: okLists, Boards: okListBoards})
		defer unreg()
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	ready, _ := json.Marshal(map[string]any{"serverTime": time.Now().UTC().Format(time.RFC3339), "resumeFrom": events.FormatID(last)})
	fmt.Fprintf(w, "event: ready\ndata: %s\n\n", ready)
	fl.Flush()

	// Credentials and board access are re-checked while the stream is open: rotating or
	// deactivating a key, deleting an agent or revoking access must end the stream
	// (and with it the listener registration).
	recheck := time.NewTicker(3 * time.Second)
	defer recheck.Stop()
	scope := append(append([]string{}, okBoards...), okListBoards...)
	stillAllowed := func() bool {
		p := ops.Authenticate(&ops.Env{Ctx: r.Context(), DB: s.DB, Hub: s.Hub, Svc: s.Svc, Req: r}, r)
		if p == nil || p.UserID != e.User.UserID {
			return false
		}
		if len(scope) == 0 {
			return true
		}
		for _, b := range scope {
			if authz.BoardLevel(e, b) >= authz.View {
				return true
			}
		}
		return false
	}
	beat := time.NewTicker(time.Duration(hb) * time.Second)
	defer beat.Stop()
	poll := time.NewTicker(2 * time.Second) // safety net if a tick is ever missed
	defer poll.Stop()
	for {
		for {
			f.Since = last
			rows, err := events.Query(r.Context(), s.DB, f)
			if err != nil {
				return
			}
			for _, row := range rows {
				last = row.Seq
				if !ac.can(row.BoardID) {
					continue
				}
				fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", events.FormatID(row.Seq), row.Type, row.JSON())
			}
			if len(rows) > 0 {
				fl.Flush()
			}
			if len(rows) < 500 {
				break
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticks:
		case <-poll.C:
		case <-recheck.C:
			if !stillAllowed() {
				return
			}
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}
