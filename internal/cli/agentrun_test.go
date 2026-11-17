package cli

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func needSh(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs sh")
	}
}

// common stubs for the runner tests (lists: l1 Ready, l2 In progress, l3 Review, l4 Needs info)
func runnerFake(t *testing.T, initial map[string]string) (*fake, *cardState) {
	f := newFake(t)
	f.json("POST /api/v1/cards/{cardId}/comments", map[string]any{"item": map[string]any{"id": "m"}})
	return f, f.trackCards(initial)
}

func TestRunnerStaleEventDoesNotClaimOrDragTheCard(t *testing.T) {
	// the event says "card.moved into Ready" but the card has long since moved on to Review
	f, _ := runnerFake(t, map[string]string{"c-e1": "l3"})
	cliSSE(f, ev("e1", "card.moved", nil))
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--claim-move-to", "In progress", "--timeout", "600ms", "--no-cursor", "--", "true")
	if code != ExitEmpty {
		t.Fatalf("%d %s", code, errs)
	}
	if f.called("POST /api/v1/cards/c-e1/claim") != 0 || f.called("/move") != 0 {
		t.Fatalf("a stale event must not claim or move: %v", f.calls)
	}
}

func TestRunnerClaimNeverUsesTheEventListAsTarget(t *testing.T) {
	f, cs := runnerFake(t, nil)
	cliSSE(f, ev("e1", "card.created", map[string]any{"listId": "l5", "toListId": "l5"})) // event claims the card is in Backlog
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--listen", "Backlog", "--once", "--no-cursor", "--timeout", "3s", "--", "true")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if b := f.body("POST /api/v1/cards/c-e1/claim"); strings.Contains(b, "listId") {
		t.Fatalf("claim must not carry a list (that would MOVE the card): %s", b)
	}
	if cs.listOf("c-e1") != "l1" {
		t.Fatalf("card dragged to %s", cs.listOf("c-e1"))
	}
}

// waitFor polls cond.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestRunnerCardMovedByAHumanIsNotMovedAgain(t *testing.T) {
	needSh(t)
	f, cs := runnerFake(t, nil)
	cliSSE(f, ev("e1", "card.created", nil))
	dir := t.TempDir()
	started, proceed := filepath.Join(dir, "started"), filepath.Join(dir, "go")
	go func() {
		waitFor(t, 5*time.Second, func() bool { _, err := os.Stat(started); return err == nil })
		cs.set("c-e1", "l4") // a person drags the card to Needs info
		os.WriteFile(proceed, nil, 0o600)
	}()
	script := fmt.Sprintf(`touch %s; while [ ! -f %s ]; do sleep 0.02; done; exit 0`, started, proceed)
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--claim-move-to", "In progress", "--done-list", "Review",
		"--once", "--no-cursor", "--timeout", "5s", "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	for _, m := range f.posts["POST /api/v1/cards/c-e1/move"] {
		t.Errorf("the runner must not move a card a person moved: %s", m)
	}
	if cs.listOf("c-e1") != "l4" || f.called("POST /api/v1/cards/c-e1/release") != 1 {
		t.Fatalf("want released in place: list=%s %v", cs.listOf("c-e1"), f.calls)
	}
	if !strings.Contains(f.body("POST /api/v1/cards/c-e1/comments"), "moved to another column") {
		t.Errorf("expected an explanatory comment: %v", f.calls)
	}
}

func TestRunnerShutdownCancelReleasesQuietlyAndMovesBack(t *testing.T) {
	needSh(t)
	f, cs := runnerFake(t, nil)
	cliSSE(f, ev("e1", "card.created", nil))
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--claim-move-to", "In progress", "--fail-list", "Needs info",
		"--timeout", "700ms", "--grace", "200ms", "--no-cursor", "--", "sh", "-c", "sleep 30")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if f.called("POST /api/v1/cards/c-e1/comments") != 0 {
		t.Errorf("a shutdown is not a failure: %s", f.body("POST /api/v1/cards/c-e1/comments"))
	}
	if cs.listOf("c-e1") != "l1" || !strings.Contains(f.body("POST /api/v1/cards/c-e1/release"), `"listId":"l1"`) {
		t.Fatalf("card should go back to Ready: list=%s release=%s", cs.listOf("c-e1"), f.body("POST /api/v1/cards/c-e1/release"))
	}
}

func TestRunnerStrandedCardIsRepickedAfterLeaseExpiry(t *testing.T) {
	f, cs := runnerFake(t, map[string]string{"c-e1": "l2", "c-e2": "l2"})
	// e1: lease expired in In progress (card.released, no actor); e2: a person moved a card into In progress
	cliSSE(f,
		ev("e1", "card.released", map[string]any{"listId": "l2", "toListId": "l2", "actor": nil, "card": map[string]any{"id": "c-e1", "listId": "l2"}}),
		ev("e2", "card.moved", map[string]any{"listId": "l2", "toListId": "l2", "fromListId": "l3", "card": map[string]any{"id": "c-e2", "listId": "l2"}}))
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--claim-move-to", "In progress", "--done-list", "Review",
		"--timeout", "800ms", "--no-cursor", "--", "true")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if f.called("POST /api/v1/cards/c-e1/claim") != 1 {
		t.Errorf("the stranded card should be re-picked: %v", f.calls)
	}
	if f.called("POST /api/v1/cards/c-e2/claim") != 0 {
		t.Errorf("a person's move into the claim list is not ours to claim")
	}
	if cs.listOf("c-e1") != "l3" {
		t.Errorf("handled card should end in Review, is %s", cs.listOf("c-e1"))
	}
}

func TestRunnerStartupSweepOfClaimListWithIncludeExisting(t *testing.T) {
	f, _ := runnerFake(t, map[string]string{"k1": "l2"})
	f.handleFunc("GET /api/v1/boards/{boardId}/cards", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("listId") == "l2" {
			w.Write([]byte(`{"items":[{"id":"k1","name":"Stuck","listId":"l2"}]}`))
			return
		}
		w.Write([]byte(`{"items":[]}`))
	})
	cliSSE(f)
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--claim-move-to", "In progress", "--include-existing",
		"--timeout", "800ms", "--no-cursor", "--", "true")
	if code != 0 {
		t.Fatalf("%d %s %v", code, errs, f.calls)
	}
	if f.called("POST /api/v1/cards/k1/claim") != 1 {
		t.Fatalf("startup sweep should re-pick the stranded card: %v %s", f.calls, errs)
	}
}

func TestRunnerOutboxModeIsTokenFreeAndReportsFromTheOutbox(t *testing.T) {
	needSh(t)
	f, cs := runnerFake(t, nil)
	f.json("GET /api/v1/lists/{id}", map[string]any{"item": map[string]any{"id": "l1", "name": "Ready", "description": "Implement it."}})
	var uploads atomic.Int32
	f.handleFunc("POST /api/v1/cards/{cardId}/attachments", func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"item":{"id":"a1"}}`))
	})
	cliSSE(f, ev("e1", "card.created", nil))
	dir := t.TempDir()
	envDump, stdinDump, orderDump := filepath.Join(dir, "env"), filepath.Join(dir, "stdin"), filepath.Join(dir, "order")
	script := fmt.Sprintf(`env > %s; cat > %s; cat "$WEAVE_WORKORDER_FILE" > %s; test -s "$WEAVE_CARD_FILE" || exit 9
echo done > "$WEAVE_OUT_DIR/result"; printf 'All done. token wk_leakleakleak1 here' > "$WEAVE_OUT_DIR/comment.md"; echo data > "$WEAVE_OUT_DIR/attachments/r.txt"`, envDump, stdinDump, orderDump)
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--claim-move-to", "In progress", "--done-list", "Review",
		"--outbox", "--once", "--no-cursor", "--timeout", "5s", "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	envs, _ := os.ReadFile(envDump)
	for _, bad := range []string{"WEAVE_TOKEN", "wk_test", "WEAVE_URL", "WEAVE_PROFILE", "WEAVE_CARD_NAME", "Card e1"} {
		if strings.Contains(string(envs), bad) {
			t.Errorf("outbox env leaks %q", bad)
		}
	}
	for _, want := range []string{"WEAVE_CARD_ID=c-e1", "WEAVE_CARD_FILE=", "WEAVE_WORKORDER_FILE=", "WEAVE_OUT_DIR=", "WEAVE_LIST_ID=", "WEAVE_BOARD_ID=b1"} {
		if !strings.Contains(string(envs), want) {
			t.Errorf("outbox env lacks %q", want)
		}
	}
	if b, _ := os.ReadFile(stdinDump); len(b) != 0 {
		t.Errorf("stdin must be empty in outbox mode, got %q", b)
	}
	order, _ := os.ReadFile(orderDump)
	if !strings.Contains(string(order), "Implement it.") || strings.Contains(string(order), "Card e1") {
		t.Errorf("work order: brief yes, card text no:\n%s", order)
	}
	c := f.body("POST /api/v1/cards/c-e1/comments")
	if !strings.Contains(c, "All done.") || strings.Contains(c, "wk_leakleakleak1") {
		t.Errorf("comment must be posted with tokens redacted: %s", c)
	}
	if uploads.Load() != 1 || cs.listOf("c-e1") != "l3" || f.called("POST /api/v1/cards/c-e1/release") != 1 {
		t.Errorf("uploads=%d list=%s %v", uploads.Load(), cs.listOf("c-e1"), f.calls)
	}
}

func TestRunnerOutboxBlockedGoesToBlockedList(t *testing.T) {
	needSh(t)
	f, cs := runnerFake(t, nil)
	cliSSE(f, ev("e1", "card.created", nil))
	script := `echo blocked > "$WEAVE_OUT_DIR/result"; echo "Which database?" > "$WEAVE_OUT_DIR/comment.md"`
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--blocked-list", "Needs info", "--done-list", "Review",
		"--outbox", "--once", "--no-cursor", "--timeout", "5s", "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if cs.listOf("c-e1") != "l4" || !strings.Contains(f.body("POST /api/v1/cards/c-e1/comments"), "Which database?") {
		t.Fatalf("blocked: list=%s %v", cs.listOf("c-e1"), f.calls)
	}
	// no result file and exit 0 is a failure, not a silent success
	f2, cs2 := runnerFake(t, nil)
	cliSSE(f2, ev("e1", "card.created", nil))
	code, _, _ = f2.run("agent", "run", "--board", "Product", "--listen", "Ready", "--fail-list", "Needs info", "--done-list", "Review",
		"--outbox", "--once", "--no-cursor", "--timeout", "5s", "--", "sh", "-c", "true")
	if code != 0 || cs2.listOf("c-e1") != "l4" || !strings.Contains(f2.body("POST /api/v1/cards/c-e1/comments"), failMarker) {
		t.Fatalf("missing result must fail: list=%s %v", cs2.listOf("c-e1"), f2.calls)
	}
}

func TestRunnerDefaultExecTimeoutIsNonZero(t *testing.T) {
	f, _ := runnerFake(t, nil)
	cliSSE(f)
	_, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--timeout", "300ms", "--no-cursor", "--", "true")
	if !strings.Contains(errs, "exec-timeout=30m0s") {
		t.Fatalf("default exec timeout should be 30m: %s", errs)
	}
	f, _ = runnerFake(t, nil)
	cliSSE(f)
	_, _, errs = f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--exec-timeout", "0", "--timeout", "300ms", "--no-cursor", "--", "true")
	if !strings.Contains(errs, "exec-timeout=0s") {
		t.Fatalf("0 turns the timeout off explicitly: %s", errs)
	}
}

func TestRunnerExecTimeoutKillsAHungHandler(t *testing.T) {
	needSh(t)
	f, _ := runnerFake(t, nil)
	cliSSE(f, ev("e1", "card.created", nil))
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--exec-timeout", "300ms", "--fail-list", "Needs info",
		"--once", "--no-cursor", "--timeout", "5s", "--", "sh", "-c", "sleep 30")
	if code != 0 || !strings.Contains(f.body("POST /api/v1/cards/c-e1/comments"), failMarker) {
		t.Fatalf("%d %s %v", code, errs, f.calls)
	}
}

func TestRunnerMaxAttemptsCountsFailureCommentsOnTheCard(t *testing.T) {
	needSh(t)
	f, cs := runnerFake(t, nil)
	f.json("GET /api/v1/cards/{cardId}/comments", map[string]any{"items": []any{
		map[string]any{"id": "1", "text": "agent run failed (exit 1)", "userId": "other-agent"},
		map[string]any{"id": "2", "text": "agent run failed: boom", "userId": "u1"},
		map[string]any{"id": "3", "text": "thanks!", "userId": "u9"}}})
	cliSSE(f, ev("e1", "card.created", nil))
	flag := filepath.Join(t.TempDir(), "ran")
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--fail-list", "Needs info", "--max-attempts", "2",
		"--once", "--no-cursor", "--timeout", "5s", "--", "sh", "-c", "touch "+flag)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if _, err := os.Stat(flag); err == nil {
		t.Fatal("the handler ran although the attempts were used up")
	}
	if cs.listOf("c-e1") != "l4" || !strings.Contains(f.body("POST /api/v1/cards/c-e1/comments"), "giving up") {
		t.Fatalf("list=%s %v", cs.listOf("c-e1"), f.calls)
	}
	if code, _, _ := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--max-attempts", "2", "--", "true"); code != ExitUsage {
		t.Errorf("--max-attempts without a fail list is a usage error: %d", code)
	}
}

func TestRunnerRespectWIPDefersThenClaims(t *testing.T) {
	old := wipRecheck
	wipRecheck = 40 * time.Millisecond
	t.Cleanup(func() { wipRecheck = old })
	f, _ := runnerFake(t, nil)
	var full atomic.Bool
	full.Store(true)
	f.handleFunc("GET /api/v1/lists/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"item":{"id":"l2","name":"In progress","wipLimit":1}}`))
	})
	f.handleFunc("GET /api/v1/boards/{boardId}/cards", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if full.Load() {
			w.Write([]byte(`{"items":[{"id":"x","name":"busy","listId":"l2"}]}`))
		} else {
			w.Write([]byte(`{"items":[]}`))
		}
	})
	cliSSE(f, ev("e1", "card.created", nil))
	go func() {
		time.Sleep(400 * time.Millisecond)
		if f.called("POST /api/v1/cards/c-e1/claim") != 0 {
			t.Error("claimed while the target list was full")
		}
		full.Store(false)
	}()
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--claim-move-to", "In progress", "--respect-wip",
		"--once", "--no-cursor", "--timeout", "5s", "--", "true")
	if code != 0 || f.called("POST /api/v1/cards/c-e1/claim") != 1 {
		t.Fatalf("%d %s %v", code, errs, f.calls)
	}
}

func TestRunnerUsageErrorWhenListenedColumnIsADestination(t *testing.T) {
	f := newFake(t)
	for _, flag := range []string{"--done-list", "--fail-list", "--blocked-list", "--claim-move-to"} {
		if code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", flag, "Ready", "--", "true"); code != ExitUsage || !strings.Contains(errs, "loop") {
			t.Errorf("%s: want usage error naming the loop, got %d %s", flag, code, errs)
		}
	}
}

// The real server wraps struct list outputs as {"item":{"items":[...]}}; --include-existing and the
// stranded-card sweep must read that shape too.
func TestItemsOfUnwrapsItemItems(t *testing.T) {
	v := map[string]any{"item": map[string]any{"items": []any{map[string]any{"id": "a"}}, "nextCursor": "c2"}}
	if got := itemsOf(v); len(got) != 1 {
		t.Fatalf("items: %v", got)
	}
	if nextCursorOf(v) != "c2" {
		t.Fatal("cursor not unwrapped")
	}
	if got := itemsOf(map[string]any{"item": map[string]any{"id": "x"}}); got != nil {
		t.Fatalf("a plain item is not a list: %v", got)
	}
}
