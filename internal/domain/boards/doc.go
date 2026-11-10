// Package boards owns boards, memberships, labels (+groups, project-wide labels),
// lists, saved filters, public boards, templates, import/export and the board
// page (kanban / list / grid views).
//
// Exported helpers for other domains:
//
//	GetBoard(ctx, q, id) (Board, error)           load a board row (no authz)
//	GetList(ctx, q, id) (List, error)             load a list row (no authz)
//	SystemList(ctx, q, boardID, "archive"|"trash")  id of a board's system list
//	EnsureSystemLists(ctx, q, boardID)            create archive+trash lists
//	PlaceBetween(ctx, q, table, scope, args, afterID, beforeID) float64
//	                                              position for a drop between two rows (rebalances)
//	Rebalance(ctx, q, table, scope, args)         renumber a scope with PositionGap spacing
//	SingleSelectConflicts(ctx, db, cardID, labelID) ([]string, error)
//	                                              label ids to remove when adding labelID (single-select groups)
//	CallByForm(env, opName, url.Values)           call another domain's op via its own input tags
//
// Realtime events (room board:<id>): board.created|updated|deleted,
// boardMembership.created|updated|deleted, label.created|updated|deleted,
// list.created|updated|moved|deleted. Event Type strings (listCreate, labelUpdate,
// ...) are not Planka card actions; notify should only log those it knows.
//
// Browser contract with the cards domain: tiles open /ui/cards/{id} into #modal;
// the add-card form posts to the UI route of op "card.create" (hidden if absent);
// drops call op "card.move" bound with form fields id, cardId, listId, position,
// afterId, beforeId, index.
package boards
