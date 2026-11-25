package cards

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

// Label and Member are the compact references shown in the archive table.
type Label struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type Member struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsAgent bool   `json:"isAgent"`
}

type ArchiveListIn struct {
	BoardID    string `path:"boardId" required:"true" desc:"board whose archive or trash to list"`
	Kind       string `query:"kind" desc:"archived (default) or trash"`
	Q          string `query:"q" desc:"escaped substring match on card name or description"`
	LabelID    string `query:"labelId" desc:"only cards with this label, when the label belongs to this board or its project"`
	MemberID   string `query:"memberId" desc:"only cards this user is a member of; accepts a user id, me (the signed-in user), or none (unassigned)"`
	FromListID string `query:"fromListId" desc:"only cards whose previous column on this board is this list"`
	Since      string `query:"since" desc:"inclusive lower archive-time bound (RFC3339 or YYYY-MM-DD, normalized to UTC)"`
	Until      string `query:"until" desc:"inclusive upper archive-time bound (RFC3339 or YYYY-MM-DD); a date includes the whole day"`
	Sort       string `query:"sort" desc:"archived (default), name, or from"`
	Dir        string `query:"dir" desc:"asc or desc (default desc)"`
	Limit      int    `query:"limit" desc:"page size (default 50, max 200)"`
	Cursor     string `query:"cursor" desc:"opaque nextCursor from the previous page; bound to this board, filters, sort and direction"`
}

type ArchiveListOut struct {
	Items      []ArchiveListItem `json:"items"`
	Total      int               `json:"total"`
	NextCursor string            `json:"nextCursor"`
}

type ArchiveListItem struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	ArchivedAt   string         `json:"archivedAt"`
	FromListID   string         `json:"fromListId"`
	FromListName string         `json:"fromListName"`
	Closed       bool           `json:"closed"`
	Labels       []Label        `json:"labels"`
	Members      []Member       `json:"members"`
	AlsoActiveOn []ArchiveBoard `json:"alsoActiveOn"`
	CanRestore   bool           `json:"canRestore"`
	CanDelete    bool           `json:"canDelete"`
}

type ArchiveBoard struct {
	BoardID   string `json:"boardId"`
	BoardName string `json:"boardName"`
}

type RestoreBulkIn struct {
	BoardID      string   `path:"boardId" required:"true" desc:"board whose archived/trashed cards are restored"`
	CardIDs      []string `json:"cardIds" desc:"card ids to restore (max 500); exclusive with all"`
	All          bool     `json:"all" desc:"restore every card matching the content and date filters"`
	Kind         string   `json:"kind" desc:"with all, restrict to archived or trash; empty restores both kinds"`
	ConfirmCount *int     `json:"confirmCount" desc:"with all, required: the exact matching count observed by the client (missing is 400, a mismatch 409)"`
	Q            string   `json:"q" desc:"with all, escaped substring match on card name or description"`
	LabelID      string   `json:"labelId" desc:"with all, only cards with this board-visible label"`
	MemberID     string   `json:"memberId" desc:"with all, only cards this user is a member of; accepts a user id, me (the signed-in user), or none (unassigned)"`
	FromListID   string   `json:"fromListId" desc:"with all, only cards whose previous column on this board is this list"`
	Since        string   `json:"since" desc:"with all, inclusive lower archive-time bound (RFC3339 or YYYY-MM-DD, UTC)"`
	Until        string   `json:"until" desc:"with all, inclusive upper archive-time bound (RFC3339 or YYYY-MM-DD); a date includes the whole day"`
}

type RestoreBulkOut struct {
	Restored int `json:"restored"`
	Skipped  int `json:"skipped"`
}

type DeleteBulkIn struct {
	BoardID      string   `path:"boardId" required:"true" desc:"board whose trashed cards are deleted permanently"`
	CardIDs      []string `json:"cardIds" desc:"card ids to delete (max 500); exclusive with all"`
	All          bool     `json:"all" desc:"delete every card matching the content and date filters"`
	ConfirmCount *int     `json:"confirmCount" desc:"with all, required: the exact matching count observed by the client (missing is 400, a mismatch 409)"`
	Q            string   `json:"q" desc:"with all, escaped substring match on card name or description"`
	LabelID      string   `json:"labelId" desc:"with all, only cards with this board-visible label"`
	MemberID     string   `json:"memberId" desc:"with all, only cards this user is a member of; accepts a user id, me (the signed-in user), or none (unassigned)"`
	FromListID   string   `json:"fromListId" desc:"with all, only cards whose previous column on this board is this list"`
	Since        string   `json:"since" desc:"with all, inclusive lower archive-time bound (RFC3339 or YYYY-MM-DD, UTC)"`
	Until        string   `json:"until" desc:"with all, inclusive upper archive-time bound (RFC3339 or YYYY-MM-DD); a date includes the whole day"`
}

type DeleteBulkOut struct {
	Deleted int `json:"deleted"`
	Skipped int `json:"skipped"`
}

const (
	archiveListKindArchived = "archived"
	archiveListKindTrash    = "trash"
	archiveMemberMe         = "me"
	archiveMemberNone       = "none"
	archiveBulkChunk        = 500
	archiveCursorMaxLen     = 16 << 10
	archiveCursorPayloadMax = 12_000 // worst valid 1000-rune card + 255-rune list names, JSON-escaped and base64url encoded
)

func init() {
	ops.Register(ops.Op{Name: "card.archive.list", Summary: "List a board's archived or trashed cards with filters, stable cursor paging, board-visible labels and visible active placements", Method: "GET", Path: "/boards/{boardId}/archive",
		UI: "GET /ui/boards/{boardId}/archive"}, cardArchiveList)
	ops.Register(ops.Op{Name: "card.restore.bulk", Summary: "Restore selected or all matching archived/trashed cards on a board, quietly, with one board activity summary", Method: "POST", Path: "/boards/{boardId}/archive/restore",
		UI: "POST /ui/boards/{boardId}/archive/restore"}, cardRestoreBulk)
	ops.Register(ops.Op{Name: "card.delete.bulk", Summary: "Permanently delete selected or all matching trashed cards that are in trash on every board, quietly, with one board activity summary", Method: "POST", Path: "/boards/{boardId}/archive/delete",
		UI: "POST /ui/boards/{boardId}/archive/delete"}, cardDeleteBulk)
}

type archiveFilter struct {
	Q, LabelID, MemberID, FromListID string
	Since, Until                     string
	UntilExclusive                   bool
}

type archiveBound struct {
	Value     string
	At        time.Time
	Exclusive bool
}

func normalizeArchiveFilter(q, labelID, memberID, fromListID, since, until string) (archiveFilter, error) {
	f := archiveFilter{Q: strings.TrimSpace(q), LabelID: labelID, MemberID: memberID, FromListID: fromListID}
	var err error
	if f.Since, _, err = normalizeArchiveDate(since, false); err != nil {
		return f, err
	}
	untilValue, untilBound, err := normalizeArchiveDate(until, true)
	if err != nil {
		return f, err
	}
	f.Until, f.UntilExclusive = untilValue, untilBound.Exclusive
	return f, nil
}

// resolveArchiveMemberID maps the common member filters to the value used by archiveWhere. "none" stays
// symbolic so it can match cards without memberships; concrete ids are left untouched. Resolving "me" before
// fingerprinting and bulk snapshotting keeps paging and confirmCount bound to the same set of cards.
func resolveArchiveMemberID(e *ops.Env, memberID string) (string, error) {
	switch memberID {
	case "":
		return "", nil
	case archiveMemberNone:
		return archiveMemberNone, nil
	case archiveMemberMe:
		if e.User == nil || e.User.UserID == "" {
			return "", ops.Unauthorized()
		}
		return e.User.UserID, nil
	default:
		return memberID, nil
	}
}

// normalizeArchiveDate returns a UTC bound suitable for the second-resolution timestamps stored on placements.
// Fractional lower bounds round up and fractional upper bounds round down, preserving inclusive/exclusive semantics
// against those stored values. A date-only until is the next midnight, exclusive.
func normalizeArchiveDate(raw string, until bool) (string, archiveBound, error) {
	var out archiveBound
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", out, nil
	}
	dateOnly := len(raw) == 10
	var t time.Time
	var err error
	if dateOnly {
		t, err = time.Parse("2006-01-02", raw)
	} else {
		t, err = time.Parse(time.RFC3339Nano, raw)
	}
	if err != nil {
		return "", out, ops.Invalid("invalid date: " + raw)
	}
	t = t.UTC()
	out.At = t
	out.Exclusive = until && dateOnly
	canonical := t
	if dateOnly {
		if until {
			canonical = canonical.AddDate(0, 0, 1)
		}
	} else if until {
		canonical = canonical.Truncate(time.Second)
	} else {
		canonical = canonical.Truncate(time.Second)
		if canonical.Before(t) {
			canonical = canonical.Add(time.Second)
		}
	}
	out.Value = canonical.Format(time.RFC3339)
	return out.Value, out, nil
}

func validateArchiveRange(f archiveFilter, since, until archiveBound) error {
	if since.Value != "" && until.Value != "" {
		end := until.At
		if until.Exclusive {
			end = end.AddDate(0, 0, 1)
		}
		if since.At.After(end) || (until.Exclusive && since.At.Equal(end)) {
			return ops.Invalid("since must be before until")
		}
	}
	return nil
}

const archiveFrom = ` FROM card_placements p JOIN cards c ON c.id=p.card_id
	JOIN lists l ON l.id=p.list_id AND l.board_id=p.board_id
	LEFT JOIN lists fl ON fl.id=p.prev_list_id AND fl.board_id=p.board_id
	LEFT JOIN lists stored_fl ON stored_fl.id=p.prev_list_id`

// archiveFromName never reads a foreign column's name. A missing stored id means the source column was deleted;
// an id that exists only on another board stays unresolved and empty.
const archiveFromName = `CASE
	WHEN p.prev_list_id IS NULL OR p.prev_list_id='' THEN ''
	WHEN fl.id IS NOT NULL THEN COALESCE(fl.name,'')
	WHEN stored_fl.id IS NULL THEN '(removed)'
	ELSE '' END`

func archiveWhere(boardID string, kinds []string, f archiveFilter) (string, []any) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := []any{boardID}
	for _, k := range kinds {
		args = append(args, k)
	}
	q := ` WHERE p.board_id=? AND l.type IN (` + ph + `) AND c.is_draft=0 AND c.is_template=0`
	if f.Q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(f.Q) + "%"
		q += ` AND (c.name LIKE ? ESCAPE '\' OR COALESCE(c.description,'') LIKE ? ESCAPE '\')`
		args = append(args, like, like)
	}
	if f.LabelID != "" {
		q += ` AND c.id IN (SELECT cl.card_id FROM card_labels cl JOIN labels lab ON lab.id=cl.label_id
			WHERE cl.label_id=? AND ` + db.LabelOnBoard("lab", "p.board_id") + `)`
		args = append(args, f.LabelID)
	}
	if f.MemberID != "" {
		if f.MemberID == archiveMemberNone {
			q += ` AND NOT EXISTS (SELECT 1 FROM card_memberships cm WHERE cm.card_id=c.id)`
		} else {
			q += ` AND c.id IN (SELECT card_id FROM card_memberships WHERE user_id=?)`
			args = append(args, f.MemberID)
		}
	}
	if f.FromListID != "" {
		q += ` AND fl.id=?` // board-scoped join: a stale id from another board behaves as removed
		args = append(args, f.FromListID)
	}
	if f.Since != "" {
		q += ` AND p.list_changed_at>=?`
		args = append(args, f.Since)
	}
	if f.Until != "" {
		op := "<="
		if f.UntilExclusive {
			op = "<"
		}
		q += ` AND p.list_changed_at` + op + `?`
		args = append(args, f.Until)
	}
	return q, args
}

type archiveListRow struct {
	Item ArchiveListItem
}

func cardArchiveList(e *ops.Env, in ArchiveListIn) (ArchiveListOut, error) {
	if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
		return ArchiveListOut{}, err
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind == "" {
		kind = archiveListKindArchived
	}
	listType := "archive"
	switch kind {
	case archiveListKindArchived:
	case archiveListKindTrash:
		listType = "trash"
	default:
		return ArchiveListOut{}, ops.Invalid("kind must be archived or trash")
	}
	sortBy := strings.ToLower(strings.TrimSpace(in.Sort))
	if sortBy == "" {
		sortBy = "archived"
	}
	dir := strings.ToLower(strings.TrimSpace(in.Dir))
	if dir == "" {
		dir = "desc"
	}
	if sortBy != "archived" && sortBy != "name" && sortBy != "from" {
		return ArchiveListOut{}, ops.Invalid("sort must be archived, name or from")
	}
	if dir != "asc" && dir != "desc" {
		return ArchiveListOut{}, ops.Invalid("dir must be asc or desc")
	}
	limit := in.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return ArchiveListOut{}, ops.Invalid("limit must be 1..200")
	}
	f, err := normalizeArchiveFilter(in.Q, in.LabelID, in.MemberID, in.FromListID, in.Since, in.Until)
	if err != nil {
		return ArchiveListOut{}, err
	}
	if f.MemberID, err = resolveArchiveMemberID(e, f.MemberID); err != nil {
		return ArchiveListOut{}, err
	}
	_, sinceBound, _ := normalizeArchiveDate(in.Since, false)
	_, untilBound, _ := normalizeArchiveDate(in.Until, true)
	if err := validateArchiveRange(f, sinceBound, untilBound); err != nil {
		return ArchiveListOut{}, err
	}
	fp := archiveFingerprint{BoardID: in.BoardID, Kind: kind, Filter: f, Sort: sortBy, Dir: dir}
	fingerprint := fp.sum()
	var cursor archiveCursor
	if in.Cursor != "" {
		cursor, err = decodeArchiveCursor(e, in.Cursor)
		if err != nil || cursor.Version != 1 || cursor.BoardID != in.BoardID || cursor.Fingerprint != fingerprint {
			return ArchiveListOut{}, ops.Invalid("invalid cursor")
		}
	}
	where, args := archiveWhere(in.BoardID, []string{listType}, f)
	var total int
	if err := e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*)`+archiveFrom+where, args...).Scan(&total); err != nil {
		return ArchiveListOut{}, err
	}
	order := archiveOrder(sortBy, dir)
	pageWhere, pageArgs := archiveCursorWhere(where, args, sortBy, dir, cursor, in.Cursor != "")
	q := `SELECT c.id,c.name,p.list_changed_at,COALESCE(p.prev_list_id,''),` + archiveFromName + `,c.is_closed` + archiveFrom + pageWhere + order + ` LIMIT ?`
	pageArgs = append(pageArgs, limit+1)
	rows, err := e.DB.QueryContext(e.Ctx, q, pageArgs...)
	if err != nil {
		return ArchiveListOut{}, err
	}
	var page []archiveListRow
	for rows.Next() {
		var r archiveListRow
		if err := rows.Scan(&r.Item.ID, &r.Item.Name, &r.Item.ArchivedAt, &r.Item.FromListID, &r.Item.FromListName, &r.Item.Closed); err != nil {
			rows.Close()
			return ArchiveListOut{}, err
		}
		r.Item.Labels, r.Item.Members, r.Item.AlsoActiveOn = []Label{}, []Member{}, []ArchiveBoard{}
		page = append(page, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ArchiveListOut{}, err
	}
	rows.Close()
	out := ArchiveListOut{Items: []ArchiveListItem{}, Total: total}
	more := len(page) > limit
	if more {
		page = page[:limit]
	}
	ids := make([]string, len(page))
	for i := range page {
		ids[i] = page[i].Item.ID
	}
	labels, members, active, err := loadArchiveRelations(e, in.BoardID, ids)
	if err != nil {
		return ArchiveListOut{}, err
	}
	canRestore := authz.BoardLevel(e, in.BoardID) >= authz.Edit
	var canDelete map[string]bool
	if canRestore && listType == archiveListKindTrash {
		canDelete, err = loadArchiveDeletable(e, ids)
		if err != nil {
			return ArchiveListOut{}, err
		}
	}
	for _, r := range page {
		item := r.Item
		item.Labels, item.Members, item.AlsoActiveOn = labels[item.ID], members[item.ID], active[item.ID]
		if item.Labels == nil {
			item.Labels = []Label{}
		}
		if item.Members == nil {
			item.Members = []Member{}
		}
		if item.AlsoActiveOn == nil {
			item.AlsoActiveOn = []ArchiveBoard{}
		}
		item.CanRestore = canRestore
		item.CanDelete = canDelete[item.ID]
		out.Items = append(out.Items, item)
	}
	if more {
		last := page[len(page)-1].Item
		out.NextCursor, err = encodeArchiveCursor(e, archiveCursor{Version: 1, BoardID: in.BoardID, Fingerprint: fingerprint,
			ArchivedAt: last.ArchivedAt, Name: last.Name, FromName: last.FromListName, FromListID: last.FromListID, ID: last.ID})
		if err != nil {
			return ArchiveListOut{}, err
		}
	}
	return out, nil
}

func archiveOrder(sortBy, dir string) string {
	dir = strings.ToUpper(dir)
	switch sortBy {
	case "name":
		return ` ORDER BY c.name COLLATE NOCASE ` + dir + `, c.id ` + dir
	case "from":
		return ` ORDER BY (` + archiveFromName + `) COLLATE NOCASE ` + dir + `, COALESCE(p.prev_list_id,'') ` + dir + `, c.id ` + dir
	default:
		return ` ORDER BY p.list_changed_at ` + dir + `, c.id ` + dir
	}
}

func archiveCursorWhere(where string, args []any, sortBy, dir string, c archiveCursor, have bool) (string, []any) {
	if !have {
		return where, args
	}
	op := ">"
	if dir == "desc" {
		op = "<"
	}
	var cond string
	var keys []any
	switch sortBy {
	case "name":
		cond = `(c.name COLLATE NOCASE` + op + `? OR (c.name COLLATE NOCASE=? AND c.id` + op + `?))`
		keys = []any{c.Name, c.Name, c.ID}
	case "from":
		cond = `((` + archiveFromName + `) COLLATE NOCASE` + op + `? OR
			((` + archiveFromName + `) COLLATE NOCASE=? AND COALESCE(p.prev_list_id,'')` + op + `?) OR
			((` + archiveFromName + `) COLLATE NOCASE=? AND COALESCE(p.prev_list_id,'')=? AND c.id` + op + `?))`
		keys = []any{c.FromName, c.FromName, c.FromListID, c.FromName, c.FromListID, c.ID}
	default:
		cond = `(p.list_changed_at` + op + `? OR (p.list_changed_at=? AND c.id` + op + `?))`
		keys = []any{c.ArchivedAt, c.ArchivedAt, c.ID}
	}
	return where + ` AND ` + cond, append(args, keys...)
}

func loadArchiveRelations(e *ops.Env, boardID string, ids []string) (map[string][]Label, map[string][]Member, map[string][]ArchiveBoard, error) {
	labels := map[string][]Label{}
	members := map[string][]Member{}
	active := map[string][]ArchiveBoard{}
	if len(ids) == 0 {
		return labels, members, active, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	lrows, err := e.DB.QueryContext(e.Ctx, `SELECT cl.card_id,lab.id,COALESCE(lab.name,''),lab.color
		FROM card_labels cl JOIN labels lab ON lab.id=cl.label_id
		JOIN card_placements p ON p.card_id=cl.card_id AND p.board_id=?
		WHERE cl.card_id IN (`+ph+`) AND `+db.LabelOnBoard("lab", "p.board_id")+` ORDER BY cl.card_id,lab.position,lab.id`, append([]any{boardID}, args...)...)
	if err != nil {
		return nil, nil, nil, err
	}
	for lrows.Next() {
		var card string
		var l Label
		if err := lrows.Scan(&card, &l.ID, &l.Name, &l.Color); err != nil {
			lrows.Close()
			return nil, nil, nil, err
		}
		labels[card] = append(labels[card], l)
	}
	if err := lrows.Err(); err != nil {
		lrows.Close()
		return nil, nil, nil, err
	}
	lrows.Close()
	mrows, err := e.DB.QueryContext(e.Ctx, `SELECT cm.card_id,u.id,u.name,u.is_agent FROM card_memberships cm JOIN users u ON u.id=cm.user_id
		WHERE cm.card_id IN (`+ph+`) ORDER BY cm.card_id,u.name,u.id`, args...)
	if err != nil {
		return nil, nil, nil, err
	}
	for mrows.Next() {
		var card string
		var m Member
		if err := mrows.Scan(&card, &m.ID, &m.Name, &m.IsAgent); err != nil {
			mrows.Close()
			return nil, nil, nil, err
		}
		members[card] = append(members[card], m)
	}
	if err := mrows.Err(); err != nil {
		mrows.Close()
		return nil, nil, nil, err
	}
	mrows.Close()
	arows, err := e.DB.QueryContext(e.Ctx, `SELECT p.card_id,p.board_id,b.name FROM card_placements p
		JOIN boards b ON b.id=p.board_id JOIN lists l ON l.id=p.list_id AND l.board_id=p.board_id
		WHERE p.card_id IN (`+ph+`) AND p.board_id<>? AND l.type IN ('active','closed') ORDER BY p.card_id,b.name,p.board_id`, append(args, boardID)...)
	if err != nil {
		return nil, nil, nil, err
	}
	var pairs []ArchiveBoard
	var pairCards []string
	seenBoards := map[string]bool{}
	var boardIDs []string
	for arows.Next() {
		var card string
		var b ArchiveBoard
		if err := arows.Scan(&card, &b.BoardID, &b.BoardName); err != nil {
			arows.Close()
			return nil, nil, nil, err
		}
		pairCards, pairs = append(pairCards, card), append(pairs, b)
		if !seenBoards[b.BoardID] {
			seenBoards[b.BoardID] = true
			boardIDs = append(boardIDs, b.BoardID)
		}
	}
	if err := arows.Err(); err != nil {
		arows.Close()
		return nil, nil, nil, err
	}
	arows.Close()
	levels := authz.BoardLevels(e, boardIDs)
	for i, b := range pairs {
		if levels[b.BoardID] >= authz.View {
			active[pairCards[i]] = append(active[pairCards[i]], b)
		}
	}
	return labels, members, active, nil
}

// loadArchiveDeletable applies card.delete's all-placements-in-trash rule to the current page in one query.
// It deliberately ignores viewer access: hidden placements participate in the safety check but are never returned.
func loadArchiveDeletable(e *ops.Env, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT p.card_id FROM card_placements p
		JOIN lists l ON l.id=p.list_id AND l.board_id=p.board_id
		WHERE p.card_id IN (`+ph+`)
		GROUP BY p.card_id HAVING COUNT(*)=SUM(l.type='trash')`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

type archiveFingerprint struct {
	BoardID string        `json:"b"`
	Kind    string        `json:"k"`
	Filter  archiveFilter `json:"f"`
	Sort    string        `json:"s"`
	Dir     string        `json:"d"`
}

func (f archiveFingerprint) sum() string {
	b, _ := json.Marshal(f)
	s := sha256.Sum256(b)
	return fmt.Sprintf("%x", s[:])
}

type archiveCursor struct {
	Version     int    `json:"v"`
	BoardID     string `json:"b"`
	Fingerprint string `json:"f"`
	ArchivedAt  string `json:"a,omitempty"`
	Name        string `json:"n,omitempty"`
	FromName    string `json:"fn,omitempty"`
	FromListID  string `json:"fi,omitempty"`
	ID          string `json:"i"`
}

func archiveCursorKey(e *ops.Env) []byte {
	cfg, _ := e.Svc["config"].(config.Config)
	k := sha256.Sum256([]byte("weave-archive-cursor:v1:" + cfg.SecretKey))
	return k[:]
}

func archiveMAC(key, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte("weave-archive-cursor:v1:"))
	_, _ = m.Write(payload)
	return m.Sum(nil)
}

func encodeArchiveCursor(e *ops.Env, c archiveCursor) (string, error) {
	payload, _ := json.Marshal(c)
	if len(payload) > archiveCursorPayloadMax {
		return "", ops.Invalid("archive cursor is too large")
	}
	sig := archiveMAC(archiveCursorKey(e), payload)
	token := "v1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
	if len(token) > archiveCursorMaxLen {
		return "", ops.Invalid("archive cursor is too large")
	}
	return token, nil
}

func decodeArchiveCursor(e *ops.Env, token string) (archiveCursor, error) {
	var out archiveCursor
	if len(token) > archiveCursorMaxLen {
		return out, ops.Invalid("invalid cursor")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return out, ops.Invalid("invalid cursor")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > archiveCursorPayloadMax {
		return out, ops.Invalid("invalid cursor")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, archiveMAC(archiveCursorKey(e), payload)) {
		return out, ops.Invalid("invalid cursor")
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return archiveCursor{}, ops.Invalid("invalid cursor")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return archiveCursor{}, ops.Invalid("invalid cursor")
	}
	if out.Version != 1 || out.BoardID == "" || out.Fingerprint == "" || out.ID == "" {
		return archiveCursor{}, ops.Invalid("invalid cursor")
	}
	return out, nil
}

func normalizeBulkIDs(cardIDs []string, all bool, confirmCount *int, f archiveFilter) ([]string, error) {
	if all {
		if len(cardIDs) != 0 {
			return nil, ops.Invalid("cardIds and all are mutually exclusive")
		}
		if confirmCount == nil {
			return nil, ops.Invalid("confirmCount is required with all")
		}
		if *confirmCount < 0 {
			return nil, ops.Invalid("confirmCount cannot be negative")
		}
		return nil, nil
	}
	if len(cardIDs) == 0 {
		return nil, ops.Invalid("cardIds or all=true is required")
	}
	if len(cardIDs) > archiveBulkChunk {
		return nil, ops.Invalid("cardIds may contain at most 500 ids")
	}
	if f.Q != "" || f.LabelID != "" || f.MemberID != "" || f.FromListID != "" || f.Since != "" || f.Until != "" {
		return nil, ops.Invalid("filters are only valid with all")
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(cardIDs))
	for _, id := range cardIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, ops.Invalid("cardIds cannot contain an empty id")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func archiveSnapshot(e *ops.Env, boardID string, kinds []string, f archiveFilter, confirmCount int) ([]string, error) {
	where, args := archiveWhere(boardID, kinds, f)
	var ids []string
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		var total int
		if err := tx.QueryRowContext(e.Ctx, `SELECT COUNT(*)`+archiveFrom+where, args...).Scan(&total); err != nil {
			return err
		}
		if total != confirmCount {
			return ops.Conflict(fmt.Sprintf("confirmCount must equal the current matching count (%d)", total))
		}
		rows, err := tx.QueryContext(e.Ctx, `SELECT c.id`+archiveFrom+where+` ORDER BY c.id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

func archivePlacementOnBoard(ctx context.Context, q querier, cardID, boardID string, kinds []string) (bool, error) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := []any{cardID, boardID}
	for _, k := range kinds {
		args = append(args, k)
	}
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_placements p JOIN lists l ON l.id=p.list_id AND l.board_id=p.board_id
		WHERE p.card_id=? AND p.board_id=? AND l.type IN (`+ph+`)`, args...).Scan(&n)
	return n > 0, err
}

// preflightArchiveRestore checks every selected candidate in read-only batches of at most 500 ids before any write
// chunk starts. It catches the deterministic failures single-card restore would report, including a hidden board with
// no active fallback, without leaving earlier chunks committed.
func preflightArchiveRestore(e *ops.Env, boardID string, ids []string) error {
	for start := 0; start < len(ids); start += archiveBulkChunk {
		chunk := ids[start:min(start+archiveBulkChunk, len(ids))]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		type check struct {
			Draft, Template, Candidate bool
			Places                     []placementRow
		}
		byCard := map[string]*check{}
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT p.card_id,c.is_draft,c.is_template,p.board_id,p.list_id,l.type,p.prev_list_id
			FROM card_placements p JOIN cards c ON c.id=p.card_id JOIN lists l ON l.id=p.list_id AND l.board_id=p.board_id
			WHERE p.card_id IN (`+ph+`) ORDER BY p.card_id,p.is_home DESC,p.placed_at,p.board_id`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var p placementRow
			var c check
			if err := rows.Scan(&id, &c.Draft, &c.Template, &p.Board, &p.List, &p.ListType, &p.Prev); err != nil {
				rows.Close()
				return err
			}
			x := byCard[id]
			if x == nil {
				x = &check{}
				byCard[id] = x
			}
			x.Draft, x.Template = c.Draft, c.Template
			if p.Board == boardID && (p.ListType == "archive" || p.ListType == "trash") {
				x.Candidate = true
			}
			x.Places = append(x.Places, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, id := range chunk {
			c := byCard[id]
			if c == nil || !c.Candidate {
				continue // selected ids that are not candidates are counted as skipped by the write transaction
			}
			if c.Template {
				return ops.Invalid("templates cannot be moved")
			}
			if c.Draft {
				return ops.Invalid("publish the draft before moving it")
			}
			for _, p := range c.Places {
				if p.ListType != "archive" && p.ListType != "trash" {
					continue
				}
				if _, err := restoreTarget(e.Ctx, e.DB, p.Board, boardID, p.Prev); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type restoreEmission struct {
	CardID string
	Result restoreTxResult
}

func emitArchiveBulkSummary(e *ops.Env, boardID, auto string, changed, skipped int) {
	data := map[string]any{"id": boardID, "quiet": true, "auto": auto, "skipped": skipped}
	if auto == "bulkDelete" {
		data["deleted"] = changed
	} else {
		data["restored"] = changed
	}
	emitScoped(e, "", "boardUpdate", "board.updated", boardID, "", data, []string{boardID})
}

func cardRestoreBulk(e *ops.Env, in RestoreBulkIn) (out RestoreBulkOut, err error) {
	if err := authz.RequireBoard(e, in.BoardID, authz.Edit); err != nil {
		return RestoreBulkOut{}, err
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind != "" && kind != archiveListKindArchived && kind != archiveListKindTrash {
		return RestoreBulkOut{}, ops.Invalid("kind must be archived or trash")
	}
	f, err := normalizeArchiveFilter(in.Q, in.LabelID, in.MemberID, in.FromListID, in.Since, in.Until)
	if err != nil {
		return RestoreBulkOut{}, err
	}
	_, sinceBound, _ := normalizeArchiveDate(in.Since, false)
	_, untilBound, _ := normalizeArchiveDate(in.Until, true)
	if err := validateArchiveRange(f, sinceBound, untilBound); err != nil {
		return RestoreBulkOut{}, err
	}
	ids, err := normalizeBulkIDs(in.CardIDs, in.All, in.ConfirmCount, f)
	if err != nil {
		return RestoreBulkOut{}, err
	}
	candidateKinds := []string{"archive", "trash"}
	if in.All {
		if f.MemberID, err = resolveArchiveMemberID(e, f.MemberID); err != nil {
			return RestoreBulkOut{}, err
		}
		switch kind {
		case archiveListKindArchived:
			candidateKinds = []string{"archive"}
		case archiveListKindTrash:
			candidateKinds = []string{"trash"}
		}
		ids, err = archiveSnapshot(e, in.BoardID, candidateKinds, f, *in.ConfirmCount)
		if err != nil {
			return RestoreBulkOut{}, err
		}
	}
	if err := preflightArchiveRestore(e, in.BoardID, ids); err != nil {
		return RestoreBulkOut{}, err
	}
	committed := false
	defer func() {
		if (err == nil || committed) && out.Restored > 0 {
			emitArchiveBulkSummary(e, in.BoardID, "bulkRestore", out.Restored, out.Skipped)
		}
	}()
	for start := 0; start < len(ids); start += archiveBulkChunk {
		var emissions []restoreEmission
		var landed boards.LandResult
		var chunk RestoreBulkOut
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			emissions, landed = nil, boards.LandResult{}
			for _, id := range ids[start:min(start+archiveBulkChunk, len(ids))] {
				candidate, err := archivePlacementOnBoard(e.Ctx, tx, id, in.BoardID, candidateKinds)
				if err != nil {
					return err
				}
				if !candidate {
					chunk.Skipped++
					continue
				}
				res, restored, err := restoreCardTx(e, tx, id, in.BoardID)
				if err != nil {
					return err
				}
				if !restored {
					chunk.Skipped++
					continue
				}
				chunk.Restored++
				landed.Add(res.Landed)
				emissions = append(emissions, restoreEmission{CardID: id, Result: res})
			}
			return nil
		})
		if err != nil {
			return out, err
		}
		out.Restored += chunk.Restored
		out.Skipped += chunk.Skipped
		committed = true
		for _, em := range emissions {
			res := em.Result
			c, err := loadCardOn(e, e.DB, em.CardID, in.BoardID)
			if err != nil {
				continue // committed already; a card removed concurrently gets no event, the rest still do
			}
			moves := make(map[string][2]string, len(res.Moves))
			from, to := "", ""
			for _, m := range res.Moves {
				moves[m.Board] = [2]string{m.From, m.Target}
				if m.Board == in.BoardID {
					from, to = m.From, m.Target
				}
			}
			emitKind(e, "card.restored", "cardMove", "card.moved", in.BoardID, em.CardID, map[string]any{"card": c,
				"fromListId": from, "toListId": to, "fromBoardId": in.BoardID, "trash": false,
				"quiet": true, "auto": "bulkRestore", events.CardLevel: true, events.PlacementLists: moves})
		}
		boards.EmitClosedElsewhere(e, in.BoardID, landed)
		boards.EmitAutoArchived(e, landed)
	}
	return out, nil
}

type deleteEmission struct {
	CardID string
	Data   deletedCardData
}

func cardDeleteBulk(e *ops.Env, in DeleteBulkIn) (out DeleteBulkOut, err error) {
	if err := authz.RequireBoard(e, in.BoardID, authz.Edit); err != nil {
		return DeleteBulkOut{}, err
	}
	f, err := normalizeArchiveFilter(in.Q, in.LabelID, in.MemberID, in.FromListID, in.Since, in.Until)
	if err != nil {
		return DeleteBulkOut{}, err
	}
	_, sinceBound, _ := normalizeArchiveDate(in.Since, false)
	_, untilBound, _ := normalizeArchiveDate(in.Until, true)
	if err := validateArchiveRange(f, sinceBound, untilBound); err != nil {
		return DeleteBulkOut{}, err
	}
	ids, err := normalizeBulkIDs(in.CardIDs, in.All, in.ConfirmCount, f)
	if err != nil {
		return DeleteBulkOut{}, err
	}
	kinds := []string{"trash"}
	if in.All {
		if f.MemberID, err = resolveArchiveMemberID(e, f.MemberID); err != nil {
			return DeleteBulkOut{}, err
		}
		ids, err = archiveSnapshot(e, in.BoardID, kinds, f, *in.ConfirmCount)
		if err != nil {
			return DeleteBulkOut{}, err
		}
	}
	committed := false
	defer func() {
		if (err == nil || committed) && out.Deleted > 0 {
			emitArchiveBulkSummary(e, in.BoardID, "bulkDelete", out.Deleted, out.Skipped)
		}
	}()
	for start := 0; start < len(ids); start += archiveBulkChunk {
		var emissions []deleteEmission
		var chunk DeleteBulkOut
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			emissions = nil
			for _, id := range ids[start:min(start+archiveBulkChunk, len(ids))] {
				candidate, err := archivePlacementOnBoard(e.Ctx, tx, id, in.BoardID, kinds)
				if err != nil {
					return err
				}
				if !candidate {
					chunk.Skipped++
					continue
				}
				data, deleted, err := deleteCardTx(e, tx, id, in.BoardID, false)
				if err != nil {
					return err
				}
				if !deleted {
					chunk.Skipped++
					continue
				}
				chunk.Deleted++
				emissions = append(emissions, deleteEmission{CardID: id, Data: data})
			}
			return nil
		})
		if err != nil {
			return out, err
		}
		out.Deleted += chunk.Deleted
		out.Skipped += chunk.Skipped
		committed = true
		for _, em := range emissions {
			emitScoped(e, "", "cardDelete", "card.deleted", in.BoardID, em.CardID, map[string]any{"name": em.Data.Name,
				"listId": em.Data.ListID, "quiet": true, "auto": "bulkDelete"}, em.Data.Boards)
		}
	}
	return out, nil
}
