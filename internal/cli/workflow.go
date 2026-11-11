package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() {
	registerSpec("next", nextFlags, cmdNext)
	registerSpec("inbox", []FlagSpec{{Name: "assigned", Bool: true}, {Name: "claimed", Bool: true}}, cmdInbox)
	registerSpec("board tree", nil, cmdBoardTree)
	registerSpec("list stats", []FlagSpec{{Name: "list"}}, cmdListStats)
	registerSpec("card show", []FlagSpec{{Name: "full", Bool: true}}, cmdCardShow)
	registerSpec("card move", []FlagSpec{{Name: "to-list", Aliases: []string{"list", "listId"}}, {Name: "position"}, {Name: "board"}}, cmdCardMove)
	registerSpec("card comment", []FlagSpec{{Name: "board"}}, cmdCardComment)
	registerSpec("card attach", []FlagSpec{{Name: "name"}, {Name: "board"}}, cmdCardAttach)
	registerSpec("card claim", []FlagSpec{{Name: "ttl"}, {Name: "note"}, {Name: "list"}, {Name: "board"}}, cmdCardClaim)
	registerSpec("card release", []FlagSpec{{Name: "note"}, {Name: "board"}}, cmdCardRelease)
	registerSpec("card renew", []FlagSpec{{Name: "ttl"}, {Name: "board"}}, cmdCardRenew)
	registerSpec("card done", []FlagSpec{{Name: "note"}, {Name: "to-list", Aliases: []string{"list"}}, {Name: "board"}, {Name: "keep-claim", Bool: true}}, cmdCardDone)
	registerSpec("card create", cardCreateFlags, cmdCardCreate)
	registerSpec("task add", []FlagSpec{{Name: "list"}, {Name: "assignee"}, {Name: "board"}}, cmdTaskAdd)
	registerSpec("task check", []FlagSpec{{Name: "card"}, {Name: "text"}, {Name: "n"}, {Name: "board"}}, func(a *App, args []string) int { return a.taskCheck(args, true) })
	registerSpec("task uncheck", []FlagSpec{{Name: "card"}, {Name: "text"}, {Name: "n"}, {Name: "board"}}, func(a *App, args []string) int { return a.taskCheck(args, false) })
	registerSpec("task list", []FlagSpec{{Name: "board"}}, cmdTaskList)
}

// ---- shared helpers ----

func (a *App) boardFromFlags(ctx context.Context, p *Parsed) (string, error) {
	raw := firstNonEmpty(p.Str("board"), os.Getenv("WEAVE_BOARD"))
	if raw == "" {
		return "", nil
	}
	return a.Resolve(ctx, "board", raw, Scope{})
}

// cardRef resolves a card id or (with a board) a card name.
func (a *App) cardRef(ctx context.Context, ref, boardID string) (string, error) {
	if ref == "" {
		return "", usageErr("missing card id")
	}
	return a.Resolve(ctx, "card", ref, Scope{BoardID: boardID})
}

// boardOfCard finds the board a card is on (card.get boardId).
func (a *App) boardOfCard(ctx context.Context, cardID string) string {
	if c, err := a.Item(ctx, "card.get", Params{"id": cardID}); err == nil {
		return firstStr(c, "boardId")
	}
	return ""
}

func (a *App) emitItem(item any) int {
	if err := a.emit(map[string]any{"item": item}, nil); err != nil {
		return a.fail(err)
	}
	return 0
}

func (a *App) emitItems(items []any) int {
	if items == nil {
		items = []any{}
	}
	if err := a.emit(map[string]any{"items": items}, nil); err != nil {
		return a.fail(err)
	}
	return 0
}

func (a *App) emitResp(r *Response) int {
	if err := a.emitResponse(r); err != nil {
		return a.fail(err)
	}
	return 0
}

func ttlSeconds(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	d, err := parseDur(s)
	if err != nil {
		return 0, err
	}
	return float64(int(d.Seconds())), nil
}

// ---- next ----

var nextFlags = []FlagSpec{{Name: "list", Aliases: []string{"column"}, Multi: true}, {Name: "claim", Bool: true},
	{Name: "move-to"}, {Name: "ttl"}, {Name: "wait"}, {Name: "interval"}, {Name: "note"}}

func cmdNext(a *App, args []string) int {
	p, err := a.parse(args, nextFlags)
	if err != nil {
		return a.fail(err)
	}
	ctx := a.ctx
	boardID, err := a.boardFromFlags(ctx, p)
	if err != nil {
		return a.fail(err)
	}
	lists := splitMulti(p.All("list"))
	if len(lists) == 0 {
		return a.failUsage("weave next --board B --list Ready [--list Other] [--claim] [--move-to NAME] [--ttl 15m] [--wait 30s]")
	}
	var ids []string
	for _, l := range lists {
		id, err := a.Resolve(ctx, "list", l, Scope{BoardID: boardID})
		if err != nil {
			return a.fail(err)
		}
		ids = append(ids, id)
	}
	params := func(listID string) (Params, error) {
		pr := Params{"listId": listID}
		if p.Bool("claim") {
			pr["claim"] = true
		}
		if s, err := ttlSeconds(p.Str("ttl")); err != nil {
			return nil, err
		} else if s > 0 {
			pr["ttlSeconds"] = s
		}
		if mt := p.Str("move-to"); mt != "" {
			id, err := a.Resolve(ctx, "list", mt, Scope{BoardID: boardID})
			if err != nil {
				return nil, err
			}
			pr["moveTo"] = id
		}
		return pr, nil
	}
	wait, _ := parseDur(p.Str("wait"))
	interval := 3 * time.Second
	if p.Has("interval") {
		interval, _ = parseDur(p.Str("interval"))
	}
	deadline := time.Now().Add(wait)
	for {
		for _, id := range ids {
			pr, err := params(id)
			if err != nil {
				return a.fail(err)
			}
			r, err := a.Do(ctx, "card.next", pr)
			if err != nil {
				if ae, ok := err.(*APIError); ok && (ae.Code == "empty" || ae.Code == "queue_empty") {
					continue
				}
				return a.fail(err)
			}
			if r.Status == 204 || len(r.Body) == 0 {
				continue
			}
			v := r.JSON()
			if m, ok := v.(map[string]any); ok && (m["item"] == nil) {
				continue
			}
			if p.Bool("claim") && p.Str("note") != "" {
				if it := itemOf(v); it != nil && !a.DryRun {
					_, _ = a.Do(ctx, "comment.create", Params{"cardId": str(it, "id"), "text": p.Str("note")})
				}
			}
			return a.emitResp(r)
		}
		if wait <= 0 || time.Now().Add(interval).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(interval):
		}
	}
	if isTTY(os.Stderr) {
		fmt.Fprintln(a.Stderr, "queue is empty")
	}
	return ExitEmpty
}

// ---- inbox ----

func cmdInbox(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "assigned", Bool: true}, {Name: "claimed", Bool: true}})
	if err != nil {
		return a.fail(err)
	}
	ctx := a.ctx
	me, err := a.Whoami(ctx)
	if err != nil {
		return a.fail(err)
	}
	boards := me.Boards
	if bid, _ := a.boardFromFlags(ctx, p); bid != "" {
		boards = []Candidate{{ID: bid}}
	}
	if len(boards) == 0 {
		projects, err := a.Items(ctx, "project.list", nil)
		if err != nil {
			return a.fail(err)
		}
		for _, pr := range projects {
			pm, _ := pr.(map[string]any)
			bs, _ := a.Items(ctx, "board.list", Params{"projectId": str(pm, "id")})
			for _, b := range bs {
				bm, _ := b.(map[string]any)
				boards = append(boards, Candidate{ID: str(bm, "id"), Name: str(bm, "name")})
			}
		}
	}
	wantAssigned, wantClaimed := p.Bool("assigned") || !p.Bool("claimed"), p.Bool("claimed") || !p.Bool("assigned")
	op, err := a.mustOp("card.list")
	if err != nil {
		return a.fail(err)
	}
	var out []any
	seen := map[string]bool{}
	serverAssigned := op.Field("memberId") != nil
	for _, b := range boards {
		var rows []any
		if serverAssigned && wantAssigned {
			// the server filters assignments (card.list ?memberId=me); members may not be in the card payload
			r, err := a.fetchPaged(ctx, op, Params{"boardId": b.ID, "memberId": me.ID}, nil, 0, true)
			if err != nil {
				a.stderrf("board %s: %v", b.ID, err)
				continue
			}
			for _, x := range itemsOf(r.JSON()) {
				if c, ok := x.(map[string]any); ok {
					c["assigned"] = true
					rows = append(rows, c)
				}
			}
		}
		if wantClaimed || !serverAssigned {
			r, err := a.fetchPaged(ctx, op, Params{"boardId": b.ID}, nil, 0, true)
			if err != nil {
				a.stderrf("board %s: %v", b.ID, err)
				continue
			}
			rows = append(rows, itemsOf(r.JSON())...)
		}
		for _, x := range rows {
			c, ok := x.(map[string]any)
			if !ok || seen[str(c, "id")] {
				continue
			}
			if v, _ := c["isClosed"].(bool); v {
				continue
			}
			assigned, claimed := cardHasMember(c, me.ID) || c["assigned"] == true, firstStr(c, "claimedBy") == me.ID
			if (wantAssigned && assigned) || (wantClaimed && claimed) {
				if seen[str(c, "id")] {
					continue
				}
				seen[str(c, "id")] = true
				c["boardId"] = firstNonEmpty(str(c, "boardId"), b.ID)
				if b.Name != "" {
					c["boardName"] = b.Name
				}
				c["assigned"], c["claimed"] = assigned, claimed
				out = append(out, c)
			}
		}
	}
	return a.emitItems(out)
}

func cardHasMember(c map[string]any, uid string) bool {
	arr, _ := c["members"].([]any)
	for _, m := range arr {
		switch t := m.(type) {
		case string:
			if t == uid {
				return true
			}
		case map[string]any:
			if str(t, "id") == uid || str(t, "userId") == uid {
				return true
			}
		}
	}
	return false
}

// ---- board tree / list stats ----

func cmdBoardTree(a *App, args []string) int {
	p, err := a.parse(args, nil)
	if err != nil {
		return a.fail(err)
	}
	ctx := a.ctx
	raw := firstNonEmpty(p.Str("board"), firstOr(p.Pos), os.Getenv("WEAVE_BOARD"))
	if raw == "" {
		return a.failUsage("weave board tree --board NAME")
	}
	bid, err := a.Resolve(ctx, "board", raw, Scope{})
	if err != nil {
		return a.fail(err)
	}
	tree, err := a.boardTree(ctx, bid)
	if err != nil {
		return a.fail(err)
	}
	if a.Out.Format == "table" || (a.Out.Format == "json" && a.stdoutTTY() && a.Out.JQ == "" && len(a.Out.Fields) == 0) {
		printTree(a, tree)
		return 0
	}
	return a.emitItem(tree)
}

func firstOr(xs []string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return ""
}

// boardTree uses board.tree, or assembles the same shape from board.get + list.list + card.list.
func (a *App) boardTree(ctx context.Context, boardID string) (map[string]any, error) {
	r, err := a.Do(ctx, "board.tree", Params{"id": boardID})
	if err == nil {
		t := itemOf(r.JSON())
		if b, ok := t["board"].(map[string]any); ok { // server shape {board, lists[]}: flatten
			flat := map[string]any{}
			for k, v := range b {
				flat[k] = v
			}
			flat["lists"] = t["lists"]
			return flat, nil
		}
		return t, nil
	}
	if ae, ok := err.(*APIError); !ok || (ae.Code != "not_found" && ae.Code != "usage") {
		return nil, err
	}
	board, err := a.Item(ctx, "board.get", Params{"id": boardID})
	if err != nil {
		return nil, err
	}
	lists, err := a.Items(ctx, "list.list", Params{"boardId": boardID})
	if err != nil {
		return nil, err
	}
	op, err := a.mustOp("card.list")
	if err != nil {
		return nil, err
	}
	cr, err := a.fetchPaged(ctx, op, Params{"boardId": boardID}, nil, 0, true)
	if err != nil {
		return nil, err
	}
	byList := map[string][]any{}
	for _, c := range itemsOf(cr.JSON()) {
		if m, ok := c.(map[string]any); ok {
			byList[str(m, "listId")] = append(byList[str(m, "listId")], c)
		}
	}
	for _, l := range lists {
		if m, ok := l.(map[string]any); ok {
			cs := byList[str(m, "id")]
			if cs == nil {
				cs = []any{}
			}
			m["cards"] = cs
		}
	}
	board["lists"] = lists
	return board, nil
}

func printTree(a *App, tree map[string]any) {
	fmt.Fprintf(a.Stdout, "%s  (%s)\n", cleanText(str(tree, "name"), false), cleanText(str(tree, "id"), false))
	lists, _ := tree["lists"].([]any)
	for _, l := range lists {
		lm, _ := l.(map[string]any)
		cards, _ := lm["cards"].([]any)
		fmt.Fprintf(a.Stdout, "  %s  (%s, %d cards)\n", cleanText(str(lm, "name"), false), cleanText(str(lm, "id"), false), len(cards))
		for _, c := range cards {
			cm, _ := c.(map[string]any)
			extra := ""
			if cb := str(cm, "claimedBy"); cb != "" {
				extra = "  [claimed]"
			}
			fmt.Fprintf(a.Stdout, "    - %s  (%s)%s\n", cleanText(str(cm, "name"), false), cleanText(str(cm, "id"), false), extra)
		}
	}
}

func cmdListStats(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "list"}})
	if err != nil {
		return a.fail(err)
	}
	ctx := a.ctx
	raw := firstNonEmpty(p.Str("board"), os.Getenv("WEAVE_BOARD"))
	if raw == "" {
		return a.failUsage("weave list stats --board NAME [--list NAME]")
	}
	bid, err := a.Resolve(ctx, "board", raw, Scope{})
	if err != nil {
		return a.fail(err)
	}
	tree, err := a.boardTree(ctx, bid)
	if err != nil {
		return a.fail(err)
	}
	only := ""
	if p.Str("list") != "" {
		if only, err = a.Resolve(ctx, "list", p.Str("list"), Scope{BoardID: bid}); err != nil {
			return a.fail(err)
		}
	}
	now := time.Now()
	var out []any
	lists, _ := tree["lists"].([]any)
	for _, l := range lists {
		lm, _ := l.(map[string]any)
		if only != "" && str(lm, "id") != only {
			continue
		}
		cards, _ := lm["cards"].([]any)
		st := map[string]any{"id": str(lm, "id"), "name": str(lm, "name"), "cards": len(cards)}
		claimed, overdue, closed := 0, 0, 0
		for _, c := range cards {
			cm, _ := c.(map[string]any)
			if str(cm, "claimedBy") != "" {
				claimed++
			}
			if v, _ := cm["isClosed"].(bool); v {
				closed++
			}
			if d := str(cm, "dueDate"); d != "" {
				if t, err := time.Parse(time.RFC3339, d); err == nil && t.Before(now) {
					if done, _ := cm["isDueCompleted"].(bool); !done {
						overdue++
					}
				}
			}
		}
		st["claimed"], st["unclaimed"], st["overdue"], st["closed"] = claimed, len(cards)-claimed, overdue, closed
		out = append(out, st)
	}
	return a.emitItems(out)
}

// ---- card shortcuts ----

func cmdCardShow(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "full", Bool: true}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 1 {
		return a.failUsage("weave card show CARD [--full] [--board B]")
	}
	ctx := a.ctx
	bid, err := a.boardFromFlags(ctx, p)
	if err != nil {
		return a.fail(err)
	}
	id, err := a.cardRef(ctx, p.Pos[0], bid)
	if err != nil {
		return a.fail(err)
	}
	params := Params{"id": id}
	if p.Bool("full") {
		params["full"] = "true"
	}
	r, err := a.Do(ctx, "card.get", params)
	if err != nil {
		return a.fail(err)
	}
	if p.Bool("full") {
		if m, ok := r.JSON().(map[string]any); ok {
			if it := itemOf(m); it != nil && it["comments"] == nil {
				// older servers: assemble the same extras from other ops
				if cs, err := a.Items(ctx, "comment.list", Params{"cardId": id}); err == nil {
					it["comments"] = cs
				}
				if ts, err := a.Items(ctx, "tasklist.list", Params{"cardId": id}); err == nil {
					it["taskLists"] = ts
				}
				if at, err := a.Items(ctx, "attachment.list", Params{"cardId": id}); err == nil {
					it["attachments"] = at
				}
				return a.emitItem(it)
			}
		}
	}
	return a.emitResp(r)
}

func cmdCardMove(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "to-list", Aliases: []string{"list", "listId"}}, {Name: "position"}, {Name: "board"}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 1 || p.Str("to-list") == "" {
		return a.failUsage("weave card move CARD --to-list NAME [--board B] [--position N]")
	}
	ctx := a.ctx
	bid, err := a.boardFromFlags(ctx, p)
	if err != nil {
		return a.fail(err)
	}
	id, err := a.cardRef(ctx, p.Pos[0], bid)
	if err != nil {
		return a.fail(err)
	}
	if bid == "" {
		bid = a.boardOfCard(ctx, id)
	}
	lid, err := a.Resolve(ctx, "list", p.Str("to-list"), Scope{BoardID: bid})
	if err != nil {
		return a.fail(err)
	}
	params := Params{"id": id, "listId": lid}
	if p.Has("position") {
		params["position"] = parseValue("number", p.Str("position"))
	}
	r, err := a.Do(ctx, "card.move", params)
	if err != nil {
		return a.fail(err)
	}
	return a.emitResp(r)
}

func (a *App) commentText(p *Parsed, from int) (string, error) {
	if p.Bool("stdin") || (len(p.Pos) > from && p.Pos[from] == "-") {
		return a.readValue("-")
	}
	if len(p.Pos) > from {
		return a.readValueMaybe(strings.Join(p.Pos[from:], " "))
	}
	if !a.stdinTTY() && !p.Has("text") {
		if s, err := a.readValue("-"); err == nil && s != "" {
			return s, nil
		}
	}
	return p.Str("text"), nil
}

func cmdCardComment(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "board"}, {Name: "text"}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 1 {
		return a.failUsage(`weave card comment CARD "text"   |   ... | weave card comment CARD --stdin`)
	}
	ctx := a.ctx
	bid, err := a.boardFromFlags(ctx, p)
	if err != nil {
		return a.fail(err)
	}
	id, err := a.cardRef(ctx, p.Pos[0], bid)
	if err != nil {
		return a.fail(err)
	}
	text, err := a.commentText(p, 1)
	if err != nil {
		return a.fail(err)
	}
	if strings.TrimSpace(text) == "" {
		return a.failUsage("empty comment")
	}
	r, err := a.Do(ctx, "comment.create", Params{"cardId": id, "text": text})
	if err != nil {
		return a.fail(err)
	}
	return a.emitResp(r)
}

func cmdCardAttach(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "name"}, {Name: "board"}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 2 {
		return a.failUsage("weave card attach CARD FILE [FILE...] [--name NAME]")
	}
	ctx := a.ctx
	bid, err := a.boardFromFlags(ctx, p)
	if err != nil {
		return a.fail(err)
	}
	id, err := a.cardRef(ctx, p.Pos[0], bid)
	if err != nil {
		return a.fail(err)
	}
	op, err := a.mustOp("attachment.create")
	if err != nil {
		return a.fail(err)
	}
	var out []any
	for _, f := range p.Pos[1:] {
		params := Params{"cardId": id}
		if n := p.Str("name"); n != "" && len(p.Pos) == 2 {
			params["name"] = n
		}
		r, err := a.Call(ctx, op, params, map[string]string{"file": f})
		if err != nil {
			return a.fail(err)
		}
		out = append(out, itemOf(r.JSON()))
	}
	if len(out) == 1 {
		return a.emitItem(out[0])
	}
	return a.emitItems(out)
}

func cmdCardClaim(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "ttl"}, {Name: "note"}, {Name: "list"}, {Name: "board"}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 1 {
		return a.failUsage("weave card claim CARD [--ttl 15m] [--note TEXT] [--list NAME]  (exit 7 if already claimed)")
	}
	ctx := a.ctx
	bid, err := a.boardFromFlags(ctx, p)
	if err != nil {
		return a.fail(err)
	}
	id, err := a.cardRef(ctx, p.Pos[0], bid)
	if err != nil {
		return a.fail(err)
	}
	params := Params{"id": id}
	if s, err := ttlSeconds(p.Str("ttl")); err != nil {
		return a.fail(err)
	} else if s > 0 {
		params["ttlSeconds"] = s
	}
	if n := p.Str("note"); n != "" {
		params["note"] = n
	}
	if l := p.Str("list"); l != "" {
		if bid == "" {
			bid = a.boardOfCard(ctx, id)
		}
		if params["listId"], err = a.Resolve(ctx, "list", l, Scope{BoardID: bid}); err != nil {
			return a.fail(err)
		}
	}
	r, err := a.Do(ctx, "card.claim", params)
	if err != nil {
		return a.fail(err)
	}
	return a.emitResp(r)
}

func cmdCardRelease(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "note"}, {Name: "board"}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 1 {
		return a.failUsage("weave card release CARD [--note TEXT]")
	}
	ctx := a.ctx
	bid, _ := a.boardFromFlags(ctx, p)
	id, err := a.cardRef(ctx, p.Pos[0], bid)
	if err != nil {
		return a.fail(err)
	}
	params := Params{"id": id}
	if n := p.Str("note"); n != "" {
		params["note"] = n
	}
	r, err := a.Do(ctx, "card.release", params)
	if err != nil {
		return a.fail(err)
	}
	return a.emitResp(r)
}

func cmdCardRenew(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "ttl"}, {Name: "board"}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 1 {
		return a.failUsage("weave card renew CARD [--ttl 15m]")
	}
	ctx := a.ctx
	bid, _ := a.boardFromFlags(ctx, p)
	id, err := a.cardRef(ctx, p.Pos[0], bid)
	if err != nil {
		return a.fail(err)
	}
	params := Params{"id": id}
	if s, err := ttlSeconds(p.Str("ttl")); err != nil {
		return a.fail(err)
	} else if s > 0 {
		params["ttlSeconds"] = s
	}
	r, err := a.Do(ctx, "card.renew", params)
	if err != nil {
		return a.fail(err)
	}
	return a.emitResp(r)
}

// card done: comment the result, move to the done list, release the claim.
func cmdCardDone(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "note"}, {Name: "to-list", Aliases: []string{"list"}}, {Name: "board"}, {Name: "keep-claim", Bool: true}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 1 {
		return a.failUsage("weave card done CARD [--note TEXT] [--to-list NAME] [--keep-claim]")
	}
	ctx := a.ctx
	bid, err := a.boardFromFlags(ctx, p)
	if err != nil {
		return a.fail(err)
	}
	id, err := a.cardRef(ctx, p.Pos[0], bid)
	if err != nil {
		return a.fail(err)
	}
	var steps []any
	if n := p.Str("note"); n != "" {
		if n, err = a.readValueMaybe(n); err != nil {
			return a.fail(err)
		}
		if _, err := a.Do(ctx, "comment.create", Params{"cardId": id, "text": n}); err != nil {
			return a.fail(err)
		}
		steps = append(steps, "comment")
	}
	if tl := p.Str("to-list"); tl != "" {
		if bid == "" {
			bid = a.boardOfCard(ctx, id)
		}
		if err := a.moveCardToList(ctx, id, bid, tl); err != nil {
			return a.fail(err)
		}
		steps = append(steps, "move")
	}
	if !p.Bool("keep-claim") {
		if _, err := a.Do(ctx, "card.release", Params{"id": id}); err != nil {
			if ae, ok := err.(*APIError); !ok || (ae.Code != "conflict" && ae.Code != "validation") {
				return a.fail(err)
			}
		} else {
			steps = append(steps, "release")
		}
	}
	return a.emitItem(map[string]any{"id": id, "done": true, "steps": steps})
}

// ---- card create ----

var cardCreateFlags = []FlagSpec{{Name: "list", Aliases: []string{"list-id"}}, {Name: "name"}, {Name: "desc", Aliases: []string{"description"}},
	{Name: "label", Multi: true}, {Name: "assign", Aliases: []string{"assignee", "member"}, Multi: true}, {Name: "due"},
	{Name: "type"}, {Name: "position"}, {Name: "from-stdin", Bool: true}, {Name: "claim", Bool: true}}

type cardSpec struct {
	Name, Desc, List, Due, Type string
	Labels, Assign              []string
	Position                    any
}

func cmdCardCreate(a *App, args []string) int {
	p, err := a.parse(args, cardCreateFlags)
	if err != nil {
		return a.fail(err)
	}
	ctx := a.ctx
	bid, err := a.boardFromFlags(ctx, p)
	if err != nil {
		return a.fail(err)
	}
	base := cardSpec{Name: p.Str("name"), List: p.Str("list"), Due: p.Str("due"), Type: p.Str("type"),
		Labels: splitMulti(p.All("label")), Assign: splitMulti(p.All("assign"))}
	if d := p.Str("desc"); d != "" {
		if base.Desc, err = a.readValueMaybe(d); err != nil {
			return a.fail(err)
		}
	}
	if p.Has("position") {
		base.Position = parseValue("number", p.Str("position"))
	}
	if len(p.Pos) > 0 && base.Name == "" {
		base.Name = strings.Join(p.Pos, " ")
	}
	var specs []cardSpec
	if p.Bool("from-stdin") {
		sc := bufio.NewScanner(a.Stdin)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for n := 1; sc.Scan(); n++ {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var in struct {
				Name      string   `json:"name"`
				Desc      string   `json:"description"`
				Desc2     string   `json:"desc"`
				List      string   `json:"list"`
				Due       string   `json:"dueDate"`
				Type      string   `json:"type"`
				Labels    []string `json:"labels"`
				Assign    []string `json:"assign"`
				Assignees []string `json:"assignees"`
				Members   []string `json:"members"`
				Position  any      `json:"position"`
			}
			if json.Unmarshal([]byte(line), &in) != nil {
				// a bare line is a card title
				in.Name = line
			}
			s := base
			s.Labels = append([]string{}, base.Labels...)
			s.Assign = append([]string{}, base.Assign...)
			if in.Name != "" {
				s.Name = in.Name
			}
			if in.Desc != "" {
				s.Desc = in.Desc
			} else if in.Desc2 != "" {
				s.Desc = in.Desc2
			}
			if in.List != "" {
				s.List = in.List
			}
			if in.Due != "" {
				s.Due = in.Due
			}
			if in.Type != "" {
				s.Type = in.Type
			}
			if in.Position != nil {
				s.Position = in.Position
			}
			s.Labels = append(s.Labels, in.Labels...)
			s.Assign = append(append(append(s.Assign, in.Assign...), in.Assignees...), in.Members...)
			if s.Name == "" {
				return a.failUsage(fmt.Sprintf("stdin line %d has no name", n))
			}
			specs = append(specs, s)
		}
	} else {
		if base.Name == "" {
			return a.failUsage(`weave card create --board B --list L --name "Title" [--desc TEXT|@file] [--label X ...] [--assign USER ...] [--due RFC3339] [--from-stdin]`)
		}
		specs = []cardSpec{base}
	}
	var out []any
	for _, s := range specs {
		item, err := a.createCard(ctx, bid, s, p.Bool("claim"))
		if err != nil {
			if len(specs) > 1 {
				a.stderrf("card %q: %v", s.Name, err)
				a.fail(err)
				return ExitError
			}
			return a.fail(err)
		}
		out = append(out, item)
		if len(specs) > 1 {
			b, _ := json.Marshal(item)
			fmt.Fprintln(a.Stdout, string(b)) // NDJSON as we go
		}
	}
	if len(specs) == 1 {
		return a.emitItem(out[0])
	}
	return 0
}

func (a *App) createCard(ctx context.Context, boardID string, s cardSpec, claim bool) (map[string]any, error) {
	if s.List == "" {
		return nil, usageErr("missing --list")
	}
	lid, err := a.Resolve(ctx, "list", s.List, Scope{BoardID: boardID})
	if err != nil {
		return nil, err
	}
	params := Params{"listId": lid, "name": s.Name}
	if s.Desc != "" {
		params["description"] = s.Desc
	}
	if s.Due != "" {
		params["dueDate"] = s.Due
	}
	if s.Type != "" {
		params["type"] = s.Type
	}
	if s.Position != nil {
		params["position"] = s.Position
	}
	item, err := a.Item(ctx, "card.create", params)
	if err != nil {
		return nil, err
	}
	cid := str(item, "id")
	if a.DryRun {
		return item, nil
	}
	for _, l := range s.Labels {
		id, err := a.Resolve(ctx, "label", l, Scope{BoardID: boardID, CardID: cid})
		if err != nil {
			return item, err
		}
		if _, err := a.Do(ctx, "card.label.add", Params{"id": cid, "labelId": id}); err != nil {
			return item, err
		}
	}
	for _, u := range s.Assign {
		id, err := a.Resolve(ctx, "user", u, Scope{})
		if err != nil {
			return item, err
		}
		if _, err := a.Do(ctx, "card.member.add", Params{"id": cid, "userId": id}); err != nil {
			return item, err
		}
	}
	if claim {
		if r, err := a.Do(ctx, "card.claim", Params{"id": cid}); err == nil {
			item["claim"] = itemOf(r.JSON())
		}
	}
	return item, nil
}

// ---- tasks (checklists) ----

func cmdTaskAdd(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "list"}, {Name: "assignee"}, {Name: "board"}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 2 {
		return a.failUsage(`weave task add CARD "text" [--list CHECKLIST-NAME]   (creates the checklist if the card has none)`)
	}
	ctx := a.ctx
	bid, _ := a.boardFromFlags(ctx, p)
	cid, err := a.cardRef(ctx, p.Pos[0], bid)
	if err != nil {
		return a.fail(err)
	}
	lists, err := a.Items(ctx, "tasklist.list", Params{"cardId": cid})
	if err != nil {
		return a.fail(err)
	}
	var tlID string
	want := p.Str("list")
	for _, l := range lists {
		m, _ := l.(map[string]any)
		if want == "" || strings.EqualFold(str(m, "name"), want) {
			tlID = str(m, "id")
			break
		}
	}
	if tlID == "" {
		name := want
		if name == "" {
			name = "Checklist"
		}
		it, err := a.Item(ctx, "tasklist.create", Params{"cardId": cid, "name": name})
		if err != nil {
			return a.fail(err)
		}
		tlID = str(it, "id")
	}
	var out []any
	for _, text := range p.Pos[1:] {
		params := Params{"taskListId": tlID, "name": text}
		if u := p.Str("assignee"); u != "" {
			if params["assigneeUserId"], err = a.Resolve(ctx, "user", u, Scope{}); err != nil {
				return a.fail(err)
			}
		}
		it, err := a.Item(ctx, "task.create", params)
		if err != nil {
			return a.fail(err)
		}
		out = append(out, it)
	}
	if len(out) == 1 {
		return a.emitItem(out[0])
	}
	return a.emitItems(out)
}

func (a *App) tasksOf(ctx context.Context, cardID string) ([]map[string]any, error) {
	lists, err := a.Items(ctx, "tasklist.list", Params{"cardId": cardID})
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, l := range lists {
		lm, _ := l.(map[string]any)
		ts, _ := lm["tasks"].([]any)
		for _, t := range ts {
			if tm, ok := t.(map[string]any); ok {
				tm["taskList"] = str(lm, "name")
				out = append(out, tm)
			}
		}
	}
	return out, nil
}

func cmdTaskList(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "board"}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 1 {
		return a.failUsage("weave task list CARD")
	}
	bid, _ := a.boardFromFlags(a.ctx, p)
	cid, err := a.cardRef(a.ctx, p.Pos[0], bid)
	if err != nil {
		return a.fail(err)
	}
	ts, err := a.tasksOf(a.ctx, cid)
	if err != nil {
		return a.fail(err)
	}
	items := make([]any, len(ts))
	for i, t := range ts {
		items[i] = t
	}
	return a.emitItems(items)
}

// taskCheck marks tasks complete/incomplete: by task id, or --card C with --n INDEX (1-based) or --text PREFIX.
func (a *App) taskCheck(args []string, done bool) int {
	p, err := a.parse(args, []FlagSpec{{Name: "card"}, {Name: "text"}, {Name: "n"}, {Name: "board"}})
	if err != nil {
		return a.fail(err)
	}
	ctx := a.ctx
	var ids []string
	if p.Has("card") {
		bid, _ := a.boardFromFlags(ctx, p)
		cid, err := a.cardRef(ctx, p.Str("card"), bid)
		if err != nil {
			return a.fail(err)
		}
		ts, err := a.tasksOf(ctx, cid)
		if err != nil {
			return a.fail(err)
		}
		switch {
		case p.Has("n"):
			n, _ := strconv.Atoi(p.Str("n"))
			if n < 1 || n > len(ts) {
				return a.fail(&APIError{Code: "not_found", Message: fmt.Sprintf("card has %d tasks; --n %s is out of range", len(ts), p.Str("n"))})
			}
			ids = append(ids, str(ts[n-1], "id"))
		case p.Has("text"):
			var m []map[string]any
			for _, t := range ts {
				if strings.HasPrefix(strings.ToLower(str(t, "name")), strings.ToLower(p.Str("text"))) {
					m = append(m, t)
				}
			}
			if len(m) != 1 {
				return a.fail(&APIError{Code: map[bool]string{true: "ambiguous", false: "not_found"}[len(m) > 1], Message: fmt.Sprintf("%d tasks match %q", len(m), p.Str("text"))})
			}
			ids = append(ids, str(m[0], "id"))
		default:
			return a.failUsage("with --card give --n INDEX or --text PREFIX")
		}
	} else {
		ids = p.Pos
	}
	if len(ids) == 0 {
		return a.failUsage("weave task check TASK_ID...   |   weave task check --card CARD (--n N | --text PREFIX)")
	}
	var out []any
	for _, id := range ids {
		it, err := a.Item(ctx, "task.update", Params{"id": id, "isCompleted": done})
		if err != nil {
			return a.fail(err)
		}
		out = append(out, it)
	}
	if len(out) == 1 {
		return a.emitItem(out[0])
	}
	return a.emitItems(out)
}

var _ = sort.Strings
