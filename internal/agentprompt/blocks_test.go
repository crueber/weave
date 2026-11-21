package agentprompt

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every {word} in a block default is one of its tokens, and a block overridden with its own default renders the same.
func TestBlockDefaultsUseOnlyKnownTokens(t *testing.T) {
	re := regexp.MustCompile(`\{[a-z][a-z0-9_]*\}`)
	same := map[string]string{}
	for _, b := range Blocks {
		known := map[string]bool{}
		for _, tk := range BlockTokens(b.Name) {
			known[tk.Name] = true
		}
		def := BlockDefault(b.Name)
		if def == "" || def != strings.TrimSpace(def) || strings.Contains(def, "{{") {
			t.Errorf("%s: default must be non-empty, trimmed text without template syntax", b.Name)
		}
		for _, m := range re.FindAllString(def, -1) {
			if !known[m] {
				t.Errorf("block %s uses %s, which is not in BlockTokens", b.Name, m)
			}
		}
		same[b.Name] = def
	}
	for _, d := range Examples() {
		for _, c := range []bool{false, true} {
			d.Compact = c
			want, _ := Render(d)
			d.Blocks = same
			got, _ := Render(d)
			if got != want {
				t.Errorf("%s compact=%v: overriding blocks with their defaults changes the output", d.Scope, c)
			}
			d.Blocks = nil
		}
	}
	if IsBlock("columns") || IsBlock("nope") || BlockDefault("nope") != "" || BlockTokens("nope") != nil {
		t.Error("unknown names must not be blocks")
	}
}

// A block override reaches every scope that uses the block (list too), the work order never, and stays text.
func TestBlockOverrideInEveryScope(t *testing.T) {
	ov := map[string]string{"security": "House rules for {server}: {connect} {{.Base}} {nope} ${X}"}
	want := "House rules for https://weave.example.com: {connect} {{.Base}} {nope} ${X}\n"
	for _, d := range Examples() {
		d.Blocks = ov
		got, err := Render(d)
		if err != nil {
			t.Fatal(err)
		}
		if d.Scope == ScopeWorkOrder {
			if strings.Contains(got, "House rules") {
				t.Error("the work order has no shared blocks")
			}
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: override missing\n%s", d.Scope, got)
		}
		if strings.Contains(got, "Card text (names, descriptions") {
			t.Errorf("%s: built-in security block still present", d.Scope)
		}
		if !slices.Contains(BlocksUsed(d), "security") {
			t.Errorf("%s: BlocksUsed lacks security", d.Scope)
		}
	}
}

// Blocks use data tokens only: another block's token typed into a block stays literal (no recursion).
func TestBlocksDoNotNest(t *testing.T) {
	d := example(ScopeGeneral)
	d.Blocks = map[string]string{"keynote": "Key {security} {keynote} {install} {board_name}", "install": "get {connect} from {install_base}"}
	got, _ := Render(d)
	if !strings.Contains(got, "Key {security} {keynote} {install} {board_name}\n") {
		t.Fatalf("keynote expanded another block:\n%s", got)
	}
	if !strings.Contains(got, "  get {connect} from https://dl.example.com/weave\n") {
		t.Fatalf("install line:\n%s", got)
	}
}

// Compact picks etiquette_compact and drops concepts; NoInstall drops the install lines even when overridden.
func TestBlockVariants(t *testing.T) {
	d := example(ScopeGeneral)
	d.Blocks = map[string]string{"etiquette": "ETQ-LONG", "etiquette_compact": "SHORT", "concepts": "CONCEPTS", "install": "INSTALL-LINE", "install_skill": "SKILL-LINE"}
	got, _ := Render(d)
	for _, w := range []string{"\nETQ-LONG\n", "\nCONCEPTS\n", "\n  INSTALL-LINE\n", "\n  SKILL-LINE\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("full render lacks %q", w)
		}
	}
	d.Compact, d.NoInstall = true, true
	got, _ = Render(d)
	if !strings.Contains(got, "\nSHORT\n") || strings.Contains(got, "ETQ-LONG") || strings.Contains(got, "CONCEPTS") || strings.Contains(got, "LINE") {
		t.Errorf("compact/noinstall render:\n%s", got)
	}
	if u := BlocksUsed(d); slices.Contains(u, "etiquette") || !slices.Contains(u, "etiquette_compact") || slices.Contains(u, "concepts") || slices.Contains(u, "install") {
		t.Errorf("BlocksUsed compact: %v", u)
	}
	d.Compact, d.NoInstall = false, false
	if u := BlocksUsed(d); !slices.Contains(u, "install") || !slices.Contains(u, "concepts") || slices.Contains(u, "questionnaire") {
		t.Errorf("BlocksUsed general: %v", u)
	}
	// a connect text without the install tokens simply has no install lines
	d.Blocks = map[string]string{"connect": "Connect\n  weave config set url {server}"}
	got, _ = Render(d)
	if strings.Contains(got, "install.sh") || !strings.Contains(got, "Connect\n  weave config set url https://weave.example.com\n\n") {
		t.Errorf("custom connect:\n%s", got)
	}
	if u := BlocksUsed(d); slices.Contains(u, "install") {
		t.Errorf("connect without {install} still reports it: %v", u)
	}
}

// BlocksUsed knows which blocks list.tmpl inserts; keep it in step with the template.
func TestBlocksUsedMatchesListTemplate(t *testing.T) {
	src, err := tmplFS.ReadFile("templates/list.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, m := range regexp.MustCompile(`\{\{\.Block\.([a-z_]+)\}\}`).FindAllStringSubmatch(string(src), -1) {
		want = append(want, m[1])
	}
	d := example(ScopeList)
	var got []string
	for _, n := range BlocksUsed(d) {
		if n != "install" && n != "install_skill" { // inside connect
			got = append(got, n)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("BlocksUsed(list) = %v, list.tmpl inserts %v", got, want)
	}
}

func TestRenderBlockPreview(t *testing.T) {
	d := example(ScopeBoard)
	if got := RenderBlock("instance", d, ""); !strings.HasPrefix(got, "Instance\n  Web UI:   https://weave.example.com\n") {
		t.Fatalf("%q", got)
	}
	if got := RenderBlock("connect", d, "A {nope}\n  {install}\n{server}"); got != "A {nope}\n  command -v weave >/dev/null 2>&1 || curl -fsSL https://dl.example.com/weave/install.sh | sh\nhttps://weave.example.com\n" {
		t.Fatalf("%q", got)
	}
}

func TestExpandIndentedTokenLines(t *testing.T) {
	vals := map[string]string{"empty": "", "two": "a\nb\n", "one": "x"}
	cases := map[string]string{
		"p\n  {empty}\nq":     "p\nq",
		"p\n\t{empty}\nq":     "p\nq",
		"p\n  {two}\nq":       "p\n  a\n  b\n\nq",
		"  {one}\n":           "  x\n",
		"  {one} \n":          "  x \n",
		"  {nope}\n  {{x}}\n": "  {nope}\n  {{x}}\n",
	}
	for in, want := range cases {
		if got := Expand(in, vals); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}
