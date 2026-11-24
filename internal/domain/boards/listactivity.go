package boards

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

// ListActivityIn pages a column's activity feed.
type ListActivityIn struct {
	ID     string `path:"id" desc:"list id"`
	Before string `query:"before" desc:"Event id cursor: return events older than this one"`
	Limit  int    `query:"limit" desc:"Page size (default 20, max 100)"`
}

// ListActivityItem is one thing that happened in a column.
type ListActivityItem struct {
	ID           string `json:"id" desc:"event id (use as the before cursor)"`
	Type         string `json:"type" desc:"event type: card.created, card.moved, card.archived, card.deleted, list.created, list.updated"`
	Actor        string `json:"actor"`
	ActorIsAgent bool   `json:"actorIsAgent"`
	Summary      string `json:"summary" desc:"what happened, as a sentence without the actor"`
	CardID       string `json:"cardId,omitempty"`
	CardName     string `json:"cardName,omitempty"`
	At           string `json:"at"`
}

// listActivityTypes are the stream types that say something about a column. Card edits (comments,
// labels, due dates...) carry the card's list too but are card activity, not column activity.
var listActivityTypes = []string{"card.created", "card.moved", "card.archived", "card.deleted", "list.created", "list.updated"}

func init() {
	ops.Register(ops.Op{Name: "list.activity", Summary: "Activity feed of one column: cards created, moved in or out, archived, and changes to the column itself (newest first; events older than the retention window are gone)",
		Method: "GET", Path: "/lists/{id}/activity", UI: "GET /ui/lists/{id}/activity"}, func(e *ops.Env, in ListActivityIn) ([]ListActivityItem, error) {
		l, err := requireList(e, in.ID, authz.View)
		if err != nil {
			return nil, err
		}
		if !isFinite(l.Type) {
			return nil, ops.NotFound("list") // archive and trash are system lists, not columns
		}
		limit := in.Limit
		if limit <= 0 || limit > 100 {
			limit = 20
		}
		var before int64
		if in.Before != "" {
			if before, err = events.ParseID(in.Before); err != nil {
				return nil, ops.Invalid("before must be an event id")
			}
		}
		rows, err := events.Recent(e.Ctx, e.DB, l.BoardID, l.ID, listActivityTypes, before, limit)
		if err != nil {
			return nil, err
		}
		names := map[string]string{}
		if all, err := listsOf(e, l.BoardID, false); err == nil {
			for _, x := range all {
				if x.Name != nil {
					names[x.ID] = *x.Name
				}
			}
		}
		out := make([]ListActivityItem, 0, len(rows))
		for _, r := range rows {
			var env events.Envelope
			if json.Unmarshal([]byte(r.Raw), &env) != nil {
				continue
			}
			it := ListActivityItem{ID: events.FormatID(r.Seq), Type: env.Type, Actor: env.Actor.Name, ActorIsAgent: env.Actor.IsAgent, CardID: env.CardID, At: env.At}
			if env.Card != nil {
				it.CardName = env.Card.Name
			}
			it.Summary = activitySummary(env, l.ID, it.CardName, names)
			if it.Summary != "" {
				out = append(out, it)
			}
		}
		return out, nil
	})
}

func activitySummary(env events.Envelope, listID, card string, names map[string]string) string {
	q := "a card"
	if card != "" {
		q = "“" + clipRunes(card, 60) + "”"
	}
	name := func(id string) string {
		if n := names[id]; n != "" {
			return "“" + clipRunes(n, 40) + "”"
		}
		return "another board"
	}
	switch env.Type {
	case "card.created":
		return "added " + q
	case "card.moved":
		switch {
		case env.ToListID == listID && env.FromListID != "" && env.FromListID != listID:
			return "moved " + q + " here from " + name(env.FromListID)
		case env.FromListID == listID && env.ToListID != "":
			return "moved " + q + " to " + name(env.ToListID)
		default:
			return "moved " + q
		}
	case "card.archived":
		if t, _ := env.Data["trash"].(bool); t {
			return "moved " + q + " to the trash"
		}
		return "archived " + q
	case "card.deleted":
		return "removed " + q
	case "list.created":
		return "created this column"
	case "list.updated":
		d := env.Data
		switch {
		case d["autoArchived"] != nil: // the archive-after rule ran (one summary per pass; the per-card events are quiet)
			return "auto-archived " + plural(num(d["autoArchived"]), "card") + ": the column keeps the newest " + strconv.Itoa(num(d["keep"]))
		case d["archiveAfter"] != nil && d["closes"] == nil:
			if n := num(d["archiveAfter"]); n > 0 {
				return "set auto-archive: keep the newest " + plural(n, "card")
			}
			return "turned auto-archive off"
		case d["closes"] != nil:
			on, _ := d["closes"].(bool)
			if on {
				s := "set this column to close cards that land here"
				if n := num(d["closed"]); n > 0 {
					s += " and closed the " + plural(n, "card") + " in it"
				}
				return s
			}
			s := "stopped closing cards that land here"
			if n := num(d["reopened"]); n > 0 {
				s += " and re-opened the " + plural(n, "card") + " in it"
			}
			return s
		case d["sorted"] == true:
			return "sorted the cards"
		case d["cleared"] == true:
			return "cleared the column"
		case d["targetId"] != nil:
			id, _ := d["targetId"].(string)
			return "moved all cards to " + name(id)
		case d["position"] != nil:
			return "moved the column"
		}
		return "updated this column"
	}
	return ""
}

func clipRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Initial is the avatar letter for the actor.
func (a ListActivityItem) Initial() string { return initialOf(a.Actor) }

func initialOf(name string) string {
	if r := []rune(strings.TrimSpace(name)); len(r) > 0 {
		return strings.ToUpper(string(r[0]))
	}
	return "?"
}

// num reads a count from event data (an int when fresh, a float64 once it went through JSON).
func num(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
