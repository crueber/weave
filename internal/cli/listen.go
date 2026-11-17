package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var listenFlags = []FlagSpec{
	{Name: "board", Multi: true}, {Name: "list", Aliases: []string{"listen", "column"}, Multi: true},
	{Name: "type", Aliases: []string{"types"}, Multi: true},
	{Name: "exec"}, {Name: "once", Bool: true}, {Name: "timeout"},
	{Name: "cursor-file"}, {Name: "since"}, {Name: "include-existing", Bool: true},
	{Name: "agent-label", Aliases: []string{"agent"}}, {Name: "claim", Bool: true}, {Name: "claim-move-to"},
	{Name: "ttl"}, {Name: "claim-note"}, {Name: "concurrency"}, {Name: "exec-timeout"}, {Name: "on-error"},
	{Name: "ignore-self", Bool: true}, {Name: "print", Bool: true}, {Name: "heartbeat"}, {Name: "grace"},
	{Name: "no-cursor", Bool: true}, {Name: "rooms"}, {Name: "verbose", Short: "v", Bool: true},
}

func init() {
	registerSpec("listen", listenFlags, cmdListen)
	registerSpec("watch", listenFlags, cmdWatch)
}

// shortcutSpecs remembers each shortcut's own flags (see delegateToOp).
var shortcutSpecs = map[string][]FlagSpec{}

func registerSpec(name string, specs []FlagSpec, f cmdFunc) {
	register(name, f)
	shortcutSpecs[name] = specs
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	commandFlags[name] = names
}

// listenCfg is the resolved configuration shared by `listen` and `agent run`.
type listenCfg struct {
	boards, lists, types []string
	extraLists           []string          // the claim-move-to lists: watched only for expired leases (stranded cards)
	listBoard            map[string]string // list id -> board id
	cursorFile           string
	since                string
	heartbeat            time.Duration
	label                string
	includeExisting      bool
	ignoreSelf           bool
	verbose              bool
}

func splitMulti(vs []string) []string {
	var out []string
	for _, v := range vs {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// resolveWatch resolves --board / --list names to ids.
func (a *App) resolveWatch(ctx context.Context, boards, lists []string) (*listenCfg, error) {
	c := &listenCfg{listBoard: map[string]string{}}
	if len(boards) == 0 {
		if v := os.Getenv("WEAVE_BOARD"); v != "" {
			boards = []string{v}
		}
	}
	for _, b := range boards {
		id, err := a.Resolve(ctx, "board", b, Scope{})
		if err != nil {
			return nil, err
		}
		c.boards = append(c.boards, id)
	}
	for _, l := range lists {
		if looksLikeID(l) {
			c.lists = append(c.lists, l)
			if len(c.boards) == 1 {
				c.listBoard[l] = c.boards[0]
			}
			continue
		}
		if len(c.boards) == 0 {
			return nil, usageErr(fmt.Sprintf("list %q given by name: add --board NAME", l))
		}
		var found []string
		var lastErr error
		for _, bid := range c.boards {
			id, err := a.Resolve(ctx, "list", l, Scope{BoardID: bid})
			if err != nil {
				lastErr = err
				if ae, ok := err.(*APIError); ok && ae.Code == "ambiguous" {
					return nil, err
				}
				continue
			}
			found = append(found, id)
			c.listBoard[id] = bid
		}
		if len(found) == 0 {
			return nil, lastErr
		}
		c.lists = append(c.lists, found...)
	}
	if len(c.boards) == 0 && len(c.lists) == 0 {
		return nil, usageErr("say what to listen to: --board NAME and/or --list NAME (repeatable)")
	}
	return c, nil
}

func (a *App) defaultCursorFile(c *listenCfg) string {
	key := strings.Join(c.boards, ",") + "|" + strings.Join(c.lists, ",") + "|" + strings.Join(c.types, ",")
	return filepath.Join(configDir(), "cursors", safeName(a.Cfg.Profile)+"-"+shortHash(a.Cfg.URL+"|"+key)+".cursor")
}

func (a *App) stderrf(format string, args ...any) {
	fmt.Fprintf(a.Stderr, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

// buildListener wires the SSE listener with filters, self-skip and the include-existing backlog.
func (a *App) buildListener(c *listenCfg) *Listener {
	var serverTypes []string
	for _, t := range c.types {
		if t != "card.existing" { // synthetic: only filtered client-side
			serverTypes = append(serverTypes, t)
		}
	}
	l := &Listener{
		App: a, Boards: c.boards, Lists: append(append([]string{}, c.lists...), c.extraLists...), Types: serverTypes, Since: c.since,
		CursorFile: c.cursorFile, Heartbeat: c.heartbeat, AgentLabel: c.label,
	}
	if c.verbose {
		l.Log = a.stderrf
	} else {
		l.Log = func(f string, args ...any) {
			a.stderrf(f, args...)
		}
	}
	selfID := ""
	if c.ignoreSelf {
		if me, err := a.Whoami(a.ctx); err == nil {
			selfID = me.ID
		} else {
			a.stderrf("warning: cannot determine own identity (%v); not skipping own events", err)
		}
	}
	listSet := map[string]bool{}
	for _, x := range c.lists {
		listSet[x] = true
	}
	for _, x := range c.extraLists {
		listSet[x] = true
	}
	typeSet := map[string]bool{}
	for _, t := range c.types {
		typeSet[t] = true
	}
	l.Skip = func(ev Event) bool {
		if selfID != "" && ev.ActorID() == selfID {
			return true
		}
		if strings.HasPrefix(ev.Type, "listener.") && !typeSet[ev.Type] {
			return true
		}
		if len(typeSet) > 0 && ev.Type != "card.existing" && !typeSet[ev.Type] {
			return true
		}
		if len(listSet) > 0 {
			ids := []string{ev.S("listId"), ev.S("fromListId"), ev.S("toListId")}
			any, match := false, false
			for _, id := range ids {
				if id != "" {
					any = true
					if listSet[id] {
						match = true
					}
				}
			}
			if any && !match {
				return true
			}
		}
		return false
	}
	if c.includeExisting {
		l.Existing = func(ctx context.Context) ([]Event, error) { return a.existingEvents(ctx, c) }
	}
	return l
}

// existingEvents returns synthetic card.existing envelopes for cards already in the watched lists.
func (a *App) existingEvents(ctx context.Context, c *listenCfg) ([]Event, error) {
	op, err := a.mustOp("card.list")
	if err != nil {
		return nil, err
	}
	var out []Event
	targets := map[string]string{} // list -> board
	for _, l := range append(append([]string{}, c.lists...), c.extraLists...) {
		targets[l] = c.listBoard[l]
	}
	if len(c.lists) == 0 { // whole boards
		for _, b := range c.boards {
			targets["*"+b] = b
		}
	}
	for l, b := range targets {
		if b == "" {
			return nil, usageErr("--include-existing needs --board so cards can be listed")
		}
		params := Params{"boardId": b}
		if !strings.HasPrefix(l, "*") {
			params["listId"] = l
		}
		r, err := a.fetchPaged(ctx, op, params, nil, 0, true)
		if err != nil {
			return nil, err
		}
		for _, x := range itemsOf(r.JSON()) {
			card, ok := x.(map[string]any)
			if !ok {
				continue
			}
			lid := firstStr(card, "listId")
			if lid == "" {
				lid = strings.TrimPrefix(l, "*")
			}
			if len(c.lists) > 0 && !contains(c.lists, lid) && !contains(c.extraLists, lid) {
				continue
			}
			env := map[string]any{"id": "", "type": "card.existing", "boardId": b, "listId": lid, "toListId": lid,
				"cardId": str(card, "id"), "card": card, "actor": nil, "at": time.Now().UTC().Format(time.RFC3339), "synthetic": true}
			raw, _ := json.Marshal(env)
			out = append(out, Event{Type: "card.existing", M: env, Raw: raw})
		}
	}
	return out, nil
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ---- exec ----

type execSpec struct {
	shell string   // run through the platform shell
	argv  []string // or run directly
	env   []string
	// clean (outbox mode): no inherited WEAVE_* variables (so no token, no URL), no card text, and stdin is
	// /dev/null instead of the event envelope; env carries only ids and file paths.
	clean bool
}

// cleanEnv is the inherited environment without any WEAVE_* variable.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "WEAVE_") {
			out = append(out, kv)
		}
	}
	return out
}

func (a *App) childEnv(ev Event) []string {
	env := os.Environ()
	add := func(k, v string) { env = append(env, k+"="+v) }
	add("WEAVE_EVENT_TYPE", ev.Type)
	add("WEAVE_EVENT_ID", ev.ID)
	add("WEAVE_CARD_ID", ev.CardID())
	add("WEAVE_LIST_ID", ev.ListID())
	add("WEAVE_FROM_LIST_ID", ev.S("fromListId"))
	add("WEAVE_BOARD_ID", ev.BoardID())
	add("WEAVE_ACTOR_ID", ev.ActorID())
	if c := ev.Card(); c != nil {
		add("WEAVE_CARD_NAME", str(c, "name"))
	}
	add("WEAVE_URL", a.Cfg.URL)
	add("WEAVE_TOKEN", a.Cfg.Token)
	add("WEAVE_PROFILE", a.Cfg.Profile)
	return env
}

type tailBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 8192 {
		t.buf = t.buf[len(t.buf)-8192:]
	}
	return len(p), nil
}
func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// runChild runs the command with the envelope on stdin. Returns the exit code (-1 if it could not start).
func (a *App) runChild(ctx context.Context, es execSpec, ev Event, timeout time.Duration, stdout, errTail io.Writer) (int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var cmd *exec.Cmd
	switch {
	case es.shell != "" && runtime.GOOS == "windows":
		cmd = exec.CommandContext(ctx, "cmd", "/C", es.shell)
	case es.shell != "":
		cmd = exec.CommandContext(ctx, "sh", "-c", es.shell)
	default:
		cmd = exec.CommandContext(ctx, es.argv[0], es.argv[1:]...)
	}
	if es.clean {
		cmd.Env = append(cleanEnv(), es.env...)
	} else {
		cmd.Env = append(a.childEnv(ev), es.env...)
		cmd.Stdin = bytes.NewReader(append(ev.marshal(), '\n'))
	}
	cmd.Stdout = stdout
	cmd.Stderr = io.MultiWriter(a.Stderr, errTail)
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// ---- listen command ----

func cmdListen(a *App, args []string) int {
	p, err := a.parse(args, listenFlags)
	if err != nil {
		return a.fail(err)
	}
	return a.runListen(p)
}

func cmdWatch(a *App, args []string) int {
	p, err := a.parse(args, listenFlags)
	if err != nil {
		return a.fail(err)
	}
	if p.Has("rooms") { // legacy: raw realtime stream
		l := &Listener{App: a, Rooms: p.Str("rooms"), Log: a.stderrf}
		l.Handle = func(ctx context.Context, ev Event, done func()) {
			fmt.Fprintln(a.Stdout, string(ev.marshal()))
			done()
		}
		if err := l.Run(a.ctx); err != nil {
			return a.fail(err)
		}
		return 0
	}
	return a.runListen(p)
}

func (a *App) runListen(p *Parsed) int {
	parent := a.ctx
	cfg, err := a.resolveWatch(parent, splitMulti(p.All("board")), splitMulti(p.All("list")))
	if err != nil {
		return a.fail(err)
	}
	cfg.types = splitMulti(p.All("type"))
	claim := p.Bool("claim")
	if claim && len(cfg.types) == 0 {
		cfg.types = claimTypes()
	}
	if err := a.fillListenCfg(cfg, p); err != nil {
		return a.fail(err)
	}
	var ttl time.Duration
	if p.Has("ttl") {
		if ttl, err = parseDur(p.Str("ttl")); err != nil {
			return a.fail(err)
		}
	}
	execTimeout, _ := parseDur(p.Str("exec-timeout"))
	grace := 30 * time.Second
	if p.Has("grace") {
		grace, _ = parseDur(p.Str("grace"))
	}
	var waitFor time.Duration
	if p.Has("timeout") {
		if waitFor, err = parseDur(p.Str("timeout")); err != nil {
			return a.fail(err)
		}
	}
	conc := 1
	if p.Has("concurrency") {
		fmt.Sscan(p.Str("concurrency"), &conc)
		if conc < 1 {
			conc = 1
		}
	}
	execCmd := p.Str("exec")
	onError := p.Str("on-error")
	printEnv := p.Bool("print") || (execCmd == "" && a.Out.Format != "quiet")

	lctx, cancel := context.WithCancel(parent)
	defer cancel()
	l := a.buildListener(cfg)
	var count atomic.Int64
	var wg sync.WaitGroup
	var outMu sync.Mutex
	sem := make(chan struct{}, conc)
	execCtx, execCancel := context.WithCancel(context.Background())
	defer execCancel()
	once := p.Bool("once")
	var firstTaken atomic.Bool
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(cancel) }

	l.Handle = func(ctx context.Context, ev Event, done func()) {
		if once && firstTaken.Load() { // --once: never claim a second card
			done()
			return
		}
		if claim {
			if a.tryClaim(ctx, &ev, cfg, claimOpts{ttl: ttl, note: p.Str("claim-note"), moveTo: p.Str("claim-move-to")}) != claimYes {
				done()
				return
			}
		}
		if once && !firstTaken.CompareAndSwap(false, true) {
			done()
			return
		}
		if printEnv {
			outMu.Lock()
			fmt.Fprintln(a.Stdout, string(ev.marshal()))
			outMu.Unlock()
		}
		if execCmd == "" {
			count.Add(1)
			done()
			if once {
				stop()
			}
			return
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			done()
			return
		}
		wg.Add(1)
		count.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			tail := &tailBuf{}
			code, err := a.runChild(execCtx, execSpec{shell: execCmd}, ev, execTimeout, a.Stdout, tail)
			if err != nil || code != 0 {
				msg := fmt.Sprintf("exec failed for %s %s (exit %d)", ev.Type, ev.CardID(), code)
				if err != nil {
					msg += ": " + err.Error()
				}
				a.stderrf("%s", msg)
				a.onExecError(onError, ev, msg, tail.String())
			}
			done()
			if once {
				stop()
			}
		}()
	}
	var timedOut atomic.Bool
	if waitFor > 0 {
		t := time.AfterFunc(waitFor, func() { timedOut.Store(true); stop() })
		defer t.Stop()
	}
	if cfg.verbose {
		a.stderrf("listening: boards=%v lists=%v types=%v cursor=%s", cfg.boards, cfg.lists, cfg.types, cfg.cursorFile)
	}
	err = l.Run(lctx)
	// graceful drain of in-flight exec children
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(grace):
		a.stderrf("grace period over; stopping running commands")
		execCancel()
		<-finished
	}
	if err != nil {
		return a.fail(err)
	}
	if timedOut.Load() && count.Load() == 0 {
		if isTTY(os.Stderr) {
			a.stderrf("no matching event within %s", waitFor)
		}
		return ExitEmpty
	}
	return 0
}

// fillListenCfg handles the options common to listen and agent run.
func (a *App) fillListenCfg(cfg *listenCfg, p *Parsed) error {
	cfg.since = p.Str("since")
	cfg.label = p.Str("agent-label")
	cfg.includeExisting = p.Bool("include-existing")
	cfg.verbose = p.Bool("verbose")
	cfg.ignoreSelf = true
	if p.Has("ignore-self") {
		cfg.ignoreSelf = p.Bool("ignore-self")
	}
	cfg.heartbeat = 15 * time.Second
	if p.Has("heartbeat") {
		d, err := parseDur(p.Str("heartbeat"))
		if err != nil {
			return err
		}
		cfg.heartbeat = d
	}
	switch {
	case p.Has("cursor-file"):
		cfg.cursorFile = p.Str("cursor-file")
	case !p.Bool("no-cursor"):
		cfg.cursorFile = a.defaultCursorFile(cfg)
	}
	return nil
}

func claimTypes() []string {
	return []string{"card.created", "card.moved", "card.released", "card.restored", "card.existing"}
}

// claimOpts tunes tryClaim.
type claimOpts struct {
	ttl       time.Duration
	note      string
	moveTo    string                   // list NAME to move the card to when the claim succeeds
	respecWIP bool                     // do not claim-move into a list at or over its WIP limit
	inflight  func(cardID string) bool // true when this process is already working the card
}

type claimResult int

const (
	claimNo       claimResult = iota // not claimable (or lost the race): skip quietly
	claimYes                         // claimed
	claimDeferred                    // claimable but the target list is full (WIP): try again later
)

// claimable reports whether a card with this claim state may be claimed by me: nobody holds a live
// lease, or (mine=true) the lease is mine.
func (a *App) claimable(ctx context.Context, card map[string]any, allowMine bool) bool {
	cb := str(card, "claimedBy")
	if cb == "" {
		return true
	}
	if until := str(card, "claimedUntil"); until != "" {
		if t, err := time.Parse(time.RFC3339, until); err == nil && t.Before(time.Now()) {
			return true // lapsed lease: unclaimed
		}
	}
	if !allowMine {
		return false
	}
	me, err := a.Whoami(ctx)
	return err == nil && cb == me.ID
}

// tryClaim claims the event's card when it is claimable. It never trusts the event: the card is read
// fresh and must still be in a watched list (or, for a stranded card, in the claim-move-to list after a
// lease expiry), open, and unclaimed (or mine in a watched list). The event's list is never the claim
// target, so a stale or replayed event cannot drag a card backwards; only --claim-move-to moves it,
// atomically with the claim. A 409 is quiet (someone else won). On success the envelope gains a
// "claim" object, the fresh card, and "originListId".
func (a *App) tryClaim(ctx context.Context, ev *Event, cfg *listenCfg, o claimOpts) claimResult {
	cardID := ev.CardID()
	if cardID == "" {
		return claimNo
	}
	switch ev.Type {
	case "card.created", "card.moved", "card.released", "card.restored", "card.existing", "card.updated":
	default:
		return claimNo
	}
	if o.inflight != nil && o.inflight(cardID) {
		return claimNo
	}
	r, err := a.Do(ctx, "card.get", Params{"id": cardID})
	if err != nil {
		if ctx.Err() == nil && cfg.verbose {
			a.stderrf("claim %s: cannot read the card: %v", cardID, err)
		}
		return claimNo
	}
	card := itemOf(r.JSON())
	if v, _ := card["isClosed"].(bool); v {
		return claimNo
	}
	cur := str(card, "listId")
	boardID := firstNonEmpty(str(card, "boardId"), ev.BoardID())
	switch {
	case contains(cfg.lists, cur):
		if !a.claimable(ctx, card, true) {
			return claimNo
		}
	case contains(cfg.extraLists, cur) && (ev.Type == "card.released" || ev.Type == "card.existing"):
		// a stranded card: its lease expired in the claim-move-to list, so nothing else would pick it up again
		if !a.claimable(ctx, card, false) {
			return claimNo
		}
	default:
		return claimNo
	}
	params := Params{"id": cardID}
	target := ""
	if o.moveTo != "" {
		id, err := a.Resolve(ctx, "list", o.moveTo, Scope{BoardID: boardID})
		if err != nil {
			a.stderrf("claim %s: cannot resolve --claim-move-to %q: %v", cardID, o.moveTo, err)
			return claimNo
		}
		if id != cur {
			target = id
			if o.respecWIP && a.listFull(ctx, boardID, id) {
				if cfg.verbose {
					a.stderrf("claim %s deferred: %q is at its WIP limit", cardID, o.moveTo)
				}
				return claimDeferred
			}
			params["listId"] = id
		}
	}
	if o.ttl > 0 {
		params["ttlSeconds"] = float64(int(o.ttl.Seconds()))
	}
	if o.note != "" {
		params["note"] = o.note
	}
	cr, err := a.Do(ctx, "card.claim", params)
	if err != nil {
		if ae, ok := err.(*APIError); ok && ae.Code == "conflict" {
			if cfg.verbose {
				a.stderrf("card %s already claimed; skipping", cardID)
			}
			return claimNo
		}
		if ctx.Err() == nil {
			a.stderrf("claim %s failed: %v", cardID, err)
		}
		return claimNo
	}
	ev.M["claim"] = itemOf(cr.JSON())
	ev.M["originListId"] = cur
	now := cur
	if target != "" {
		now = target
	}
	card["listId"] = now
	ev.M["card"] = card
	ev.M["listId"] = now
	if _, ok := ev.M["toListId"]; ok {
		ev.M["toListId"] = now
	}
	raw, _ := json.Marshal(ev.M)
	ev.Raw = raw
	return claimYes
}

// listFull reports whether a list has a WIP limit and already holds that many cards (advisory).
func (a *App) listFull(ctx context.Context, boardID, listID string) bool {
	lr, err := a.Do(ctx, "list.get", Params{"id": listID})
	if err != nil {
		return false
	}
	lim := 0
	if f, ok := itemOf(lr.JSON())["wipLimit"].(float64); ok {
		lim = int(f)
	}
	if lim <= 0 {
		return false
	}
	op, err := a.mustOp("card.list")
	if err != nil {
		return false
	}
	r, err := a.fetchPaged(ctx, op, Params{"boardId": boardID, "listId": listID}, nil, 0, true)
	if err != nil {
		return false
	}
	return len(itemsOf(r.JSON())) >= lim
}

func (a *App) moveCardToList(ctx context.Context, cardID, boardID, listName string) error {
	sc := Scope{BoardID: boardID, CardID: cardID}
	id, err := a.Resolve(ctx, "list", listName, sc)
	if err != nil {
		return err
	}
	_, err = a.Do(ctx, "card.move", Params{"id": cardID, "listId": id})
	return err
}

func (a *App) onExecError(mode string, ev Event, msg, tail string) {
	cardID := ev.CardID()
	if cardID == "" || mode == "" {
		return
	}
	ctx := context.Background()
	for _, m := range strings.Split(mode, ",") {
		switch strings.TrimSpace(m) {
		case "comment":
			text := redactSecrets(msg, a.Cfg.Token)
			if tail = sanitizeTail(tail, a.Cfg.Token); tail != "" {
				text += "\n\n```\n" + tail + "\n```"
			}
			if _, err := a.Do(ctx, "comment.create", Params{"cardId": cardID, "text": text}); err != nil {
				a.stderrf("on-error comment failed: %v", err)
			}
		case "release":
			if _, err := a.Do(ctx, "card.release", Params{"id": cardID}); err != nil {
				a.stderrf("on-error release failed: %v", err)
			}
		}
	}
}
