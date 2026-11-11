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
