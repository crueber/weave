package notify_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// The Agent prompts page: two tabs kept in the URL, one expandable editor per text, a save redraws only its editor,
// Reset clears it, Preview renders the unsaved text with sample data; none of it works for a non-admin.
func TestAgentPromptsAdminTabs(t *testing.T) {
	w := seed(t)
	serve := func(p *ops.Env) *httptest.Server {
		mux := http.NewServeMux()
		web.Mount(mux, func(r *http.Request) *ops.Env { c := *p; c.Ctx = r.Context(); c.Req = r; return &c })
		return httptest.NewServer(mux)
	}
	srv := serve(w.admin)
	defer srv.Close()
	do := func(srv *httptest.Server, method, path string, form url.Values) (int, string) {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, _ := http.NewRequest(method, srv.URL+path, body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if method == "POST" {
			req.Header.Set("HX-Request", "true")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	has := func(what, body string, want ...string) {
		t.Helper()
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Errorf("%s lacks %q", what, s)
			}
		}
	}
	hasNot := func(what, body string, bad ...string) {
		t.Helper()
		for _, s := range bad {
			if strings.Contains(body, s) {
				t.Errorf("%s has %q", what, s)
			}
		}
	}

	// default tab: the four prompts, no blocks
	_, p := do(srv, "GET", "/admin/agent-prompts", nil)
	has("prompts tab", p, `href="/admin/agent-prompts?tab=prompts" aria-current="page">Prompts`, `name="agentprompt.general"`, `name="agentprompt.project"`, `name="agentprompt.board"`, `name="agentprompt.runner"`, `id="ap-board"`, "{questionnaire}")
	hasNot("prompts tab", p, `name="agentblock.`, "Customised", " open>")
	// blocks tab, one editor opened from the URL
	_, b := do(srv, "GET", "/admin/agent-prompts?tab=blocks&open=connect", nil)
	has("blocks tab", b, `href="/admin/agent-prompts?tab=blocks" aria-current="page">Blocks`, `name="agentblock.instance"`, `name="agentblock.etiquette_compact"`, `name="agentblock.install_skill"`, `<details class="adm-ap" id="ab-connect" open>`, "{install_skill}", `name="_section" value="blocks"`)
	hasNot("blocks tab", b, `name="agentprompt.`, `id="ab-security" open`)

	// save one block: only its editor comes back, open, customised, saved
	code, frag := do(srv, "POST", "/ui/admin/agent-prompts", url.Values{"_section": {"blocks"}, "agentblock.security": {"Rules for {server}\r\n"}})
	if code != 200 || notify.Config(w.admin, "agentblock.security") != "Rules for {server}" {
		t.Fatalf("block save: %d %q", code, notify.Config(w.admin, "agentblock.security"))
	}
	has("saved block", frag, `<details class="adm-ap" id="ab-security" open>`, "Customised", "Saved", "Reset to default", "Rules for {server}</textarea>")
	hasNot("saved block", frag, `id="adm-prompts"`, `id="ab-connect"`)
	_, b = do(srv, "GET", "/admin/agent-prompts?tab=blocks", nil)
	has("blocks tab after save", b, `aria-current="page">Blocks <span class="adm-tab-n" title="customised">1</span>`)
	// a prompt save on the prompts tab
	_, frag = do(srv, "POST", "/ui/admin/agent-prompts", url.Values{"_section": {"prompts"}, "agentprompt.general": {"Hi {server}"}})
	has("saved prompt", frag, `id="ap-general" open>`, "Hi {server}</textarea>")
	// too long: the error shows in the editor and the typed text stays
	_, frag = do(srv, "POST", "/ui/admin/agent-prompts", url.Values{"_section": {"blocks"}, "agentblock.keynote": {strings.Repeat("k", 20001)}})
	has("too long", frag, `role="alert"`, `id="ab-keynote" open>`)
	if notify.Config(w.admin, "agentblock.keynote") != "" {
		t.Fatal("over-long block stored")
	}

	// preview: the unsaved text, rendered with sample data
	_, pv := do(srv, "POST", "/ui/admin/agent-prompts/preview", url.Values{"_section": {"blocks"}, "agentblock.instance": {"At {server} {connect}"}})
	has("block preview", pv, `class="adm-ap-preview"`, "At (your Weave server URL) {connect}")
	_, pv = do(srv, "POST", "/ui/admin/agent-prompts/preview", url.Values{"_section": {"prompts"}, "agentprompt.board": {"Board {board_name}\n{security}"}})
	has("prompt preview", pv, "Board &#34;Launch plan&#34;\nRules for (your Weave server URL)")

	// reset
	do(srv, "POST", "/ui/admin/agent-prompts", url.Values{"_section": {"blocks"}, "agentblock.security": {""}})
	if notify.Config(w.admin, "agentblock.security") != "" {
		t.Fatal("block not reset")
	}

	// an env-pinned block: read only, badge, and Preview (which cannot include the disabled textarea) shows the env text
	t.Setenv("WEAVE_AGENTBLOCK_READMORE", "Pinned {server}")
	_, b = do(srv, "GET", "/admin/agent-prompts?tab=blocks&open=readmore", nil)
	has("pinned block", b, `name="agentblock.readmore" rows="8" class="bs-mono bs-prompt-text adm-ap-text" spellcheck="false" maxlength="20000" disabled>Pinned {server}</textarea>`, "Environment", "WEAVE_AGENTBLOCK_READMORE")
	has("pinned block", b, `hx-vals='{"_section":"blocks","agentblock.readmore":""}' hx-target="#ab-readmore-prev"`)
	hasNot("pinned block", b, `hx-target="#ab-readmore" hx-swap="outerHTML" hx-confirm`) // no Reset
	code, pv = do(srv, "POST", "/ui/admin/agent-prompts/preview", url.Values{"_section": {"blocks"}, "agentblock.readmore": {""}})
	if code != 200 || !strings.Contains(pv, "Pinned (your Weave server URL)") {
		t.Fatalf("pinned preview: %d %s", code, pv)
	}

	// non-admins get nothing
	other := serve(w.alice)
	defer other.Close()
	for _, x := range []struct{ m, p string }{{"GET", "/admin/agent-prompts?tab=blocks"}, {"POST", "/ui/admin/agent-prompts"}, {"POST", "/ui/admin/agent-prompts/preview"}} {
		code, body := do(other, x.m, x.p, url.Values{"_section": {"blocks"}, "agentblock.security": {"pwned"}})
		if code == 200 && strings.Contains(body, "agentblock") {
			t.Errorf("%s %s served to a non-admin", x.m, x.p)
		}
	}
	if notify.Config(w.admin, "agentblock.security") != "" {
		t.Fatal("non-admin wrote a block")
	}
}
