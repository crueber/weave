package authz_test

// TestNoAdminContentBypass: the admin role is system administration and carries NO content access. Content access
// comes only from ownership and membership (BoardLevel/ProjectLevel and the helpers built on them).
//
// The test parses every non-test Go file under internal/ and cmd/ and finds every place the role name "admin" turns
// into a decision: a comparison with the string "admin", a call of authz.SystemAdmin (or a helper that only wraps
// it: ProjectRecoveryAdmin, AuditLogAdmin, the auth domain's isAdmin), and SQL that tests role='admin'. Each site, keyed by file and
// enclosing function, must be on the allowlist below with the SYSTEM function it serves. A new site fails the test
// until someone adds it here with a reason, which is the moment to ask "is this a system function, or am I letting
// the admin into someone's content?". Stale entries fail too, so the list stays the truth. Templates and static
// JavaScript may not branch on the admin role at all (the capability layer, authz.Home().AdminConsole, decides).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// adminSites: "path/to/file.go:Func" -> the system function it serves. Content access is never a valid reason.
var adminSites = map[string]string{
	// the definitions
	"internal/domain/auth/ui.go:RoleLabel":                  "the display label of a role name (Admin / Member / ...): no decision",
	"cmd/weave/main.go:main":                                "the host CLI's `weave admin ...` subcommand name (not a role check)",
	"cmd/weave/recovery.go:writePreflight":                  "host-only upgrade preflight report (labels non-admin accounts checked with --email)",
	"internal/authz/recovery.go:RequireProjectRecovery":     "ownerless-project recovery (audited; names a new manager, shows no content)",
	"internal/cli/mcp_server.go:newMCPServer":               "MCP: hide admin-only tools from non-admins (presentation only; the server enforces)",
	"internal/cli/mcp_tools.go:mcpOpSummary":                "MCP: mark admin-only tools (op schema Auth=admin)",
	"internal/cli/mcp_tools.go:mcpVisible":                  "MCP: hide admin-only tools from non-admins (presentation only; the server enforces)",
	"internal/domain/auth/agents.go:agentIDsFor":            "agent ACCOUNT listing: admins list every agent account",
	"internal/domain/auth/oidc.go:OIDCLogin":                "IdP-owned roles: the last active admin is never demoted on sign-in (never lock the instance)",
	"internal/domain/auth/users.go:redact":                  "user directory: admins see e-mail and account details (user administration)",
	"internal/domain/auth/users.go:resolve":                 "user administration: admins edit any person's profile, avatar, preferences and API key (account data)",
	"internal/domain/boards/agentprompt_ui.go:init":         "the prompt dialog links admins to the instance-wide prompt defaults (system settings)",
	"internal/ops/ops.go:authorize":                         "ops.AuthAdmin: system ops (users, groups, config, backup, recovery...)",
	"internal/authz/caps.go:SystemAdmin":                    "THE definition: a human (non-agent) account with role admin",
	"internal/authz/caps.go:Home":                           "the Admin console link in the account menu",
	"internal/authz/authz.go:CanCreateProject":              "admins (like boardUsers) may create projects of their own; they then own them",
	"internal/authz/recovery.go:ProjectRecoveryAdmin":       "ownerless-project recovery (audited; names a new manager, shows no content)",
	"internal/authz/webhooks.go:RequireWebhookScope":        "global and project webhooks are instance integrations (admin console); board webhooks need board Manage",
	"internal/authz/webhooks.go:RequireWebhook":             "global and project webhooks are instance integrations; board webhooks need board Manage",
	"internal/authz/agents.go:CanManageAgent":               "agent ACCOUNT control (rename, deactivate, rotate key, delete); board grants still need board Manage",
	"internal/authz/agents.go:CanSeeAgent":                  "agent ACCOUNT listing (names, creator, status, board count; no board names)",
	"internal/domain/auth/users.go:isAdmin":                 "wrapper of authz.SystemAdmin for the auth domain (user and group administration)",
	"internal/domain/auth/users.go:countOtherAdmins":        "the last active admin cannot be deactivated or deleted",
	"internal/domain/auth/users.go:init":                    "user.deactivate/delete refuse to remove the last admin; user.list shows deactivated accounts to admins",
	"internal/domain/auth/ui.go:adminOnly":                  "admin console pages (users, SSO, agents)",
	"internal/domain/auth/ui.go:renderGroups":               "group administration page",
	"internal/domain/auth/agents.go:init":                   "agent ACCOUNTS: admins choose an agent's role, create board-less agents, and are not capped per person",
	"internal/domain/boards/members.go:redactMember":        "user directory: e-mail addresses are shown to admins (as in user.list)",
	"internal/domain/projects/managers.go:Managers":         "user directory: e-mail addresses are shown to admins (as in user.list)",
	"internal/domain/projects/preflight.go:adminReach":      "host-only upgrade preflight: lists what each admin would stop seeing",
	"internal/domain/projects/preflight.go:adminOnlyBoards": "host-only upgrade preflight",
	"internal/domain/notify/ui_webhooks.go:adminWebhooks":   "admin console webhooks page (global and project webhooks)",
	"internal/domain/notify/webhooks.go:forViewer":          "admins see more of a receiver's response body (their own integrations' diagnostics)",
	"internal/domain/notify/webhooks.go:init":               "global/project webhook scopes and the admin destination policy are instance settings",
	"internal/domain/projects/orphans_ui.go:recoveryOnly":   "Orphaned projects page",
	"internal/authz/audit.go:AuditLogAdmin":                 "admin audit log (who rotated agent keys, changed groups, unlinked identities; no content)",
	"internal/authz/audit.go:RequireAuditLog":               "admin audit log (audit.list)",
}

// adminSiteHits is how many admin-role decisions each allowlisted function holds (a function such as a domain's init
// registers many ops, so a new shortcut inside an allowlisted function must still fail the test).
var adminSiteHits = map[string]int{
	"cmd/weave/main.go:main":                                4,
	"cmd/weave/recovery.go:writePreflight":                  1,
	"internal/authz/agents.go:CanManageAgent":               1,
	"internal/authz/agents.go:CanSeeAgent":                  1,
	"internal/authz/authz.go:CanCreateProject":              1,
	"internal/authz/caps.go:Home":                           1,
	"internal/authz/caps.go:SystemAdmin":                    1,
	"internal/authz/recovery.go:ProjectRecoveryAdmin":       1,
	"internal/authz/recovery.go:RequireProjectRecovery":     1,
	"internal/authz/webhooks.go:RequireWebhook":             1,
	"internal/authz/webhooks.go:RequireWebhookScope":        1,
	"internal/cli/mcp_server.go:newMCPServer":               1,
	"internal/cli/mcp_tools.go:mcpOpSummary":                1,
	"internal/cli/mcp_tools.go:mcpVisible":                  1,
	"internal/domain/auth/agents.go:agentIDsFor":            1,
	"internal/domain/auth/agents.go:init":                   2,
	"internal/domain/auth/oidc.go:OIDCLogin":                1,
	"internal/domain/auth/ui.go:adminOnly":                  1,
	"internal/domain/auth/ui.go:renderGroups":               1,
	"internal/domain/auth/ui.go:RoleLabel":                  1,
	"internal/domain/auth/users.go:countOtherAdmins":        1,
	"internal/domain/auth/users.go:init":                    4,
	"internal/domain/auth/users.go:isAdmin":                 1,
	"internal/domain/auth/users.go:redact":                  1,
	"internal/domain/auth/users.go:resolve":                 2,
	"internal/domain/boards/agentprompt_ui.go:init":         1,
	"internal/domain/boards/members.go:redactMember":        1,
	"internal/domain/notify/ui_webhooks.go:adminWebhooks":   1,
	"internal/domain/notify/webhooks.go:forViewer":          1,
	"internal/domain/notify/webhooks.go:init":               4,
	"internal/domain/projects/managers.go:Managers":         1,
	"internal/domain/projects/orphans_ui.go:recoveryOnly":   1,
	"internal/domain/projects/preflight.go:adminOnlyBoards": 1,
	"internal/domain/projects/preflight.go:adminReach":      1,
	"internal/ops/ops.go:authorize":                         1,
	"internal/authz/audit.go:AuditLogAdmin":                 1,
	"internal/authz/audit.go:RequireAuditLog":               1,
}

// contentFuncs must never consult the admin role, not even through an allowlisted helper.
var contentFuncs = []string{"BoardLevel", "BoardLevels", "ProjectLevel", "ProjectLevels", "ViewersAmong", "IsBoardMember",
	"RequireBoard", "RequireCard", "RequireProject", "TemplateVisible", "RoomAllowed", "CanCreateBoard", "Board", "Card", "Project",
	"cardEditable", "RequireCardEdit", "RequireBoardLabels", "CanRemoveOwningManager",
	"cardAccess", "CardAccessFor", "RequireCardOn", "CardViewers", "CardLevels", "BoardOfCard", "decide", "CardAccesses", "AlsoOn"}

var adminWrappers = map[string]bool{"SystemAdmin": true, "ProjectRecoveryAdmin": true, "AuditLogAdmin": true, "isAdmin": true}

var sqlAdmin = regexp.MustCompile(`role\s*(=|==|<>|!=)\s*'admin'`)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("go.mod not found")
	return ""
}

func TestNoAdminContentBypass(t *testing.T) {
	root := repoRoot(t)
	found := map[string]int{}
	var where []string
	for _, top := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			fs := token.NewFileSet()
			f, err := parser.ParseFile(fs, p, nil, 0)
			if err != nil {
				t.Fatalf("%s: %v", rel, err)
			}
			for _, decl := range f.Decls {
				name := "init"
				var body ast.Node = decl
				if fd, ok := decl.(*ast.FuncDecl); ok {
					name = fd.Name.Name
					if fd.Body == nil {
						continue
					}
					body = fd.Body
				} else {
					continue // package-level vars/consts: role tables (Roles, roleRank) are data, not decisions
				}
				hits := 0
				ast.Inspect(body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.BinaryExpr:
						if (x.Op == token.EQL || x.Op == token.NEQ) && (isAdminLit(x.X) || isAdminLit(x.Y)) {
							hits++
						}
					case *ast.CallExpr:
						switch fn := x.Fun.(type) {
						case *ast.Ident:
							if adminWrappers[fn.Name] {
								hits++
							}
						case *ast.SelectorExpr:
							if adminWrappers[fn.Sel.Name] {
								hits++
							}
						}
					case *ast.BasicLit:
						if x.Kind == token.STRING && sqlAdmin.MatchString(x.Value) {
							hits++
						}
					case *ast.CaseClause:
						for _, e := range x.List {
							if isAdminLit(e) {
								hits++
							}
						}
					}
					return true
				})
				if hits == 0 {
					continue
				}
				key := rel + ":" + name
				found[key] = hits
				if strings.HasPrefix(rel, "internal/authz/") {
					for _, c := range contentFuncs {
						if name == c {
							t.Errorf("%s: a content-access function consults the admin role; the admin role carries no content access", key)
						}
					}
				}
				if want, ok := adminSiteHits[key]; ok && want != hits {
					t.Errorf("%s: %d admin-role decisions, the allowlist expects %d: a new one inside an allowlisted function needs the same review (update adminSiteHits with its reason)", key, hits, want)
				}
				if os.Getenv("NOBYPASS_DUMP") != "" {
					t.Logf("%q: %d,", key, hits)
				}
				if _, ok := adminSites[key]; !ok {
					where = append(where, key)
				}
			}
			return nil
		})
	}
	sort.Strings(where)
	for _, k := range where {
		t.Errorf("new admin-role decision at %s: if it is a SYSTEM function add it to adminSites with the reason; content access only comes from membership/ownership", k)
	}
	var stale []string
	for k := range adminSites {
		if found[k] == 0 {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		t.Errorf("stale adminSites entry %s (no admin-role decision there any more): remove it", k)
	}

	// templates and static JS never branch on the role name
	tplAdmin := regexp.MustCompile(`(?:eq|ne)\s+[^}]*Role[^}]*"admin"|\.Role\s*(?:===?|!==?)\s*['"]admin['"]|role\s*(?:===?|!==?)\s*['"]admin['"]`)
	for _, dir := range []string{"internal/web/templates", "internal/web/static"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js")) {
				return nil
			}
			b, _ := os.ReadFile(p)
			if m := tplAdmin.Find(b); m != nil {
				rel, _ := filepath.Rel(root, p)
				t.Errorf("%s branches on the admin role (%s): use the capability structs (authz.Home/Project/Board/Card)", filepath.ToSlash(rel), m)
			}
			return nil
		})
	}
}

func isAdminLit(e ast.Expr) bool {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(bl.Value)
	return err == nil && s == "admin"
}
