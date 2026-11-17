package agentprompt

import "strings"

// Example data used by the golden tests and by skillgen (skills/weave/references/prompts.md).
func exampleBoard() Board {
	return Board{ID: "01JBOARD", Name: "Launch plan", Columns: []Column{
		{ID: "01JREADY", Name: "Ready", Brief: "Implement the card, comment what changed, then move it to Review.", Limit: 3},
		{ID: "01JPROG", Name: "In progress", Brief: "Cards an agent is working on right now."},
		{ID: "01JREV", Name: "Review", Brief: "A human reviews the result."},
		{ID: "01JINFO", Name: "Needs info", Brief: "Blocked on a question; a human answers, then moves it back to Ready."},
	}}
}

// Examples returns one representative Data per scope, in Scopes order.
func Examples() []Data {
	b := exampleBoard()
	base := Data{BaseURL: "https://weave.example.com", InstallBase: "https://dl.example.com/weave"}
	with := func(scope string, f func(*Data)) Data {
		d := base
		d.Scope = scope
		f(&d)
		return d
	}
	return []Data{
		with(ScopeGeneral, func(d *Data) {}),
		with(ScopeProject, func(d *Data) {
			d.ProjectID, d.ProjectName = "01JPROJ", "Product"
			d.Boards = []Board{b, {ID: "01JB2", Name: "Platform", Columns: []Column{{ID: "01JL9", Name: "Backlog"}, {ID: "01JL10", Name: "Done"}}}}
		}),
		with(ScopeBoard, func(d *Data) { d.Board = b }),
		with(ScopeList, func(d *Data) { d.Board = b; d.List = b.Columns[0]; d.Board.Columns = b.Columns[1:] }),
		with(ScopeRunner, func(d *Data) { d.Board = b; d.HasBoard = true }),
		with(ScopeWorkOrder, func(d *Data) {
			d.List = b.Columns[0]
			d.CardID = "01JCARD"
			d.CardFile, d.BoardFile = "/tmp/weave-card-1/card.json", "/tmp/weave-card-1/board.json"
			d.WorkOrderFile, d.OutDir = "/tmp/weave-card-1/workorder.md", "/tmp/weave-card-1/out"
		}),
	}
}

// Reference renders every example prompt as the markdown of references/prompts.md.
func Reference() string {
	var sb strings.Builder
	sb.WriteString("# Weave agent prompts\n\n")
	sb.WriteString("These are the prompts Weave hands to AI agents (`weave agent prompt ...`, or Connect an agent in the browser).\n")
	sb.WriteString("They are rendered from the same templates the server uses, here with example data. When a user gives you one of\n")
	sb.WriteString("them: run its questionnaire, confirm the PLAN, and only then listen. Never put a key in a prompt or chat; use `$WEAVE_TOKEN`.\n\n")
	notes := map[string]string{
		ScopeGeneral:   "Concepts, connecting, etiquette. Ends by asking the user what they want.",
		ScopeProject:   "Asks which board(s) to attach to, then runs the board questionnaire per board.",
		ScopeBoard:     "The questionnaire (mode, columns, destinations, claim policy, filters, authority, limits) and the PLAN format. Wait for an explicit go.",
		ScopeList:      "One column: its brief and how to watch it.",
		ScopeRunner:    "Unattended runner: a wrapper script plus service unit around `weave agent run --outbox`. The result channel is the out directory.",
		ScopeWorkOrder: "What the runner gives the headless coding agent per card: file paths only, no card text, no credentials.",
	}
	for _, d := range Examples() {
		text, err := Render(d)
		if err != nil {
			panic(err)
		}
		sb.WriteString("## " + Title(d) + " (`" + d.Scope + "`)\n\n" + notes[d.Scope] + "\n\n`````text\n" + text + "`````\n\n")
	}
	return sb.String()
}
