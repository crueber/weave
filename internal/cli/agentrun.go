package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

var agentRunFlags = append(append([]FlagSpec{}, listenFlags...),
	FlagSpec{Name: "done-list"}, FlagSpec{Name: "fail-list"}, FlagSpec{Name: "no-claim", Bool: true},
	FlagSpec{Name: "comment-output", Bool: true}, FlagSpec{Name: "no-renew", Bool: true},
	FlagSpec{Name: "keep-claim", Bool: true},
)

// exit codes a handler script can use with `weave agent run`
const (
	// AgentSkipExit: the script declined the card; release it without a comment.
	AgentSkipExit = 11
)

func init() {
	registerSpec("agent run", agentRunFlags, cmdAgentRun)
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
			return a.failUsage("weave agent run --board B --listen Ready [flags] -- COMMAND [args...]   (the command gets the event JSON on stdin and WEAVE_* env vars)")
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
	ttl := 15 * time.Minute
	if p.Has("ttl") {
		if ttl, err = parseDur(p.Str("ttl")); err != nil {
			return a.fail(err)
		}
	}
	grace := 60 * time.Second
	if p.Has("grace") {
		grace, _ = parseDur(p.Str("grace"))
	}
	execTimeout, _ := parseDur(p.Str("exec-timeout"))
	conc := 1
	if p.Has("concurrency") {
		fmt.Sscan(p.Str("concurrency"), &conc)
		if conc < 1 {
			conc = 1
		}
	}
	claimMove, doneList, failList := p.Str("claim-move-to"), p.Str("done-list"), p.Str("fail-list")
	noClaim := p.Bool("no-claim")
	spec := execSpec{argv: argv}
	if len(argv) == 0 {
		spec = execSpec{shell: p.Str("exec")}
	}
	var waitFor time.Duration
	if p.Has("timeout") {
		waitFor, _ = parseDur(p.Str("timeout"))
	}

	lctx, cancel := context.WithCancel(ctx0)
	defer cancel()
	execCtx, execCancel := context.WithCancel(context.Background())
	defer execCancel()
	l := a.buildListener(cfg)
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var handled int
	var mu sync.Mutex
	once := p.Bool("once")

	l.Handle = func(ctx context.Context, ev Event, done func()) {
		mu.Lock()
		already := once && handled > 0
		mu.Unlock()
		if already { // --once: the first handled card is the only one
			done()
			return
		}
		select {
		case sem <- struct{}{}: // acquire BEFORE claiming so we never hold more leases than workers
		case <-ctx.Done():
			done()
			return
		}
		if !noClaim {
			if !a.tryClaim(ctx, &ev, cfg, ttl, "claimed by weave agent run", claimMove) {
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
			a.agentWork(execCtx, ev, spec, ttl, execTimeout, workOpts{
				doneList: doneList, failList: failList, commentOutput: p.Bool("comment-output"),
				renew: !p.Bool("no-renew") && !noClaim, keepClaim: p.Bool("keep-claim") || noClaim,
			})
			if once {
				cancel()
			}
		}()
	}
	var timedOut bool
	if waitFor > 0 {
		t := time.AfterFunc(waitFor, func() { mu.Lock(); timedOut = true; mu.Unlock(); cancel() })
		defer t.Stop()
	}
	a.stderrf("agent %q listening: boards=%v lists=%v (claim=%v, ttl=%s, concurrency=%d)", cfg.label, cfg.boards, cfg.lists, !noClaim, ttl, conc)
	err = l.Run(lctx)
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
	mu.Lock()
	defer mu.Unlock()
	if timedOut && handled == 0 {
		return ExitEmpty
	}
	return 0
}

type workOpts struct {
	doneList, failList   string
	commentOutput, renew bool
	keepClaim            bool
}

// agentWork runs the handler for one claimed card, keeps the lease alive, and reports back.
func (a *App) agentWork(ctx context.Context, ev Event, spec execSpec, ttl, execTimeout time.Duration, w workOpts) {
	cardID, boardID := ev.CardID(), ev.BoardID()
	bg := context.Background()
	stopRenew := make(chan struct{})
	if w.renew && cardID != "" && ttl > 0 {
		go func() {
			t := time.NewTicker(ttl / 3)
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
	name := ""
	if c := ev.Card(); c != nil {
		name = str(c, "name")
	}
	a.stderrf("working on %s %q (%s)", cardID, name, ev.Type)
	var stdout tailBuf
	tail := &tailBuf{}
	code, err := a.runChild(ctx, spec, ev, execTimeout, &stdout, tail)
	close(stopRenew)
	if cardID == "" {
		return
	}
	comment := func(text string) {
		if _, e := a.Do(bg, "comment.create", Params{"cardId": cardID, "text": text}); e != nil {
			a.stderrf("comment on %s failed: %v", cardID, e)
		}
	}
	release := func() {
		if w.keepClaim {
			return
		}
		if _, e := a.Do(bg, "card.release", Params{"id": cardID}); e != nil {
			a.stderrf("release %s failed: %v", cardID, e)
		}
	}
	switch {
	case err == nil && code == 0:
		a.stderrf("done %s", cardID)
		if w.commentOutput {
			if out := stdout.String(); out != "" {
				comment(out)
			}
		}
		if w.doneList != "" {
			if e := a.moveCardToList(bg, cardID, boardID, w.doneList); e != nil {
				a.stderrf("move %s to %q failed: %v", cardID, w.doneList, e)
			}
		}
		release()
	case err == nil && code == AgentSkipExit:
		a.stderrf("handler declined %s; releasing", cardID)
		release()
	default:
		msg := fmt.Sprintf("agent run failed (exit %d)", code)
		if err != nil {
			msg = "agent run failed: " + err.Error()
		}
		a.stderrf("%s on %s", msg, cardID)
		text := msg
		if t := strings.TrimSpace(sanitizeTail(tail.String(), a.Cfg.Token)); t != "" {
			text += "\n\n```\n" + t + "\n```"
		}
		comment(redactSecrets(text, a.Cfg.Token))
		if w.failList != "" {
			if e := a.moveCardToList(bg, cardID, boardID, w.failList); e != nil {
				a.stderrf("move %s to %q failed: %v", cardID, w.failList, e)
			}
		}
		release()
	}
}
