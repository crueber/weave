package cards

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
	"github.com/crueber/weave/internal/server"
)

// Claims: an agent (or person) takes a card with a time-limited lease so two workers
// never process the same card. The lease is the pair claimed_by/claimed_until on the
// card row; every claim is a single guarded write inside an IMMEDIATE transaction, so
// exactly one concurrent claimer wins. Expired leases count as unclaimed everywhere
// (loadCard, card.next, claim itself) and a sweeper emits card.released for them.

const (
	defaultClaimTTL = 900
	maxClaimTTL     = 86400
	minClaimTTL     = 10
)

type ClaimIn struct {
	ID         string `path:"id"`
	ListID     string `json:"listId" desc:"also move the card to this list when the claim succeeds (e.g. 'In progress')"`
	TTLSeconds int    `json:"ttlSeconds" desc:"lease length in seconds (default 900, 10..86400); renew before it ends"`
	Note       string `json:"note" desc:"short free text shown next to the claim"`
}

type ReleaseIn struct {
	ID     string `path:"id"`
	ListID string `json:"listId" desc:"also move the card to this list (e.g. back to 'Ready' or on to 'Review')"`
}

type RenewIn struct {
	ID         string `path:"id"`
	TTLSeconds int    `json:"ttlSeconds" desc:"new lease length from now (default 900)"`
}

type NextIn struct {
	ListID     string `path:"listId"`
	Claim      bool   `json:"claim" desc:"true = claim the card atomically (requires edit access)"`
	TTLSeconds int    `json:"ttlSeconds" desc:"lease length when claiming (default 900)"`
	MoveTo     string `json:"moveTo" desc:"with claim=true: move the claimed card to this list"`
	Note       string `json:"note" desc:"claim note"`
}

func init() {
	ops.Register(ops.Op{Name: "card.claim", Summary: "Claim a card with a lease (409 when someone else holds an unexpired claim); optionally move it", Method: "POST", Path: "/cards/{id}/claim",
		UI: "exempt:claims are taken by agents; the UI shows the claim on tiles and in the card sheet and people use card.move/member ops"}, cardClaim)
	ops.Register(ops.Op{Name: "card.release", Summary: "Release your claim on a card (managers may release anyone's); optionally move it", Method: "POST", Path: "/cards/{id}/release",
		UI: "POST /ui/cards/{id}/release"}, cardRelease)
	ops.Register(ops.Op{Name: "card.renew", Summary: "Extend your claim lease", Method: "POST", Path: "/cards/{id}/renew",
		UI: "exempt:lease renewal is a machine heartbeat"}, cardRenew)
	ops.Register(ops.Op{Name: "card.next", Summary: "Next unclaimed open card of a list in position order, optionally claiming it atomically ({item:null} when none)", Method: "POST", Path: "/lists/{listId}/next",
		UI: "exempt:work-queue pull for agents"}, cardNext)

	events.Snapshot = snapshotOne
	events.Snapshots = snapshotMany

	server.Hooks = append(server.Hooks, func(s *server.Server) error {
		go func() {
			t := time.NewTicker(20 * time.Second)
			defer t.Stop()
			for range t.C {
				_, _ = SweepClaims(context.Background(), s.DB, s.Hub, time.Now())
			}
		}()
		return nil
	})
}

func claimTTL(sec int) (time.Duration, error) {
	if sec == 0 {
		sec = defaultClaimTTL
	}
	if sec < minClaimTTL || sec > maxClaimTTL {
		return 0, ops.Invalid(fmt.Sprintf("ttlSeconds must be %d..%d", minClaimTTL, maxClaimTTL))
	}
	return time.Duration(sec) * time.Second, nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// claimInTx takes the lease inside tx (an IMMEDIATE write transaction). It returns
// the id of a previous holder whose lease had expired ("" if none) and whether the
// caller was newly added as a card member.
func claimInTx(e *ops.Env, tx *sql.Tx, cardID, note string, ttl time.Duration) (expiredHolder string, newMember bool, err error) {
	now := db.Now()
	var by, until sql.NullString
	var draft, tmpl bool
	if err = tx.QueryRowContext(e.Ctx, `SELECT claimed_by,claimed_until,is_draft,is_template FROM cards WHERE id=?`, cardID).Scan(&by, &until, &draft, &tmpl); err != nil {
		if err == sql.ErrNoRows {
			return "", false, ops.NotFound("card")
		}
		return "", false, err
	}
	if draft || tmpl {
		return "", false, ops.Invalid("drafts and templates cannot be claimed")
	}
	me := e.User.UserID
	if by.Valid && by.String != me {
		if until.Valid && until.String >= now {
			var n string
			_ = tx.QueryRowContext(e.Ctx, `SELECT name FROM users WHERE id=?`, by.String).Scan(&n)
			return "", false, ops.Conflict(fmt.Sprintf("card is claimed by %s until %s", n, until.String))
		}
		expiredHolder = by.String
	}
	var nv any
	if n := strings.TrimSpace(note); n != "" {
		nv = clipStr(n, 500)
	}
	res, err := tx.ExecContext(e.Ctx, `UPDATE cards SET claimed_by=?,claimed_until=?,claim_note=?,updated_at=? WHERE id=?
		AND (claimed_by IS NULL OR claimed_by=? OR claimed_until IS NULL OR claimed_until<?)`,
		me, stamp(time.Now().Add(ttl)), nv, now, cardID, me, now)
	if err != nil {
		return "", false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", false, ops.Conflict("card was claimed by someone else")
	}
	r2, err := tx.ExecContext(e.Ctx, `INSERT OR IGNORE INTO card_memberships(card_id,user_id) VALUES(?,?)`, cardID, me)
	if err != nil {
		return "", false, err
	}
	n, _ := r2.RowsAffected()
	return expiredHolder, n > 0, nil
}

func clipStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// finishClaim emits the events of a successful claim and performs the optional move.
func finishClaim(e *ops.Env, boardID, cardID, listID, note string, ttl time.Duration, expired string, newMember bool) (Card, error) {
	c, err := loadCard(e, e.DB, cardID)
	if err != nil {
		return c, err
	}
	if expired != "" {
		emit(e, "cardRelease", "card.released", boardID, cardID, map[string]any{"reason": "lease_expired", "previousClaimedBy": expired})
	}
	if newMember {
		emit(e, "cardMembershipCreate", "card.member.added", boardID, cardID, map[string]any{"userId": e.User.UserID})
	}
	data := map[string]any{"ttlSeconds": int(ttl.Seconds())}
	if c.ClaimedUntil != nil {
		data["claimedUntil"] = *c.ClaimedUntil
	}
	if note != "" {
		data["note"] = note
	}
	emit(e, "cardClaim", "card.claimed", boardID, cardID, data)
	if listID != "" && listID != c.ListID {
		if _, err := doMove(e, cardID, listID, nil, "", ""); err != nil {
			// keep claim+move atomic from the caller's point of view
			if ok, _ := releaseNow(e, cardID); ok {
				emit(e, "cardRelease", "card.released", boardID, cardID, map[string]any{"reason": "move_failed", "previousClaimedBy": e.User.UserID})
			}
			return Card{}, err
		}
		return loadCard(e, e.DB, cardID)
	}
	return c, nil
}

// checkMoveTarget validates a requested move before anything is claimed.
func checkMoveTarget(e *ops.Env, srcBoard, listID string) error {
	if listID == "" {
		return nil
	}
	dst, typ, err := listRef(e.Ctx, e.DB, listID)
	if err != nil {
		return err
	}
	if typ != "active" && typ != "closed" {
		return ops.Invalid("claims can only move a card to an active or closed list")
	}
	if dst != srcBoard {
		return authz.RequireBoard(e, dst, authz.Edit)
	}
	return nil
}

func cardClaim(e *ops.Env, in ClaimIn) (Card, error) {
	boardID, err := authz.RequireCard(e, in.ID, authz.Edit)
	if err != nil {
		return Card{}, err
	}
	ttl, err := claimTTL(in.TTLSeconds)
	if err != nil {
		return Card{}, err
	}
	if err := checkMoveTarget(e, boardID, in.ListID); err != nil {
		return Card{}, err
	}
	var expired string
	var newMember bool
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		var err error
		expired, newMember, err = claimInTx(e, tx, in.ID, in.Note, ttl)
		return err
	})
	if err != nil {
		return Card{}, err
	}
	return finishClaim(e, boardID, in.ID, in.ListID, strings.TrimSpace(in.Note), ttl, expired, newMember)
}

// releaseNow clears the caller's own claim (no authz; callers check).
func releaseNow(e *ops.Env, cardID string) (bool, error) {
	res, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET claimed_by=NULL,claimed_until=NULL,claim_note=NULL,updated_at=? WHERE id=? AND claimed_by=?`, db.Now(), cardID, e.User.UserID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func cardRelease(e *ops.Env, in ReleaseIn) (Card, error) {
	boardID, err := authz.RequireCard(e, in.ID, authz.Edit)
	if err != nil {
		return Card{}, err
	}
	if err := checkMoveTarget(e, boardID, in.ListID); err != nil {
		return Card{}, err
	}
	var by sql.NullString
	var until sql.NullString
	if err := e.DB.QueryRowContext(e.Ctx, `SELECT claimed_by,claimed_until FROM cards WHERE id=?`, in.ID).Scan(&by, &until); err != nil {
		return Card{}, ops.NotFound("card")
	}
	held := by.Valid && until.Valid && until.String >= db.Now()
	if held && by.String != e.User.UserID && authz.BoardLevel(e, boardID) < authz.Manage {
		return Card{}, ops.Conflict("card is claimed by someone else")
	}
	if by.Valid {
		res, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET claimed_by=NULL,claimed_until=NULL,claim_note=NULL,updated_at=? WHERE id=? AND claimed_by=?`, db.Now(), in.ID, by.String)
		if err != nil {
			return Card{}, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			reason := "released"
			if !held {
				reason = "lease_expired"
			} else if by.String != e.User.UserID {
				reason = "forced"
			}
			emit(e, "cardRelease", "card.released", boardID, in.ID, map[string]any{"reason": reason, "previousClaimedBy": by.String})
		}
	}
	if in.ListID != "" {
		if c, err := loadCard(e, e.DB, in.ID); err == nil && c.ListID != in.ListID {
			return doMove(e, in.ID, in.ListID, nil, "", "")
		}
	}
	return loadCard(e, e.DB, in.ID)
}

func cardRenew(e *ops.Env, in RenewIn) (Card, error) {
	boardID, err := authz.RequireCard(e, in.ID, authz.Edit)
	if err != nil {
		return Card{}, err
	}
	ttl, err := claimTTL(in.TTLSeconds)
	if err != nil {
		return Card{}, err
	}
	until := stamp(time.Now().Add(ttl))
	// Only the current holder may renew; a lapsed lease can still be renewed until someone else takes it.
	res, err := e.DB.ExecContext(e.Ctx, `UPDATE cards SET claimed_until=?,updated_at=? WHERE id=? AND claimed_by=?`, until, db.Now(), in.ID, e.User.UserID)
	if err != nil {
		return Card{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Card{}, ops.Conflict("you do not hold a claim on this card")
	}
	emit(e, "cardClaim", "card.claimed", boardID, in.ID, map[string]any{"renewed": true, "ttlSeconds": int(ttl.Seconds()), "claimedUntil": until})
	return loadCard(e, e.DB, in.ID)
}

func cardNext(e *ops.Env, in NextIn) (*events.CardSnap, error) {
	var boardID, ltype string
	if err := e.DB.QueryRowContext(e.Ctx, `SELECT board_id,type FROM lists WHERE id=?`, in.ListID).Scan(&boardID, &ltype); err != nil {
		return nil, ops.NotFound("list")
	}
	need := authz.View
	if in.Claim {
		need = authz.Edit
	}
	if err := authz.RequireBoard(e, boardID, need); err != nil {
		return nil, err
	}
	const pick = `SELECT id FROM cards WHERE list_id=? AND is_closed=0 AND is_draft=0 AND is_template=0
		AND (claimed_by IS NULL OR claimed_until IS NULL OR claimed_until<?) ORDER BY position, created_at, id LIMIT 1`
	if !in.Claim {
		var id string
		if err := e.DB.QueryRowContext(e.Ctx, pick, in.ListID, db.Now()).Scan(&id); err != nil {
			return nil, nil
		}
		return snapshotOne(e, id), nil
	}
	ttl, err := claimTTL(in.TTLSeconds)
	if err != nil {
		return nil, err
	}
	if err := checkMoveTarget(e, boardID, in.MoveTo); err != nil {
		return nil, err
	}
	var id, expired string
	var newMember bool
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(e.Ctx, pick, in.ListID, db.Now()).Scan(&id); err != nil {
			if err == sql.ErrNoRows {
				id = ""
				return nil
			}
			return err
		}
		var err error
		expired, newMember, err = claimInTx(e, tx, id, in.Note, ttl)
		return err
	})
	if err != nil || id == "" {
		return nil, err
	}
	if _, err := finishClaim(e, boardID, id, in.MoveTo, strings.TrimSpace(in.Note), ttl, expired, newMember); err != nil {
		return nil, err
	}
	return snapshotOne(e, id), nil
}

// SweepClaims releases every expired lease and emits card.released (reason lease_expired).
// The guarded UPDATE leaves a lease that was just re-taken or renewed alone.
func SweepClaims(ctx context.Context, d *db.DB, hub *realtime.Hub, now time.Time) (int, error) {
	cut := stamp(now)
	rows, err := d.QueryContext(ctx, `SELECT id,board_id,claimed_by FROM cards WHERE claimed_by IS NOT NULL AND (claimed_until IS NULL OR claimed_until<?)`, cut)
	if err != nil {
		return 0, err
	}
	type exp struct{ id, board, by string }
	var list []exp
	for rows.Next() {
		var x exp
		if rows.Scan(&x.id, &x.board, &x.by) == nil {
			list = append(list, x)
		}
	}
	rows.Close()
	e := &ops.Env{Ctx: ctx, DB: d, Hub: hub, Svc: map[string]any{}}
	n := 0
	for _, x := range list {
		res, err := d.ExecContext(ctx, `UPDATE cards SET claimed_by=NULL,claimed_until=NULL,claim_note=NULL WHERE id=? AND claimed_by=? AND (claimed_until IS NULL OR claimed_until<?)`, x.id, x.by, cut)
		if err != nil {
			continue
		}
		if a, _ := res.RowsAffected(); a > 0 {
			n++
			emit(e, "cardRelease", "card.released", x.board, x.id, map[string]any{"reason": "lease_expired", "previousClaimedBy": x.by})
		}
	}
	return n, nil
}

// ---- compact snapshots (events, board.tree, card.next)

func snapshotOne(e *ops.Env, id string) *events.CardSnap {
	s := snapshotMany(e, []string{id})
	if len(s) == 0 {
		return nil
	}
	return &s[0]
}

const snapDescMax = 4000

func snapshotMany(e *ops.Env, ids []string) []events.CardSnap {
	byID := map[string]*events.CardSnap{}
	out := make([]events.CardSnap, 0, len(ids))
	now := db.Now()
	for start := 0; start < len(ids); start += 400 {
		end := min(start+400, len(ids))
		chunk := ids[start:end]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT c.id,c.board_id,c.name,COALESCE(c.description,''),c.list_id,c.position,c.due_date,c.is_closed,c.claimed_by,u.name,c.claimed_until
			FROM cards c LEFT JOIN users u ON u.id=c.claimed_by WHERE c.id IN (`+ph+`)`, args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var s events.CardSnap
			var by, byName, until sql.NullString
			if rows.Scan(&s.ID, &s.BoardID, &s.Name, &s.Description, &s.ListID, &s.Position, &s.DueDate, &s.IsClosed, &by, &byName, &until) != nil {
				continue
			}
			if r := []rune(s.Description); len(r) > snapDescMax {
				s.Description, s.DescriptionTruncated = string(r[:snapDescMax]), true
			}
			if by.Valid && until.Valid && until.String >= now {
				s.ClaimedBy, s.ClaimedUntil = &by.String, &until.String
				if byName.Valid {
					s.ClaimedByName = &byName.String
				}
			}
			s.Labels, s.Members = []events.Ref{}, []events.Ref{}
			byID[s.ID] = &s
		}
		rows.Close()
		if lr, err := e.DB.QueryContext(e.Ctx, `SELECT cl.card_id,l.id,COALESCE(l.name,''),l.color FROM card_labels cl JOIN labels l ON l.id=cl.label_id
			WHERE cl.card_id IN (`+ph+`) ORDER BY l.position`, args...); err == nil {
			for lr.Next() {
				var cid string
				var r events.Ref
				var col sql.NullString
				if lr.Scan(&cid, &r.ID, &r.Name, &col) == nil {
					r.Color = col.String
					if s := byID[cid]; s != nil {
						s.Labels = append(s.Labels, r)
					}
				}
			}
			lr.Close()
		}
		if mr, err := e.DB.QueryContext(e.Ctx, `SELECT cm.card_id,u.id,u.name,u.is_agent FROM card_memberships cm JOIN users u ON u.id=cm.user_id
			WHERE cm.card_id IN (`+ph+`) ORDER BY u.name`, args...); err == nil {
			for mr.Next() {
				var cid string
				var r events.Ref
				if mr.Scan(&cid, &r.ID, &r.Name, &r.IsAgent) == nil {
					if s := byID[cid]; s != nil {
						s.Members = append(s.Members, r)
					}
				}
			}
			mr.Close()
		}
	}
	for _, id := range ids {
		if s := byID[id]; s != nil {
			out = append(out, *s)
		}
	}
	return out
}
