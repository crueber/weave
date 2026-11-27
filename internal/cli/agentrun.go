package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crueber/weave/internal/agentprompt"
)

var agentRunFlags = append(append([]FlagSpec{}, listenFlags...),
	FlagSpec{Name: "done-list"}, FlagSpec{Name: "fail-list"}, FlagSpec{Name: "blocked-list"}, FlagSpec{Name: "no-claim", Bool: true},
	FlagSpec{Name: "comment-output", Bool: true}, FlagSpec{Name: "no-renew", Bool: true},
	FlagSpec{Name: "keep-claim", Bool: true}, FlagSpec{Name: "outbox", Bool: true}, FlagSpec{Name: "no-board-file", Bool: true},
	FlagSpec{Name: "max-attempts"}, FlagSpec{Name: "respect-wip", Bool: true}, FlagSpec{Name: "allow-loop", Bool: true},
)

// exit codes a handler script can use with `weave agent run`
const (
	// AgentSkipExit: the script declined the card; release it without a comment.
	AgentSkipExit = 11
	// AgentBlockedExit: (outbox mode) the work is blocked on a human; post comment.md and hand the card over.
	AgentBlockedExit = 3
)

// DefaultExecTimeout bounds one handler run unless --exec-timeout says otherwise (0 = unlimited), so a hung
// agent cannot hold a lease for ever.
const DefaultExecTimeout = 30 * time.Minute

// failMarker starts every failure comment; --max-attempts counts these on the card, so the count survives
// restarts and also stops two agents passing a card back and forth.
const failMarker = "agent run failed"

// wipRecheck is how often cards deferred by --respect-wip are retried (besides card.moved out of the list).
var wipRecheck = 60 * time.Second

const (
	maxOutboxComment    = 20000
	maxOutboxAttachment = 25 << 20
	maxOutboxFiles      = 10
)

func init() {
	registerSpec("agent run", agentRunFlags, cmdAgentRun)
}

// roleLists resolves a list name on every watched board and returns id -> board id.
func (a *App) roleLists(ctx context.Context, boards []string, name string) (map[string]string, error) {
	out := map[string]string{}
	var last error
	for _, b := range boards {
		id, err := a.Resolve(ctx, "list", name, Scope{BoardID: b})
		if err != nil {
			last = err
			continue
		}
		out[id] = b
	}
	if len(out) == 0 && last != nil {
		return nil, last
	}
	return out, nil
}

func touchesList(ev Event, lists []string) bool {
	for _, id := range []string{ev.S("listId"), ev.S("fromListId"), ev.S("toListId")} {
		if id != "" && slices.Contains(lists, id) {
			return true
		}
	}
	if c := ev.Card(); c != nil && slices.Contains(lists, str(c, "listId")) && ev.S("listId") == "" && ev.S("toListId") == "" {
		return true
	}
	return false
}

func cmdAgentRun(a *App, args []string) int {
	p, err := a.parse(args, agentRunFlags)
	if err != nil {
		return a.fail(err)
	}
	argv := p.Rest
	if len(argv) == 0 {
		if p.Has("exec") {
			argv = nil
		} else {
			return a.failUsage("weave agent run --board B --listen Ready [flags] -- COMMAND [args...]   (the command gets the event JSON on stdin and WEAVE_* env vars; with --outbox it gets only ids and file paths)")
		}
	}
	ctx0 := a.ctx
	cfg, err := a.resolveWatch(ctx0, splitMulti(p.All("board")), splitMulti(p.All("list")))
	if err != nil {
		return a.fail(err)
	}
	if len(cfg.lists) == 0 {
		return a.failUsage("agent run needs at least one --listen LIST")
	}
	cfg.types = splitMulti(p.All("type"))
	if len(cfg.types) == 0 {
		cfg.types = claimTypes()
	}
	if err := a.fillListenCfg(cfg, p); err != nil {
		return a.fail(err)
	}
	if cfg.label == "" {
		h, _ := os.Hostname()
		cfg.label = "agent@" + h
	}
	rt, err := parseRunnerTiming(p, 15*time.Minute, DefaultExecTimeout, 60*time.Second)
	if err != nil {
		return a.fail(err)
	}
	maxAttempts := 0
	if p.Has("max-attempts") {
		n, err := strconv.Atoi(strings.TrimSpace(p.Str("max-attempts")))
		if err != nil || n < 0 {
			return a.failUsage("--max-attempts needs a number >= 0")
		}
		maxAttempts = n
	}
	claimMove, doneList, failList, blockedList := p.Str("claim-move-to"), p.Str("done-list"), p.Str("fail-list"), p.Str("blocked-list")
	noClaim := p.Bool("no-claim")
	if maxAttempts > 0 && failList == "" && blockedList == "" {
		return a.failUsage("--max-attempts needs --fail-list (or --blocked-list): that is where a card goes after the last attempt")
	}
	// a listened column must not also be a destination, or every finished card would be picked up again
	if len(cfg.boards) > 0 {
		for role, name := range map[string]string{"--claim-move-to": claimMove, "--done-list": doneList, "--fail-list": failList, "--blocked-list": blockedList} {
			if name == "" {
				continue
			}
			ids, err := a.roleLists(ctx0, cfg.boards, name)
			if err != nil {
				return a.fail(err)
			}
			for id, b := range ids {
				if slices.Contains(cfg.lists, id) && !p.Bool("allow-loop") {
					return a.failUsage(fmt.Sprintf("%s %q is also a --listen column: finished cards would be picked up again in a loop (pass --allow-loop if you really mean it)", role, name))
				}
				if role == "--claim-move-to" {
					cfg.extraLists = append(cfg.extraLists, id)
					cfg.listBoard[id] = b
				}
			}
		}
	}
	spec := execSpec{argv: argv}
	if len(argv) == 0 {
		spec = execSpec{shell: p.Str("exec")}
	}
	outbox := p.Bool("outbox")

	lctx, cancel := context.WithCancel(ctx0)
	defer cancel()
	execCtx, execCancel := context.WithCancel(context.Background())
	defer execCancel()
	l := a.buildListener(cfg)
	sem := make(chan struct{}, rt.concurrency)
	var wg sync.WaitGroup
	var retryWg sync.WaitGroup
	var handled int
	var mu sync.Mutex
	var dispatchMu sync.Mutex
	once := p.Bool("once")
	inflight := map[string]bool{}
	deferred := map[string]Event{}
	respectWIP := p.Bool("respect-wip") && claimMove != ""
	claimO := claimOpts{ttl: rt.ttl, note: "claimed by weave agent run", moveTo: claimMove, respecWIP: respectWIP}

	retryCh := make(chan struct{}, 1)
	triggerRetry := func() {
		select {
		case retryCh <- struct{}{}:
		default:
		}
	}

	var handle func(ctx context.Context, ev Event, done func())
	retryDeferred := func(ctx context.Context) {
		mu.Lock()
		if len(deferred) == 0 {
			mu.Unlock()
			return
		}
		evs := make([]Event, 0, len(deferred))
		for _, e := range deferred {
			evs = append(evs, e)
		}
		deferred = map[string]Event{}
		mu.Unlock()
		for _, e := range evs {
			if ctx.Err() != nil {
				return
			}
			handle(ctx, e, func() {})
		}
	}

	handle = func(ctx context.Context, ev Event, done func()) {
		dispatchMu.Lock()
		defer dispatchMu.Unlock()
		if ctx.Err() != nil {
			done()
			return
		}
		mu.Lock()
		already := once && handled > 0
		mu.Unlock()
		if already { // --once: the first handled card is the only one
			done()
			return
		}
		// events from the claim-move-to list itself: only an expired lease (card.released) or the startup sweep
		// can strand a card there; everything else is people moving cards around and is not ours to claim.
		if len(cfg.extraLists) > 0 && !touchesList(ev, cfg.lists) {
			if ev.Type == "card.moved" && slices.Contains(cfg.extraLists, ev.S("fromListId")) {
				triggerRetry() // room may have opened up under the WIP limit
			}
			if ev.Type != "card.released" && ev.Type != "card.existing" {
				done()
				return
			}
		}
		select {
		case sem <- struct{}{}: // acquire BEFORE claiming so we never hold more leases than workers
		case <-ctx.Done():
			done()
			return
		}
		cid := ev.CardID()
		mu.Lock()
		if (once && handled > 0) || (cid != "" && inflight[cid]) {
			mu.Unlock()
			<-sem
			done()
			return
		}
		if cid != "" {
			inflight[cid] = true
		}
		mu.Unlock()
		if !noClaim {
			switch a.tryClaim(ctx, &ev, cfg, claimO) {
			case claimDeferred:
				mu.Lock()
				if cid != "" {
					delete(inflight, cid)
				}
				if len(deferred) < 100 {
					deferred[cid] = ev
				}
				mu.Unlock()
				<-sem
				done()
				return
			case claimNo:
				mu.Lock()
				if cid != "" {
					delete(inflight, cid)
				}
				mu.Unlock()
				<-sem
				done()
				return
			}
		}
		mu.Lock()
		handled++
		mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer done()
			defer func() {
				if cid != "" {
					mu.Lock()
					delete(inflight, cid)
					mu.Unlock()
				}
			}()
			a.agentWork(execCtx, ev, spec, rt.ttl, rt.execTimeout, workOpts{
				doneList: doneList, failList: failList, blockedList: blockedList, commentOutput: p.Bool("comment-output"),
				renew: !p.Bool("no-renew") && !noClaim, keepClaim: p.Bool("keep-claim") || noClaim,
				outbox: outbox, boardFile: !p.Bool("no-board-file"), maxAttempts: maxAttempts,
			})
			if respectWIP {
				triggerRetry()
			}
			if once {
				cancel()
			}
		}()
	}
	l.Handle = handle
	if respectWIP || len(cfg.extraLists) > 0 {
		retryWg.Add(1)
		go func() {
			defer retryWg.Done()
			var tickC <-chan time.Time
			if respectWIP {
				d := wipRecheck
				if d <= 0 {
					d = time.Second
				}
				t := time.NewTicker(d)
				defer t.Stop()
				tickC = t.C
			}
			for {
				select {
				case <-lctx.Done():
					return
				case <-retryCh:
					retryDeferred(lctx)
				case <-tickC:
					retryDeferred(lctx)
				}
			}
		}()
	}
	var timedOut bool
	if rt.waitFor > 0 {
		t := time.AfterFunc(rt.waitFor, func() { mu.Lock(); timedOut = true; mu.Unlock(); cancel() })
		defer t.Stop()
	}
	a.stderrf("agent %q listening: boards=%v lists=%v (claim=%v, ttl=%s, concurrency=%d, exec-timeout=%s)", cfg.label, cfg.boards, cfg.lists, !noClaim, rt.ttl, rt.concurrency, rt.execTimeout)
	err = l.Run(lctx)
	cancel()
	retryWg.Wait()
	a.drainWorkers(&wg, rt.grace, execCancel)
	if err != nil {
		return a.fail(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if timedOut && handled == 0 {
		return ExitEmpty
	}
	return 0
}

type workOpts struct {
	doneList, failList, blockedList string
	commentOutput, renew            bool
	keepClaim                       bool
	outbox, boardFile               bool
	maxAttempts                     int
}

type outcome int

const (
	outDone outcome = iota
	outBlocked
	outSkip
	outFail
	outCancelled // the runner is shutting down: give the card back quietly
)

// outbox is the per-card directory the runner prepares for a token-free handler.
type outbox struct {
	dir, out, card, board, order string
}

func (o *outbox) env(ev Event) []string {
	e := []string{"WEAVE_CARD_ID=" + ev.CardID(), "WEAVE_LIST_ID=" + ev.ListID(), "WEAVE_BOARD_ID=" + ev.BoardID(),
		"WEAVE_CARD_FILE=" + o.card, "WEAVE_WORKORDER_FILE=" + o.order, "WEAVE_OUT_DIR=" + o.out}
	if o.board != "" {
		e = append(e, "WEAVE_BOARD_FILE="+o.board)
	}
	return e
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// prepOutbox fetches the card (and board tree) into a private temp directory and writes the work order.
// The work order names file paths only; it never contains card text.
func (a *App) prepOutbox(ctx context.Context, ev Event, withBoard bool) (*outbox, error) {
	dir, err := os.MkdirTemp("", "weave-card-")
	if err != nil {
		return nil, err
	}
	ob := &outbox{dir: dir, out: filepath.Join(dir, "out"), card: filepath.Join(dir, "card.json"), order: filepath.Join(dir, "workorder.md")}
	fail := func(err error) (*outbox, error) { os.RemoveAll(dir); return nil, err }
	if err := os.MkdirAll(filepath.Join(ob.out, "attachments"), 0o700); err != nil {
		return fail(err)
	}
	r, err := a.Do(ctx, "card.get", Params{"id": ev.CardID(), "full": true})
	if err != nil {
		return fail(err)
	}
	if err := writeJSONFile(ob.card, itemOf(r.JSON())); err != nil {
		return fail(err)
	}
	if withBoard && ev.BoardID() != "" {
		if tree, err := a.boardTree(ctx, ev.BoardID()); err == nil {
			ob.board = filepath.Join(dir, "board.json")
			if err := writeJSONFile(ob.board, tree); err != nil {
				return fail(err)
			}
		}
	}
	// the brief is the column the card was picked up from
	listID := firstNonEmpty(ev.S("originListId"), ev.ListID())
	col := agentprompt.Column{ID: listID}
	if lr, err := a.Do(ctx, "list.get", Params{"id": listID}); err == nil {
		li := itemOf(lr.JSON())
		col.Name, col.Brief = str(li, "name"), str(li, "description")
	}
	text, err := agentprompt.Render(agentprompt.Data{Scope: agentprompt.ScopeWorkOrder, List: col, CardID: ev.CardID(),
		CardFile: ob.card, BoardFile: ob.board, WorkOrderFile: ob.order, OutDir: ob.out})
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(ob.order, []byte(text), 0o600); err != nil {
		return fail(err)
	}
	return ob, nil
}

// readRootRegular opens a regular file inside root without following symlinks and reads at most maxBytes.
func readRootRegular(root *os.Root, name string, maxBytes int64) ([]byte, bool) {
	st, err := root.Lstat(name)
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	f, err := root.OpenFile(name, os.O_RDONLY|openNoBlock|openNoFollow, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fst, err := f.Stat()
	if err != nil || !fst.Mode().IsRegular() || !os.SameFile(st, fst) {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return nil, false
	}
	return b, true
}

// readOutbox returns the agent's verdict, comment and attachment files.
func readOutbox(ob *outbox) (result, comment string, files []string) {
	root, err := os.OpenRoot(ob.out)
	if err != nil {
		return "", "", nil
	}
	defer root.Close()
	if b, ok := readRootRegular(root, "result", 256); ok {
		result = strings.ToLower(strings.TrimSpace(string(b)))
	}
	if b, ok := readRootRegular(root, "comment.md", maxOutboxComment*utf8.UTFMax+1); ok {
		comment = strings.TrimSpace(string(b))
	}
	if ents, err := os.ReadDir(filepath.Join(ob.out, "attachments")); err == nil {
		for _, e := range ents {
			if len(files) >= maxOutboxFiles {
				break
			}
			fi, err := root.Lstat(filepath.Join("attachments", e.Name()))
			if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxOutboxAttachment || fi.Size() == 0 {
				continue // symlinks, directories, empty or huge files are not uploaded
			}
			files = append(files, filepath.Join(ob.out, "attachments", e.Name()))
		}
	}
	return
}

// capComment limits a comment's size.
func capComment(s string) string {
	if utf8.RuneCountInString(s) > maxOutboxComment {
		s = string([]rune(s)[:maxOutboxComment]) + "\n\n[...truncated by weave agent run]"
	}
	return s
}

// priorFailures counts the failure comments already on the card (by anyone).
func (a *App) priorFailures(ctx context.Context, cardID string) int {
	items, err := a.Items(ctx, "comment.list", Params{"cardId": cardID, "limit": float64(200)})
	if err != nil {
		return 0
	}
	n := 0
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && strings.HasPrefix(strings.TrimSpace(str(m, "text")), failMarker) {
			n++
		}
	}
	return n
}

// agentWork runs the handler for one claimed card, keeps the lease alive, and reports back.
func (a *App) agentWork(ctx context.Context, ev Event, spec execSpec, ttl, execTimeout time.Duration, w workOpts) {
	cardID, boardID := ev.CardID(), ev.BoardID()
	workList, origin := ev.ListID(), ev.S("originListId")
	bg := context.Background()
	comment := func(text string) {
		if cardID == "" {
			return
		}
		if _, e := a.Do(bg, "comment.create", Params{"cardId": cardID, "text": redactSecrets(capComment(text), a.Cfg.Token)}); e != nil {
			a.stderrf("comment on %s failed: %v", cardID, e)
		}
	}
	release := func(toList string) {
		if w.keepClaim || cardID == "" {
			return
		}
		params := Params{"id": cardID}
		if toList != "" {
			params["listId"] = toList
		}
		if _, e := a.Do(bg, "card.release", params); e != nil {
			a.stderrf("release %s failed: %v", cardID, e)
		}
	}
	// A claim is a lease, not a lock: a person may have dragged the card elsewhere meanwhile. Only move it
	// on when it is still in the list it was in when work began.
	stillThere := func() bool {
		r, err := a.Do(bg, "card.get", Params{"id": cardID})
		if err != nil {
			a.stderrf("cannot re-read %s; leaving it where it is: %v", cardID, err)
			return false
		}
		if lid := str(itemOf(r.JSON()), "listId"); lid != workList {
			a.stderrf("%s was moved away from its work list while working; not moving it", cardID)
			comment("weave agent run: this card was moved to another column while I worked on it, so I left it there and only released my claim.")
			return false
		}
		return true
	}
	moveTo := func(name string) {
		if name == "" || cardID == "" || !stillThere() {
			return
		}
		if e := a.moveCardToList(bg, cardID, boardID, name); e != nil {
			a.stderrf("move %s to %q failed: %v", cardID, name, e)
		}
	}
	giveBack := func() { // skip / cancel: undo the claim-move so the card is not stranded
		if origin != "" && origin != workList && stillThere() {
			release(origin)
			return
		}
		release("")
	}

	if w.maxAttempts > 0 && cardID != "" {
		if n := a.priorFailures(bg, cardID); n >= w.maxAttempts {
			a.stderrf("%s has %d failed attempts (max %d); not running", cardID, n, w.maxAttempts)
			comment(fmt.Sprintf("weave agent run: giving up on this card after %d failed attempts (max %d). A person should look at it.", n, w.maxAttempts))
			moveTo(firstNonEmpty(w.failList, w.blockedList))
			release("")
			return
		}
	}

	stopRenew := make(chan struct{})
	if w.renew && cardID != "" && ttl > 0 {
		interval := max(ttl/3, time.Second)
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-stopRenew:
					return
				case <-t.C:
					if _, err := a.Do(bg, "card.renew", Params{"id": cardID, "ttlSeconds": float64(int(ttl.Seconds()))}); err != nil {
						a.stderrf("renew %s failed: %v", cardID, err)
					}
				}
			}
		}()
	}
	// the card name is only ever logged locally (never exported to the child in outbox mode)
	name := ""
	if c := ev.Card(); c != nil {
		name = str(c, "name")
	}
	a.stderrf("working on %s %q (%s)", cardID, name, ev.Type)

	var ob *outbox
	if w.outbox && cardID != "" {
		var err error
		if ob, err = a.prepOutbox(bg, ev, w.boardFile); err != nil {
			close(stopRenew)
			a.stderrf("cannot prepare the work directory for %s: %v", cardID, err)
			comment(failMarker + ": cannot prepare the work directory: " + err.Error())
			moveTo(w.failList)
			release("")
			return
		}
		defer os.RemoveAll(ob.dir)
		spec.clean = true
		spec.env = append(append([]string{}, spec.env...), ob.env(ev)...)
	}
	var stdout tailBuf
	tail := &tailBuf{}
	code, err := a.runChild(ctx, spec, ev, execTimeout, &stdout, tail)
	close(stopRenew)
	if cardID == "" {
		return
	}

	res, text, files := outFail, "", []string(nil)
	failMsg := fmt.Sprintf("%s (exit %d)", failMarker, code)
	if err != nil {
		failMsg = failMarker + ": " + err.Error()
	}
	switch {
	case ctx.Err() != nil: // the runner is shutting down and cancelled the child after the grace period
		res = outCancelled
	case err != nil:
	case code == AgentSkipExit:
		res = outSkip
	case ob != nil:
		var verdict string
		verdict, text, files = readOutbox(ob)
		switch {
		case code == AgentBlockedExit:
			res = outBlocked
		case code == 0 && verdict == "done":
			res = outDone
		case code == 0 && verdict == "blocked":
			res = outBlocked
		case code == 0 && verdict == "skip":
			res = outSkip
		case code == 0:
			failMsg = failMarker + ": the handler exited 0 but wrote no valid result file (done, blocked or skip)"
		}
	case code == 0:
		res = outDone
		if w.commentOutput {
			text = stdout.String()
		}
	}

	switch res {
	case outCancelled:
		a.stderrf("cancelled %s by shutdown; giving the card back", cardID)
		giveBack()
	case outSkip:
		a.stderrf("handler declined %s; releasing", cardID)
		giveBack()
	case outDone, outBlocked:
		if res == outDone {
			a.stderrf("done %s", cardID)
		} else {
			a.stderrf("blocked %s", cardID)
		}
		if text != "" {
			comment(text)
		}
		a.uploadOutbox(bg, cardID, files)
		if res == outDone {
			moveTo(w.doneList)
		} else {
			moveTo(firstNonEmpty(w.blockedList, w.failList))
		}
		release("")
	default:
		a.stderrf("%s on %s", failMsg, cardID)
		out := failMsg
		if t := strings.TrimSpace(sanitizeTail(tail.String(), a.Cfg.Token)); t != "" {
			out += "\n\n```\n" + t + "\n```"
		}
		comment(out)
		moveTo(w.failList)
		release("")
	}
}

// uploadOutbox attaches the files the agent left in out/attachments.
func (a *App) uploadOutbox(ctx context.Context, cardID string, files []string) {
	if len(files) == 0 {
		return
	}
	op, err := a.mustOp(ctx, "attachment.create")
	if err != nil {
		a.stderrf("cannot attach files: %v", err)
		return
	}
	for _, f := range files {
		if _, err := a.Call(ctx, op, Params{"cardId": cardID}, map[string]string{"file": f}); err != nil {
			a.stderrf("attach %s to %s failed: %v", filepath.Base(f), cardID, err)
		}
	}
}
