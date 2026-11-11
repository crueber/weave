package realtime

import (
	"sort"
	"sync"
	"time"
)

// Listener is one open event-stream connection that subscribed to lists.
type Listener struct {
	UserID  string   `json:"userId"`
	Name    string   `json:"name"`
	IsAgent bool     `json:"isAgent"`
	Label   string   `json:"label"`
	Since   string   `json:"since"`
	Lists   []string `json:"-"`
	Boards  []string `json:"-"` // board of each entry of Lists (parallel slice)
	id      int
}

// Listeners is the in-memory presence registry: a listener exists exactly as long
// as its SSE connection (the handler unregisters on disconnect). Nothing is stored.
type Listeners struct {
	mu   sync.RWMutex
	next int
	all  map[int]*Listener
	hub  *Hub
}

func NewListeners(h *Hub) *Listeners { return &Listeners{all: map[int]*Listener{}, hub: h} }

// Register adds a listener and announces listener.joined to the boards' rooms.
// The returned func unregisters it (listener.left).
func (r *Listeners) Register(l Listener) func() {
	if r == nil {
		return func() {}
	}
	if l.Since == "" {
		l.Since = time.Now().UTC().Format(time.RFC3339)
	}
	r.mu.Lock()
	r.next++
	l.id = r.next
	cp := l
	r.all[l.id] = &cp
	r.mu.Unlock()
	r.announce("listener.joined", &cp)
	return func() {
		r.mu.Lock()
		_, ok := r.all[cp.id]
		delete(r.all, cp.id)
		r.mu.Unlock()
		if ok {
			r.announce("listener.left", &cp)
		}
	}
}

func (r *Listeners) announce(name string, l *Listener) {
	if r.hub == nil {
		return
	}
	byBoard := map[string][]string{}
	var order []string
	for i, b := range l.Boards {
		if b == "" {
			continue
		}
		if _, ok := byBoard[b]; !ok {
			order = append(order, b)
		}
		byBoard[b] = append(byBoard[b], l.Lists[i])
	}
	for _, b := range order {
		r.hub.Publish(Event{Name: name, Room: "board:" + b, Data: map[string]any{
			"boardId": b, "listIds": byBoard[b], "userId": l.UserID, "name": l.Name,
			"isAgent": l.IsAgent, "label": l.Label, "since": l.Since}})
	}
}

// ForList returns the distinct (user,label) listeners on a list, oldest first.
func (r *Listeners) ForList(listID string) []Listener {
	return r.collect(func(l *Listener) bool {
		for _, x := range l.Lists {
			if x == listID {
				return true
			}
		}
		return false
	})
}

// ForBoard returns the distinct listeners on any list of the board.
func (r *Listeners) ForBoard(boardID string) []Listener {
	return r.collect(func(l *Listener) bool {
		for _, b := range l.Boards {
			if b == boardID {
				return true
			}
		}
		return false
	})
}

func (r *Listeners) collect(match func(*Listener) bool) []Listener {
	out := []Listener{}
	if r == nil {
		return out
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	idx := map[string]int{}
	for _, l := range r.all {
		if !match(l) {
			continue
		}
		k := l.UserID + "\x00" + l.Label
		if i, ok := idx[k]; ok {
			if l.Since < out[i].Since {
				out[i].Since = l.Since
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		return out[i].Name < out[j].Name
	})
	return out
}
