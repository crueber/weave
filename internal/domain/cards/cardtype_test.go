package cards

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/ops"
)

// Card types (project|story) were removed. Older clients and agents may still send "type": the binder only
// reads the fields an op declares, so the key is accepted and ignored on every input path, never stored and
// never returned.
func TestLegacyCardTypeIgnored(t *testing.T) {
	f := seed(t)
	noType := func(where string, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if _, ok := m["type"]; ok {
			t.Errorf("%s: response still has a type: %s", where, b)
		}
	}
	// argument map (the CLI/MCP shape)
	out, err := ops.CallMap(f.e, "card.create", map[string]any{"listId": f.todo, "name": "Legacy", "type": "story"})
	if err != nil {
		t.Fatalf("create with legacy type: %v", err)
	}
	c := out.(Card)
	noType("create", c)
	if _, err := ops.CallMap(f.e, "card.update", map[string]any{"id": c.ID, "name": "Legacy 2", "type": "epic"}); err != nil {
		t.Fatalf("update with legacy type: %v", err)
	}

	// JSON body (REST API)
	op := ops.Get("card.update")
	r := httptest.NewRequest("PATCH", "/api/v1/cards/"+c.ID, bytes.NewBufferString(`{"name":"Legacy 3","type":"story"}`))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("id", c.ID)
	in, err := op.Bind(r)
	if err != nil {
		t.Fatalf("bind JSON with legacy type: %v", err)
	}
	got, err := ops.Call(f.e, "card.update", in)
	if err != nil {
		t.Fatalf("update via JSON: %v", err)
	}
	noType("update", got)
	if got.(Card).Name != "Legacy 3" {
		t.Fatalf("update lost: %+v", got)
	}
	noType("get", call[Card](t, f.e, "card.get", GetIn{ID: c.ID}))

	// form post from an old card sheet
	mux := mount(f, f.e)
	w := do(mux, "POST", "/ui/cards/"+c.ID+"/update", url.Values{"type": {"story"}})
	if w.Code != 200 {
		t.Fatalf("form with legacy type: %d %s", w.Code, w.Body.String())
	}
	// the sheet has no type badge or picker any more
	body := do(mux, "GET", "/ui/cards/"+c.ID, nil).Body.String()
	for _, bad := range []string{"cards-type-", `data-prop="type"`, `aria-label="Card type"`, `name="type"`, ">story<", ">project<"} {
		if strings.Contains(body, bad) {
			t.Errorf("card sheet still renders %q", bad)
		}
	}
	// and the op schema no longer advertises the field
	for _, name := range []string{"card.create", "card.update"} {
		for _, fl := range ops.Get(name).Schema().Fields {
			if fl.Name == "type" {
				t.Errorf("%s still declares type", name)
			}
		}
	}
}
