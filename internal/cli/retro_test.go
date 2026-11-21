package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Regressions from an agent's field report: --fields flattening board tree, silent --jq typos,
// task counts in the tree, and card edit / --desc @file on update.

// serverTree is the board.tree REST shape ({board, lists[{..., cards}]}).
func serverTree() map[string]any {
	card := func(id, name string, total, done float64) map[string]any {
		return map[string]any{"id": id, "name": name, "listId": "l1", "description": "long text", "labels": []any{map[string]any{"id": "lb1", "name": "bug"}},
			"members": []any{map[string]any{"id": "u1", "name": "Agent Smith", "isAgent": true}}, "claimedBy": nil, "tasksTotal": total, "tasksCompleted": done}
	}
	return map[string]any{"item": map[string]any{
		"board": map[string]any{"id": "b1", "name": "Product", "hasInbox": false},
		"lists": []any{
			map[string]any{"id": "l1", "name": "Ready", "type": "active", "cards": []any{card("c1", "First", 3, 1), card("c2", "Second", 0, 0)}},
			map[string]any{"id": "l2", "name": "Empty", "type": "active", "cards": []any{}},
		},
	}}
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not json: %v\n%s", err, s)
	}
	return m
}

func TestBoardTreeFieldsKeepTheTree(t *testing.T) {
	f := newFake(t)
	f.json("GET /api/v1/boards/{id}/tree", serverTree())

	// the exact repro: --fields name used to print {"item":{"name":"Product"}}
	code, out, errs := f.run("board", "tree", "--board", "Product", "--fields", "name")
	if code != 0 {
		t.Fatalf("exit %d %s", code, errs)
	}
	want := `{"item":{"id":"b1","lists":[{"cards":[{"id":"c1","name":"First"},{"id":"c2","name":"Second"}],"id":"l1","name":"Ready"},{"cards":[],"id":"l2","name":"Empty"}],"name":"Product"}}`
	if strings.TrimSpace(out) != want {
		t.Fatalf("--fields name:\n got %s\nwant %s", out, want)
	}

	// bare paths are card paths; board.X is a board field; nested card paths keep their arrays
	_, out, _ = f.run("board", "tree", "Product", "--fields", "tasksTotal,tasksCompleted,members.name,board.hasInbox")
	m := decode(t, out)["item"].(map[string]any)
	if m["hasInbox"] != false {
		t.Fatalf("board.hasInbox missing: %s", out)
	}
	c1 := m["lists"].([]any)[0].(map[string]any)["cards"].([]any)[0].(map[string]any)
	wantC1 := map[string]any{"id": "c1", "name": "First", "tasksTotal": 3.0, "tasksCompleted": 1.0, "members": []any{map[string]any{"name": "Agent Smith"}}}
	if !reflect.DeepEqual(c1, wantC1) {
		t.Fatalf("card projection: %v", c1)
	}

	// literal lists paths, with or without []
	for _, fl := range []string{"lists[].cards[].name", "lists.cards.name"} {
		_, out, _ = f.run("board", "tree", "Product", "--fields", fl)
		if !strings.Contains(out, `"cards":[{"id":"c1","name":"First"}`) || strings.Contains(out, "long text") {
			t.Fatalf("%s: %s", fl, out)
		}
	}

	// ndjson goes through the same rule
	_, out, _ = f.run("board", "tree", "Product", "--fields", "name", "--output", "ndjson")
	if !strings.Contains(out, `"cards":[{"id":"c1","name":"First"}`) {
		t.Fatalf("ndjson: %s", out)
	}
}

func TestFieldsPlainOpsUnchangedAndUnmatchedNote(t *testing.T) {
	f := newFake(t)
	f.json("GET /api/v1/boards/{boardId}/cards", map[string]any{"items": []any{
		map[string]any{"id": "c1", "name": "A", "labels": []any{map[string]any{"id": "x", "name": "bug"}}},
	}})
	code, out, errs := f.run("card", "list", "--board", "b1", "--fields", "id,name")
	if code != 0 || strings.TrimSpace(out) != `{"items":[{"id":"c1","name":"A"}]}` || errs != "" {
		t.Fatalf("card list --fields: %d %s %s", code, out, errs)
	}
	code, out, errs = f.run("card", "list", "--board", "b1", "--fields", "titel")
	if code != 0 || !strings.Contains(errs, `--fields titel matched nothing; available: id, labels, name`) {
		t.Fatalf("unmatched note: %d %q %q", code, out, errs)
	}
}

func TestJQStrictKeys(t *testing.T) {
	tree := map[string]any{"item": map[string]any{"id": "b1", "dueDate": nil, "lists": []any{
		map[string]any{"id": "l1", "cards": []any{map[string]any{"name": "a", "extra": 1.0}, map[string]any{"name": "b"}}},
		map[string]any{"id": "l2", "cards": []any{}},
	}}}
	ok := map[string][]any{
		".item.lists[].cards[].name":  {"a", "b"},
		".item.lists[].id":            {"l1", "l2"},
		".item.dueDate":               {nil},               // exists with null value
		".item.dueDate.x":             {nil},               // descending into null stays null
		".item.lists[].cards[].extra": {1.0},               // missing on some elements is fine
		".item.lists.id":              {[]any{"l1", "l2"}}, // implicit map keeps the array
		".item.lists[5]":              nil,                 // out of range: nothing
	}
	for expr, want := range ok {
		got, err := EvalJQ(tree, expr)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", expr, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %#v want %#v", expr, got, want)
		}
	}
	bad := map[string]string{
		".items[]":                   `no key "items"; available: item`,
		".item.list[]":               `no key "list"; available: dueDate, id, lists`,
		".item.lists[].cards[].nmae": `no key "nmae"; available: extra, name`,
		".item[]":                    `cannot use [] on an object`,
		".item.id.x":                 `cannot read key "x" of a string`,
	}
	for expr, want := range bad {
		_, err := EvalJQ(tree, expr)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want error containing %q", expr, err, want)
		}
		if ae, ok := err.(*APIError); !ok || ae.Code != "usage" {
			t.Errorf("%s: want usage error, got %#v", expr, err)
		}
	}
}

func TestJQWrongPathFailsOnBoardTree(t *testing.T) {
	f := newFake(t)
	f.json("GET /api/v1/boards/{id}/tree", serverTree())
	code, out, errs := f.run("board", "tree", "--board", "Product", "--jq", ".items[]")
	if code != ExitUsage || out != "" || !strings.Contains(errs, `no key \"items\"; available: item`) {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
	code, out, _ = f.run("board", "tree", "--board", "Product", "--jq", ".item.lists[].cards[].tasksTotal")
	if code != 0 || strings.Fields(out)[0] != "3" {
		t.Fatalf("jq tasksTotal: %d %q", code, out)
	}
}

func TestCardEditAndDescFile(t *testing.T) {
	f := newFake(t)
	f.json("PATCH /api/v1/cards/{id}", map[string]any{"item": map[string]any{"id": "c1"}})
	f.json("POST /api/v1/lists/{listId}/cards", map[string]any{"item": map[string]any{"id": "c9"}})
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.md")
	body := "Line one\nrun `make test` first\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	id := "0190aaaabbbbccccdddd1111"
	for _, args := range [][]string{
		{"card", "edit", id, "--desc", "@" + file},
		{"card", "update", id, "--desc", "@" + file},
		{"card", "update", id, "--description", "@" + file},
	} {
		code, _, errs := f.run(args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, errs)
		}
		var got map[string]any
		_ = json.Unmarshal([]byte(f.body("PATCH /api/v1/cards/"+id)), &got)
		if got["description"] != body {
			t.Fatalf("%v: body %v", args, got)
		}
	}
	// - reads stdin; --due is dueDate
	code, _, errs := f.runIn("from stdin\n", "card", "edit", id, "--desc", "-", "--due", "2026-12-01T00:00:00Z")
	var got map[string]any
	_ = json.Unmarshal([]byte(f.body("PATCH /api/v1/cards/"+id)), &got)
	if code != 0 || got["description"] != "from stdin" || got["dueDate"] != "2026-12-01T00:00:00Z" {
		t.Fatalf("stdin: %d %s %v", code, errs, got)
	}
	// an op-only flag hands card create to the generic path, which must still know --desc
	code, _, errs = f.run("card", "create", "--board", "Product", "--list", "Ready", "--name", "X", "--desc", "@"+file, "--dueDate", "2026-12-01T00:00:00Z")
	_ = json.Unmarshal([]byte(f.body("POST /api/v1/lists/l1/cards")), &got)
	if code != 0 || got["description"] != body || got["dueDate"] != "2026-12-01T00:00:00Z" {
		t.Fatalf("create delegated: %d %s %v", code, errs, got)
	}
	// help and completion know the alias
	_, out, _ := f.run("card", "help")
	if !strings.Contains(out, "edit") || !strings.Contains(out, "alias of update") {
		t.Fatalf("tag help: %s", out)
	}
	_, out, _ = f.run("card", "edit", "--help")
	if !strings.Contains(out, "(also --desc)") {
		t.Fatalf("op help: %s", out)
	}
	_, out, _ = f.run("__complete", "card", "ed")
	if !strings.Contains(out, "edit") {
		t.Fatalf("completion verbs: %q", out)
	}
	_, out, _ = f.run("__complete", "card", "edit", "--de")
	if !strings.Contains(out, "--desc") || !strings.Contains(out, "--description") {
		t.Fatalf("completion flags: %q", out)
	}
}

// Paged list ops answer {"item":{"items":[...],"nextCursor":..}}; --fields, ids and table must reach the
// records instead of printing {"item":{}} or nothing.
func TestPagedListOutputs(t *testing.T) {
	f := newFake(t)
	f.json("GET /api/v1/boards/{boardId}/cards", map[string]any{"item": map[string]any{"nextCursor": "", "items": []any{
		map[string]any{"id": "c1", "name": "A", "x": 1.0}, map[string]any{"id": "c2", "name": "B"},
	}}})
	_, out, errs := f.run("card", "list", "--board", "b1", "--fields", "id,name")
	if strings.TrimSpace(out) != `{"item":{"items":[{"id":"c1","name":"A"},{"id":"c2","name":"B"}],"nextCursor":""}}` || errs != "" {
		t.Fatalf("fields: %s %s", out, errs)
	}
	if _, out, _ = f.run("card", "list", "--board", "b1", "--output", "ids"); out != "c1\nc2\n" {
		t.Fatalf("ids: %q", out)
	}
	if _, out, _ = f.run("card", "list", "--board", "b1", "--output", "table", "--fields", "id,name"); !strings.Contains(out, "c2") {
		t.Fatalf("table: %q", out)
	}
}
