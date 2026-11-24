package boards

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// board.template.apply: lay a template over a board that already exists (docs/DESIGN.md "Board templates").
//
//   - The template's columns (active and closing ones, with their names, colours, descriptions, WIP limits and
//     close/archive settings) are added to the LEFT of the board.
//   - Every card in an active or closing column moves to the end of the target column, the first template column that
//     does not close cards, in board order (old column order, then position). A closed card stays closed
//     (LandedKeepClosed). Archive and trash are not touched.
//   - The board's old columns, now empty, are removed in the same transaction (removeOldColumns, default true; false
//     keeps them, emptied, to the right). Removing a column would cascade-delete every card row still on it, so the
//     inbox drafts and card templates parked there (they need a real list_id) are re-pointed to the target first, and
//     the delete refuses to run while any card row is left on an old column. Their settings and watches go with them;
//     archived/trashed cards whose prev_list_id named one restore into the board's first open column (card.restore).
//   - Labels and label groups are only ever added: a template label whose name matches a label of the board (trimmed,
//     case-insensitive; unnamed labels by colour) is reused exactly as it is.
//   - Custom-field groups are only added: a template group whose name exists on the board is skipped whole.
//   - Board settings only when applySettings (never the agent prompt); sample cards only when includeSampleCards.
//   - Archive after N on the target column: the bulk move never archives anything. When the cards that land in the
//     target would exceed the template's N, the target column is created WITHOUT the setting (otherwise the next
//     ordinary landing would archive the whole excess at once) and the result says so in a warning.
//
// A dry run runs exactly the same code inside the same transaction and rolls it back, so the preview and the real
// apply cannot drift.

// TemplateApplyIn is the input of board.template.apply.
type TemplateApplyIn struct {
	ID                 string `path:"id"`
	TemplateID         string `json:"templateId" required:"true" desc:"builtin:<slug> or the id of a published template board (see board.template.list)"`
	DryRun             bool   `json:"dryRun" desc:"preview only: return exactly what would change and write nothing"`
	Confirm            bool   `json:"confirm" desc:"must be true to apply (not needed for dryRun). Every card moves; there is no automatic undo, so export the board first (board.export)"`
	ApplySettings      bool   `json:"applySettings" desc:"also apply the template's board settings: colour, card colouring, card counter, card ages, task lists, card creator, inbox, default view (default false; the agent prompt is never applied)"`
	IncludeSampleCards bool   `json:"includeSampleCards" desc:"also add the template's sample cards to its columns (default false)"`
	RemoveOldColumns   *bool  `json:"removeOldColumns" desc:"remove the board's old columns once their cards have moved (default true: they are empty by then; their WIP/auto-close/auto-archive settings and column watches go with them). false keeps them, emptied, to the right"`
}

// removeOld is the effective removeOldColumns (default true).
func (in TemplateApplyIn) removeOld() bool { return in.RemoveOldColumns == nil || *in.RemoveOldColumns }

// ApplyColumn is one column the apply adds (the id is empty on a dry run).
type ApplyColumn struct {
	ID           string `json:"id,omitempty"`
	Name         string `json:"name"`
	Closes       bool   `json:"closes,omitempty" desc:"closes cards that land here"`
	WIPLimit     int    `json:"wipLimit,omitempty"`
	ArchiveAfter int    `json:"archiveAfter,omitempty" desc:"auto-archive: keeps the newest N cards"`
}

// TemplateApplyOut is the preview (dryRun) or the result of board.template.apply.
type TemplateApplyOut struct {
	DryRun               bool          `json:"dryRun"`
	TemplateID           string        `json:"templateId"`
	TemplateName         string        `json:"templateName"`
	ColumnsAdded         []ApplyColumn `json:"columnsAdded" desc:"the template's columns, added left of the existing ones, in order"`
	TargetColumn         ApplyColumn   `json:"targetColumn" desc:"where every card moves: the first template column that does not close cards"`
	RemoveOldColumns     bool          `json:"removeOldColumns" desc:"whether the board's old columns are (or would be) removed"`
	ColumnsRemoved       []string      `json:"columnsRemoved" desc:"the board's old columns, removed after their cards moved (removeOldColumns, the default)"`
	ColumnsKept          []string      `json:"columnsKept" desc:"the board's old columns, kept (emptied) to the right (removeOldColumns=false)"`
	DuplicateColumnNames []string      `json:"duplicateColumnNames,omitempty" desc:"template columns named like an old column that is kept (added again)"`
	CardsMoved           int           `json:"cardsMoved" desc:"cards that move (or would move) into the target column"`
	ClosedCardsMoved     int           `json:"closedCardsMoved" desc:"of those, cards that are closed and stay closed"`
	CardsAutoArchived    int           `json:"cardsAutoArchived" desc:"cards the archive-after rule archived (only ever sample cards; moved cards are never archived)"`
	SampleCardsAdded     int           `json:"sampleCardsAdded"`
	LabelsAdded          []string      `json:"labelsAdded"`
	LabelsReused         []string      `json:"labelsReused" desc:"template labels that match a label of the board by name (unnamed: by colour); used as they are"`
	LabelsSkipped        []string      `json:"labelsSkipped,omitempty" desc:"labels not added: the project uses project-wide labels and the caller does not manage the project"`
	LabelGroupsAdded     []string      `json:"labelGroupsAdded"`
	FieldGroupsAdded     []string      `json:"fieldGroupsAdded"`
	FieldGroupsSkipped   []string      `json:"fieldGroupsSkipped" desc:"custom-field groups skipped whole because the board has a group with the same name"`
	SettingsChanged      []string      `json:"settingsChanged" desc:"board settings changed (only with applySettings)"`
	SettingsAvailable    []string      `json:"settingsAvailable" desc:"board settings that applySettings would change"`
	Warnings             []string      `json:"warnings"`
}

// errApplyDryRun rolls the dry run's transaction back.
var errApplyDryRun = errors.New("dry run")

// applyHook lets tests fail the transaction at a named stage (atomicity); nil in production.
var applyHook func(stage string) error

func hookStage(stage string) error {
	if applyHook != nil {
		return applyHook(stage)
	}
	return nil
}

func registerTemplateApplyOp() {
	ops.Register(ops.Op{Name: "board.template.apply", Summary: "Apply a built-in or published template to an existing board (board manager): adds the template's columns on the left, moves every card into its first open column (closed cards stay closed), removes the old, now empty columns (removeOldColumns=false keeps them), adds missing labels, label groups and custom-field groups (never replaces), optionally the board settings and sample cards. dryRun previews; confirm=true applies. No automatic undo",
		Method: "POST", Path: "/boards/{id}/template/apply", UI: ui("POST", "/boards/{id}/template/apply")}, templateApply)
}

func labelKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func templateApply(e *ops.Env, in TemplateApplyIn) (TemplateApplyOut, error) {
	// the board first: an outsider gets "board not found" whatever the template id
	if err := authz.RequireBoard(e, in.ID, authz.Manage); err != nil {
		return TemplateApplyOut{}, err
	}
	if strings.TrimSpace(in.TemplateID) == "" {
		return TemplateApplyOut{}, ops.Invalid("templateId is required")
	}
	if in.TemplateID == in.ID {
		return TemplateApplyOut{}, ops.Invalid("a board cannot be applied to itself")
	}
	if !in.DryRun && !in.Confirm {
		return TemplateApplyOut{}, ops.Invalid("applying a template moves every card of the board (and by default removes the old columns) and cannot be undone automatically: preview it with dryRun, export the board, then resend with confirm=true")
	}
	t, err := loadTemplate(e, in.TemplateID, true)
	if err != nil {
		return TemplateApplyOut{}, err
	}
	pid, useProject := projectOfBoard(e, in.ID)
	canProjectLabels := !useProject || authz.ProjectLevel(e, pid) >= authz.Manage

	var out TemplateApplyOut
	var ev applyEvents
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		var err error
		out, ev, err = writeApply(e, tx, in, t, pid, useProject, canProjectLabels)
		if err != nil {
			return err
		}
		if in.DryRun {
			return errApplyDryRun
		}
		return nil
	})
	if in.DryRun && errors.Is(err, errApplyDryRun) {
		out.DryRun = true
		for i := range out.ColumnsAdded {
			out.ColumnsAdded[i].ID = ""
		}
		out.TargetColumn.ID = ""
		return out, nil
	}
	if err != nil {
		return TemplateApplyOut{}, err
	}
	emitApply(e, in.ID, out, ev)
	return out, nil
}

// applyEvents is what the commit announces.
type applyEvents struct {
	lists    []string
	removed  []string  // old columns deleted after the move
	moved    []Landing // card -> the column it left
	target   string
	samples  []Landing
	labels   []string
	landing  LandResult
	settings bool
}

func writeApply(e *ops.Env, tx *sql.Tx, in TemplateApplyIn, t tpl, projectID string, useProject, canProjectLabels bool) (TemplateApplyOut, applyEvents, error) {
	ctx, now, nowT := e.Ctx, db.Now(), time.Now().UTC()
	boardID := in.ID
	removeOld := in.removeOld()
	out := TemplateApplyOut{TemplateID: t.info.ID, TemplateName: t.info.Name, RemoveOldColumns: removeOld, ColumnsAdded: []ApplyColumn{}, ColumnsRemoved: []string{}, ColumnsKept: []string{}, LabelsAdded: []string{},
		LabelsReused: []string{}, LabelGroupsAdded: []string{}, FieldGroupsAdded: []string{}, FieldGroupsSkipped: []string{}, SettingsChanged: []string{},
		SettingsAvailable: []string{}, Warnings: []string{}}
	var ev applyEvents
	ib := t.ib

	// ---- the template's columns and the target ----
	var cols []impList
	for _, l := range ib.Lists {
		if l.Type == "archive" || l.Type == "trash" {
			continue
		}
		cols = append(cols, l)
	}
	target := -1
	for i, l := range cols {
		if l.Type != "closed" {
			target = i
			break
		}
	}
	if len(cols) == 0 {
		return out, ev, ops.Invalid("this template has no columns")
	}
	if target < 0 {
		return out, ev, ops.Invalid("this template has no column that keeps cards open (every column closes cards), so the board's cards would all be closed: pick another template")
	}
	if target > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("The template's first column, %s, closes cards: the cards move to %s, the first column that keeps them open.", cols[0].Name, cols[target].Name))
	}

	// ---- the board as it is ----
	b, err := GetBoard(ctx, tx, boardID)
	if err != nil {
		return out, ev, err
	}
	var existing []oldCol
	rows, err := tx.QueryContext(ctx, `SELECT id,COALESCE(name,''),type,wip_limit,archive_after,COALESCE(description,'')<>'' FROM lists WHERE board_id=? AND type IN ('active','closed') ORDER BY position, id`, boardID)
	if err != nil {
		return out, ev, err
	}
	for rows.Next() {
		var c oldCol
		if err := rows.Scan(&c.id, &c.name, &c.typ, &c.wip, &c.keep, &c.brief); err != nil {
			rows.Close()
			return out, ev, err
		}
		existing = append(existing, c)
	}
	rows.Close()
	if removeOld {
		for _, c := range existing {
			out.ColumnsRemoved = append(out.ColumnsRemoved, c.name)
		}
		if err := removalWarnings(e, tx, existing, cols[target].Name, &out); err != nil {
			return out, ev, err
		}
	} else {
		names := map[string]bool{}
		for _, c := range existing {
			out.ColumnsKept = append(out.ColumnsKept, c.name)
			names[labelKey(c.name)] = true
		}
		for _, l := range cols {
			if names[labelKey(l.Name)] {
				out.DuplicateColumnNames = append(out.DuplicateColumnNames, l.Name)
			}
		}
		if len(out.DuplicateColumnNames) > 0 {
			out.Warnings = append(out.Warnings, "These columns are added again next to the existing ones with the same name: "+strings.Join(out.DuplicateColumnNames, ", ")+".")
		}
	}

	// the cards that move, in board order (old column order, then position)
	type moving struct {
		id, from string
		closed   bool
	}
	var move []moving
	// every card placed on this board (whichever board is its home), with this board's column and position
	rows, err = tx.QueryContext(ctx, `SELECT c.id,p.list_id,c.is_closed FROM card_placements p JOIN cards c ON c.id=p.card_id JOIN lists l ON l.id=p.list_id
		WHERE p.board_id=? AND c.is_draft=0 AND c.is_template=0 AND l.board_id=? AND l.type IN ('active','closed') ORDER BY l.position, l.id, p.position, c.id`, boardID, boardID)
	if err != nil {
		return out, ev, err
	}
	for rows.Next() {
		var m moving
		if err := rows.Scan(&m.id, &m.from, &m.closed); err != nil {
			rows.Close()
			return out, ev, err
		}
		move = append(move, m)
	}
	rows.Close()
	out.CardsMoved = len(move)
	for _, m := range move {
		if m.closed {
			out.ClosedCardsMoved++
		}
	}

	// sample cards (counted for the target's archive-after decision)
	var samples []impCard
	if in.IncludeSampleCards {
		samples = ib.Cards
	}
	known := map[string]bool{}
	for _, l := range cols {
		known[l.Key] = true
	}
	inTarget := len(move)
	for _, c := range samples {
		if c.ListKey == cols[target].Key || !known[c.ListKey] {
			inTarget++
		}
	}
	// Archive after N on the target: never archive the user's cards on an apply.
	if n := importWIP(cols[target].ArchiveAfter); n != nil && inTarget > n.(int) {
		out.Warnings = append(out.Warnings, fmt.Sprintf("In the template, %s keeps the newest %d cards. %d cards land there, so auto-archive is left off on it: nothing is archived. Turn it on in the column's settings when you want it (it then archives the oldest cards over the limit at once).",
			cols[target].Name, n.(int), inTarget))
		cols[target].ArchiveAfter = 0
	}

	// ---- columns: template ones at 1..n, existing ones after them in their order (until they are removed) ----
	wl, err := writeLists(ctx, tx, boardID, cols, now, func(i int, _ impList) float64 { return float64(i+1) * db.PositionGap })
	if err != nil {
		return out, ev, err
	}
	for j, c := range existing {
		if _, err := tx.ExecContext(ctx, `UPDATE lists SET position=? WHERE id=?`, float64(len(cols)+j+1)*db.PositionGap, c.id); err != nil {
			return out, ev, err
		}
	}
	for i, l := range cols {
		ac := ApplyColumn{ID: wl.order[i], Name: l.Name, Closes: l.Type == "closed"}
		if v := importWIP(l.WIP); v != nil {
			ac.WIPLimit = v.(int)
		}
		if v := importWIP(l.ArchiveAfter); v != nil {
			ac.ArchiveAfter = v.(int)
		}
		out.ColumnsAdded = append(out.ColumnsAdded, ac)
	}
	out.TargetColumn = out.ColumnsAdded[target]
	targetID := wl.order[target]
	wl.first = targetID // cards of unknown columns (samples) go to the target
	ev.lists, ev.target = wl.order, targetID
	if err := hookStage("columns"); err != nil {
		return out, ev, err
	}

	// ---- move the cards: appended to the target in board order, one UPDATE per chunk ----
	lands := make([]Landing, 0, len(move))
	for i := 0; i < len(move); i += archiveChunk {
		chunk := move[i:min(i+archiveChunk, len(move))]
		in := "(?" + strings.Repeat(",?", len(chunk)-1) + ")"
		args := make([]any, 0, 3+5*len(chunk))
		args = append(args, targetID, now)
		for _, m := range chunk {
			args = append(args, m.id, m.from)
		}
		for k, m := range chunk {
			args = append(args, m.id, float64(i+k+1)*db.PositionGap)
		}
		args = append(args, boardID)
		ids := make([]any, 0, len(chunk))
		for _, m := range chunk {
			ids = append(ids, m.id)
		}
		// the cards' placements on this board move (the mirror updates the cards columns), then updated_at
		if _, err := tx.ExecContext(ctx, `UPDATE card_placements SET list_id=?,list_changed_at=?,prev_list_id=CASE card_id`+strings.Repeat(" WHEN ? THEN ?", len(chunk))+
			` END,position=CASE card_id`+strings.Repeat(" WHEN ? THEN ?", len(chunk))+` END WHERE board_id=? AND card_id IN `+in, append(args, ids...)...); err != nil {
			return out, ev, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET updated_at=? WHERE id IN `+in, append([]any{now}, ids...)...); err != nil {
			return out, ev, err
		}
	}
	for _, m := range move {
		lands = append(lands, Landing{CardID: m.id, ListID: targetID})
		ev.moved = append(ev.moved, Landing{CardID: m.id, ListID: m.from})
	}
	if _, err := LandedKeepClosed(ctx, tx, lands...); err != nil {
		return out, ev, err
	}
	if err := hookStage("cards"); err != nil {
		return out, ev, err
	}

	// ---- the old columns: removed, now that they are empty ----
	if removeOld && len(existing) > 0 {
		if err := removeOldColumns(ctx, tx, existing, targetID, now); err != nil {
			return out, ev, err
		}
		for _, c := range existing {
			ev.removed = append(ev.removed, c.id)
		}
		if err := hookStage("removed"); err != nil {
			return out, ev, err
		}
	}

	// ---- labels: only added ----
	labels, err := mergeLabels(e, tx, boardID, projectID, useProject, canProjectLabels, ib, now, &out, &ev)
	if err != nil {
		return out, ev, err
	}
	if err := hookStage("labels"); err != nil {
		return out, ev, err
	}

	// ---- sample cards ----
	var wc writtenCards
	if len(samples) > 0 {
		base := map[string]float64{}
		for _, id := range wl.order {
			base[id] = maxPos(ctx, tx, "card_placements", "list_id=?", id)
		}
		if wc, err = writeCards(e, tx, boardID, samples, wl, labels, nowT, now, base); err != nil {
			return out, ev, err
		}
		res, err := Landed(ctx, tx, wc.lands...)
		if err != nil {
			return out, ev, err
		}
		ev.landing = res
		out.CardsAutoArchived = res.ArchivedCount()
		out.SampleCardsAdded = len(wc.lands)
		ev.samples = wc.lands
	}

	// ---- custom-field groups: only added; a same-named board group skips the template's whole group ----
	if len(ib.FieldGroups) > 0 && ImportCustomFields != nil && ExportCustomFields != nil {
		have, _, err := ExportCustomFields(ctx, tx, boardID, false)
		if err != nil {
			return out, ev, err
		}
		taken := map[string]bool{}
		for _, g := range have {
			if g.Card == "" {
				taken[labelKey(g.Name)] = true
			}
		}
		var add []FieldGroupDoc
		for _, g := range ib.FieldGroups {
			if g.Card != "" { // a card-level group travels with its sample card only
				if in.IncludeSampleCards {
					add = append(add, g)
				}
				continue
			}
			if strings.TrimSpace(g.Name) == "" {
				continue
			}
			if taken[labelKey(g.Name)] {
				out.FieldGroupsSkipped = append(out.FieldGroupsSkipped, g.Name)
				continue
			}
			taken[labelKey(g.Name)] = true
			out.FieldGroupsAdded = append(out.FieldGroupsAdded, g.Name)
			add = append(add, g)
		}
		if len(add) > 0 {
			if err := ImportCustomFields(ctx, tx, boardID, add, wc.values, wc.ids); err != nil {
				return out, ev, err
			}
		}
	}

	// ---- board settings: only when asked, never the agent prompt ----
	out.SettingsAvailable = settingsDiff(b, ib)
	if in.ApplySettings && len(out.SettingsAvailable) > 0 {
		if err := applyBoardSettings(ctx, tx, b, ib, now); err != nil {
			return out, ev, err
		}
		out.SettingsChanged = out.SettingsAvailable
		ev.settings = true
	}
	if err := hookStage("done"); err != nil {
		return out, ev, err
	}
	if out.ClosedCardsMoved > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s closed: closed cards stay closed (with the check mark) in %s.", map[bool]string{true: "1 moved card is", false: strconv.Itoa(out.ClosedCardsMoved) + " moved cards are"}[out.ClosedCardsMoved == 1], out.TargetColumn.Name))
	}
	return out, ev, nil
}

// oldCol is a column the board had before the apply.
type oldCol struct {
	id, name, typ string
	wip, keep     sql.NullInt64
	brief         bool
}

// removeOldColumns deletes the board's old (emptied) columns. cards.list_id and card_placements.list_id cascade, so the rows that may still sit on
// them are dealt with first: inbox drafts and card templates (they are hidden from the columns but need a real list)
// move to the target column, appended in their order. Then nothing may be left: any other card row on an old column
// fails the whole apply instead of being deleted with it. list_subscriptions (column watches) cascade away.
func removeOldColumns(ctx context.Context, tx *sql.Tx, old []oldCol, targetID, now string) error {
	ph := "?" + strings.Repeat(",?", len(old)-1)
	ids := make([]any, len(old))
	for i, c := range old {
		ids[i] = c.id
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id FROM card_placements p JOIN cards c ON c.id=p.card_id JOIN lists l ON l.id=p.list_id WHERE p.list_id IN (`+ph+`) AND (c.is_draft=1 OR c.is_template=1)
		ORDER BY l.position, p.position, c.id`, ids...)
	if err != nil {
		return err
	}
	var parked []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		parked = append(parked, id)
	}
	rows.Close()
	base := maxPos(ctx, tx, "card_placements", "list_id=?", targetID)
	for i, id := range parked {
		// drafts and card templates have exactly one placement (their home); move it, then updated_at
		if _, err := tx.ExecContext(ctx, `UPDATE card_placements SET list_id=?,position=? WHERE card_id=? AND is_home=1`, targetID, base+float64(i+1)*db.PositionGap, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET updated_at=? WHERE id=?`, now, id); err != nil {
			return err
		}
	}
	var left int
	// any placement (a card homed on another board included: its placement here would cascade away with the column)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_placements WHERE list_id IN (`+ph+`)`, ids...).Scan(&left); err != nil {
		return err
	}
	if left > 0 { // never let the delete cascade a card away
		return fmt.Errorf("board.template.apply: %d card rows still on the old columns; nothing was changed", left)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM lists WHERE id IN (`+ph+`)`, ids...)
	return err
}

// removalWarnings says what goes with the old columns: their own settings, the column watches, listeners on them, and
// where archived/trashed cards that came from them restore to.
func removalWarnings(e *ops.Env, tx *sql.Tx, old []oldCol, firstOpen string, out *TemplateApplyOut) error {
	if len(old) == 0 {
		return nil
	}
	ctx := e.Ctx
	var settings []string
	for _, c := range old {
		var s []string
		if c.typ == "closed" {
			s = append(s, "closes cards")
		}
		if c.wip.Valid && c.wip.Int64 > 0 {
			s = append(s, "WIP limit "+strconv.FormatInt(c.wip.Int64, 10))
		}
		if c.keep.Valid && c.keep.Int64 > 0 {
			s = append(s, "keeps the newest "+strconv.FormatInt(c.keep.Int64, 10))
		}
		if c.brief {
			s = append(s, "its description")
		}
		if len(s) > 0 {
			settings = append(settings, c.name+" ("+strings.Join(s, ", ")+")")
		}
	}
	if len(settings) > 0 {
		out.Warnings = append(out.Warnings, "The old columns' own settings go with them: "+strings.Join(settings, "; ")+".")
	}
	ph := "?" + strings.Repeat(",?", len(old)-1)
	ids := make([]any, len(old))
	for i, c := range old {
		ids[i] = c.id
	}
	var watchers, fromOld int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT user_id) FROM list_subscriptions WHERE list_id IN (`+ph+`)`, ids...).Scan(&watchers); err != nil {
		return err
	}
	if watchers > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s the old columns; those column watches end (card and board watches stay).", map[bool]string{true: "1 person watches", false: strconv.Itoa(watchers) + " people watch"}[watchers == 1]))
	}
	if e.Hub != nil && e.Hub.Listeners != nil {
		var on []string
		for _, c := range old {
			for _, l := range e.Hub.Listeners.ForList(c.id) {
				on = append(on, l.Label+" on "+c.name)
			}
		}
		if len(on) > 0 {
			out.Warnings = append(out.Warnings, "Listening on an old column right now: "+strings.Join(on, ", ")+". Its stream sees the cards leave and the column deleted, then nothing more: point it at a new column.")
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_placements p JOIN lists l ON l.id=p.list_id WHERE l.type IN ('archive','trash') AND p.prev_list_id IN (`+ph+`)`, ids...).Scan(&fromOld); err != nil {
		return err
	}
	if fromOld > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s in the archive or trash came from the old columns; restored, %s into %s, the board's first open column.",
			map[bool]string{true: "1 card", false: strconv.Itoa(fromOld) + " cards"}[fromOld == 1], map[bool]string{true: "it goes", false: "they go"}[fromOld == 1], firstOpen))
	}
	return nil
}

// mergeLabels adds the template's labels that the board does not have yet and returns template key -> label id.
// Nothing that exists is renamed, recoloured, regrouped or deleted.
func mergeLabels(e *ops.Env, tx *sql.Tx, boardID, projectID string, useProject, canProject bool, ib impBoard, now string, out *TemplateApplyOut, ev *applyEvents) (map[string]string, error) {
	ctx := e.Ctx
	byName, byColor := map[string]string{}, map[string]string{}
	q, args := `SELECT id,COALESCE(name,''),color FROM labels WHERE board_id=?`, []any{boardID}
	if useProject {
		q += ` OR (project_id=? AND board_id IS NULL)`
		args = append(args, projectID)
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY position,id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, name, color string
		if err := rows.Scan(&id, &name, &color); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.TrimSpace(name) == "" {
			if h, ok := ResolveColor(color); ok && byColor[h] == "" {
				byColor[h] = id
			}
		} else if k := labelKey(name); byName[k] == "" {
			byName[k] = id
		}
	}
	rows.Close()
	// the board's label groups by name (board labels only: project-wide labels have no board groups)
	groupByName := map[string]string{}
	if !useProject {
		rows, err := tx.QueryContext(ctx, `SELECT id,name FROM label_groups WHERE board_id=? ORDER BY position,id`, boardID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, err
			}
			if k := labelKey(name); groupByName[k] == "" {
				groupByName[k] = id
			}
		}
		rows.Close()
	}
	tplGroup := map[string]impGroup{}
	for _, g := range ib.Groups {
		tplGroup[g.Key] = g
	}
	display := func(l impLabel, color string) string {
		if strings.TrimSpace(l.Name) != "" {
			return strings.TrimSpace(l.Name)
		}
		return "(unnamed " + color + ")"
	}
	labels := map[string]string{}
	for _, l := range ib.Labels {
		color := importLabelColor(l)
		named := strings.TrimSpace(l.Name) != ""
		var match string
		if named {
			match = byName[labelKey(l.Name)]
		} else if h, ok := ResolveColor(color); ok {
			match = byColor[h]
		}
		if match != "" {
			labels[l.Key] = match
			out.LabelsReused = append(out.LabelsReused, display(l, color))
			continue
		}
		if useProject && !canProject {
			out.LabelsSkipped = append(out.LabelsSkipped, display(l, color))
			continue
		}
		id := db.NewID()
		if useProject {
			if _, err := tx.ExecContext(ctx, `INSERT INTO labels(id,project_id,name,color,position,created_at) VALUES(?,?,?,?,?,?)`, id, projectID, nullStr(strings.TrimSpace(l.Name)), color,
				appendPos(ctx, tx, "labels", "project_id=? AND board_id IS NULL", projectID), now); err != nil {
				return nil, err
			}
		} else {
			var grp any
			if g, ok := tplGroup[l.Group]; ok && strings.TrimSpace(g.Name) != "" {
				gid := groupByName[labelKey(g.Name)]
				if gid == "" {
					gid = db.NewID()
					if _, err := tx.ExecContext(ctx, `INSERT INTO label_groups(id,board_id,name,single_select,position) VALUES(?,?,?,?,?)`, gid, boardID, strings.TrimSpace(g.Name), boolInt(g.SingleSelect),
						appendPos(ctx, tx, "label_groups", "board_id=?", boardID)); err != nil {
						return nil, err
					}
					groupByName[labelKey(g.Name)] = gid
					out.LabelGroupsAdded = append(out.LabelGroupsAdded, strings.TrimSpace(g.Name))
				}
				grp = gid
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO labels(id,board_id,group_id,name,color,position,created_at) VALUES(?,?,?,?,?,?,?)`, id, boardID, grp, nullStr(strings.TrimSpace(l.Name)), color,
				appendPos(ctx, tx, "labels", "board_id=?", boardID), now); err != nil {
				return nil, err
			}
		}
		labels[l.Key] = id
		ev.labels = append(ev.labels, id)
		out.LabelsAdded = append(out.LabelsAdded, display(l, color))
		// a second template label with the same name (or unnamed colour) reuses this one
		if named {
			byName[labelKey(l.Name)] = id
		} else if h, ok := ResolveColor(color); ok {
			byColor[h] = id
		}
	}
	if len(out.LabelsSkipped) > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("This project uses project-wide labels, which only project managers can add: %s not added (%s).", plural(len(out.LabelsSkipped), "label"), strings.Join(out.LabelsSkipped, ", ")))
	}
	return labels, nil
}

// settingsDiff names the board settings the template would change. Only what the template states counts: a built-in
// that does not mention card ages or the inbox leaves them as they are.
func settingsDiff(b Board, ib impBoard) []string {
	out := []string{}
	st := func(k string) bool { return ib.Stated[k] }
	if ib.Color != "" && (b.Color == nil || !strings.EqualFold(*b.Color, ib.Color)) {
		out = append(out, "board colour")
	}
	if st("cardColorMode") && ib.CardColorMode != "" && ib.CardColorMode != b.CardColorMode {
		out = append(out, "card colouring")
	}
	f := ib.Flags
	onOff := func(name string, on bool) string {
		if on {
			return name + " on"
		}
		return name + " off"
	}
	if st("showCardCounter") && f.CardCounter != b.ShowCardCounter {
		out = append(out, onOff("card counter", f.CardCounter))
	}
	if st("showCardAges") && f.CardAges != b.ShowCardAges {
		out = append(out, onOff("card ages", f.CardAges))
	}
	if st("expandTaskListsByDefault") && f.ExpandTasks != b.ExpandTaskListsByDefault {
		out = append(out, onOff("expand task lists", f.ExpandTasks))
	}
	if st("alwaysDisplayCardCreator") && f.AlwaysCreator != b.AlwaysDisplayCardCreator {
		out = append(out, onOff("always show card creator", f.AlwaysCreator))
	}
	if st("hasInbox") && f.Inbox && !b.HasInbox { // the inbox is only ever turned on: turning it off would hide the drafts
		out = append(out, onOff("board inbox", true))
	}
	if validView(ib.View) && ib.View != b.DefaultView {
		out = append(out, "default view")
	}
	return out
}

// applyBoardSettings writes the template's board settings on an existing board: only what the template states (a
// colour or view it does not have leaves the board's own), the inbox is never turned off, the agent prompt never
// touched.
func applyBoardSettings(ctx context.Context, tx *sql.Tx, b Board, ib impBoard, now string) error {
	st := func(k string) bool { return ib.Stated[k] }
	color := b.Color
	if ib.Color != "" {
		color = &ib.Color
	}
	mode := b.CardColorMode
	if st("cardColorMode") && ib.CardColorMode != "" {
		mode = ib.CardColorMode
	}
	view := b.DefaultView
	if validView(ib.View) {
		view = ib.View
	}
	f := ib.Flags
	pick := func(k string, tpl, cur bool) bool {
		if st(k) {
			return tpl
		}
		return cur
	}
	counter := pick("showCardCounter", f.CardCounter, b.ShowCardCounter)
	ages := pick("showCardAges", f.CardAges, b.ShowCardAges)
	expand := pick("expandTaskListsByDefault", f.ExpandTasks, b.ExpandTaskListsByDefault)
	creator := pick("alwaysDisplayCardCreator", f.AlwaysCreator, b.AlwaysDisplayCardCreator)
	inbox := b.HasInbox || (st("hasInbox") && f.Inbox)
	_, err := tx.ExecContext(ctx, `UPDATE boards SET color=?,card_color_mode=?,always_display_card_creator=?,expand_task_lists_by_default=?,show_card_counter=?,show_card_ages=?,has_inbox=?,default_view=?,updated_at=? WHERE id=?`,
		color, mode, boolInt(creator), boolInt(expand), boolInt(counter), boolInt(ages), boolInt(inbox), view, now, b.ID)
	return err
}

// emitApply announces a committed apply: the new columns (list.created), every moved card (quiet card.moved with
// auto "applyTemplate": agents, webhooks and the stream see it, watchers get no notification per card), each removed
// old column (quiet list.deleted, auto "applyTemplate"), the sample
// cards (quiet card.created), the new labels, what the archive-after rule did to sample cards, and one board.updated
// summary (the board Activity row and the live refresh of open boards).
func emitApply(e *ops.Env, boardID string, out TemplateApplyOut, ev applyEvents) {
	for _, id := range ev.lists {
		emit(e, "listCreate", "list.created", boardID, map[string]any{"id": id, "auto": "applyTemplate"})
	}
	for _, m := range ev.moved {
		emitCard(e, "cardMove", "card.moved", "", boardID, m.CardID, map[string]any{"fromListId": m.ListID, "toListId": ev.target, "auto": "applyTemplate", "templateId": out.TemplateID, "quiet": true})
	}
	for _, id := range ev.removed { // after the moves, so a listener on an old column sees its cards leave first
		emit(e, "listDelete", "list.deleted", boardID, map[string]any{"id": id, "auto": "applyTemplate", "templateId": out.TemplateID, "quiet": true})
	}
	for _, id := range ev.samples { // id: the sample card and its column
		emitCard(e, "cardCreate", "card.created", "", boardID, id.CardID, map[string]any{"listId": id.ListID, "auto": "applyTemplate", "templateId": out.TemplateID, "quiet": true})
	}
	for _, id := range ev.labels {
		if l, err := scanLabel(e.DB.QueryRowContext(e.Ctx, `SELECT `+labelCols+` FROM labels WHERE id=?`, id)); err == nil {
			emitLabel(e, "labelCreate", "label.created", l)
		}
	}
	EmitAutoArchived(e, ev.landing)
	emit(e, "boardUpdate", "board.updated", boardID, map[string]any{"id": boardID, "appliedTemplate": out.TemplateName, "templateId": out.TemplateID,
		"columnsAdded": len(out.ColumnsAdded), "columnsRemoved": len(ev.removed), "cardsMoved": out.CardsMoved, "targetListId": ev.target, "labelsAdded": len(out.LabelsAdded),
		"sampleCards": out.SampleCardsAdded, "settings": out.SettingsChanged})
}
