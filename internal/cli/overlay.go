package cli

// The contract overlay: agent-oriented ops (claim/lease, next, trees, listeners, agent accounts,
// whoami) and query filters the CLI shortcuts depend on. They are merged into the catalog ONLY
// when the server schema / snapshot lacks them, so a server that already serves them wins and
// this table becomes dead weight that can be deleted once the snapshot contains everything.

func p(name string) Field { return Field{Name: name, In: "path", Type: "string", Required: true} }
func q(name, typ, desc string) Field {
	return Field{Name: name, In: "query", Type: typ, Desc: desc}
}
func b(name, typ, desc string) Field { return Field{Name: name, In: "body", Type: typ, Desc: desc} }
func br(name, typ, desc string) Field {
	return Field{Name: name, In: "body", Type: typ, Required: true, Desc: desc}
}

var overlayOps = []Op{
	{Name: "whoami", Tag: "whoami", Summary: "Who am I: user, role, isAgent and accessible boards", Method: "GET", Path: "/whoami", Auth: "user"},
	{Name: "card.claim", Tag: "card", Summary: "Claim a card with a time-limited lease (409 conflict if already claimed)", Method: "POST", Path: "/cards/{id}/claim", Auth: "user",
		Fields: []Field{p("id"), b("listId", "string", "only claim if the card is still in this list"), b("ttlSeconds", "number", "lease length in seconds"), b("note", "string", "why/what you are doing")}},
	{Name: "card.release", Tag: "card", Summary: "Release your claim on a card", Method: "POST", Path: "/cards/{id}/release", Auth: "user",
		Fields: []Field{p("id"), b("note", "string", "optional note")}},
	{Name: "card.renew", Tag: "card", Summary: "Extend your lease on a claimed card", Method: "POST", Path: "/cards/{id}/renew", Auth: "user",
		Fields: []Field{p("id"), b("ttlSeconds", "number", "new lease length in seconds")}},
	{Name: "card.next", Tag: "card", Summary: "Pull the next unclaimed card of a list, optionally claiming and moving it", Method: "POST", Path: "/lists/{listId}/next", Auth: "user",
		Fields: []Field{p("listId"), b("claim", "bool", "claim the card atomically"), b("ttlSeconds", "number", "lease length"), b("moveTo", "string", "list id to move the card to after claiming")}},
	{Name: "board.tree", Tag: "board", Summary: "Board with its lists and cards in one call", Method: "GET", Path: "/boards/{id}/tree", Auth: "user", Fields: []Field{p("id")}},
	{Name: "board.listeners", Tag: "board", Summary: "Agents/clients currently listening on a board", Method: "GET", Path: "/boards/{id}/listeners", Auth: "user", Fields: []Field{p("id")}},
	{Name: "list.listeners", Tag: "list", Summary: "Agents/clients currently listening on a list", Method: "GET", Path: "/lists/{id}/listeners", Auth: "user", Fields: []Field{p("id")}},
	{Name: "agent.create", Tag: "agent", Summary: "Create an agent account (admin); returns its API key once", Method: "POST", Path: "/agents", Auth: "admin",
		Fields: []Field{br("name", "string", "display name"), b("description", "string", "what this agent does")}},
	{Name: "agent.list", Tag: "agent", Summary: "List agent accounts (admin)", Method: "GET", Path: "/agents", Auth: "admin"},
	{Name: "agent.update", Tag: "agent", Summary: "Rename or describe an agent (admin)", Method: "PATCH", Path: "/agents/{id}", Auth: "admin",
		Fields: []Field{p("id"), b("name", "string", ""), b("description", "string", "")}},
	{Name: "agent.rotateKey", Tag: "agent", Summary: "Issue a new API key for an agent (admin); the old one stops working", Method: "POST", Path: "/agents/{id}/rotate-key", Auth: "admin", Fields: []Field{p("id")}},
	{Name: "agent.delete", Tag: "agent", Summary: "Delete an agent account (admin)", Method: "DELETE", Path: "/agents/{id}", Auth: "admin", Fields: []Field{p("id")}},
	{Name: "agent.access", Tag: "agent", Summary: "Grant or revoke an agent's access to a board (admin)", Method: "POST", Path: "/agents/{id}/access", Auth: "admin",
		Fields: []Field{p("id"), br("boardId", "string", "board"), b("role", "string", "editor | viewer; empty with revoke=true removes access"), b("revoke", "bool", "remove access")}},
}

// overlayFields adds contract filters to ops that exist in the older snapshot.
var overlayFields = map[string][]Field{
	"card.get":  {q("full", "bool", "include comments, tasks, attachments and activity")},
	"card.list": {q("labelId", "string", "only cards with this label"), q("memberId", "string", "only cards assigned to this user"), q("claimed", "string", "true|false|me: filter by claim state"), q("q", "string", "text search"), q("limit", "number", "page size"), q("cursor", "string", "pagination cursor")},
}

func applyOverlay(c *Catalog) {
	for i := range overlayOps {
		o := overlayOps[i]
		if _, ok := c.byName[o.Name]; !ok {
			c.byName[o.Name] = &o
		}
	}
	for name, fields := range overlayFields {
		o := c.byName[name]
		if o == nil {
			continue
		}
		have := map[string]bool{}
		for _, f := range o.Fields {
			have[f.Name] = true
		}
		var add []Field
		for _, f := range fields {
			if !have[f.Name] {
				add = append(add, f)
			}
		}
		if len(add) > 0 {
			cp := *o
			cp.Fields = append(append([]Field{}, o.Fields...), add...)
			c.byName[name] = &cp
		}
	}
}
