// Package realtime is an in-process pub/sub hub feeding SSE. Weave runs as a single replica.
package realtime

import "sync"

// Event is pushed to browsers (and `weave watch`). Name examples: card.updated, list.created.
type Event struct {
	Name string `json:"name"`
	Data any    `json:"data"`
	// Room: "board:<id>", "project:<id>", "user:<id>", "global".
	Room string `json:"room"`
}

type Hub struct {
	mu   sync.RWMutex
	subs map[chan Event]func(room string) bool
	// Tap receives every event (webhook dispatcher). Set once at startup.
	Tap   func(Event)
	ticks map[chan struct{}]struct{}
	// Listeners is the in-memory registry of event-stream listeners (agents on lists).
	Listeners *Listeners
}

func New() *Hub {
	h := &Hub{subs: map[chan Event]func(string) bool{}}
	h.Listeners = NewListeners(h)
	return h
}

// Tick wakes every event-stream tailer: new durable events were written.
// Ticks coalesce (buffer 1); tailers re-read the database from their last seen
// id, so a coalesced tick never loses data.
func (h *Hub) Tick() {
	if h == nil {
		return
	}
	h.mu.RLock()
	for ch := range h.ticks {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.mu.RUnlock()
}

// SubscribeTicks returns a coalescing wake-up channel.
func (h *Hub) SubscribeTicks() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.ticks == nil {
		h.ticks = map[chan struct{}]struct{}{}
	}
	h.ticks[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() { h.mu.Lock(); delete(h.ticks, ch); h.mu.Unlock() }
}

// Subscribe registers a listener filtered by allow(room).
func (h *Hub) Subscribe(allow func(room string) bool) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = allow
	h.mu.Unlock()
	return ch, func() { h.mu.Lock(); delete(h.subs, ch); h.mu.Unlock() }
}

func (h *Hub) Publish(e Event) {
	if h == nil {
		return
	}
	h.mu.RLock()
	for ch, allow := range h.subs {
		if allow(e.Room) {
			select {
			case ch <- e:
			default: // slow consumer: drop; client refetches on reconnect
			}
		}
	}
	h.mu.RUnlock()
	if h.Tap != nil {
		h.Tap(e)
	}
}
