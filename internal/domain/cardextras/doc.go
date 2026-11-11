// Package cardextras owns everything attached to a card besides the card itself:
// task lists and tasks, comments, attachments (thumbnails, covers, media view),
// custom fields (groups, fields, values, repeatable rows), time tracking and the
// activity (actions) feeds, plus the htmx fragments loaded into the card modal.
//
// Exported Go API for other domains:
//
//	RecordAction(e *ops.Env, ev ops.Event) (id string, err error)
//	    Writes the activity (actions) row for ev. Called by notify's ops.EmitHook.
//	CardBadges(e *ops.Env, cardIDs []string) (map[string]Badges, error)
//	    Face-of-card summary: task/attachment/comment counts, tracked time, task lists
//	    and custom-field values flagged "show on front of card". No permission check.
//	ActionText(a ActionItem) string
//	    Human sentence for an activity row (shared with notification text).
//
// UI fragments (GET, public routes; ops enforce access, public boards render read-only):
// /ui/cards/{id}/tasks | comments | attachments | fields | time | activity and
// /ui/boards/{id}/activity. Each fragment is one `.x-sec` root that swaps itself.
// The card modal shell should embed them with
// <div hx-get="/ui/cards/{id}/tasks" hx-trigger="load"></div>.
// Other routes: GET /boards/{boardId}/media, GET /attachments/{id}/download/{name},
// GET /attachments/{id}/thumbnail.
package cardextras
