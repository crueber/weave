package cards

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// Card is the API representation of a card.
type Card struct {
	ID                string   `json:"id"`
	BoardID           string   `json:"boardId"`
	ListID            string   `json:"listId"`
	CreatorUserID     *string  `json:"creatorUserId"`
	PrevListID        *string  `json:"prevListId"`
	CoverAttachmentID *string  `json:"coverAttachmentId"`
	Position          *float64 `json:"position"`
	Name              string   `json:"name"`
	Description       *string  `json:"description"`
	DueDate           *string  `json:"dueDate"`
	IsDueCompleted    *bool    `json:"isDueCompleted"`
	CommentsTotal     int      `json:"commentsTotal"`
	IsClosed          bool     `json:"isClosed"`
	IsDraft           bool     `json:"isDraft"`
	IsTemplate        bool     `json:"isTemplate"`
	Lat               *float64 `json:"lat"`
	Lng               *float64 `json:"lng"`
	LocationName      *string  `json:"locationName"`
	Recurrence        *string  `json:"recurrence"`
	ShareToken        *string  `json:"shareToken,omitempty"`
	ListChangedAt     string   `json:"listChangedAt"`
	CreatedAt         string   `json:"createdAt"`
	UpdatedAt         *string  `json:"updatedAt"`
	TemplateProjectID *string  `json:"templateProjectId"`
	LabelIDs          []string `json:"labelIds"`
	MemberIDs         []string `json:"memberIds"`
	IsSubscribed      bool     `json:"isSubscribed"`
	ClaimedBy         *string  `json:"claimedBy"`
	ClaimedByName     *string  `json:"claimedByName,omitempty"`
	ClaimedUntil      *string  `json:"claimedUntil"`
	ClaimNote         *string  `json:"claimNote,omitempty"`
	// Per viewer, set by card.get only (never by loadCard, so never in events, activity or webhook payloads):
	// HomeBoardID is the card's home board when the caller can view it, Placements the boards the card is on that
	// the caller can view (home first), AlsoOnBoards how many of those are not the context board (BoardID).
	HomeBoardID  string      `json:"homeBoardId,omitempty"`
	Placements   []Placement `json:"placements,omitempty"`
	AlsoOnBoards int         `json:"alsoOnBoards,omitempty"`
	// card.get?full=true extras (shapes of the cardextras ops they come from)
	Tasks        any `json:"tasks,omitempty"`
	Comments     any `json:"comments,omitempty"`
	Attachments  any `json:"attachments,omitempty"`
	CustomFields any `json:"customFields,omitempty"`
	Activity     any `json:"activity,omitempty"`
}

// cardGetCard is the in-process card.get (no extras).
func cardGetCard(e *ops.Env, id string) (Card, error) {
	return ops.Typed[Card](e, "card.get", GetIn{ID: id})
}

// Placement is one board a card is on, as card.get reports it (only boards the caller can view).
type Placement struct {
	BoardID     string `json:"boardId"`
	BoardName   string `json:"boardName"`
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	ListID      string `json:"listId"`
	ListName    string `json:"listName"`
	IsHome      bool   `json:"isHome"`
}

// cardCols reads a card with the board, column, position, previous column and column-changed time of ONE placement
// (alias p, joined by loadCardOn); every other column is the card's own (alias c).
const cardCols = `c.id,p.board_id,p.list_id,c.creator_user_id,p.prev_list_id,c.cover_attachment_id,p.position,c.name,c.description,c.due_date,c.is_due_completed,
c.comments_total,c.is_closed,c.is_draft,c.is_template,c.lat,c.lng,c.location_name,c.recurrence,c.share_token,p.list_changed_at,c.created_at,c.updated_at,c.template_project_id,c.claimed_by,c.claimed_until,c.claim_note`

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// LoadCard fetches a card with labels, members and the caller's subscription (no authz), as seen on its home board.
func LoadCard(e *ops.Env, id string) (Card, error) { return loadCard(e, e.DB, id) }

// loadCard is loadCardOn the home board (no authz): the writers and the events keep meaning the home placement.
func loadCard(e *ops.Env, q querier, id string) (Card, error) { return loadCardOn(e, q, id, "") }

// loadCardOn fetches a card as seen on boardID ("" = its home board): boardId, listId, position, prevListId and
// listChangedAt describe that placement, and labelIds are only the labels shown on that board (db.LabelOnBoard).
// No authz: callers pass a board the caller may view (the context board of authz.CardAccessFor / RequireCard).
// not_found when the card is not placed on boardID.
func loadCardOn(e *ops.Env, q querier, id, boardID string) (Card, error) {
	var c Card
	err := q.QueryRowContext(e.Ctx, `SELECT `+cardCols+` FROM cards c JOIN card_placements p ON p.card_id=c.id AND (p.board_id=?2 OR (?2='' AND p.is_home=1))
		WHERE c.id=?1`, id, boardID).Scan(&c.ID, &c.BoardID, &c.ListID, &c.CreatorUserID,
		&c.PrevListID, &c.CoverAttachmentID, &c.Position, &c.Name, &c.Description, &c.DueDate, &c.IsDueCompleted, &c.CommentsTotal,
		&c.IsClosed, &c.IsDraft, &c.IsTemplate, &c.Lat, &c.Lng, &c.LocationName, &c.Recurrence, &c.ShareToken, &c.ListChangedAt,
		&c.CreatedAt, &c.UpdatedAt, &c.TemplateProjectID, &c.ClaimedBy, &c.ClaimedUntil, &c.ClaimNote)
	if err == sql.ErrNoRows {
		return c, ops.NotFound("card")
	}
	if err != nil {
		return c, err
	}
	if c.ClaimedBy != nil && (c.ClaimedUntil == nil || *c.ClaimedUntil < db.Now()) { // an expired lease counts as unclaimed
		c.ClaimedBy, c.ClaimedUntil, c.ClaimNote = nil, nil, nil
	}
	if c.ClaimedBy != nil {
		var n string
		if q.QueryRowContext(e.Ctx, `SELECT name FROM users WHERE id=?`, *c.ClaimedBy).Scan(&n) == nil {
			c.ClaimedByName = &n
		}
	}
	c.LabelIDs, c.MemberIDs = []string{}, []string{}
	c.LabelIDs = strs(e, q, `SELECT cl.label_id FROM card_labels cl JOIN labels l ON l.id=cl.label_id
		WHERE cl.card_id=?1 AND `+db.LabelOnBoard("l", "?2")+` ORDER BY cl.label_id`, id, c.BoardID)
	c.MemberIDs = strs(e, q, `SELECT user_id FROM card_memberships WHERE card_id=?`, id)
	if e.User != nil {
		var muted bool // a muted row is "not watching" (see muteCard)
		if q.QueryRowContext(e.Ctx, `SELECT muted FROM card_subscriptions WHERE card_id=? AND user_id=?`, id, e.User.UserID).Scan(&muted) == nil {
			c.IsSubscribed = !muted
		}
	}
	return c, nil
}

// placementsFor lists the placements of a card the caller can view (a.Boards; hidden ones are never named),
// home first, then by placed_at: one query.
func placementsFor(e *ops.Env, a authz.CardAccess) []Placement {
	out := []Placement{}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT p.board_id,b.name,b.project_id,COALESCE(pr.name,''),p.list_id,COALESCE(l.name,l.type),p.is_home
		FROM card_placements p JOIN boards b ON b.id=p.board_id JOIN lists l ON l.id=p.list_id LEFT JOIN projects pr ON pr.id=b.project_id
		WHERE p.card_id=? ORDER BY p.is_home DESC, p.placed_at, p.board_id`, a.CardID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var x Placement
		if rows.Scan(&x.BoardID, &x.BoardName, &x.ProjectID, &x.ProjectName, &x.ListID, &x.ListName, &x.IsHome) != nil {
			continue
		}
		if _, ok := a.Boards[x.BoardID]; ok {
			out = append(out, x)
		}
	}
	return out
}

func strs(e *ops.Env, q querier, query string, args ...any) []string {
	out := []string{}
	rows, err := q.QueryContext(e.Ctx, query, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			out = append(out, s)
		}
	}
	return out
}

// emit writes activity/notifications via ops.Emit and always publishes realtime.
func emit(e *ops.Env, typ, name, boardID, cardID string, data map[string]any) {
	emitKind(e, "", typ, name, boardID, cardID, data)
}

// emitKind is emit with an explicit event-stream type (e.g. card.archived).
func emitKind(e *ops.Env, kind, typ, name, boardID, cardID string, data map[string]any) {
	emitScoped(e, kind, typ, name, boardID, cardID, data, nil)
}

// emitScoped is emitKind with an explicit board scope (ops.Event.Boards): the boards of a card read before a write
// that removes them (a deleted card, a removed placement), or the fewer boards an event must stay on (one board's
// labels, one board's placement: the source half of a relocation, a placement, an unplacement).
func emitScoped(e *ops.Env, kind, typ, name, boardID, cardID string, data map[string]any, boards []string) {
	if data == nil {
		data = map[string]any{}
	}
	data["boardId"] = boardID
	if cardID != "" {
		data["id"], data["cardId"] = cardID, cardID
	} else {
		data["cardId"] = ""
		if _, ok := data["id"]; !ok {
			data["id"] = ""
		}
	}
	if c, ok := data["card"].(Card); ok {
		// isSubscribed is the acting user's own watch state: never ship it to the board room, the activity log or webhooks;
		// the placement list is per viewer too (hidden placements are never named). notify.Record replaces the card by
		// each board's own view (events.CardOn) anyway.
		data["card"] = eventCard(c)
	}
	if ops.EmitHook != nil {
		ops.Emit(e, ops.Event{Type: typ, Kind: kind, BoardID: boardID, CardID: cardID, Name: name, Data: data, Boards: boards})
		return
	}
	e.Hub.Publish(realtime.Event{Name: name, Room: "board:" + boardID, Data: data})
}

// editCard is the rule of every cards-domain write on an existing card: authz.RequireCardEdit (board Edit, or a
// worker who is a member of the card). The card sheet asks authz.Card for the same answer.
func editCard(e *ops.Env, id string) (string, error) { return authz.RequireCardEdit(e, id) }

func viewCard(e *ops.Env, id string) (string, error) { return authz.RequireCard(e, id, authz.View) }

// ---- time helpers

var timeLayouts = []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, ops.Invalid("invalid date: " + s)
}

func normTime(s string) (string, error) {
	t, err := parseTime(s)
	if err != nil {
		return "", err
	}
	return t.Format(time.RFC3339), nil
}

// ---- positions

// onePlacement checks that a placement write changed exactly one row (a move names one placement; zero rows means
// the card is not on that board any more, which must not pass as a successful move).
func onePlacement(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("card placement write changed %d rows, want 1 (%v)", n, err)
	}
	return nil
}

func listRef(ctx context.Context, q querier, id string) (boardID, typ string, err error) {
	err = q.QueryRowContext(ctx, `SELECT board_id,type FROM lists WHERE id=?`, id).Scan(&boardID, &typ)
	if err == sql.ErrNoRows {
		return "", "", ops.NotFound("list")
	}
	return
}

// Positions live on the card's placement in the column (card_placements, migration 0404): endPos, rebalance and
// positionFor read and write placements; the cards columns follow the home placement through the mirror triggers.

func endPos(ctx context.Context, q querier, listID string) float64 {
	var m sql.NullFloat64
	_ = q.QueryRowContext(ctx, `SELECT MAX(position) FROM card_placements WHERE list_id=?`, listID).Scan(&m)
	if !m.Valid {
		return db.PositionGap
	}
	return m.Float64 + db.PositionGap
}

func rebalance(ctx context.Context, q querier, listID string) error {
	ids := []string{}
	rows, err := q.QueryContext(ctx, `SELECT card_id FROM card_placements WHERE list_id=? ORDER BY position, card_id`, listID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		if _, err := q.ExecContext(ctx, `UPDATE card_placements SET position=? WHERE card_id=? AND list_id=?`, float64(i+1)*db.PositionGap, id, listID); err != nil {
			return err
		}
	}
	return nil
}

// positionFor picks a position in listID: explicit, after/before a sibling (midpoint), or the end.
func positionFor(ctx context.Context, q querier, listID, selfID, afterID, beforeID string, explicit *float64) (float64, error) {
	if explicit != nil {
		return *explicit, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		var p, o sql.NullFloat64
		switch {
		case afterID != "" && afterID != selfID:
			if q.QueryRowContext(ctx, `SELECT position FROM card_placements WHERE card_id=? AND list_id=?`, afterID, listID).Scan(&p) != nil || !p.Valid {
				return endPos(ctx, q, listID), nil
			}
			_ = q.QueryRowContext(ctx, `SELECT MIN(position) FROM card_placements WHERE list_id=? AND position>? AND card_id<>?`, listID, p.Float64, selfID).Scan(&o)
			if !o.Valid {
				return p.Float64 + db.PositionGap, nil
			}
			if o.Float64-p.Float64 >= 1 {
				return (p.Float64 + o.Float64) / 2, nil
			}
		case beforeID != "" && beforeID != selfID:
			if q.QueryRowContext(ctx, `SELECT position FROM card_placements WHERE card_id=? AND list_id=?`, beforeID, listID).Scan(&p) != nil || !p.Valid {
				return endPos(ctx, q, listID), nil
			}
			_ = q.QueryRowContext(ctx, `SELECT MAX(position) FROM card_placements WHERE list_id=? AND position<? AND card_id<>?`, listID, p.Float64, selfID).Scan(&o)
			if !o.Valid {
				return p.Float64 / 2, nil
			}
			if p.Float64-o.Float64 >= 1 {
				return (p.Float64 + o.Float64) / 2, nil
			}
		default:
			return endPos(ctx, q, listID), nil
		}
		if err := rebalance(ctx, q, listID); err != nil {
			return 0, err
		}
	}
	return endPos(ctx, q, listID), nil
}

// ---- shared relations

func boardProject(ctx context.Context, q querier, boardID string) (project string) {
	_ = q.QueryRowContext(ctx, `SELECT project_id FROM boards WHERE id=?`, boardID).Scan(&project)
	return
}

// boardMemberIDs lists users with a direct or group membership on the board.
func boardMemberIDs(ctx context.Context, q querier, boardID string) map[string]bool {
	out := map[string]bool{}
	rows, err := q.QueryContext(ctx, `SELECT u.id FROM users u WHERE u.deleted_at IS NULL AND u.is_deactivated=0 AND (
		u.id IN (SELECT user_id FROM board_memberships WHERE board_id=? AND user_id IS NOT NULL) OR
		u.id IN (SELECT gm.user_id FROM user_group_members gm JOIN board_memberships bm ON bm.group_id=gm.group_id WHERE bm.board_id=?))`, boardID, boardID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out[id] = true
	}
	return out
}

const labelsOfBoard = `SELECT l.id,COALESCE(l.name,''),l.color,COALESCE(l.group_id,''),COALESCE(g.name,''),COALESCE(g.single_select,0)
	FROM labels l LEFT JOIN label_groups g ON g.id=l.group_id
	WHERE l.board_id=? OR (l.project_id=? AND (SELECT use_project_labels FROM projects WHERE id=?)=1)
	ORDER BY COALESCE(g.position,0), l.position`

func labelAllowed(ctx context.Context, q querier, boardID, labelID string) bool {
	var n int
	_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM labels l WHERE l.id=? AND (l.board_id=? OR (l.project_id=? AND (SELECT use_project_labels FROM projects WHERE id=?)=1))`,
		labelID, boardID, boardProject(ctx, q, boardID), boardProject(ctx, q, boardID)).Scan(&n)
	return n > 0
}

// Watching a card (card_subscriptions), one row per (card, user):
//   - watchCard (card.subscribe, an explicit choice): is_permanent=1, muted=0; it also clears a mute.
//   - muteCard (card.unsubscribe): muted=1, is_permanent=0, keeping or creating the row. A muted card never notifies
//     its user, not even through a watched column or board, and auto-watch never re-adds it.
//   - autoWatch (creating a card, commenting, being added as a member): inserts is_permanent=0 only when there is no
//     row at all, so it never overrides a mute or an explicit choice. Agents (users.is_agent) never auto-watch.
//
// is_permanent therefore means "watched explicitly"; it is reported by subscription.list and changes no delivery rule.
func watchCard(ctx context.Context, q querier, cardID, userID string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO card_subscriptions(card_id,user_id,is_permanent,muted) VALUES(?,?,1,0)
		ON CONFLICT(card_id,user_id) DO UPDATE SET is_permanent=1, muted=0`, cardID, userID)
	return err
}

func muteCard(ctx context.Context, q querier, cardID, userID string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO card_subscriptions(card_id,user_id,is_permanent,muted) VALUES(?,?,0,1)
		ON CONFLICT(card_id,user_id) DO UPDATE SET is_permanent=0, muted=1`, cardID, userID)
	return err
}

// autoWatch adds an implicit watch; pref names the users column that must be on ("" = no preference check).
func autoWatch(ctx context.Context, q querier, cardID, userID, pref string) {
	cond := ""
	switch pref {
	case "subscribe_own_cards", "subscribe_to_card_when_commenting":
		cond = " AND " + pref + "=1"
	}
	_, _ = q.ExecContext(ctx, `INSERT OR IGNORE INTO card_subscriptions(card_id,user_id,is_permanent,muted)
		SELECT ?,id,0,0 FROM users WHERE id=? AND is_agent=0`+cond, cardID, userID)
}

// AutoWatchComment makes a commenter watch the card when their "Watch when I comment" preference
// (users.subscribe_to_card_when_commenting) is on. cardextras calls it after comment.create commits; no authz.
func AutoWatchComment(e *ops.Env, cardID, userID string) {
	autoWatch(e.Ctx, e.DB, cardID, userID, "subscribe_to_card_when_commenting")
}
