package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCardCreateWithLabelsAssignAndBulk(t *testing.T) {
	f := newFake(t)
	f.json("GET /api/v1/boards/{boardId}/labels", map[string]any{"items": []any{map[string]any{"id": "lb1", "name": "bug"}, map[string]any{"id": "lb2", "name": "feature"}}})
	f.json("GET /api/v1/users", map[string]any{"items": []any{map[string]any{"id": "u7", "name": "Ada Lovelace", "username": "ada", "email": "ada@x.io"}}})
	f.json("POST /api/v1/lists/{listId}/cards", map[string]any{"item": map[string]any{"id": "c1", "name": "T"}})
	f.json("POST /api/v1/cards/{id}/labels", map[string]any{"item": map[string]any{}})
	f.json("POST /api/v1/cards/{id}/members", map[string]any{"item": map[string]any{}})
	code, out, errs := f.run("card", "create", "--board", "Product", "--list", "Ready", "--name", "T", "--desc", "d", "--label", "bug", "--assign", "ada", "--assign", "me")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	if !strings.Contains(f.body("POST /api/v1/cards/c1/labels"), `"labelId":"lb1"`) {
		t.Fatalf("labels: %v", f.calls)
	}
	if f.called("POST /api/v1/cards/c1/members") != 2 || !strings.Contains(f.body("POST /api/v1/cards/c1/members"), `"userId":"u1"`) {
		t.Fatalf("members: %v", f.calls)
	}
	// bulk from stdin: NDJSON and bare lines, default list/label applied
	stdin := `{"name":"One","labels":["feature"],"description":"x"}` + "\nplain title\n"
	code, out, errs = f.runIn(stdin, "card", "create", "--board", "Product", "--list", "Backlog", "--from-stdin", "--label", "bug")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n != 2 {
		t.Fatalf("want 2 output lines, got %q", out)
	}
	if f.called("POST /api/v1/lists/l5/cards") != 2 {
		t.Fatalf("calls %v", f.calls)
	}
	// dry run creates nothing beyond reads
	n := f.called("POST /api/v1/lists/")
	f.run("card", "create", "--board", "Product", "--list", "Ready", "--name", "Z", "--dry-run")
	if f.called("POST /api/v1/lists/") != n {
		t.Fatal("dry-run sent a POST")
	}
}

func TestCardMoveCommentAttachClaimDone(t *testing.T) {
	f := newFake(t)
	f.json("GET /api/v1/cards/{id}", map[string]any{"item": map[string]any{"id": "c1", "boardId": "b1", "name": "X"}})
	f.json("POST /api/v1/cards/{id}/move", map[string]any{"item": map[string]any{"id": "c1"}})
	f.json("POST /api/v1/cards/{cardId}/comments", map[string]any{"item": map[string]any{"id": "m1"}})
	f.json("POST /api/v1/cards/{cardId}/attachments", map[string]any{"item": map[string]any{"id": "a1"}})
	f.json("POST /api/v1/cards/{id}/release", map[string]any{"item": map[string]any{}})
	// move derives the board from the card
	if code, _, errs := f.run("card", "move", "c1", "--to-list", "Review"); code != 0 || !strings.Contains(f.body("POST /api/v1/cards/c1/move"), `"listId":"l3"`) {
		t.Fatalf("move %d %s %v", code, errs, f.calls)
	}
	// comment: positional, stdin
	f.run("card", "comment", "c1", "hello", "world")
	if !strings.Contains(f.body("POST /api/v1/cards/c1/comments"), `"text":"hello world"`) {
		t.Fatalf("comment body %s", f.body("POST /api/v1/cards/c1/comments"))
	}
	f.runIn("from stdin\nline2\n", "card", "comment", "c1", "--stdin")
	if !strings.Contains(f.body("POST /api/v1/cards/c1/comments"), "from stdin\\nline2") {
		t.Fatalf("stdin comment: %s", f.body("POST /api/v1/cards/c1/comments"))
	}
	if code, _, _ := f.run("card", "comment", "c1"); code != ExitUsage {
		// no text and a non-tty empty stdin
		t.Fatalf("empty comment should be usage error, got %d", code)
	}
	// attach uploads multipart
	file := filepath.Join(t.TempDir(), "report.txt")
	os.WriteFile(file, []byte("data"), 0o644)
	if code, _, errs := f.run("card", "attach", "c1", file); code != 0 || !strings.Contains(f.body("POST /api/v1/cards/c1/attachments"), "report.txt") {
		t.Fatalf("attach %d %s", code, errs)
	}
	// done: comment + move + release
	code, out, errs := f.run("card", "done", "c1", "--note", "all good", "--to-list", "Review")
	if code != 0 || !strings.Contains(out, `"release"`) || !strings.Contains(out, `"move"`) {
		t.Fatalf("done %d %s %s", code, out, errs)
	}
	// claim conflict -> exit 7
	f.status("POST /api/v1/cards/{id}/claim", 409, "conflict", "already claimed")
	if code, _, _ := f.run("card", "claim", "c1", "--ttl", "10m"); code != ExitConflict {
		t.Fatalf("claim conflict: %d", code)
	}
	f.json("POST /api/v1/cards/{id}/claim", map[string]any{"item": map[string]any{"claimedUntil": "x"}})
	if code, _, _ := f.run("card", "claim", "c1", "--ttl", "10m", "--note", "triage"); code != 0 {
		t.Fatal("claim")
	}
	if b := f.body("POST /api/v1/cards/c1/claim"); !strings.Contains(b, `"ttlSeconds":600`) || !strings.Contains(b, "triage") {
		t.Fatalf("claim body %s", b)
	}
}

func TestTaskCommands(t *testing.T) {
	f := newFake(t)
	f.json("GET /api/v1/cards/{cardId}/task-lists", map[string]any{"items": []any{map[string]any{"id": "tl1", "name": "Checklist", "tasks": []any{
		map[string]any{"id": "t1", "name": "write tests"}, map[string]any{"id": "t2", "name": "write docs"}}}}})
	f.json("POST /api/v1/task-lists/{taskListId}/tasks", map[string]any{"item": map[string]any{"id": "t3"}})
	f.json("PATCH /api/v1/tasks/{id}", map[string]any{"item": map[string]any{"id": "t1", "isCompleted": true}})
	if code, _, errs := f.run("task", "add", "c1", "ship it"); code != 0 || !strings.Contains(f.body("POST /api/v1/task-lists/tl1/tasks"), "ship it") {
		t.Fatalf("add %d %s", code, errs)
	}
	if code, _, errs := f.run("task", "check", "--card", "c1", "--n", "2"); code != 0 || !strings.Contains(f.body("PATCH /api/v1/tasks/t2"), `"isCompleted":true`) {
		t.Fatalf("check %d %s", code, errs)
	}
	if code, _, _ := f.run("task", "uncheck", "--card", "c1", "--text", "write t"); code != 0 || !strings.Contains(f.body("PATCH /api/v1/tasks/t1"), `"isCompleted":false`) {
		t.Fatalf("uncheck: %s", f.body("PATCH /api/v1/tasks/t1"))
	}
	if code, _, errs := f.run("task", "check", "--card", "c1", "--text", "write"); code != ExitUsage || !strings.Contains(errs, "ambiguous") {
		t.Fatalf("ambiguous text: %d %s", code, errs)
	}
	if code, _, _ := f.run("task", "check", "--card", "c1", "--n", "9"); code != ExitNotFound {
		t.Fatalf("range: %d", code)
	}
}

func TestInboxStatsAndTreeFallback(t *testing.T) {
	f := newFake(t)
	cards := map[string]any{"items": []any{
		map[string]any{"id": "c1", "name": "mine", "listId": "l1", "members": []any{map[string]any{"id": "u1"}}},
		map[string]any{"id": "c2", "name": "claimed", "listId": "l1", "claimedBy": "u1", "dueDate": "2000-01-01T00:00:00Z"},
		map[string]any{"id": "c3", "name": "other", "listId": "l2", "claimedBy": "u2"},
		map[string]any{"id": "c4", "name": "closed", "listId": "l2", "isClosed": true, "members": []any{"u1"}},
	}}
	f.handleFunc("GET /api/v1/boards/{boardId}/cards", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("memberId") == "u1" { // server-side assignment filter
			json.NewEncoder(w).Encode(map[string]any{"items": []any{cards["items"].([]any)[0], cards["items"].([]any)[3]}})
			return
		}
		json.NewEncoder(w).Encode(cards)
	})
	code, out, errs := f.run("inbox", "--output", "ids")
	if code != 0 || strings.TrimSpace(out) != "c1\nc2" {
		t.Fatalf("inbox %d %q %s", code, out, errs)
	}
	f.json("GET /api/v1/boards/{id}", map[string]any{"item": map[string]any{"id": "b1", "name": "Product"}})
	// board.tree missing -> assembled from board.get + list.list + card.list
	f.status("GET /api/v1/boards/{id}/tree", 404, "not_found", "no route")
	code, out, errs = f.run("list", "stats", "--board", "Product", "--output", "ndjson")
	if code != 0 {
		t.Fatalf("stats %d %s", code, errs)
	}
	if !strings.Contains(out, `"name":"Ready"`) || !strings.Contains(out, `"claimed":1`) || !strings.Contains(out, `"overdue":1`) {
		t.Fatalf("stats: %s", out)
	}
	code, out, _ = f.run("board", "tree", "--board", "Product", "--output", "json")
	if code != 0 || !strings.Contains(out, `"lists"`) || !strings.Contains(out, `"cards"`) {
		t.Fatalf("tree %s", out)
	}
	// server-provided tree wins
	f.json("GET /api/v1/boards/{id}/tree", map[string]any{"item": map[string]any{"id": "b1", "name": "Product", "lists": []any{}}})
	_, out, _ = f.run("board", "tree", "Product", "--output", "json")
	if !strings.Contains(out, `"lists":[]`) {
		t.Fatalf("server tree: %s", out)
	}
}

func TestSkillInstallPrintCheck(t *testing.T) {
	f := newFake(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir()) // no real agent binaries may be detected
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	code, out, _ := f.run("skill", "print")
	if code != 0 || !strings.Contains(out, "name: weave") {
		t.Fatalf("print: %d", code)
	}
	if code, _, _ := f.run("skill", "check"); code != ExitError {
		t.Fatalf("missing skill must fail check: %d", code)
	}
	code, out, errs := f.run("skill", "install", "--target", "claude")
	if code != 0 || !strings.Contains(out, `"created"`) {
		t.Fatalf("install %d %s %s", code, out, errs)
	}
	p := filepath.Join(home, ".claude", "skills", "weave", "SKILL.md")
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "weave", "references", "commands.md")); err != nil {
		t.Fatal("references not installed")
	}
	if code, _, _ := f.run("skill", "check"); code != 0 {
		t.Fatal("check after install")
	}
	_, out, _ = f.run("skill", "install", "--target", "claude")
	if !strings.Contains(out, "unchanged") {
		t.Fatalf("idempotent: %s", out)
	}
	os.WriteFile(p, []byte("local edit"), 0o644)
	if code, _, _ := f.run("skill", "check"); code != ExitError {
		t.Fatal("stale must fail check")
	}
	if code, _, errs := f.run("skill", "install"); code != ExitConflict {
		t.Fatalf("stale install without --force: %d %s", code, errs)
	}
	if code, out, _ := f.run("skill", "install", "--force"); code != 0 || !strings.Contains(out, "updated") {
		t.Fatalf("force: %d %s", code, out)
	}
	// AGENTS.md section is managed and idempotent
	dir := t.TempDir()
	agents := filepath.Join(dir, "AGENTS.md")
	os.WriteFile(agents, []byte("# My rules\n\nbe nice\n"), 0o644)
	if code, _, errs := f.run("skill", "install", "--target", "agents-md", "--dir", dir); code != 0 {
		t.Fatal(errs)
	}
	b, _ := os.ReadFile(agents)
	if !strings.Contains(string(b), "be nice") || !strings.Contains(string(b), blockBeginPrefix) {
		t.Fatalf("agents.md: %s", b)
	}
	f.run("skill", "install", "--target", "agents-md", "--dir", dir)
	b2, _ := os.ReadFile(agents)
	if string(b2) != string(b) || strings.Count(string(b2), blockBeginPrefix) != 1 {
		t.Fatal("agents-md install must be idempotent")
	}
	if code, _, _ := f.run("skill", "check", "--target", "agents-md", "--dir", dir); code != 0 {
		t.Fatal("agents-md check")
	}
	// plain dir target
	d2 := filepath.Join(t.TempDir(), "x")
	if code, _, _ := f.run("skill", "install", "--target", "dir", "--dir", d2); code != 0 {
		t.Fatal("dir install")
	}
	if _, err := os.Stat(filepath.Join(d2, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}
