// Package cards owns cards: CRUD, move/duplicate/close/archive, cover, due date,
// types, memberships, labels, subscriptions, drafts + board inbox, recurring
// cards (scheduler), project-wide card templates, share links, calendar and map
// views, collaborative-edit locks and the card modal shell. Migrations: 0400-0499.
//
// Exported Go API for other domains:
//
//	LoadCard(e, id) (Card, error)                       card + labels/members/subscription, no authz
//	RenderMarkdown(src) template.HTML                   markdown-lite renderer (HTML-escaped, safe)
//	RunRecurrence(ctx, db, hub, now) (int, error)       one scheduler sweep (also run every minute via server.Hooks)
//	DuplicateHooks []func(e, tx, srcID, dstID) error    cardextras registers here to copy tasks/attachments/
//	                                                    custom fields when a card is duplicated, spawned by
//	                                                    recurrence or created from a template
//	MarkNotificationsRead func(e, cardID) (int64, error) notify may replace the default implementation
//	IdleAfter                                           edit-lock idle threshold (60s)
//
// Contract with boards: card rows with is_draft=1 or is_template=1 must be excluded from
// list/kanban rendering (card.list already does). Card tiles open the modal with
// hx-get="/ui/cards/{id}" hx-target="#modal". The modal lazy-loads /ui/cards/{id}/tasks,
// /comments, /attachments, /fields, /time and /activity (cardextras). Realtime events:
// card.created|updated|deleted|moved|member.added|member.removed|label.added|label.removed|editing.
package cards
