package ops

// Event is a domain event: activity row + notifications + realtime + webhooks.
type Event struct {
	Type    string // "cardCreate", "commentCreate", … (Planka action types, camelCase)
	BoardID string
	CardID  string
	Room    string // realtime room; default "board:"+BoardID
	Name    string // realtime event name e.g. "card.created"
	Data    map[string]any
	// Kind overrides the event-stream type derived from Name (e.g. "card.archived"
	// for an archive whose realtime name stays "card.moved").
	Kind string
	// Boards, when set, is the explicit list of boards a card event concerns (BoardID should be among them; it stays
	// the origin). notify.Record then writes one activity row and publishes to one room per listed board instead of
	// asking the cards domain for the card's placements. Writers set it when the placements are gone by the time the
	// event is recorded (a deleted card: read them BEFORE the write) and when an event must stay on fewer boards than
	// the card is on (one board's labels or custom fields, the source half of a cross-board move).
	Boards []string
	// Mention/subscriber notification inputs are derived by notify from Type+Data.
}

// EmitHook is installed by the notify domain. Domain code calls ops.Emit(e, ev)
// after a successful mutation; before notify is linked it only publishes realtime.
var EmitHook func(e *Env, ev Event)

func Emit(e *Env, ev Event) {
	if ev.Room == "" && ev.BoardID != "" {
		ev.Room = "board:" + ev.BoardID
	}
	if EmitHook != nil {
		EmitHook(e, ev)
	}
}
