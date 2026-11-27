// Package boards owns boards, memberships, labels (+groups, project-wide labels),
// lists, saved filters, public boards, templates, import/export and the board
// page (board / list / grid views).
//
// Card positions: the cards domain owns card_placements (migration 0404); the boards writers that move cards
// (list.sort, list.moveCards, archive/delete a column, archive-after, template apply, drop positions) update the
// placements directly and never the mirrored cards columns (board_id, list_id, position, prev_list_id,
// list_changed_at), which a guard trigger refuses. See placements.go and docs/DESIGN.md "Placements".
//
// Exported helpers for other domains:
//
//	GetBoard(ctx, q, id) (Board, error)           load a board row (no authz)
//	BoardCrumb(ctx, q, id) (Crumb, error)         board + project name, for the top-bar breadcrumbs on calendar/map (no authz)
//	GetList(ctx, q, id) (List, error)             load a list row (no authz)
//	SystemList(ctx, q, boardID, "archive"|"trash")  id of a board's system list
//	EnsureSystemLists(ctx, q, boardID)            create archive+trash lists
//	PlaceBetween(ctx, q, table, scope, args, afterID, beforeID) float64
//	                                              position for a drop between two rows (rebalances)
//	Rebalance(ctx, q, table, scope, args)         renumber a scope with PositionGap spacing (not for the cards table: card
//	                                              positions live on card_placements, see placements.go)
//	PlaceCardBetween(ctx, q, listID, cardID, afterID, beforeID) (float64, error)
//	                                              PlaceBetween over the card placements of a column (rebalances them)
//	SingleSelectConflicts(ctx, db, cardID, labelID) ([]string, error)
//	                                              label ids to remove when adding labelID (single-select groups)
//	CallByForm(env, opName, url.Values)           call another domain's op via its own input tags
//	Stats(env, boardIDs) (map[string]BoardStats, error)
//	                                              at-a-glance counts for many boards (authz-filtered, constant queries)
//	ActivityLess(a, b BoardStats) bool            most recently active first, then position, then id
//	EmitAutoArchived(env, LandResult)             after a landing: the archive-after rule's card.archived events (board-only)
//	EmitClosedElsewhere(env, boardID, LandResult) after a landing: quiet card.updated isClosed for cards on several boards
//	LoadDrafts func(e, boardID) ([]Draft, error)  set by cards: the board's drafts for the Inbox column (inbox.go), one query
//	RemoveFromBoards, BeforeBoardsDelete          set by cards: take a card off boards (re-home, prune) for list.clear and
//	                                              board/project deletion (placement_hooks.go); DeleteBoards(e, ids, del) and
//	                                              DeleteProjectBoards(e, projectID, del) run the hook in the deleting tx
//
// Realtime events (room board:<id>): board.created|updated|deleted,
// boardMembership.created|updated|deleted, label.created|updated|deleted,
// list.created|updated|moved|deleted. Event Type strings (listCreate, labelUpdate,
// ...) are not Planka card actions; notify should only log those it knows.
//
// Browser contract with the cards domain: tiles open /ui/cards/{id} into #modal;
// the add-card form posts to the UI route of op "card.create" (hidden if absent);
// drops call op "card.move" bound with form fields id, cardId, listId, position,
// afterId, beforeId, index; a dropped draft (from the Inbox column) calls op "card.draft.publish" (id, listId,
// position) instead. The Inbox routes (/ui/boards/{boardId}/inbox, .../inbox/settings, .../inbox/publish) live here
// and call card.draft.create, card.inbox.set and card.draft.publish.
package boards
