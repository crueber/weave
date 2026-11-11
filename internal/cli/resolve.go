package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Candidate is one thing a name can resolve to.
type Candidate struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Parent string   `json:"parent,omitempty"` // e.g. project name for a board
	Alt    []string `json:"-"`                // extra matchable names (username, email)
}

func (c Candidate) label() string {
	if c.Parent != "" {
		return c.Parent + "/" + c.Name
	}
	return c.Name
}

// Scope carries the context needed to resolve names: boards need a project hint to disambiguate,
// lists/labels/cards need a board.
type Scope struct {
	ProjectID string
	BoardID   string
	CardID    string // when set and BoardID is empty, the board is derived from this card
}

// Identity is the caller as reported by whoami.
type Identity struct {
	ID      string
	Name    string
	Role    string
	IsAgent bool
	Boards  []Candidate
	Raw     map[string]any
}

// Whoami asks the server who we are (cached per run). Falls back to auth.me for servers
// that do not have the whoami op yet.
func (a *App) Whoami(ctx context.Context) (*Identity, error) {
	a.selfMu.Lock()
	defer a.selfMu.Unlock()
	if a.self != nil {
		return a.self, nil
	}
	var item map[string]any
	var err error
	for _, name := range []string{"auth.whoami", "whoami", "auth.me"} {
		if o, e := a.mustOp(name); e == nil {
			item, err = a.itemFrom(ctx, o)
			if err == nil || name == "auth.me" {
				break
			}
			if ae, ok := err.(*APIError); ok && ae.Code != "not_found" {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	id := &Identity{Raw: item}
	u := item
	if um, ok := item["user"].(map[string]any); ok {
		u = um
	}
	id.ID = firstStr(u, "id", "userId")
	id.Name = firstStr(u, "name", "username", "email")
	id.Role = firstStr(item, "role")
	if id.Role == "" {
		id.Role = firstStr(u, "role")
	}
	if v, ok := item["isAgent"].(bool); ok {
		id.IsAgent = v
	} else if v, ok := u["isAgent"].(bool); ok {
		id.IsAgent = v
	}
	for _, key := range []string{"boards", "accessibleBoards"} {
		if arr, ok := item[key].([]any); ok {
			for _, x := range arr {
				if m, ok := x.(map[string]any); ok && str(m, "id") != "" {
					id.Boards = append(id.Boards, Candidate{ID: str(m, "id"), Name: str(m, "name"), Parent: firstStr(m, "projectName", "project")})
				}
			}
			break
		}
	}
	a.self = id
	return id, nil
}

func (a *App) itemFrom(ctx context.Context, o *Op) (map[string]any, error) {
	r, err := a.Call(ctx, o, Params{}, nil)
	if err != nil {
		return nil, err
	}
	return itemOf(r.JSON()), nil
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := str(m, k); s != "" {
			return s
		}
	}
	return ""
}

func (a *App) fetchCandidates(ctx context.Context, kind string, sc Scope, raw string) ([]Candidate, error) {
	key := kind + "|" + sc.ProjectID + "|" + sc.BoardID
	if kind == "user" {
		key += "|" + raw
	}
	a.candMu.Lock()
	c, ok := a.candCache[key]
	a.candMu.Unlock()
	if ok {
		return c, nil
	}
	var out []Candidate
	add := func(items []any, parent string) {
		for _, x := range items {
			m, ok := x.(map[string]any)
			if !ok || str(m, "id") == "" {
				continue
			}
			c := Candidate{ID: str(m, "id"), Name: firstStr(m, "name", "title"), Parent: parent}
			if kind == "user" {
				c.Name = firstStr(m, "name", "username", "email")
				for _, k := range []string{"username", "email", "name"} {
					if s := str(m, k); s != "" {
						c.Alt = append(c.Alt, s)
					}
				}
			}
			out = append(out, c)
		}
	}
	switch kind {
	case "project":
		it, err := a.Items(ctx, "project.list", nil)
		if err != nil {
			return nil, err
		}
		add(it, "")
	case "board":
		if sc.ProjectID != "" {
			it, err := a.Items(ctx, "board.list", Params{"projectId": sc.ProjectID})
			if err != nil {
				return nil, err
			}
			add(it, "")
			break
		}
		if me, err := a.Whoami(ctx); err == nil && len(me.Boards) > 0 {
			out = append(out, me.Boards...)
			break
		}
		projects, err := a.Items(ctx, "project.list", nil)
		if err != nil {
			return nil, err
		}
		for _, p := range projects {
			pm, _ := p.(map[string]any)
			it, err := a.Items(ctx, "board.list", Params{"projectId": str(pm, "id")})
			if err != nil {
				continue
			}
			add(it, str(pm, "name"))
		}
	case "list", "label", "card":
		if sc.BoardID == "" {
			return nil, usageErr(fmt.Sprintf("to resolve a %s by name, say which board: --board NAME (or use its id)", kind))
		}
		op := map[string]string{"list": "list.list", "label": "label.list", "card": "card.list"}[kind]
		params := Params{"boardId": sc.BoardID}
		if kind == "card" {
			params["q"] = raw
		}
		it, err := a.Items(ctx, op, params)
		if err != nil {
			return nil, err
		}
		add(it, "")
	case "user":
		it, err := a.Items(ctx, "user.list", Params{"q": raw})
		if err != nil {
			return nil, err
		}
		add(it, "")
	default:
		return nil, usageErr("cannot resolve " + kind)
	}
	a.candMu.Lock()
	if a.candCache == nil {
		a.candCache = map[string][]Candidate{}
	}
	a.candCache[key] = out
	a.candMu.Unlock()
	return out, nil
}

// Resolve turns a name, id or unambiguous prefix into an id.
func (a *App) Resolve(ctx context.Context, kind, raw string, sc Scope) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if kind == "user" && (raw == "me" || raw == "@me" || raw == "self") {
		me, err := a.Whoami(ctx)
		if err != nil {
			return "", err
		}
		return me.ID, nil
	}
	if looksLikeID(raw) {
		return raw, nil
	}
	if sc.BoardID == "" && sc.CardID != "" && (kind == "list" || kind == "label") {
		if card, err := a.Item(ctx, "card.get", Params{"id": sc.CardID}); err == nil {
			sc.BoardID = firstStr(card, "boardId")
		}
	}
	if kind == "board" && strings.Contains(raw, "/") && sc.ProjectID == "" {
		pn, bn, _ := strings.Cut(raw, "/")
		pid, err := a.Resolve(ctx, "project", pn, sc)
		if err != nil {
			return "", err
		}
		return a.Resolve(ctx, "board", bn, Scope{ProjectID: pid})
	}
	if kind == "card" && sc.BoardID == "" {
		return raw, nil // cards are addressed by id unless a board is given to search names in
	}
	cands, err := a.fetchCandidates(ctx, kind, sc, raw)
	if err != nil {
		return "", err
	}
	id, merr := MatchName(kind, raw, cands)
	if merr != nil {
		if kind == "card" && merr.Code == "not_found" {
			return raw, nil // let the server answer 404 for an unknown id
		}
		return "", merr
	}
	return id, nil
}

// MatchName picks the candidate for raw: exact id, exact name, unique name prefix, unique id prefix.
func MatchName(kind, raw string, cands []Candidate) (string, *APIError) {
	lc := strings.ToLower(raw)
	pick := func(pred func(c Candidate) bool) []Candidate {
		var m []Candidate
		for _, c := range cands {
			if pred(c) {
				m = append(m, c)
			}
		}
		return m
	}
	resolveOne := func(m []Candidate) (string, *APIError, bool) {
		switch len(m) {
		case 0:
			return "", nil, false
		case 1:
			return m[0].ID, nil, true
		}
		return "", ambiguous(kind, raw, m), true
	}
	steps := []func(c Candidate) bool{
		func(c Candidate) bool { return c.ID == raw },
		func(c Candidate) bool {
			if strings.EqualFold(c.Name, raw) || strings.EqualFold(c.label(), raw) {
				return true
			}
			for _, a := range c.Alt {
				if strings.EqualFold(a, raw) {
					return true
				}
			}
			return false
		},
		func(c Candidate) bool {
			if strings.HasPrefix(strings.ToLower(c.Name), lc) {
				return true
			}
			for _, a := range c.Alt {
				if strings.HasPrefix(strings.ToLower(a), lc) {
					return true
				}
			}
			return false
		},
		func(c Candidate) bool { return len(raw) >= 6 && strings.HasPrefix(strings.ToLower(c.ID), lc) },
	}
	for _, st := range steps {
		if id, err, ok := resolveOne(pick(st)); ok {
			return id, err
		}
	}
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.label())
	}
	sort.Strings(names)
	if len(names) > 20 {
		names = append(names[:20], "...")
	}
	msg := fmt.Sprintf("no %s matches %q", kind, raw)
	if len(names) > 0 {
		msg += "; available: " + strings.Join(names, ", ")
	}
	return "", &APIError{Code: "not_found", Message: msg}
}

func ambiguous(kind, raw string, m []Candidate) *APIError {
	parts := make([]string, len(m))
	for i, c := range m {
		parts[i] = fmt.Sprintf("%s (%s)", c.label(), c.ID)
	}
	return &APIError{Code: "ambiguous", Message: fmt.Sprintf("%q is ambiguous for %s: %s; use the id or a longer name", raw, kind, strings.Join(parts, ", ")), Detail: m}
}

// kindOfField maps an op field/tag to a resolvable kind ("" when not resolvable).
func kindOfField(fieldName, opTag string) string {
	n := strings.ToLower(fieldName)
	switch n {
	case "id":
		switch opTag {
		case "project", "board", "list", "label", "user", "card":
			return opTag
		}
		return ""
	case "projectid":
		return "project"
	case "boardid":
		return "board"
	case "listid":
		return "list"
	case "labelid":
		return "label"
	case "userid", "assigneeuserid", "memberid":
		return "user"
	case "cardid":
		return "card"
	}
	return ""
}
