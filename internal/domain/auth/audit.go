package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// The admin audit log (table admin_audit, migration 0111; docs/DESIGN.md "Security model").
//
// Admin-side actions that change who can reach what leave one row, written in the SAME transaction as the change, plus
// an AUDIT line in the server log: agent.rotateKey (by the agent's creator, a manager of its boards or an admin),
// apikey.revoke (only when an admin revokes ANOTHER person's keys; revoking your own needs no row),
// group.member.add (every add that really adds someone; an add of a person already in the group changes nothing and
// writes nothing; a self-add is flagged), user.relink and the host command `weave admin relink` (source host, no
// actor). Rows are append-only and carry no foreign keys: names and e-mail addresses are snapshots, so the trail
// outlives the people, agents and groups it names. Nothing secret is stored: a key rotation records only the new
// key's display prefix (the same prefix the agent pages show), never the key or its hash.
//
// The ownerless-project recoveries keep their own table (project_recoveries) and their log on the Orphaned projects
// page; the projects domain imports auth, so merging them here would need a hook and a two-table paged merge.

// Audit actions.
const (
	AuditAgentRotateKey = "agent.rotateKey"
	AuditAPIKeyRevoke   = "apikey.revoke"
	AuditGroupMemberAdd = "group.member.add"
	AuditUserRelink     = "user.relink"
)

var auditActions = []string{AuditAgentRotateKey, AuditAPIKeyRevoke, AuditGroupMemberAdd, AuditUserRelink}

// AuditEntry is one row of the admin audit log.
type AuditEntry struct {
	ID          string         `json:"id"`
	At          string         `json:"at"`
	Action      string         `json:"action" desc:"agent.rotateKey | apikey.revoke | group.member.add | user.relink"`
	ActorID     string         `json:"actorId,omitempty" desc:"who did it (empty for a host command)"`
	ActorName   string         `json:"actorName,omitempty"`
	ActorEmail  string         `json:"actorEmail,omitempty"`
	TargetKind  string         `json:"targetKind" desc:"agent | user"`
	TargetID    string         `json:"targetId"`
	TargetName  string         `json:"targetName" desc:"name at the time of the change"`
	TargetEmail string         `json:"targetEmail,omitempty"`
	Details     map[string]any `json:"details" desc:"non-secret facts: keyPrefix/expiresAt (rotateKey), keysRevoked/keyPrefixes/all (apikey.revoke), groupId/groupName/self (group add), sessionsEnded (relink)"`
	Source      string         `json:"source" desc:"ui | api | host"`
}

// AuditListIn pages the audit log, newest first.
type AuditListIn struct {
	Action string `query:"action" desc:"only this action: agent.rotateKey | apikey.revoke | group.member.add | user.relink"`
	Before string `query:"before" desc:"cursor: the nextCursor of the previous page"`
	Limit  int    `query:"limit" desc:"entries per page (default 50, max 200)"`
}

// AuditPage is one page of the audit log.
type AuditPage struct {
	Items      []AuditEntry `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty" desc:"pass as before= for the next (older) page; empty on the last page"`
}

type auditExec interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// auditActor fills the actor snapshot of a from the signed-in caller (name and e-mail as they are now), read through
// x (the change's own transaction). A nil caller leaves the actor empty (host commands).
func auditActor(ctx context.Context, x auditExec, p *ops.Principal, a *AuditEntry) {
	if p == nil {
		return
	}
	a.ActorID, a.ActorName = p.UserID, p.Name
	_ = x.QueryRowContext(ctx, `SELECT name,email FROM users WHERE id=?`, p.UserID).Scan(&a.ActorName, &a.ActorEmail)
}

// auditSource is ui for the browser's /ui routes, api for the REST API and in-process calls.
func auditSource(e *ops.Env) string {
	if e.Req != nil && e.Req.URL != nil && strings.HasPrefix(e.Req.URL.Path, "/ui/") {
		return "ui"
	}
	return "api"
}

// writeAudit inserts a inside the caller's transaction (id and time are set here).
func writeAudit(ctx context.Context, x auditExec, a *AuditEntry) error {
	a.ID, a.At = db.NewID(), db.Now()
	if a.Details == nil {
		a.Details = map[string]any{}
	}
	det, err := json.Marshal(a.Details)
	if err != nil {
		return err
	}
	nz := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err = x.ExecContext(ctx, `INSERT INTO admin_audit(id,created_at,action,actor_id,actor_name,actor_email,target_kind,target_id,target_name,target_email,details,source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, a.ID, a.At, a.Action, nz(a.ActorID), nz(a.ActorName), nz(a.ActorEmail), a.TargetKind, a.TargetID, a.TargetName, a.TargetEmail, string(det), a.Source)
	return err
}

// logAudit writes the AUDIT line for a committed entry (ids, names and e-mails only, never a key).
func logAudit(a AuditEntry) {
	who := "host command"
	if a.ActorID != "" {
		who = fmt.Sprintf("%s (%s)", a.ActorID, a.ActorEmail)
	}
	log.Printf("auth: AUDIT %s on %s %s (%s) by %s via %s (audit %s)", a.Action, a.TargetKind, a.TargetID, a.TargetName, who, a.Source, a.ID)
}

// AuditLog reads one page of the log, newest first (before: the cursor of the previous page).
func AuditLog(ctx context.Context, q interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}, in AuditListIn) (AuditPage, error) {
	out := AuditPage{Items: []AuditEntry{}}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	where, args := []string{}, []any{}
	if in.Action != "" {
		ok := false
		for _, a := range auditActions {
			ok = ok || a == in.Action
		}
		if !ok {
			return out, ops.Invalid("action must be one of " + strings.Join(auditActions, ", "))
		}
		where, args = append(where, "action=?"), append(args, in.Action)
	}
	if in.Before != "" {
		at, id, ok := strings.Cut(in.Before, "|")
		if !ok {
			return out, ops.Invalid("before must be a nextCursor from a previous page")
		}
		where, args = append(where, "(created_at<? OR (created_at=? AND id<?))"), append(args, at, at, id)
	}
	sqlq := `SELECT id,created_at,action,COALESCE(actor_id,''),COALESCE(actor_name,''),COALESCE(actor_email,''),target_kind,target_id,target_name,target_email,details,source FROM admin_audit`
	if len(where) > 0 {
		sqlq += ` WHERE ` + strings.Join(where, " AND ")
	}
	rows, err := q.QueryContext(ctx, sqlq+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var a AuditEntry
		var det string
		if err := rows.Scan(&a.ID, &a.At, &a.Action, &a.ActorID, &a.ActorName, &a.ActorEmail, &a.TargetKind, &a.TargetID, &a.TargetName, &a.TargetEmail, &det, &a.Source); err != nil {
			return out, err
		}
		if json.Unmarshal([]byte(det), &a.Details) != nil || a.Details == nil {
			a.Details = map[string]any{}
		}
		out.Items = append(out.Items, a)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		last := out.Items[limit-1]
		out.NextCursor = last.At + "|" + last.ID
	}
	return out, nil
}

// ---- Admin console > Audit log ----

// auditRow is one row as the page shows it.
type auditRow struct {
	AuditEntry
	What, Detail, By string
	Self             bool
}

type auditData struct {
	Rows    []auditRow
	Action  string
	Actions []auditChoice
	Next    string
	Older   bool // this is not the first page
}

type auditChoice struct{ Value, Label string }

func auditView(a AuditEntry) auditRow {
	r := auditRow{AuditEntry: a}
	str := func(k string) string { s, _ := a.Details[k].(string); return s }
	target := a.TargetName
	if a.TargetEmail != "" {
		target += " (" + a.TargetEmail + ")"
	}
	switch a.Action {
	case AuditAgentRotateKey:
		r.What = "Rotated the API key of agent " + a.TargetName
		if p := str("keyPrefix"); p != "" {
			r.Detail = "New key " + p + "..."
		}
		if exp := str("expiresAt"); exp != "" {
			if t, err := time.Parse(time.RFC3339, exp); err == nil {
				exp = t.UTC().Format("2 Jan 2006")
			}
			r.Detail += ", expires " + exp
		}
	case AuditAPIKeyRevoke:
		n, _ := a.Details["keysRevoked"].(float64)
		if all, _ := a.Details["all"].(bool); all {
			r.What = fmt.Sprintf("Revoked all %d API keys of %s", int(n), target)
		} else {
			r.What = "Revoked an API key of " + target
		}
		if ps, ok := a.Details["keyPrefixes"].([]any); ok && len(ps) > 0 {
			var l []string
			for _, p := range ps {
				if s, ok := p.(string); ok {
					l = append(l, s+"...")
				}
			}
			r.Detail = "Keys " + strings.Join(l, ", ")
		}
	case AuditGroupMemberAdd:
		r.What = "Added " + target + " to group " + str("groupName")
		r.Self, _ = a.Details["self"].(bool)
	case AuditUserRelink:
		r.What = "Unlinked the SSO identity of " + target
		r.Detail = "Their sessions were ended; the next SSO sign-in with this e-mail binds a new identity."
	default:
		r.What = a.Action + " on " + target
	}
	switch {
	case a.Source == "host":
		r.By = "the host command"
	case a.ActorEmail != "":
		r.By = a.ActorName + " (" + a.ActorEmail + ")"
	default:
		r.By = a.ActorName
	}
	return r
}

func auditPage(c *web.Ctx) error {
	q := c.R.URL.Query()
	in := AuditListIn{Action: q.Get("action"), Before: q.Get("before")}
	p, err := ops.Typed[AuditPage](c.Env, "audit.list", in)
	if err != nil {
		return err
	}
	d := auditData{Action: in.Action, Next: p.NextCursor, Older: in.Before != "", Actions: []auditChoice{
		{"", "All actions"}, {AuditAgentRotateKey, "Agent key rotated"}, {AuditAPIKeyRevoke, "API key revoked"}, {AuditGroupMemberAdd, "Added to a group"}, {AuditUserRelink, "SSO identity unlinked"}}}
	for _, a := range p.Items {
		d.Rows = append(d.Rows, auditView(a))
	}
	return c.Render("auth_admin_audit.html", d)
}

func init() {
	ops.Register(ops.Op{Name: "audit.list", Summary: "Admin audit log, newest first: agent key rotations, revocations of other people's API keys, group additions and SSO identity unlinks (who, whom, when; never a key or secret). Admin only",
		Method: "GET", Path: "/audit", Auth: ops.AuthAdmin, UI: "GET /admin/audit"},
		func(e *ops.Env, in AuditListIn) (AuditPage, error) {
			if err := authz.RequireAuditLog(e); err != nil {
				return AuditPage{}, err
			}
			return AuditLog(e.Ctx, e.DB, in)
		})
	web.AddFuncs(template.FuncMap{"auth_when": func(s string) string {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return s
		}
		return t.UTC().Format("2 Jan 2006, 15:04 UTC")
	}})
	web.Route("GET /admin/audit", adminOnly(auditPage))
}
