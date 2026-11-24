package boards_test

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/testutil"
)

func TestTemplateApplyUI(t *testing.T) {
	e := fixture(t)
	b, _ := applyBoard(t, e, "ui")
	m := mux(e)

	// the settings row is there for a manager, not for an editor or a viewer
	if _, body := get(t, m, "/boards/"+b.ID+"/settings", false); !strings.Contains(body, "/ui/boards/"+b.ID+"/apply-template") {
		t.Fatal("the manager's settings page has no Apply a template row")
	}
	mustExec(t, e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m-e',?,?,'editor','2024-01-01T00:00:00Z'),('m-v',?,?,'viewer','2024-01-01T00:00:00Z')`,
		b.ID, editor.UserID, b.ID, viewer.UserID)
	for _, u := range []string{"editor", "viewer"} {
		p := editor
		if u == "viewer" {
			p = viewer
		}
		um := mux(testutil.As(e, p))
		if _, body := get(t, um, "/boards/"+b.ID+"/settings", false); strings.Contains(body, "apply-template") {
			t.Fatalf("%s sees the Apply a template row", u)
		}
		if code, _ := get(t, um, "/ui/boards/"+b.ID+"/apply-template", true); code != 403 {
			t.Fatalf("%s opened the apply gallery: %d", u, code)
		}
		if w := post(um, "/ui/boards/"+b.ID+"/template/apply", url.Values{"templateId": {"builtin:kanban-basics"}, "dryRun": {"true"}}, true); w.Code != 403 {
			t.Fatalf("%s previewed an apply: %d", u, w.Code)
		}
	}

	// the gallery in apply mode: board-scoped links, no blank or import tiles
	code, body := get(t, m, "/ui/boards/"+b.ID+"/apply-template", true)
	if code != 200 || !strings.Contains(body, "Apply a template") || !strings.Contains(body, `name="boardId" value="`+b.ID+`"`) ||
		!strings.Contains(body, "?boardId="+b.ID) || strings.Contains(body, "Blank board") || strings.Contains(body, "Import a file") {
		t.Fatalf("apply gallery: %d\n%s", code, body)
	}
	_, body = get(t, m, "/ui/board-templates?boardId="+b.ID+"&q=kanban", true)
	if !strings.Contains(body, "?boardId="+b.ID) || strings.Contains(body, "Blank board") {
		t.Fatalf("apply grid: %s", body)
	}
	_, body = get(t, m, "/ui/board-templates/builtin:kanban-basics?boardId="+b.ID, true)
	if !strings.Contains(body, "Preview changes") || strings.Contains(body, "Create board") || !strings.Contains(body, `name="dryRun" value="true"`) {
		t.Fatalf("apply preview pane: %s", body)
	}

	// the preview: exact lists, warnings, the undo warning with the export link; nothing written
	before := snapshot(t, e)
	w := post(m, "/ui/boards/"+b.ID+"/template/apply", url.Values{"templateId": {"builtin:kanban-basics"}, "dryRun": {"true"}, "applySettings": {"false"}, "includeSampleCards": {"false"}}, true)
	body = w.Body.String()
	for _, want := range []string{"+4 on the left:", "Backlog, To do, Doing, Done", "5 cards move to Backlog", "2 of them are closed", "+3:", "Low, Medium, Blocked",
		"Already here, used as they are: High", "Unchanged", "Before you apply", "3 old columns will be removed", "Todo, Doing, Done", "settings go with them: Done (closes cards)",
		`name="removeOldColumns" value="true" checked`, "Remove the old columns (they will be empty)", "and remove 3 old columns?", "This cannot be undone automatically",
		`/ui/boards/` + b.ID + `/export?format=json`, "Apply template", `{"dryRun":"false","confirm":"true"}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("preview lacks %q:\n%s", want, body)
		}
	}
	if snapshot(t, e) != before {
		t.Fatal("the UI preview wrote something")
	}
	// unticking the box re-previews with the old columns kept (and the duplicate-name warning back)
	w = post(m, "/ui/boards/"+b.ID+"/template/apply", url.Values{"templateId": {"builtin:kanban-basics"}, "dryRun": {"true"}, "removeOldColumns": {"false"}}, true)
	body = w.Body.String()
	if !strings.Contains(body, "stay on the right, emptied: Todo, Doing, Done") || !strings.Contains(body, "added again") || strings.Contains(body, "will be removed") ||
		!strings.Contains(body, `<input type="checkbox" name="removeOldColumns" value="true"><input type="hidden"`) {
		t.Fatalf("keep preview:\n%s", body)
	}
	// confirm: redirect to the board with a toast for the next page
	// htmx sends the hx-vals value instead of the form's hidden dryRun (checked in Chromium by csp-smoke)
	w = post(m, "/ui/boards/"+b.ID+"/template/apply", url.Values{"templateId": {"builtin:kanban-basics"}, "dryRun": {"false"}, "confirm": {"true"}}, true)
	if w.Code != 200 || w.Header().Get("HX-Redirect") != "/boards/"+b.ID || !strings.Contains(w.Header().Get("HX-Trigger"), "removed 3 old columns") {
		t.Fatalf("apply: %d %v", w.Code, w.Header())
	}
	if cols := columnNames(t, e, b.ID); !reflect.DeepEqual(cols, []string{"Backlog", "To do", "Doing", "Done"}) {
		t.Fatalf("not applied (old columns removed by default): %v", cols)
	}
}
