package db

// LabelOnBoard is the SQL condition "label alias l is shown on board b" shared by every reader of card labels
// (tiles, snapshots, card.get, card.options, exports, the calendar): a card on several boards keeps its card_labels
// rows card-level, but each board shows only its own vocabulary, the labels of that board plus the labels of its
// project (docs/DESIGN.md "Placements"). Readers deliberately do NOT apply the use_project_labels switch (that is
// the write rule, cards.labelAllowed): project labels already on cards stay visible after the switch is turned off,
// as before. b is an SQL expression for the board id (a column such as "p.board_id" or a numbered parameter such
// as "?1"; a plain "?" would need two arguments).
func LabelOnBoard(l, b string) string {
	return "(" + l + ".board_id=" + b + " OR " + l + ".project_id=(SELECT project_id FROM boards WHERE id=" + b + "))"
}
