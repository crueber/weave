package auth

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// Agent accounts: non-SSO service users for AI agents. They have no login (the OIDC
// callback and session lookup skip is_agent users) and authenticate with an API key
// only. Board access is a normal board membership, so every authz rule applies.
//
// Who may do what is decided in internal/authz (agents.go): anyone who manages a board creates agents for it;
// the creator (users.created_by) manages the account; a manager of every board the agent touches may manage it
// too; admins manage every ACCOUNT (rename, deactivate, rotate the key, delete) but, like everyone, see and change
// the agent's board access only on boards they manage themselves. Grants need Manage on the target board and are editor | viewer | commenter
// memberships only, so an agent never gets project-wide access, an admin role or the right to manage members.

type AgentBoard struct {
	BoardID   string `json:"boardId"`
	BoardName string `json:"boardName"`
	MemberID  string `json:"memberId"`
	Role      string `json:"role" desc:"editor | viewer | commenter"`
}

type AgentOut struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Role            string       `json:"role"`
	IsAgent         bool         `json:"isAgent"`
	IsDeactivated   bool         `json:"isDeactivated"`
	KeyPrefix       string       `json:"keyPrefix,omitempty" desc:"prefix of the agent's key (an agent holds exactly one)"`
	APIKeyExpiresAt *string      `json:"apikeyExpiresAt,omitempty" desc:"when the key stops working; absent = never"`
	APIKeyExpired   bool         `json:"apikeyExpired,omitempty" desc:"true when the key has expired and is rejected"`
	CreatedAt       string       `json:"createdAt"`
	CreatedBy       string       `json:"createdBy,omitempty" desc:"id of the person who created the agent (absent for agents made before owners could create them)"`
	CreatedByName   string       `json:"createdByName,omitempty"`
	CanManage       bool         `json:"canManage" desc:"true when you may rename, deactivate, rotate the key of or delete this agent"`
	Boards          []AgentBoard `json:"boards" desc:"board access on the boards you manage (admins included: the admin role shows no other board)"`
	BoardCount      int          `json:"boardCount" desc:"how many boards the agent has access to in total, including boards you cannot see"`
	Key             *APIKeyOut   `json:"key,omitempty" desc:"the API key, returned once on create and rotateKey"`
}

type AgentCreateIn struct {
	Name          string   `json:"name" required:"true" desc:"display name shown on cards and in activity"`
	BoardIDs      []string `json:"boardIds" desc:"boards to grant access to; you need Manage on each (board owner or project manager). Required unless you are an admin"`
	Role          string   `json:"role" desc:"admins only: user role worker | boardUser (default) | guest; agents created by board owners are always boardUser"`
	BoardRole     string   `json:"boardRole" desc:"access on boardIds: editor (default) | viewer | commenter"`
	ExpiresInDays int      `json:"expiresInDays" desc:"optional: the API key stops working after this many days (1-3650); 0 or omitted = never"`
}

type AgentListIn struct {
	BoardID string `query:"boardId" desc:"optional: only agents with access to this board (needs Manage on it)"`
}

type AgentUpdateIn struct {
	ID            string  `path:"id"`
	Name          *string `json:"name"`
	Role          *string `json:"role" desc:"admins only: worker | boardUser | guest"`
	IsDeactivated *bool   `json:"isDeactivated" desc:"true disables the key without deleting the account"`
}

type AgentIDIn struct {
	ID string `path:"id" desc:"agent user id"`
}

type AgentRotateIn struct {
	ID            string `path:"id" desc:"agent user id"`
	ExpiresInDays int    `json:"expiresInDays" desc:"optional: the new API key stops working after this many days (1-3650); 0 or omitted = never"`
}

type AgentAccessIn struct {
	ID      string `path:"id" desc:"agent user id"`
	BoardID string `json:"boardId" required:"true" desc:"the board; granting needs Manage on it, revoking needs Manage on it or being the agent's creator"`
	Role    string `json:"role" desc:"editor (default) | viewer | commenter"`
	Revoke  bool   `json:"revoke" desc:"true removes the agent from the board"`
}

func agentRoleOK(r string) bool { return r == "worker" || r == "boardUser" || r == "guest" }

// defaultAgentsPerUser caps the agents one person (not an admin) may create (WEAVE_AGENTS_PER_USER; 0 = no cap).
const defaultAgentsPerUser = 50

var (
	agentsCapOnce sync.Once
	agentsCapVal  = defaultAgentsPerUser
	agentsCapEnv  = func() string { return os.Getenv("WEAVE_AGENTS_PER_USER") }
)

func agentsPerUser() int {
	agentsCapOnce.Do(func() {
		if v := strings.TrimSpace(agentsCapEnv()); v != "" {
			if x, err := strconv.Atoi(v); err == nil && x >= 0 {
				agentsCapVal = x
			} else {
				fmt.Println("WEAVE_AGENTS_PER_USER: invalid value", v, "- using", defaultAgentsPerUser)
			}
		}
	})
	return agentsCapVal
}

func boardRoleOK(r string) bool { return r == "" || r == "editor" || r == "viewer" || r == "commenter" }

// loadAgent reads one agent. Callers see only the boards they manage in Boards (the admin role adds none); BoardCount
// is the total.
func loadAgent(e *ops.Env, id string) (AgentOut, error) {
	list, err := loadAgents(e, []string{id})
	if err != nil {
		return AgentOut{}, err
	}
	if len(list) == 0 {
		return AgentOut{}, ops.NotFound("agent")
	}
	return list[0], nil
}

// loadAgents reads many agents in a constant number of queries (preserving ids order; unknown ids are skipped).
func loadAgents(e *ops.Env, ids []string) ([]AgentOut, error) {
	if len(ids) == 0 {
		return []AgentOut{}, nil
	}
	const chunk = 500
	byID := make(map[string]*AgentOut, len(ids))
	for i := 0; i < len(ids); i += chunk {
		part := ids[i:min(i+chunk, len(ids))]
		ph := "?" + strings.Repeat(",?", len(part)-1)
		args := make([]any, len(part))
		for j, id := range part {
			args[j] = id
		}
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT u.id,u.name,u.role,u.is_deactivated,COALESCE(k.prefix,''),u.created_at,k.expires_at,u.created_by,c.name
			FROM users u LEFT JOIN users c ON c.id=u.created_by
			LEFT JOIN api_keys k ON k.id=(SELECT id FROM api_keys WHERE user_id=u.id ORDER BY created_at DESC,id DESC LIMIT 1)
			WHERE u.id IN (`+ph+`) AND u.is_agent=1 AND u.deleted_at IS NULL`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a AgentOut
			var cb, cbName *string
			if err := rows.Scan(&a.ID, &a.Name, &a.Role, &a.IsDeactivated, &a.KeyPrefix, &a.CreatedAt, &a.APIKeyExpiresAt, &cb, &cbName); err != nil {
				rows.Close()
				return nil, err
			}
			if cb != nil {
				a.CreatedBy = *cb
			}
			if cbName != nil {
				a.CreatedByName = *cbName
			}
			a.IsAgent, a.Boards = true, []AgentBoard{}
			a.APIKeyExpired = keyExpired(a.APIKeyExpiresAt)
			byID[a.ID] = &a
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	if len(byID) == 0 {
		return []AgentOut{}, nil
	}
	allBoards := make(map[string][]AgentBoard, len(byID))
	seenBoard := map[string]bool{}
	var distinctBoards []string
	for i := 0; i < len(ids); i += chunk {
		part := ids[i:min(i+chunk, len(ids))]
		ph := "?" + strings.Repeat(",?", len(part)-1)
		args := make([]any, len(part))
		for j, id := range part {
			args[j] = id
		}
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT m.user_id,m.board_id,b.name,m.id,m.role,m.can_comment
			FROM board_memberships m JOIN boards b ON b.id=m.board_id
			WHERE m.user_id IN (`+ph+`) ORDER BY b.name`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var uid string
			var x AgentBoard
			var cc *bool
			if err := rows.Scan(&uid, &x.BoardID, &x.BoardName, &x.MemberID, &x.Role, &cc); err != nil {
				rows.Close()
				return nil, err
			}
			if x.Role == "viewer" && cc != nil && *cc {
				x.Role = "commenter"
			}
			allBoards[uid] = append(allBoards[uid], x)
			if !seenBoard[x.BoardID] {
				seenBoard[x.BoardID] = true
				distinctBoards = append(distinctBoards, x.BoardID)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	lv := authz.BoardLevels(e, distinctBoards)
	out := make([]AgentOut, 0, len(byID))
	for _, id := range ids {
		a := byID[id]
		if a == nil {
			continue
		}
		bs := allBoards[id]
		a.BoardCount = len(bs)
		bIDs := make([]string, len(bs))
		for i, x := range bs {
			bIDs[i] = x.BoardID
			if lv[x.BoardID] >= authz.Manage {
				a.Boards = append(a.Boards, x)
			}
		}
		a.CanManage = authz.CanManageAgentFrom(e, a.CreatedBy, bIDs, lv)
		out = append(out, *a)
	}
	return out, nil
}

// grantBoard upserts the agent's membership through the boards ops (authz applies: Manage on the board).
func grantBoard(e *ops.Env, agentID, boardID, role string) error {
	if role == "" {
		role = "editor"
	}
	if !boardRoleOK(role) {
		return ops.Invalid("role must be editor, viewer or commenter")
	}
	dbRole, cc := role, any(nil)
	if role == "commenter" {
		dbRole, cc = "viewer", true
	} else if role == "viewer" {
		cc = false
	}
	var mid string
	err := e.DB.QueryRowContext(e.Ctx, `SELECT id FROM board_memberships WHERE board_id=? AND user_id=?`, boardID, agentID).Scan(&mid)
	if err == sql.ErrNoRows {
		_, err = ops.CallMap(e, "board.member.add", map[string]any{"boardId": boardID, "userId": agentID, "role": dbRole, "canComment": cc})
		return err
	}
	if err != nil {
		return err
	}
	_, err = ops.CallMap(e, "board.member.update", map[string]any{"memberId": mid, "role": dbRole, "canComment": cc})
	return err
}

// revokeBoard removes the agent's membership on a board. A manager of the board removes it as themself; the agent's
// creator who does not manage that board removes it the way a member leaves a board (the boards op allows a member to
// remove their own membership), so the boards domain still performs the write. Nobody else may (an administrator who
// does not manage the board deactivates or deletes the account instead). Authority is checked before the membership
// lookup, so the answer never tells an outsider whether the agent is on a board.
func revokeBoard(e *ops.Env, a AgentOut, boardID string) error {
	manager := authz.BoardLevel(e, boardID) >= authz.Manage
	if !manager && (a.CreatedBy == "" || a.CreatedBy != e.User.UserID || !authz.CanManageAgent(e, a.ID)) {
		return ops.NotFound("board")
	}
	var mid string
	if e.DB.QueryRowContext(e.Ctx, `SELECT id FROM board_memberships WHERE board_id=? AND user_id=?`, boardID, a.ID).Scan(&mid) != nil {
		return nil
	}
	if manager {
		_, err := ops.CallMap(e, "board.member.remove", map[string]any{"memberId": mid})
		return err
	}
	self := *e
	self.User = &ops.Principal{UserID: a.ID, Role: a.Role, Name: a.Name, IsAgent: true}
	_, err := ops.CallMap(&self, "board.member.remove", map[string]any{"memberId": mid})
	return err
}

func dedupe(l []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range l {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func noAgents(e *ops.Env) error {
	if e.User == nil || e.User.IsAgent {
		return ops.Forbidden()
	}
	return nil
}

// agentIDsFor lists the agents the caller may see (see authz.CanSeeAgent), optionally only those on one board.
func agentIDsFor(e *ops.Env, boardID string) ([]string, error) {
	var q string
	var args []any
	switch {
	case boardID != "":
		q, args = `SELECT u.id FROM users u WHERE u.is_agent=1 AND u.deleted_at IS NULL AND EXISTS(SELECT 1 FROM board_memberships m WHERE m.user_id=u.id AND m.board_id=?)
			ORDER BY u.name COLLATE NOCASE, u.id`, []any{boardID}
	case isAdmin(e):
		q = `SELECT id FROM users WHERE is_agent=1 AND deleted_at IS NULL ORDER BY name COLLATE NOCASE, id`
	default:
		// boards that carry an agent membership, filtered to the ones the caller manages (one BoardLevels call)
		var boards []string
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT DISTINCT m.board_id FROM board_memberships m JOIN users u ON u.id=m.user_id WHERE u.is_agent=1 AND u.deleted_at IS NULL`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var b string
			if rows.Scan(&b) == nil {
				boards = append(boards, b)
			}
		}
		rows.Close()
		managed := []any{e.User.UserID}
		ph := ""
		for b, l := range authz.BoardLevels(e, boards) {
			if l >= authz.Manage {
				managed = append(managed, b)
				ph += ",?"
			}
		}
		q = `SELECT u.id FROM users u WHERE u.is_agent=1 AND u.deleted_at IS NULL AND (u.created_by=?`
		if ph != "" {
			q += ` OR EXISTS(SELECT 1 FROM board_memberships m WHERE m.user_id=u.id AND m.board_id IN (` + ph[1:] + `))`
		}
		q += `) ORDER BY u.name COLLATE NOCASE, u.id`
		args = managed
	}
	rows, err := e.DB.QueryContext(e.Ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func init() {
	ops.Register(ops.Op{Name: "agent.create", Summary: "Create an AI agent account (no login) for boards you manage and return its API key once", Method: "POST", Path: "/agents",
		UI: "POST /ui/boards/{boardId}/agents"}, func(e *ops.Env, in AgentCreateIn) (AgentOut, error) {
		if err := noAgents(e); err != nil {
			return AgentOut{}, err
		}
		admin := isAdmin(e)
		name := strings.TrimSpace(in.Name)
		if name == "" || len([]rune(name)) > 100 {
			return AgentOut{}, ops.Invalid("name is required (max 100 characters)")
		}
		role := in.Role
		if role == "" {
			role = "boardUser"
		}
		if !admin && role != "boardUser" {
			return AgentOut{}, ops.Invalid("only admins choose an agent's role; agents created for a board are boardUser")
		}
		if !agentRoleOK(role) {
			return AgentOut{}, ops.Invalid("role must be worker, boardUser or guest")
		}
		if !boardRoleOK(in.BoardRole) {
			return AgentOut{}, ops.Invalid("boardRole must be editor, viewer or commenter")
		}
		boards := dedupe(in.BoardIDs)
		if !admin && len(boards) == 0 {
			return AgentOut{}, ops.Invalid("boardIds is required: an agent is created for boards you manage")
		}
		for _, b := range boards {
			if err := authz.RequireBoard(e, b, authz.Manage); err != nil {
				return AgentOut{}, err
			}
		}
		if _, err := keyExpiry(in.ExpiresInDays); err != nil {
			return AgentOut{}, err
		}
		id := db.NewID()
		var key APIKeyOut
		err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if limit := agentsPerUser(); !admin && limit > 0 {
				var n int
				if err := tx.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM users WHERE is_agent=1 AND deleted_at IS NULL AND created_by=?`, e.User.UserID).Scan(&n); err != nil {
					return err
				}
				if n >= limit {
					return ops.Invalid(fmt.Sprintf("you already have %d agents, the most one person may create (WEAVE_AGENTS_PER_USER); delete one first", n))
				}
			}
			if _, err := tx.ExecContext(e.Ctx, `INSERT INTO users(id,email,name,role,is_sso,is_email_verified,is_agent,created_by,created_at) VALUES(?,?,?,?,0,1,1,?,?)`,
				id, "agent-"+id+"@agents.invalid", name, role, e.User.UserID, db.Now()); err != nil {
				return err
			}
			var err error
			key, err = replaceAPIKey(e.Ctx, tx, id, in.ExpiresInDays)
			return err
		})
		if err != nil {
			return AgentOut{}, err
		}
		for _, b := range boards {
			if err := grantBoard(e, id, b, in.BoardRole); err != nil {
				_, _ = e.DB.ExecContext(e.Ctx, `DELETE FROM users WHERE id=? AND is_agent=1`, id)
				return AgentOut{}, err
			}
		}
		a, err := loadAgent(e, id)
		a.Key = &key
		publish(e, "user.created", id)
		return a, err
	})

	ops.Register(ops.Op{Name: "agent.list", Summary: "List AI agents you created or that have access to boards you manage (admins: every agent account), with their access on boards you manage", Method: "GET", Path: "/agents",
		UI: "GET /ui/boards/{boardId}/agents"}, func(e *ops.Env, in AgentListIn) ([]AgentOut, error) {
		if err := noAgents(e); err != nil {
			return nil, err
		}
		if in.BoardID != "" {
			if err := authz.RequireBoard(e, in.BoardID, authz.Manage); err != nil {
				return nil, err
			}
		}
		ids, err := agentIDsFor(e, in.BoardID)
		if err != nil {
			return nil, err
		}
		return loadAgents(e, ids)
	})

	ops.Register(ops.Op{Name: "agent.update", Summary: "Rename an agent or deactivate it (its creator, a manager of all its boards, or an admin; only admins change the role)", Method: "PATCH", Path: "/agents/{id}",
		UI: "POST /ui/agents/{id}/update"}, func(e *ops.Env, in AgentUpdateIn) (AgentOut, error) {
		if err := authz.RequireAgentManage(e, in.ID); err != nil {
			return AgentOut{}, err
		}
		if in.Role != nil && !isAdmin(e) {
			return AgentOut{}, ops.Forbidden()
		}
		var sets []string
		var args []any
		if in.Name != nil {
			n := strings.TrimSpace(*in.Name)
			if n == "" || len([]rune(n)) > 100 {
				return AgentOut{}, ops.Invalid("name is required (max 100 characters)")
			}
			sets = append(sets, "name=?")
			args = append(args, n)
		}
		if in.Role != nil {
			if !agentRoleOK(*in.Role) {
				return AgentOut{}, ops.Invalid("role must be worker, boardUser or guest")
			}
			sets = append(sets, "role=?")
			args = append(args, *in.Role)
		}
		if in.IsDeactivated != nil {
			sets = append(sets, "is_deactivated=?")
			args = append(args, *in.IsDeactivated)
		}
		if len(sets) > 0 {
			sets = append(sets, "updated_at=?")
			args = append(args, db.Now(), in.ID)
			res, err := e.DB.ExecContext(e.Ctx, `UPDATE users SET `+strings.Join(sets, ",")+` WHERE id=? AND is_agent=1 AND deleted_at IS NULL`, args...)
			if err != nil {
				return AgentOut{}, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return AgentOut{}, ops.NotFound("agent")
			}
		}
		publish(e, "user.updated", in.ID)
		return loadAgent(e, in.ID)
	})

	ops.Register(ops.Op{Name: "agent.rotateKey", Summary: "Issue a new API key for an agent (an agent holds exactly one key; the old one stops working); returned once", Method: "POST", Path: "/agents/{id}/key",
		UI: "POST /ui/agents/{id}/key"}, func(e *ops.Env, in AgentRotateIn) (AgentOut, error) {
		if err := authz.RequireAgentManage(e, in.ID); err != nil {
			return AgentOut{}, err
		}
		ag, err := loadAgent(e, in.ID)
		if err != nil {
			return AgentOut{}, err
		}
		// the new key and its audit row commit together (the row keeps the key's display prefix only, never the key)
		var key APIKeyOut
		au := AuditEntry{Action: AuditAgentRotateKey, TargetKind: "agent", TargetID: ag.ID, TargetName: ag.Name, Source: auditSource(e)}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			var err error
			if key, err = replaceAPIKey(e.Ctx, tx, in.ID, in.ExpiresInDays); err != nil {
				return err
			}
			au.Details = map[string]any{"keyPrefix": key.Prefix}
			if key.ExpiresAt != nil {
				au.Details["expiresAt"] = *key.ExpiresAt
			}
			auditActor(e.Ctx, tx, e.User, &au)
			return writeAudit(e.Ctx, tx, &au)
		})
		if err != nil {
			return AgentOut{}, err
		}
		logAudit(au)
		a, err := loadAgent(e, in.ID)
		a.Key = &key
		return a, err
	})

	ops.Register(ops.Op{Name: "agent.delete", Summary: "Delete an agent account; its claims are released", Method: "DELETE", Path: "/agents/{id}",
		UI: "POST /ui/agents/{id}/delete"}, func(e *ops.Env, in AgentIDIn) (AgentOut, error) {
		if err := authz.RequireAgentManage(e, in.ID); err != nil {
			return AgentOut{}, err
		}
		a, err := loadAgent(e, in.ID)
		if err != nil {
			return a, err
		}
		type held struct{ card, board string }
		var cards []held
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			cards = nil
			rows, err := tx.QueryContext(e.Ctx, `SELECT id,board_id FROM cards WHERE claimed_by=?`, in.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var h held
				if err := rows.Scan(&h.card, &h.board); err != nil {
					rows.Close()
					return err
				}
				cards = append(cards, h)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			if len(cards) > 0 {
				if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET claimed_by=NULL,claimed_until=NULL,claim_note=NULL WHERE claimed_by=?`, in.ID); err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(e.Ctx, `DELETE FROM users WHERE id=? AND is_agent=1`, in.ID)
			return err
		})
		if err != nil {
			return a, err
		}
		for _, h := range cards {
			ops.Emit(e, ops.Event{Type: "cardRelease", Name: "card.released", BoardID: h.board, CardID: h.card,
				Data: map[string]any{"reason": "agent_deleted", "previousClaimedBy": in.ID}})
		}
		publish(e, "user.deleted", in.ID)
		return a, nil
	})

	ops.Register(ops.Op{Name: "agent.access", Summary: "Grant (editor|viewer|commenter) or revoke an agent's access to a board you manage", Method: "POST", Path: "/agents/{id}/access",
		UI: "POST /ui/agents/{id}/access"}, func(e *ops.Env, in AgentAccessIn) (AgentOut, error) {
		if err := noAgents(e); err != nil {
			return AgentOut{}, err
		}
		if !authz.CanSeeAgent(e, in.ID) && authz.BoardLevel(e, in.BoardID) < authz.Manage {
			return AgentOut{}, ops.NotFound("agent")
		}
		a, err := loadAgent(e, in.ID)
		if err != nil {
			return AgentOut{}, err
		}
		if in.Revoke {
			if err := revokeBoard(e, a, in.BoardID); err != nil {
				return AgentOut{}, err
			}
		} else {
			if !boardRoleOK(in.Role) {
				return AgentOut{}, ops.Invalid("role must be editor, viewer or commenter")
			}
			if err := authz.RequireBoard(e, in.BoardID, authz.Manage); err != nil {
				return AgentOut{}, err
			}
			if err := grantBoard(e, in.ID, in.BoardID, in.Role); err != nil {
				return AgentOut{}, err
			}
		}
		return loadAgent(e, in.ID)
	})
}
