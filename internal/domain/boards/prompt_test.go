package boards_test

import (
	"html"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// A column description is a first-class field: it round-trips through the ops, the board page and the
// export document, and blank input clears it.
func TestListDescription(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Briefs")
	brief := "Implement the card, comment what changed, then move it to Review."
	l := call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Ready", Description: brief})
	if l.Description == nil || *l.Description != brief {
		t.Fatalf("create description: %+v", l.Description)
	}
	// the board page (and board.tree) carry it, so agents and people see the same brief
	tree := call[boards.TreeOut](t, e, "board.tree", boards.TreeIn{ID: b.ID})
	if got := tree.Lists[0].Description; got == nil || *got != brief {
		t.Fatalf("board.tree description: %+v", got)
	}
	if code, body := get(t, mux(e), "/boards/"+b.ID, false); code != 200 || !strings.Contains(body, brief) {
		t.Fatalf("board page missing the column description (%d)", code)
	}

	long := strings.Repeat("x", 2001)
	mustFail(t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Description: &long}, "validation")
	blank := "   "
	if up := call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Description: &blank}); up.Description != nil {
		t.Fatalf("blank description should clear: %+v", up.Description)
	}
	again := call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Description: &brief})
	if again.Description == nil || *again.Description != brief {
		t.Fatalf("update description: %+v", again.Description)
	}

	// export -> import keeps the description
	st := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: b.ID})
	imp := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "json", Data: string(st.Data)})
	d := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: imp.ID})
	var found bool
	for _, x := range d.Lists {
		if x.Type == "active" && x.Description != nil && *x.Description == brief {
			found = true
		}
	}
	if !found {
		t.Fatalf("imported board lost the list description: %+v", d.Lists)
	}
}

// The "Agent prompt" fragment names the server, the board, the column and its brief, and lists the
// other columns so the text can be extended with one --list line each.
func TestAgentPrompt(t *testing.T) {
	e := fixture(t)
	e.Svc["config"] = config.Config{BaseURL: "https://weave.example.com/", AgentInstallURL: "https://dl.example.com/weave/"}
	b := newBoard(t, e, "Launch plan")
	ready := call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Ready",
		Description: "Implement the card, comment what changed, then move it to Review."})
	call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Needs info", Description: "Answer the question, then move it back to Ready."})

	m := mux(e)
	code, body := get(t, m, "/ui/boards/"+b.ID+"/lists/"+ready.ID+"/agent-prompt", true)
	if code != 200 {
		t.Fatalf("agent prompt fragment: %d %s", code, body)
	}
	text := html.UnescapeString(body) // the prompt lives in a <textarea>: quotes arrive as entities
	for _, want := range []string{
		"Agent prompt",
		"https://weave.example.com", // the server, without a duplicated slash
		"https://weave.example.com//",
		"Launch plan",
		"Ready",
		"Implement the card, comment what changed, then move it to Review.",
		`weave listen --board "Launch plan" --list "Ready"`,
		`--list "Needs info"`,
		"Answer the question, then move it back to Ready.",
		"weave login --url",
		"WEAVE_TOKEN=",
		"https://dl.example.com/weave/install.sh",
		"weave skill install",
		"data-copy",
	} {
		if want == "https://weave.example.com//" {
			if strings.Contains(text, want) {
				t.Errorf("base URL has a doubled slash")
			}
			continue
		}
		if want == "WEAVE_TOKEN=" {
			if !strings.Contains(text, "WEAVE_TOKEN=") {
				t.Errorf("prompt has no token line")
			}
			continue
		}
		if !strings.Contains(text, want) {
			t.Errorf("agent prompt missing %q", want)
		}
	}
	if strings.Contains(body, "&lt;script") {
		t.Error("prompt text is not escaped")
	}

	// a board viewer cannot read the prompt (it is a column-settings affordance)
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	vm := mux(testutil.As(e, viewer))
	if code, _ := get(t, vm, "/ui/boards/"+b.ID+"/lists/"+ready.ID+"/agent-prompt", true); code != 403 {
		t.Errorf("viewer agent prompt: %d, want 403", code)
	}
	// and an outsider learns nothing
	if code, _ := get(t, mux(testutil.As(e, nobody)), "/ui/boards/"+b.ID+"/lists/"+ready.ID+"/agent-prompt", true); code != 403 && code != 404 {
		t.Errorf("outsider agent prompt: %d, want 403/404", code)
	}
}
