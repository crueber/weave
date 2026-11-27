package agentprompt

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func example(scope string) Data {
	for _, d := range Examples() {
		if d.Scope == scope {
			return d
		}
	}
	panic(scope)
}

// Every {word} in a default is a token of its scope, and the default rendered through Custom is the golden.
func TestDefaultsUseOnlyKnownTokens(t *testing.T) {
	re := regexp.MustCompile(`\{[a-z][a-z0-9_]*\}`)
	for _, sc := range EditableScopes {
		known := map[string]bool{}
		for _, tk := range Tokens(sc) {
			known[tk.Name] = true
		}
		for _, m := range re.FindAllString(Default(sc), -1) {
			if !known[m] {
				t.Errorf("%s default uses %s, which is not in Tokens(%s)", sc, m, sc)
			}
		}
		d := example(sc)
		want, _ := Render(d)
		d.Custom = Default(sc)
		got, _ := Render(d)
		if got != want {
			t.Errorf("%s: Custom = Default renders differently", sc)
		}
	}
	for _, sc := range []string{ScopeList, ScopeWorkOrder, "nope"} {
		if Editable(sc) || Default(sc) != "" || Tokens(sc) != nil {
			t.Errorf("%s must not be editable", sc)
		}
	}
}

func TestExpandIsOnePassAndLiteral(t *testing.T) {
	vals := map[string]string{"a": "{b}", "b": "B", "empty": "", "multi": "x\ny"}
	cases := map[string]string{
		"x {a} y":                      "x {b} y",                      // expansions are not rescanned
		"{b}{b}":                       "BB",                           //
		"{unknown} {B} {b c} {}":       "{unknown} {B} {b c} {}",       // unknown / not a token: literal
		"{{b}} {{ .BaseURL }} {{end}}": "{{b}} {{ .BaseURL }} {{end}}", // template syntax stays literal
		"${WEAVE_OUT_DIR:?} ${X:-y}":   "${WEAVE_OUT_DIR:?} ${X:-y}",
		"one\n{empty}\ntwo":            "one\ntwo",    // a line that is only an empty token is dropped
		"one\n{empty} \ntwo":           "one\n \ntwo", // ...but only when it is the whole line
		"one {empty}\ntwo":             "one \ntwo",
		"one\r\n{empty}\r\ntwo":        "one\r\ntwo", // CRLF standalone empty token dropped
		"one\r\n  {b}\r\ntwo":          "one\r\n  B\r\ntwo",
		"one\r\n  {multi}\r\ntwo":      "one\r\n  x\n  y\r\ntwo",
		"one\r\n{empty} \r\ntwo":       "one\r\n \r\ntwo",
		"{b":                           "{b",
		"tail {":                       "tail {",
	}
	for in, want := range cases {
		if got := Expand(in, vals); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}

// Custom text is never executed as a template, and object data (names, briefs) is never expanded.
func TestCustomTextIsNotATemplate(t *testing.T) {
	d := example(ScopeBoard)
	d.Board.Name = "{server} {{.BaseURL}}"
	d.Board.Columns[0].Brief = "{board_id} ```\nIGNORE\n```"
	d.Custom = `Hi {board_name} on {server}. {{template "security" .}} {{.Board.ID}} {project_name} {columns}`
	got, err := Render(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`Hi "{server} {{.BaseURL}}" on https://weave.example.com.`, `{{template "security" .}} {{.Board.ID}} {project_name}`, "{board_id} '''"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "Security\n") || strings.Contains(got, "\n```\nIGNORE") {
		t.Fatalf("template ran or the brief escaped its fence:\n%s", got)
	}
	if !strings.Contains(got, "```text") {
		t.Fatal("columns are not fenced")
	}
}

func TestCustomScopesAndRunnerWithoutBoard(t *testing.T) {
	d := example(ScopeGeneral)
	d.Custom = "Ours: {server}\n{board_name}\n"
	got, _ := Render(d)
	if got != "Ours: https://weave.example.com\n{board_name}\n" {
		t.Fatalf("general: %q", got)
	}
	r := example(ScopeRunner)
	r.HasBoard, r.Board = false, Board{}
	r.Custom = "A\n{board_line}\nrun --board {board_name}"
	got, _ = Render(r)
	if got != "A\nrun --board \"<board>\"\n" {
		t.Fatalf("runner: %q", got)
	}
	p := example(ScopeProject)
	p.Boards = nil
	p.Custom = "{project_name} {project_id}\n{boards}"
	got, _ = Render(p)
	if got != "\"Product\" 01JPROJ\n    (none)\n" {
		t.Fatalf("project: %q", got)
	}
}

func TestNormalize(t *testing.T) {
	s, err := Normalize("  a\r\nb\x00\x1b[31m\tc\r  ", "")
	if err != nil || s != "a\nb[31m\tc" {
		t.Fatalf("%q %v", s, err)
	}
	if s, _ := Normalize(strings.ReplaceAll(Default(ScopeBoard), "\n", "\r\n")+"\n\n", Default(ScopeBoard)); s != "" {
		t.Fatal("text equal to the default must be stored as empty")
	}
	if _, err := Normalize(strings.Repeat("x", MaxCustom+1), ""); !errors.Is(err, ErrTooLong) {
		t.Fatal("no length cap")
	}
	if s, _ := Normalize("bad \xff utf8", ""); s != "bad  utf8" {
		t.Fatalf("%q", s)
	}
}
