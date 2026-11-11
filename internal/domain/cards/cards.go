package cards

import (
	"context"
	"database/sql"
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
	Type              string   `json:"type"`
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

const cardCols = `id,board_id,list_id,creator_user_id,prev_list_id,cover_attachment_id,type,position,name,description,due_date,is_due_completed,
comments_total,is_closed,is_draft,is_template,lat,lng,location_name,recurrence,share_token,list_changed_at,created_at,updated_at,template_project_id,claimed_by,claimed_until,claim_note`

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// LoadCard fetches a card with labels, members and the caller's subscription (no authz).
func LoadCard(e *ops.Env, id string) (Card, error) { return loadCard(e, e.DB, id) }

func loadCard(e *ops.Env, q querier, id string) (Card, error) {
	var c Card
	err := q.QueryRowContext(e.Ctx, `SELECT `+cardCols+` FROM cards WHERE id=?`, id).Scan(&c.ID, &c.BoardID, &c.ListID, &c.CreatorUserID,
		&c.PrevListID, &c.CoverAttachmentID, &c.Type, &c.Position, &c.Name, &c.Description, &c.DueDate, &c.IsDueCompleted, &c.CommentsTotal,
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
	c.LabelIDs = strs(e, q, `SELECT label_id FROM card_labels WHERE card_id=?`, id)
	c.MemberIDs = strs(e, q, `SELECT user_id FROM card_memberships WHERE card_id=?`, id)
	if e.User != nil {
		var n int
		_ = q.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM card_subscriptions WHERE card_id=? AND user_id=?`, id, e.User.UserID).Scan(&n)
		c.IsSubscribed = n > 0
	}
	return c, nil
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
	if data == nil {
		data = map[string]any{}
	}
	data["id"], data["cardId"], data["boardId"] = cardID, cardID, boardID
	if ops.EmitHook != nil {
		ops.Emit(e, ops.Event{Type: typ, Kind: kind, BoardID: boardID, CardID: cardID, Name: name, Data: data})
		return
	}
	e.Hub.Publish(realtime.Event{Name: name, Room: "board:" + boardID, Data: data})
}

// editCard requires Edit on the card's board (workers who are card members may edit with View).
func editCard(e *ops.Env, id string) (string, error) {
	b, err := authz.BoardOfCard(e, id)
	if err != nil {
		return "", err
	}
	lvl := authz.BoardLevel(e, b)
	if lvl == authz.None {
		return "", ops.NotFound("card")
	}
	if lvl >= authz.Edit {
		return b, nil
	}
	if lvl >= authz.View && e.User != nil && e.User.Role == "worker" {
		var n int
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM card_memberships WHERE card_id=? AND user_id=?`, id, e.User.UserID).Scan(&n)
		if n > 0 {
			return b, nil
		}
	}
	return "", ops.Forbidden()
}

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

func listRef(ctx context.Context, q querier, id string) (boardID, typ string, err error) {
	err = q.QueryRowContext(ctx, `SELECT board_id,type FROM lists WHERE id=?`, id).Scan(&boardID, &typ)
	if err == sql.ErrNoRows {
		return "", "", ops.NotFound("list")
	}
	return
}

func endPos(ctx context.Context, q querier, listID string) float64 {
	var m sql.NullFloat64
	_ = q.QueryRowContext(ctx, `SELECT MAX(position) FROM cards WHERE list_id=?`, listID).Scan(&m)
	if !m.Valid {
		return db.PositionGap
	}
	return m.Float64 + db.PositionGap
}

func rebalance(ctx context.Context, q querier, listID string) {
	ids := []string{}
	rows, err := q.QueryContext(ctx, `SELECT id FROM cards WHERE list_id=? ORDER BY position, id`, listID)
	if err != nil {
		return
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		_, _ = q.ExecContext(ctx, `UPDATE cards SET position=? WHERE id=?`, float64(i+1)*db.PositionGap, id)
	}
}

// positionFor picks a position in listID: explicit, after/before a sibling (midpoint), or the end.
func positionFor(ctx context.Context, q querier, listID, selfID, afterID, beforeID string, explicit *float64) float64 {
	if explicit != nil {
		return *explicit
	}
	for attempt := 0; attempt < 2; attempt++ {
		var p, o sql.NullFloat64
		switch {
		case afterID != "" && afterID != selfID:
			if q.QueryRowContext(ctx, `SELECT position FROM cards WHERE id=? AND list_id=?`, afterID, listID).Scan(&p) != nil || !p.Valid {
				return endPos(ctx, q, listID)
			}
			_ = q.QueryRowContext(ctx, `SELECT MIN(position) FROM cards WHERE list_id=? AND position>? AND id<>?`, listID, p.Float64, selfID).Scan(&o)
			if !o.Valid {
				return p.Float64 + db.PositionGap
			}
			if o.Float64-p.Float64 >= 1 {
				return (p.Float64 + o.Float64) / 2
			}
		case beforeID != "" && beforeID != selfID:
			if q.QueryRowContext(ctx, `SELECT position FROM cards WHERE id=? AND list_id=?`, beforeID, listID).Scan(&p) != nil || !p.Valid {
				return endPos(ctx, q, listID)
			}
			_ = q.QueryRowContext(ctx, `SELECT MAX(position) FROM cards WHERE list_id=? AND position<? AND id<>?`, listID, p.Float64, selfID).Scan(&o)
			if !o.Valid {
				return p.Float64 / 2
			}
			if p.Float64-o.Float64 >= 1 {
				return (p.Float64 + o.Float64) / 2
			}
		default:
			return endPos(ctx, q, listID)
		}
		rebalance(ctx, q, listID)
	}
	return endPos(ctx, q, listID)
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

func subscribe(ctx context.Context, q querier, cardID, userID string, permanent bool) {
	p := 0
	if permanent {
		p = 1
	}
	_, _ = q.ExecContext(ctx, `INSERT INTO card_subscriptions(card_id,user_id,is_permanent) VALUES(?,?,?)
		ON CONFLICT(card_id,user_id) DO UPDATE SET is_permanent=MAX(is_permanent,excluded.is_permanent)`, cardID, userID, p)
}
