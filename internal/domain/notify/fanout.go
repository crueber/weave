package notify

import (
	"reflect"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

// Event fan-out (docs/DESIGN.md "Placements", events). One logical event about a card that is on several boards is
// recorded ONCE PER BOARD it concerns: one actions row each (the first, the origin, written as before; every other
// row names it in origin_id), one legacy-hub publish per board room, each with that board's own view of the card.
// Notifications and webhooks run once per logical event (the planner and the webhook matcher decide per recipient
// which board's view they get). Nothing that belongs to one board reaches another board's room, row or payload:
// a fan-out copy carries that board's card (events.CardOn), its own boardId, and no column or board id of another
// board.

// eventScope is the boards one logical event concerns.
type eventScope struct {
	boards    []string // ev.BoardID (the origin) first; empty for an event without a board
	placement bool     // the event concerns one placement (events.PlacementScoped): boards is [ev.BoardID]
	home      string   // the card's home board when known
	others    []string // placement events: the card's OTHER boards (they get the unstored card.placement.changed nudge)
}

// eventScopeOf decides the boards of an event: ev.Boards when the writer listed them (deletes, one board's vocabulary,
// a placement or a removal), else [ev.BoardID] for a placement event, else every board the card is on
// (events.Placements, one query). The origin board always comes first and is always included (it is where the change
// was made). A placement event also learns the card's home (webhook copy choice) and its other boards (the nudge).
func eventScopeOf(e *ops.Env, ev ops.Event) eventScope {
	var s eventScope
	if ev.BoardID == "" {
		return s
	}
	s.boards = []string{ev.BoardID}
	if ev.CardID == "" {
		return s
	}
	add := func(b string) {
		if b == "" {
			return
		}
		for _, x := range s.boards {
			if x == b {
				return
			}
		}
		s.boards = append(s.boards, b)
	}
	explicit := ev.Boards != nil
	for _, b := range ev.Boards {
		add(b)
	}
	s.placement = events.PlacementScoped(ev)
	if events.Placements == nil || (explicit && !s.placement) {
		// an explicit card-level scope (a deleted card: its placements are gone) needs nothing else
		return s
	}
	inScope := func(b string) bool {
		for _, x := range s.boards {
			if x == b {
				return true
			}
		}
		return false
	}
	for _, p := range events.Placements(e, ev.CardID) {
		if p.IsHome {
			s.home = p.BoardID
		}
		switch {
		case !s.placement:
			add(p.BoardID)
		case !inScope(p.BoardID):
			// the card's other boards: a placement event (also one that names its board explicitly: a placement, a
			// removal) gives them the unstored nudge, never the event
			s.others = append(s.others, p.BoardID)
		}
	}
	return s
}

// hidden data keys: routing for the writer and the envelope, never part of the stored data, a payload or a publish
var hiddenKeys = []string{"actionId", "originId", "actionAt", events.CardLevel, events.PlacementLists}

// perBoard builds the event once per board of the scope. The origin keeps its data (and the pre-generated actionId);
// every other copy gets its own actionId, originId = the origin's, boardId = that board, and loses the column and
// board ids that belong to another board (a card-level archive on board A names A's columns; board B's copy names
// none: its listId comes from B's own snapshot). Every copy that carries a card carries that board's view of it.
func perBoard(e *ops.Env, ev ops.Event, s eventScope, at string) []ops.Event {
	if len(s.boards) == 0 {
		return []ops.Event{ev}
	}
	ev.Data["actionAt"] = at
	var listBoard map[string]string // list id -> its board, only when a copy needs it
	out := make([]ops.Event, 0, len(s.boards))
	for i, b := range s.boards {
		x := ev
		x.BoardID = b
		if ev.Room == "board:"+ev.BoardID {
			x.Room = "board:" + b
		}
		d := make(map[string]any, len(ev.Data)+1)
		for k, v := range ev.Data {
			d[k] = v
		}
		if i > 0 {
			d["originId"], d["actionId"] = ev.Data["actionId"], db.NewID()
			if _, ok := d["boardId"]; ok {
				d["boardId"] = b
			}
			for _, k := range []string{"fromBoardId", "movedToBoardId"} {
				if v := str(d, k); v != "" && v != b {
					delete(d, k)
				}
			}
			if listBoard == nil {
				var ids []string
				for _, k := range []string{"listId", "fromListId", "toListId"} {
					if v := str(ev.Data, k); v != "" {
						ids = append(ids, v)
					}
				}
				listBoard = events.ListBoards(e.Ctx, e.DB, ids)
			}
			for _, k := range []string{"listId", "fromListId", "toListId"} {
				if v := str(d, k); v != "" && listBoard[v] != b {
					delete(d, k)
				}
			}
			// a card-level move names, on each board, that board's own placement move (events.PlacementLists)
			if pl, ok := ev.Data[events.PlacementLists].(map[string][2]string); ok {
				if fl, ok := pl[b]; ok {
					d["fromListId"], d["toListId"] = fl[0], fl[1]
				}
			}
		}
		if _, ok := d["card"]; ok && events.CardOn != nil && ev.CardID != "" {
			if !(i == 0 && cardMatchesBoard(d["card"], b)) {
				if c := events.CardOn(e, ev.CardID, b); c != nil {
					d["card"] = c
				} else {
					delete(d, "card") // not placed on that board (any more): never another board's view
				}
			}
		}
		x.Data = d
		out = append(out, x)
	}
	return out
}

// cardMatchesBoard reports whether a caller-supplied data["card"] payload is already populated for boardID
// (so the origin copy i == 0 can skip a redundant events.CardOn reload).
func cardMatchesBoard(v any, boardID string) bool {
	if v == nil || boardID == "" {
		return false
	}
	if m, ok := v.(map[string]any); ok {
		return str(m, "boardId") == boardID
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Struct {
		if f := rv.FieldByName("BoardID"); f.IsValid() && f.Kind() == reflect.String {
			return f.String() == boardID
		}
	}
	return false
}

// publicData is the data of an event without the hidden routing keys (hub publishes, webhook payloads).
func publicData(d map[string]any) map[string]any {
	out := make(map[string]any, len(d))
	for k, v := range d {
		out[k] = v
	}
	for _, k := range hiddenKeys[1:] { // actionId stays: the hub has always carried it
		delete(out, k)
	}
	return out
}
