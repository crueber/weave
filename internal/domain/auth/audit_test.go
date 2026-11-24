package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// auditRows dumps every audit row as one text line per row (all columns), newest last.
func auditRows(t *testing.T, e *ops.Env) []string {
	t.Helper()
	rows, err := e.DB.Query(`SELECT id||'|'||created_at||'|'||action||'|'||COALESCE(actor_id,'')||'|'||COALESCE(actor_name,'')||'|'||COALESCE(actor_email,'')||'|'||
		target_kind||'|'||target_id||'|'||target_name||'|'||target_email||'|'||details||'|'||source FROM admin_audit ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func auditList(t *testing.T, e *ops.Env, in AuditListIn) AuditPage {
	t.Helper()
	return call[AuditPage](t, e, "audit.list", in)
}

// agent.rotateKey writes exactly one row (actor, agent, key prefix) and never the key or its hash.
func TestAuditRotateKey(t *testing.T) {
	w := newAgentWorld(t)
	a := call[AgentOut](t, w.owner, "agent.create", AgentCreateIn{Name: "Bot", BoardIDs: []string{"b1"}})
	if n := len(auditRows(t, w.e)); n != 0 {
		t.Fatalf("agent.create wrote %d audit rows (only rotations are audited)", n)
	}
	r := call[AgentOut](t, w.owner, "agent.rotateKey", AgentRotateIn{ID: a.ID, ExpiresInDays: 30})
	rows := auditRows(t, w.e)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	sum := sha256.Sum256([]byte(r.Key.Key))
	for _, secret := range []string{r.Key.Key, hex.EncodeToString(sum[:]), r.Key.Key[len(r.Key.Prefix):]} {
		if strings.Contains(rows[0], secret) {
			t.Fatalf("the audit row holds key material %q: %s", secret, rows[0])
		}
	}
	got := auditList(t, w.admin, AuditListIn{}).Items
	if len(got) != 1 {
		t.Fatalf("list %+v", got)
	}
	g := got[0]
	if g.Action != AuditAgentRotateKey || g.ActorID != w.ownerID || g.ActorEmail != "owner@x.io" || g.TargetKind != "agent" || g.TargetID != a.ID || g.TargetName != "Bot" ||
		g.Details["keyPrefix"] != r.Key.Prefix || g.Details["expiresAt"] == nil || g.Source != "api" {
		t.Fatalf("row %+v", g)
	}
	// the admin rotating it from the console: source ui
	ui := testutil.As(w.e, w.admin.User)
	ui.Req = httptest.NewRequest("POST", "/ui/agents/"+a.ID+"/key", nil)
	call[AgentOut](t, ui, "agent.rotateKey", AgentRotateIn{ID: a.ID})
	if got := auditList(t, w.admin, AuditListIn{}).Items; len(got) != 2 || got[0].Source != "ui" || got[0].ActorEmail != "root@x.io" || got[0].Details["expiresAt"] != nil {
		t.Fatalf("console rotation %+v", got[0])
	}
}

// Every real group add is recorded; an admin adding themself is flagged; a re-add writes nothing.
func TestAuditGroupMemberAdd(t *testing.T) {
	e := newEnv(t)
	ad := mkUser(t, e, "root@x.io", "admin")
	bob := mkUser(t, e, "bob@x.io", "boardUser")
	ae := testutil.As(e, ad)
	g := call[*Group](t, ae, "group.create", GroupCreateIn{Name: "weave-team-ops"})
	call[*Group](t, ae, "group.member.add", GroupMemberAddIn{ID: g.ID, UserID: ad.UserID})
	call[*Group](t, ae, "group.member.add", GroupMemberAddIn{ID: g.ID, UserID: bob.UserID})
	call[*Group](t, ae, "group.member.add", GroupMemberAddIn{ID: g.ID, UserID: bob.UserID}) // already a member: no change
	if n := len(auditRows(t, e)); n != 2 {
		t.Fatalf("%d rows, want 2 (the re-add changes nothing)", n)
	}
	got := auditList(t, ae, AuditListIn{Action: AuditGroupMemberAdd}).Items
	if len(got) != 2 {
		t.Fatalf("list %+v", got)
	}
	self, other := got[1], got[0] // newest first
	if self.TargetID != ad.UserID || self.Details["self"] != true || self.Details["groupName"] != "weave-team-ops" || self.ActorID != ad.UserID {
		t.Fatalf("self-add %+v", self)
	}
	if other.TargetID != bob.UserID || other.TargetEmail != "bob@x.io" || other.Details["self"] != false || other.Details["groupId"] != g.ID {
		t.Fatalf("add %+v", other)
	}
}

// user.relink and the host command each write one row; the host row has no actor and source host.
func TestAuditRelink(t *testing.T) {
	e := newEnv(t)
	ad := mkUser(t, e, "root@x.io", "admin")
	ae := testutil.As(e, ad)
	if _, err := OIDCLogin(e, "https://idp|carol", claimsFor("carol@x.io", "weave-users")); err != nil {
		t.Fatal(err)
	}
	var carol string
	_ = e.DB.QueryRow(`SELECT id FROM users WHERE email='carol@x.io'`).Scan(&carol)
	call[*User](t, ae, "user.relink", IDIn{ID: carol})
	if err := RelinkByEmail(e.Ctx, e.DB, "carol@x.io"); err != nil {
		t.Fatal(err)
	}
	if err := RelinkByEmail(e.Ctx, e.DB, "nobody@x.io"); err == nil {
		t.Fatal("unknown user must fail")
	}
	got := auditList(t, ae, AuditListIn{Action: AuditUserRelink}).Items
	if len(got) != 2 {
		t.Fatalf("list %+v", got)
	}
	host, adm := got[0], got[1]
	if adm.ActorID != ad.UserID || adm.TargetID != carol || adm.TargetEmail != "carol@x.io" || adm.Source != "api" {
		t.Fatalf("admin relink %+v", adm)
	}
	if host.Source != "host" || host.ActorID != "" || host.ActorName != "" || host.TargetID != carol {
		t.Fatalf("host relink %+v", host)
	}
	for _, r := range auditRows(t, e) {
		if strings.Contains(r, "https://idp|carol") {
			t.Fatalf("the audit row holds the SSO subject: %s", r)
		}
	}
}

// Only human admins read the log; rows outlive the people they name; paging and the action filter work.
func TestAuditListAccessAndPaging(t *testing.T) {
	e := newEnv(t)
	ad := mkUser(t, e, "root@x.io", "admin")
	ae := testutil.As(e, ad)
	g := call[*Group](t, ae, "group.create", GroupCreateIn{Name: "g"})
	var ids []string
	for i := 0; i < 5; i++ {
		u := mkUser(t, e, string(rune('a'+i))+"@x.io", "boardUser")
		ids = append(ids, u.UserID)
		call[*Group](t, ae, "group.member.add", GroupMemberAddIn{ID: g.ID, UserID: u.UserID})
	}
	for _, who := range []*ops.Principal{nil, {UserID: ids[0], Role: "boardUser"}, {UserID: "agent", Role: "admin", IsAgent: true}} {
		_, err := ops.Call(testutil.As(e, who), "audit.list", &AuditListIn{})
		if oe, ok := err.(*ops.Error); !ok || (oe.Code != "unauthorized" && oe.Code != "forbidden") {
			t.Fatalf("%+v read the audit log: %v", who, err)
		}
	}
	// paging: 2 + 2 + 1, no overlap, newest first
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		p := auditList(t, ae, AuditListIn{Limit: 2, Before: cursor})
		pages++
		for _, it := range p.Items {
			if seen[it.ID] {
				t.Fatalf("row %s on two pages", it.ID)
			}
			seen[it.ID] = true
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("%d rows over %d pages", len(seen), pages)
	}
	if p := auditList(t, ae, AuditListIn{Action: AuditUserRelink}); len(p.Items) != 0 {
		t.Fatalf("filter %+v", p)
	}
	if _, err := ops.Call(ae, "audit.list", &AuditListIn{Action: "nope"}); err == nil {
		t.Fatal("an unknown action must be refused")
	}
	// hard-delete a person and the admin: the rows stay, with their snapshots
	if _, err := e.DB.Exec(`DELETE FROM users WHERE id IN (?,?)`, ids[0], ad.UserID); err != nil {
		t.Fatal(err)
	}
	other := testutil.As(e, mkUser(t, e, "root2@x.io", "admin"))
	p := auditList(t, other, AuditListIn{})
	if len(p.Items) != 5 || p.Items[4].TargetEmail != "a@x.io" || p.Items[4].ActorEmail != "root@x.io" {
		t.Fatalf("rows after deleting the people: %+v", p.Items)
	}
}

// group.create with initial members audits each add (self flagged) in the same transaction as the group.
func TestAuditGroupCreateMembers(t *testing.T) {
	e := newEnv(t)
	ad := mkUser(t, e, "root@x.io", "admin")
	bob := mkUser(t, e, "bob@x.io", "boardUser")
	ae := testutil.As(e, ad)
	g := call[*Group](t, ae, "group.create", GroupCreateIn{Name: "weave-team-x", MemberIDs: []string{ad.UserID, bob.UserID, "nobody", bob.UserID}})
	if len(g.MemberIDs) != 2 {
		t.Fatalf("members %v", g.MemberIDs)
	}
	got := auditList(t, ae, AuditListIn{}).Items
	if len(got) != 2 || got[1].TargetID != ad.UserID || got[1].Details["self"] != true || got[0].TargetID != bob.UserID || got[0].Details["groupName"] != "weave-team-x" {
		t.Fatalf("group.create audit: %+v", got)
	}
}

// The Audit log page: admins only (404 otherwise), rows rendered, and the "Older entries" link (an escaped cursor)
// leads to the next page.
func TestAuditLogPage(t *testing.T) {
	c, idp := newClient(t)
	c.signIn(idp, userClaims("boss@x.io", "weave-admins"))
	var boss string
	_ = c.e.DB.QueryRow(`SELECT id FROM users WHERE email='boss@x.io'`).Scan(&boss)
	ae := testutil.As(c.e, &ops.Principal{UserID: boss, Role: "admin", Name: "boss"})
	g := call[*Group](t, ae, "group.create", GroupCreateIn{Name: "g"})
	for i := 0; i < 51; i++ {
		u := mkUser(t, c.e, "u"+strings.Repeat("x", i)+"@x.io", "boardUser")
		call[*Group](t, ae, "group.member.add", GroupMemberAddIn{ID: g.ID, UserID: u.UserID})
	}
	w := c.do("GET", "/admin/audit", nil, nil)
	body := w.Body.String()
	if w.Code != 200 || strings.Count(body, `data-audit-action="group.member.add"`) != 50 || !strings.Contains(body, "Older entries") || !strings.Contains(body, `aria-current="page">Audit log`) {
		t.Fatalf("page 1: %d\n%s", w.Code, body)
	}
	i := strings.Index(body, `href="/admin/audit?before=`)
	if i < 0 {
		t.Fatal("no older link")
	}
	href := body[i+6:]
	href = strings.ReplaceAll(href[:strings.Index(href, `"`)], "&amp;", "&")
	w = c.do("GET", href, nil, nil)
	if w.Code != 200 || strings.Count(w.Body.String(), `data-audit-action="group.member.add"`) != 1 || strings.Contains(w.Body.String(), "Older entries") {
		t.Fatalf("page 2 (%s): %d\n%s", href, w.Code, w.Body.String())
	}
	// filter: no relinks yet
	if w := c.do("GET", "/admin/audit?action=user.relink", nil, nil); !strings.Contains(w.Body.String(), "No entries for this action.") {
		t.Fatalf("filtered page:\n%s", w.Body.String())
	}
	// a non-admin gets not found
	c2, idp2 := newClient(t)
	c2.signIn(idp2, userClaims("pleb@x.io", "weave-users"))
	if w := c2.do("GET", "/admin/audit", nil, nil); w.Code != 404 {
		t.Fatalf("non-admin audit page: %d", w.Code)
	}
}
