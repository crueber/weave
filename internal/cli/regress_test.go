package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentRunConcurrentWorkersRace(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs sh")
	}
	f := newFake(t)
	for _, p := range []string{"claim", "release", "move"} {
		f.json("POST /api/v1/cards/{id}/"+p, map[string]any{"item": map[string]any{}})
	}
	f.json("POST /api/v1/cards/{cardId}/comments", map[string]any{"item": map[string]any{}})
	cliSSE(f, ev("e1", "card.created", nil), ev("e2", "card.created", nil), ev("e3", "card.created", nil), ev("e4", "card.created", nil))
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--done-list", "Review",
		"--concurrency", "3", "--timeout", "1500ms", "--no-cursor", "--", "sh", "-c", "sleep 0.1")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if f.called("POST /api/v1/cards/c-e4/release") != 1 {
		t.Fatalf("all four cards should be handled: %v", f.calls)
	}
}

func TestOnceClaimTakesOnlyOneAndSyntheticTypeNotSent(t *testing.T) {
	f := newFake(t)
	f.json("POST /api/v1/cards/{id}/claim", map[string]any{"item": map[string]any{}})
	conns := f.sse(func(c *sseConn) {
		c.ready()
		c.event("e1", "card.created", nil)
		c.event("e2", "card.created", nil)
		c.event("e3", "card.created", nil)
		c.hold()
	})
	code, _, errs := f.run("listen", "--board", "Product", "--list", "Ready", "--claim", "--once", "--no-cursor")
	if code != 0 {
		t.Fatal(errs)
	}
	if n := f.called("/claim"); n != 1 {
		t.Fatalf("exactly one claim expected, got %d", n)
	}
	if q := (*conns)[0].query; strings.Contains(q, "card.existing") {
		t.Fatalf("synthetic type leaked to the server: %s", q)
	}
}

func TestShortcutsAcceptAllOpFields(t *testing.T) {
	f := newFake(t)
	cat := newCatalog("snapshot", SnapshotOps())
	for _, key := range []string{"card create", "card move", "card claim", "card release", "card renew"} {
		tag, verb, _ := strings.Cut(key, " ")
		o := cat.Lookup(tag, verb)
		if o == nil {
			continue
		}
		for _, fl := range o.Fields {
			if fl.In == "path" || fl.In == "file" {
				continue
			}
			_, _, errs := f.run(tag, verb, "c1", "--"+fl.Name, "1")
			if strings.Contains(errs, "unknown flag") {
				t.Errorf("%s --%s rejected: %s", key, fl.Name, errs)
			}
		}
	}
}

func TestIncludeExistingCommitsResumePoint(t *testing.T) {
	f := newFake(t)
	f.sse(func(c *sseConn) {
		c.send("", "ready", `{"resumeFrom":"000000000007"}`)
		c.hold()
	})
	cf := filepath.Join(t.TempDir(), "c")
	l := newTestListener(f, testApp(f))
	l.CursorFile = cf
	l.Existing = func(context.Context) ([]Event, error) {
		return []Event{{Type: "card.existing", M: map[string]any{}, Raw: []byte(`{}`)}}, nil
	}
	collect(t, l, 1, 5*time.Second)
	time.Sleep(50 * time.Millisecond)
	if loadCursor(cf) != "000000000007" {
		t.Fatalf("cursor %q", loadCursor(cf))
	}
}
