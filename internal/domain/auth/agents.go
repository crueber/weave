package auth

import (
	"database/sql"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// Agent accounts: non-SSO service users for AI agents. They have no login (the OIDC
// callback and session lookup skip is_agent users) and authenticate with an API key
// only. Board access is a normal board membership, so every authz rule applies.

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
	KeyPrefix       string       `json:"keyPrefix,omitempty"`
	APIKeyExpiresAt *string      `json:"apikeyExpiresAt,omitempty" desc:"when the key stops working; absent = never"`
	APIKeyExpired   bool         `json:"apikeyExpired,omitempty" desc:"true when the key has expired and is rejected"`
	CreatedAt       string       `json:"createdAt"`
	Boards          []AgentBoard `json:"boards"`
	Key             *APIKeyOut   `json:"key,omitempty" desc:"the API key, returned once on create and rotateKey"`
}

type AgentCreateIn struct {
	Name          string   `json:"name" required:"true" desc:"display name shown on cards and in activity"`
	BoardIDs      []string `json:"boardIds" desc:"boards to grant access to"`
	Role          string   `json:"role" desc:"user role: worker (default) | boardUser | guest"`
	BoardRole     string   `json:"boardRole" desc:"access on boardIds: editor (default) | viewer | commenter"`
	ExpiresInDays int      `json:"expiresInDays" desc:"optional: the API key stops working after this many days (1-3650); 0 or omitted = never"`
}

type AgentUpdateIn struct {
	ID            string  `path:"id"`
	Name          *string `json:"name"`
	Role          *string `json:"role" desc:"worker | boardUser | guest"`
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
	BoardID string `json:"boardId" required:"true"`
	Role    string `json:"role" desc:"editor (default) | viewer | commenter"`
	Revoke  bool   `json:"revoke" desc:"true removes the agent from the board"`
}

func agentRoleOK(r string) bool { return r == "worker" || r == "boardUser" || r == "guest" }

func loadAgent(e *ops.Env, id string) (AgentOut, error) {
	var a AgentOut
	err := e.DB.QueryRowContext(e.Ctx, `SELECT id,name,role,is_deactivated,COALESCE(api_key_prefix,''),created_at,api_key_expires_at FROM users WHERE id=? AND is_agent=1 AND deleted_at IS NULL`, id).
		Scan(&a.ID, &a.Name, &a.Role, &a.IsDeactivated, &a.KeyPrefix, &a.CreatedAt, &a.APIKeyExpiresAt)
	if err == sql.ErrNoRows {
		return a, ops.NotFound("agent")
	}
	if err != nil {
		return a, err
	}
	a.IsAgent, a.Boards = true, []AgentBoard{}
	a.APIKeyExpired = keyExpired(a.APIKeyExpiresAt)
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT m.board_id,b.name,m.id,m.role,m.can_comment FROM board_memberships m JOIN boards b ON b.id=m.board_id WHERE m.user_id=? ORDER BY b.name`, id)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	for rows.Next() {
		var x AgentBoard
		var cc *bool
		if err := rows.Scan(&x.BoardID, &x.BoardName, &x.MemberID, &x.Role, &cc); err != nil {
			return a, err
		}
		if x.Role == "viewer" && cc != nil && *cc {
			x.Role = "commenter"
		}
		a.Boards = append(a.Boards, x)
	}
	return a, rows.Err()
}

// grantBoard upserts the agent's membership through the boards ops (authz applies).
func grantBoard(e *ops.Env, agentID, boardID, role string) error {
	if role == "" {
		role = "editor"
	}
	if role != "editor" && role != "viewer" && role != "commenter" {
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

func init() {
	ops.Register(ops.Op{Name: "agent.create", Summary: "Create an AI agent account (no login) and return its API key once", Method: "POST", Path: "/agents", Auth: ops.AuthAdmin,
		UI: "POST /ui/admin/agents"}, func(e *ops.Env, in AgentCreateIn) (AgentOut, error) {
		name := strings.TrimSpace(in.Name)
		if name == "" || len([]rune(name)) > 100 {
			return AgentOut{}, ops.Invalid("name is required (max 100 characters)")
		}
		role := in.Role
		if role == "" {
			role = "worker"
		}
		if !agentRoleOK(role) {
			return AgentOut{}, ops.Invalid("role must be worker, boardUser or guest")
		}
		for _, b := range in.BoardIDs {
			if authz.BoardLevel(e, b) == authz.None {
				return AgentOut{}, ops.NotFound("board " + b)
			}
		}
		if _, err := keyExpiry(in.ExpiresInDays); err != nil {
			return AgentOut{}, err
		}
		id := db.NewID()
		if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO users(id,email,name,role,is_sso,is_email_verified,is_agent,created_at) VALUES(?,?,?,?,0,1,1,?)`,
			id, "agent-"+id+"@agents.invalid", name, role, db.Now()); err != nil {
			return AgentOut{}, err
		}
		key, err := setAPIKey(e.Ctx, e.DB, id, in.ExpiresInDays)
		if err != nil {
			return AgentOut{}, err
		}
		for _, b := range in.BoardIDs {
			if err := grantBoard(e, id, b, in.BoardRole); err != nil {
				return AgentOut{}, err
			}
		}
		a, err := loadAgent(e, id)
		a.Key = &key
		publish(e, "user.created", id)
		return a, err
	})

	ops.Register(ops.Op{Name: "agent.list", Summary: "List AI agent accounts with their board access", Method: "GET", Path: "/agents", Auth: ops.AuthAdmin,
		UI: "GET /admin/agents"}, func(e *ops.Env, _ NoInput) ([]AgentOut, error) {
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT id FROM users WHERE is_agent=1 AND deleted_at IS NULL ORDER BY name COLLATE NOCASE, id`)
		if err != nil {
			return nil, err
		}
		var ids []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		out := []AgentOut{}
		for _, id := range ids {
			a, err := loadAgent(e, id)
			if err != nil {
				return nil, err
			}
			out = append(out, a)
		}
		return out, nil
	})

	ops.Register(ops.Op{Name: "agent.update", Summary: "Rename an agent, change its role or deactivate it", Method: "PATCH", Path: "/agents/{id}", Auth: ops.AuthAdmin,
		UI: "POST /ui/admin/agents/{id}/update"}, func(e *ops.Env, in AgentUpdateIn) (AgentOut, error) {
		if _, err := loadAgent(e, in.ID); err != nil {
			return AgentOut{}, err
		}
		if in.Name != nil {
			n := strings.TrimSpace(*in.Name)
			if n == "" || len([]rune(n)) > 100 {
				return AgentOut{}, ops.Invalid("name is required (max 100 characters)")
			}
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE users SET name=?,updated_at=? WHERE id=?`, n, db.Now(), in.ID); err != nil {
				return AgentOut{}, err
			}
		}
		if in.Role != nil {
			if !agentRoleOK(*in.Role) {
				return AgentOut{}, ops.Invalid("role must be worker, boardUser or guest")
			}
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE users SET role=?,updated_at=? WHERE id=?`, *in.Role, db.Now(), in.ID); err != nil {
				return AgentOut{}, err
			}
		}
		if in.IsDeactivated != nil {
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE users SET is_deactivated=?,updated_at=? WHERE id=?`, *in.IsDeactivated, db.Now(), in.ID); err != nil {
				return AgentOut{}, err
			}
		}
		publish(e, "user.updated", in.ID)
		return loadAgent(e, in.ID)
	})

	ops.Register(ops.Op{Name: "agent.rotateKey", Summary: "Issue a new API key for an agent (the old key stops working); returned once", Method: "POST", Path: "/agents/{id}/key", Auth: ops.AuthAdmin,
		UI: "POST /ui/admin/agents/{id}/key"}, func(e *ops.Env, in AgentRotateIn) (AgentOut, error) {
		if _, err := loadAgent(e, in.ID); err != nil {
			return AgentOut{}, err
		}
		key, err := setAPIKey(e.Ctx, e.DB, in.ID, in.ExpiresInDays)
		if err != nil {
			return AgentOut{}, err
		}
		a, err := loadAgent(e, in.ID)
		a.Key = &key
		return a, err
	})

	ops.Register(ops.Op{Name: "agent.delete", Summary: "Delete an agent account; its claims are released", Method: "DELETE", Path: "/agents/{id}", Auth: ops.AuthAdmin,
		UI: "POST /ui/admin/agents/{id}/delete"}, func(e *ops.Env, in AgentIDIn) (AgentOut, error) {
		a, err := loadAgent(e, in.ID)
		if err != nil {
			return a, err
		}
		type held struct{ card, board string }
		var cards []held
		if rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,board_id FROM cards WHERE claimed_by=?`, in.ID); err == nil {
			for rows.Next() {
				var h held
				_ = rows.Scan(&h.card, &h.board)
				cards = append(cards, h)
			}
			rows.Close()
		}
		if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM users WHERE id=? AND is_agent=1`, in.ID); err != nil {
			return a, err
		}
		for _, h := range cards {
			ops.Emit(e, ops.Event{Type: "cardRelease", Name: "card.released", BoardID: h.board, CardID: h.card,
				Data: map[string]any{"reason": "agent_deleted", "previousClaimedBy": in.ID}})
		}
		publish(e, "user.deleted", in.ID)
		return a, nil
	})

	ops.Register(ops.Op{Name: "agent.access", Summary: "Grant (editor|viewer|commenter) or revoke an agent's access to a board", Method: "POST", Path: "/agents/{id}/access", Auth: ops.AuthAdmin,
		UI: "POST /ui/admin/agents/{id}/access"}, func(e *ops.Env, in AgentAccessIn) (AgentOut, error) {
		if _, err := loadAgent(e, in.ID); err != nil {
			return AgentOut{}, err
		}
		if in.Revoke {
			var mid string
			if e.DB.QueryRowContext(e.Ctx, `SELECT id FROM board_memberships WHERE board_id=? AND user_id=?`, in.BoardID, in.ID).Scan(&mid) == nil {
				if _, err := ops.CallMap(e, "board.member.remove", map[string]any{"memberId": mid}); err != nil {
					return AgentOut{}, err
				}
			}
		} else if err := grantBoard(e, in.ID, in.BoardID, in.Role); err != nil {
			return AgentOut{}, err
		}
		return loadAgent(e, in.ID)
	})
}
