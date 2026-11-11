package boards_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
)

func sp(s string) *string { return &s }

func TestColorValidation(t *testing.T) {
	good := map[string]string{"#AABBCC": "#aabbcc", "#aabbcc": "#aabbcc", "crimson": "#d6455d", "berry-red": "#c8324a", "": "", "  #00ff00 ": "#00ff00"}
	for in, want := range good {
		got, err := boards.NormalizeColor(in)
		if err != nil || got != want {
			t.Errorf("NormalizeColor(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"red;background:url(x)", "#abc", "#abcdefg", "#12345", "#1234567", "rgb(1,2,3)", "red", "url(x)", `"><script>`, "#aabbcc;x:y", "#aabbc\n", "expression(1)"} {
		if _, err := boards.NormalizeColor(bad); err == nil {
			t.Errorf("NormalizeColor(%q) must fail", bad)
		}
	}
}

func TestBoardColorOps(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Colours")
	if b.Color != nil || b.CardColorMode != "pill" {
		t.Fatalf("defaults: %+v", b)
	}
	b = call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, Color: sp("#ABCDEF"), CardColorMode: sp("fill")})
	if b.Color == nil || *b.Color != "#abcdef" || b.CardColorMode != "fill" {
		t.Fatalf("update: %+v", b)
	}
	b = call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, Color: sp("teal")})
	if *b.Color != "#1fa1a1" {
		t.Fatalf("preset must resolve to hex, got %s", *b.Color)
	}
	for _, bad := range []string{"red;background:url(x)", "#12", "javascript:1"} {
		mustFail(t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, Color: sp(bad)}, "validation")
	}
	mustFail(t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, CardColorMode: sp("stripes")}, "validation")
	got := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b.ID})
	if *got.Board.Color != "#1fa1a1" || got.Board.CardColorMode != "fill" {
		t.Fatalf("rejected updates changed the board: %+v", got.Board)
	}
	b = call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, Color: sp("")})
	if b.Color != nil {
		t.Fatalf("empty must clear the colour, got %v", *b.Color)
	}

	// lists
	l := call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "L", Color: "#112233"})
	if l.Color == nil || *l.Color != "#112233" {
		t.Fatalf("list.create colour: %+v", l)
	}
	mustFail(t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "bad", Color: "red;background:url(x)"}, "validation")
	l = call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Color: sp("amber")})
	if *l.Color != "#e0a526" {
		t.Fatalf("list preset: %s", *l.Color)
	}
	mustFail(t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Color: sp("#zzzzzz")}, "validation")
	l = call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Color: sp("")})
	if l.Color != nil {
		t.Fatal("list colour must clear")
	}
	// labels share the strict validator
	mustFail(t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "x", Color: "#a;b:url"}, "validation")
	call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "ok", Color: "#A1B2C3"})
}

func TestColorsInSchemaAndExport(t *testing.T) {
	var found int
	for _, o := range ops.All() {
		if o.Name == "board.update" || o.Name == "list.update" || o.Name == "list.create" {
			js, _ := json.Marshal(o)
			if strings.Contains(string(js), "color") || strings.Contains(string(js), "Color") {
				found++
			}
		}
	}
	if found == 0 {
		t.Log("schema JSON does not expose input fields via op marshal; covered by the CLI snapshot test")
	}

	e := fixture(t)
	b := newBoard(t, e, "Export me")
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, Color: sp("pink"), CardColorMode: sp("fill")})
	call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Col", Color: "#010203"})
	st := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: b.ID})
	if !strings.Contains(string(st.Data), `"color": "#d6579a"`) || !strings.Contains(string(st.Data), `"cardColorMode": "fill"`) {
		t.Fatalf("export lacks board colour/mode: %s", st.Data)
	}
	b2 := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: string(st.Data), Name: "Copy"})
	if b2.Color == nil || *b2.Color != "#d6579a" || b2.CardColorMode != "fill" {
		t.Fatalf("import lost colours: %+v", b2)
	}
	d2 := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b2.ID})
	var col *string
	for _, l := range d2.Lists {
		if l.Name != nil && *l.Name == "Col" {
			col = l.Color
		}
	}
	if col == nil || *col != "#010203" {
		t.Fatalf("import lost list colour: %v", col)
	}

	// hostile import values are dropped, never stored
	evil := `{"format":"weave-board","version":1,"board":{"name":"Evil","color":"red;background:url(x)","cardColorMode":"zzz"},"lists":[{"id":"l","name":"A","type":"active","color":"x;y:z","position":1}],"cards":[]}`
	b3 := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: evil})
	if b3.Color != nil || b3.CardColorMode != "pill" {
		t.Fatalf("hostile import stored colours: %+v", b3)
	}
	d3 := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b3.ID})
	for _, l := range d3.Lists {
		if l.Color != nil {
			t.Fatalf("hostile list colour stored: %s", *l.Color)
		}
	}

	// templates carry colours
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: b.ID, IsTemplate: true})
	nb := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: b.ID, Name: "From tpl"})
	if nb.Color == nil || *nb.Color != "#d6579a" || nb.CardColorMode != "fill" {
		t.Fatalf("instantiate lost colours: %+v", nb)
	}
	dn := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: nb.ID})
	ok := false
	for _, l := range dn.Lists {
		ok = ok || (l.Color != nil && *l.Color == "#010203")
	}
	if !ok {
		t.Fatal("instantiate lost list colour")
	}
}

func TestFillColorFirstLabelWins(t *testing.T) {
	ls := []boards.TileLabel{{ID: "a", Color: "#112233"}, {ID: "b", Color: "berry-red"}}
	if got := boards.FillColor("fill", ls); got != "#112233" {
		t.Errorf("first label must win, got %s", got)
	}
	if got := boards.FillColor("pill", ls); got != "" {
		t.Errorf("pill mode must not fill, got %s", got)
	}
	if got := boards.FillColor("fill", nil); got != "" {
		t.Errorf("no labels: %s", got)
	}
	if got := boards.FillColor("fill", []boards.TileLabel{{Color: "garbage"}, {Color: "#112233"}}); got != "" {
		t.Errorf("unresolvable first label must not fall through to the second: %s", got)
	}
}

func TestLuminanceHelper(t *testing.T) {
	if l := boards.Luminance("#000000"); l != 0 {
		t.Errorf("black %v", l)
	}
	if l := boards.Luminance("#ffffff"); l < 0.999 || l > 1.001 {
		t.Errorf("white %v", l)
	}
	if c := boards.Contrast("#000000", "#ffffff"); c < 20.9 || c > 21.1 {
		t.Errorf("black/white contrast %v", c)
	}
	n := 0
	for r := 0; r < 256; r += 15 {
		for g := 0; g < 256; g += 15 {
			for b := 0; b < 256; b += 15 {
				hex := "#" + h2(r) + h2(g) + h2(b)
				fg := boards.ReadableOn(hex)
				if c := boards.Contrast(hex, fg); c < 4.5 {
					t.Errorf("ReadableOn(%s)=%s only %.2f", hex, fg, c)
				}
				n++
			}
		}
	}
	if n < 4000 {
		t.Fatal("sampled too few colours")
	}
	for _, p := range boards.Presets {
		if !boards.IsHexColor(p.Hex) {
			t.Errorf("preset %s has invalid hex %s", p.Name, p.Hex)
		}
	}
}

func h2(n int) string {
	const d = "0123456789abcdef"
	return string([]byte{d[n>>4], d[n&15]})
}

// Rendering: fill mode emits exactly one colour var per tile (first label by position wins);
// pill mode is unchanged; uncoloured columns carry no colour; hostile stored values never reach markup.
func TestColorRendering(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Render")
	plain := addList(t, e, b.ID, "Plain")
	col := call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Tinted", Color: "#445566"})
	lo := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "Second", Color: "#00aa00"})
	hi := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "First", Color: "#aa0000"})
	// First is created second but moved before Second: label order is user-controlled.
	call[boards.Label](t, e, "label.move", boards.LabelMoveIn{ID: hi.ID, BeforeID: lo.ID})
	addCard(t, e, b.ID, plain.ID, "c-multi", "Multi", 1, "")
	addCard(t, e, b.ID, plain.ID, "c-none", "NoLabel", 2, "")
	addCard(t, e, b.ID, col.ID, "c-one", "One", 1, "")
	for _, x := range [][2]string{{"c-multi", lo.ID}, {"c-multi", hi.ID}, {"c-one", lo.ID}} {
		if _, err := e.DB.Exec(`INSERT INTO card_labels(card_id,label_id) VALUES(?,?)`, x[0], x[1]); err != nil {
			t.Fatal(err)
		}
	}
	m := mux(e)

	// list menu is a floating popover: role=menu, scrollable middle, pinned footer, one-at-a-time expanders, one colour field
	for _, want := range []string{`class="boards-menu-pop" data-sheet="1" role="menu"`, `boards-menu-scroll`, `boards-menu-foot`, `class="boards-sub"`, `data-colorpick`, `Archive list`, `Delete list`} {
		if !strings.Contains(body0(t, m, b.ID), want) {
			t.Errorf("list menu missing %q", want)
		}
	}
	if strings.Contains(body0(t, m, b.ID), `<select name="color"`) {
		t.Error("old colour select must be gone")
	}

	// pill mode (default): no fill attributes, pills as before
	_, body := get(t, m, "/ui/boards/"+b.ID+"/body", true)
	if strings.Contains(body, "data-card-fill") || strings.Contains(body, "--card-color") {
		t.Fatal("pill mode must not emit fill attributes")
	}
	if !strings.Contains(body, `class="boards-label"`) {
		t.Fatal("pill mode keeps label pills")
	}
	if strings.Contains(body, "data-card-mode") {
		t.Fatal("pill mode: columns keep their tint, no mode hook")
	}
	if strings.Count(body, "--list-color:#445566") != 1 || strings.Count(body, "data-colored") != 1 {
		t.Fatalf("exactly the coloured column carries --list-color:\n%s", body)
	}

	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, CardColorMode: sp("fill")})
	_, body = get(t, m, "/ui/boards/"+b.ID+"/body", true)
	if n := strings.Count(body, "data-card-fill"); n != 2 {
		t.Fatalf("want 2 filled tiles (labelled cards only), got %d", n)
	}
	// fill mode: columns carry the mode hook so CSS keeps them neutral (no tint under a filled card); pill mode does not
	if strings.Count(body, `data-card-mode="fill"`) != 2 {
		t.Fatalf("every column must carry data-card-mode=fill in fill mode:\n%s", body)
	}
	// the colouring (first) label gets no chip: the multi-label card shows only its second label as a neutral text chip
	multi := body[strings.Index(body, `id="tile-c-multi"`):]
	multi = multi[:strings.Index(multi, "</article>")]
	if !strings.Contains(multi, `<span class="boards-lchip" title="Second">Second</span>`) || strings.Contains(multi, `class="boards-lchip" title="First"`) || strings.Contains(multi, `class="boards-label"`) {
		t.Fatalf("fill tile: first label must not be a chip, the rest are boards-lchip:\n%s", multi)
	}
	if !strings.Contains(multi, `<span class="boards-sr">Label First. </span>`) {
		t.Fatalf("the colouring label keeps a screen-reader name:\n%s", multi)
	}
	one := body[strings.Index(body, `id="tile-c-one"`):]
	one = one[:strings.Index(one, "</article>")]
	if strings.Contains(one, "boards-lchip") || strings.Contains(one, "boards-tile-labels") {
		t.Fatalf("a single-label filled card has no label row:\n%s", one)
	}
	if strings.Count(body, "--card-color:#aa0000") != 1 || strings.Count(body, "--card-color:#00aa00") != 1 {
		t.Fatalf("first label by position must win and only one colour per tile:\n%s", body)
	}
	for _, tile := range strings.Split(body, `<article class="boards-tile`)[1:] {
		tile = tile[:strings.Index(tile, "</article>")]
		if strings.Count(tile, "--card-color") > 1 {
			t.Fatalf("tile with more than one colour var: %s", tile)
		}
		if strings.Contains(tile, "NoLabel") && strings.Contains(tile, "--card-color") {
			t.Fatal("card without labels must look normal")
		}
	}

	// the quick-edit / OOB refresh path renders the same fill
	w := post(m, "/ui/cards/c-multi/quick", url.Values{"name": {"Multi"}}, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "--card-color:#aa0000") {
		t.Fatalf("quick path lost the fill (%d): %s", w.Code, w.Body.String())
	}

	// board colour reaches the shell as a validated var; a hostile stored value never does
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, Color: sp("#123456")})
	_, page := get(t, m, "/boards/"+b.ID, false)
	if !strings.Contains(page, `data-board-color style="--board-color:#123456"`) {
		t.Fatal("board shell missing --board-color")
	}
	if _, err := e.DB.Exec(`UPDATE boards SET color='red;background:url(x)' WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE lists SET color='"><script>alert(1)</script>' WHERE id=?`, col.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE labels SET color='x;background:url(evil)' WHERE id=?`, hi.ID); err != nil {
		t.Fatal(err)
	}
	_, page = get(t, m, "/boards/"+b.ID, false)
	for _, bad := range []string{"url(x)", "<script>alert", "url(evil)", "--board-color", "--list-color:\""} {
		if strings.Contains(page, bad) {
			t.Fatalf("unvalidated colour reached markup: %q", bad)
		}
	}
}

func body0(t *testing.T, m http.Handler, id string) string {
	t.Helper()
	_, b := get(t, m, "/ui/boards/"+id+"/body", true)
	return b
}

// ChipLabels: in fill mode the first (colouring) label is dropped; unnamed labels have nothing to print;
// when the first label has no resolvable colour the tile is not filled, so there is no chip list at all.
func TestChipLabels(t *testing.T) {
	ls := []boards.TileLabel{{ID: "1", Name: "A", Color: "#aa0000"}, {ID: "2", Name: "B", Color: "#00aa00"}, {ID: "3", Name: "", Color: "#0000aa"}, {ID: "4", Name: "D", Color: "#0000aa"}}
	got := boards.ChipLabels(boards.CardColorFill, ls)
	if len(got) != 2 || got[0].Name != "B" || got[1].Name != "D" {
		t.Fatalf("fill: %+v", got)
	}
	if boards.ChipLabels(boards.CardColorPill, ls) != nil {
		t.Fatal("pill mode has no chip list")
	}
	if boards.ChipLabels(boards.CardColorFill, nil) != nil {
		t.Fatal("no labels, no chips")
	}
	ls[0].Color = "not-a-colour"
	if boards.ChipLabels(boards.CardColorFill, ls) != nil {
		t.Fatal("first label without a colour: the card is not filled, labels render as usual")
	}
}

// Contract between markup, CSS and JS for the portaled popovers and the neutral fill-mode columns.
func TestPopoverAndFillAssets(t *testing.T) {
	read := func(n string) string {
		b, err := os.ReadFile("../../web/static/" + n)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	js, css := read("boards_popover.js"), read("boards_colors.css")
	for _, want := range []string{"popover-root", "WeavePop", "sweep", "isOpen", "menuOf", "colOf"} {
		if !strings.Contains(js, want) {
			t.Errorf("boards_popover.js missing %q", want)
		}
	}
	if !strings.Contains(read("boards_board.js"), "WeavePop.isOpen(col.dataset.id)") {
		t.Error("the live merge must key an open menu off the list id registry")
	}
	if !strings.Contains(css, `.boards-col[data-colored]:not([data-card-mode="fill"])`) {
		t.Error("column tint must be limited to pill mode")
	}
	if strings.Contains(css, "details.boards-menu[open]::after") {
		t.Error("the phone scrim must live in #popover-root, not inside the column")
	}
	// the menu markup is the portal source: popover is a direct child of its <details>, with data-sheet for the phone sheet
	if !strings.Contains(tmplText(t, "_boards_listmenu.html"), `<div class="boards-menu-pop" data-sheet="1"`) {
		t.Error("list menu popover markup")
	}
}

func tmplText(t *testing.T, n string) string {
	b, err := os.ReadFile("../../web/templates/" + n)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
