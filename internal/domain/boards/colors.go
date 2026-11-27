package boards

import (
	"html/template"
	"math"
	"regexp"
	"strings"

	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// Colour model
//
// A colour is either a lowercase "#rrggbb" or, for compatibility, a Planka palette name
// (labels and old lists store those) or one of the Weave presets below.
//
//   - boards.color and lists.color written through the ops are ALWAYS normalised to
//     lowercase #rrggbb (presets and Planka names are resolved on write), so the value
//     is portable (export/import, project page) and has exactly one validator.
//   - labels.color keeps whatever the caller sent (name or hex) after strict validation.
//   - Rendering resolves any stored value through ResolveColor; an unknown value renders
//     as "no colour", never as raw text. Inline style values are only ever emitted from a
//     validated #rrggbb (see colorVarAttr), never from the stored string.

var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Colors are the label / list colour names accepted by Weave (Planka palette).
var Colors = []string{"muddy-grey", "autumn-leafs", "morning-sky", "antique-blue", "egg-yellow", "desert-sand", "dark-granite",
	"fresh-salad", "lagoon-blue", "midnight-blue", "light-orange", "pumpkin-orange", "light-concrete", "sunny-grass", "navy-blue",
	"lilac-eyes", "apricot-red", "orange-peel", "silver-glint", "bright-moss", "deep-ocean", "summer-sky", "berry-red", "light-cocoa",
	"grey-stone", "tank-green", "coral-green", "sugar-plum", "pink-tulip", "shady-rust", "wet-rock", "wet-moss", "turquoise-sea",
	"lavender-fields", "piggy-red", "light-mud", "gun-metal", "modern-green", "french-coast", "sweet-lilac", "red-burgundy", "pirate-gold"}

var colorHex = map[string]string{
	"muddy-grey": "#8a8f98", "autumn-leafs": "#b4572d", "morning-sky": "#8ec5e6", "antique-blue": "#5b7f95", "egg-yellow": "#f2d04c",
	"desert-sand": "#d9bc8c", "dark-granite": "#4b535c", "fresh-salad": "#7cc576", "lagoon-blue": "#2e9bb8", "midnight-blue": "#1f3a6e",
	"light-orange": "#f5a65b", "pumpkin-orange": "#e8741e", "light-concrete": "#c3c7cc", "sunny-grass": "#a6d142", "navy-blue": "#26407a",
	"lilac-eyes": "#a98bd0", "apricot-red": "#ee7d6a", "orange-peel": "#f59a2c", "silver-glint": "#aeb6bf", "bright-moss": "#5fae3d",
	"deep-ocean": "#1b5e86", "summer-sky": "#58b4e8", "berry-red": "#c8324a", "light-cocoa": "#a98672", "grey-stone": "#7d858e",
	"tank-green": "#4f6b3c", "coral-green": "#3fa88a", "sugar-plum": "#8c4a7d", "pink-tulip": "#ee86b4", "shady-rust": "#8c4b2f",
	"wet-rock": "#566370", "wet-moss": "#5d7a4f", "turquoise-sea": "#2bc3c0", "lavender-fields": "#9b95e0", "piggy-red": "#f29aa5",
	"light-mud": "#b9a58a", "gun-metal": "#3a4249", "modern-green": "#2f9e6a", "french-coast": "#4a7fd1", "sweet-lilac": "#c9a6e8",
	"red-burgundy": "#8a1f33", "pirate-gold": "#c9951f",
}

// validColor accepts exactly #rrggbb, a Planka palette name or a Weave preset name.
func validColor(c string) bool {
	_, ok := ResolveColor(c)
	return ok
}

// colorStr extracts a colour string from a string or *string template argument.
func colorStr(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case *string:
		if v != nil {
			return *v
		}
	}
	return ""
}

// Preset is a named palette colour: a mid-tone that stays legible as a flat 3px bar in
// both themes and is only ever used as a tint (12-22%) over large surfaces.
type Preset struct{ Name, Label, Hex string }

// Presets is the curated palette offered by the board and list colour pickers.
var Presets = []Preset{
	{"crimson", "Crimson", "#d6455d"},
	{"orange", "Orange", "#e8772e"},
	{"amber", "Amber", "#e0a526"},
	{"lime", "Lime", "#7fb83f"},
	{"green", "Green", "#2f9e6a"},
	{"teal", "Teal", "#1fa1a1"},
	{"sky", "Sky", "#3a9bdc"},
	{"blue", "Blue", "#3e63dd"},
	{"indigo", "Indigo", "#6a5fd6"},
	{"violet", "Violet", "#9a5fd0"},
	{"pink", "Pink", "#d6579a"},
	{"slate", "Slate", "#6b7a8c"},
}

var presetHex = func() map[string]string {
	m := map[string]string{}
	for _, p := range Presets {
		m[p.Name] = p.Hex
	}
	return m
}()

// IsHexColor reports whether s is exactly #rrggbb.
func IsHexColor(s string) bool { return hexColorRe.MatchString(s) }

// ResolveColor maps a stored/accepted colour (hex, preset name or Planka name) to a
// lowercase #rrggbb. ok is false for anything else (including the empty string).
func ResolveColor(s string) (hex string, ok bool) {
	s = strings.TrimSpace(s)
	switch {
	case IsHexColor(s):
		return strings.ToLower(s), true
	case presetHex[s] != "":
		return presetHex[s], true
	case colorHex[s] != "":
		return colorHex[s], true
	}
	return "", false
}

// NormalizeColor validates a colour for a board or list: "" clears it (returns ""), a
// preset or Planka name resolves to hex, #rrggbb is lowercased; anything else is an error.
func NormalizeColor(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if h, ok := ResolveColor(s); ok {
		return h, nil
	}
	return "", ops.Invalid("invalid color: use #rrggbb, a preset name (" + presetNames() + ") or empty to clear")
}

func presetNames() string {
	n := make([]string, len(Presets))
	for i, p := range Presets {
		n[i] = p.Name
	}
	return strings.Join(n, ", ")
}

// Card colouring modes (boards.card_color_mode).
const (
	CardColorPill = "pill"
	CardColorFill = "fill"
)

func validCardColorMode(m string) bool { return m == CardColorPill || m == CardColorFill }

// FillColor is the card-fill precedence rule: the FIRST label of the card (labels are
// ordered by label position ascending, then id, by loadTilesOnly) decides the colour.
// Colours are never mixed. Returns "" when the board is not in fill mode, the card has
// no labels, or the first label has no resolvable colour.
func FillColor(mode string, labels []TileLabel) string {
	if mode != CardColorFill || len(labels) == 0 {
		return ""
	}
	h, _ := ResolveColor(labels[0].Color)
	return h
}

// ChipLabels returns the labels of a filled card that still get a chip. The first label is the card colour
// (the fill already shows it), so it is skipped; unnamed labels have nothing to print.
func ChipLabels(mode string, labels []TileLabel) []TileLabel {
	if FillColor(mode, labels) == "" {
		return nil
	}
	var out []TileLabel
	for _, l := range labels[1:] {
		if l.Name != "" {
			out = append(out, l)
		}
	}
	return out
}

// colorVarAttr renders ` <extra> style="--name:#rrggbb"` from a validated colour, or ""
// when c does not resolve. Only the validated hex ever reaches the attribute.
func colorVarAttr(name, extra string, c any) template.HTMLAttr {
	h, ok := ResolveColor(colorStr(c))
	if !ok {
		return ""
	}
	return template.HTMLAttr(" " + extra + ` style="` + name + `:` + h + `"`)
}

// Contrast helpers (WCAG relative luminance); used for tests and available to renderers
// that need a readable foreground on a saturated fill. The shipped design uses tints so
// --fg always works.

func srgbLin(v float64) float64 {
	if v <= 0.03928 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

// Luminance returns the WCAG relative luminance of #rrggbb (0 for invalid input).
func Luminance(hex string) float64 {
	h, ok := ResolveColor(hex)
	if !ok {
		return 0
	}
	var c [3]float64
	for i := 0; i < 3; i++ {
		var n int
		for _, ch := range h[1+2*i : 3+2*i] {
			n = n*16 + hexVal(byte(ch))
		}
		c[i] = srgbLin(float64(n) / 255)
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

func hexVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	}
	return 0
}

// Contrast is the WCAG contrast ratio between two colours.
func Contrast(a, b string) float64 {
	la, lb := Luminance(a), Luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// ReadableOn returns "#ffffff" or "#000000", whichever has the higher contrast on bg.
func ReadableOn(bg string) string {
	if Contrast(bg, "#ffffff") >= Contrast(bg, "#000000") {
		return "#ffffff"
	}
	return "#000000"
}

func init() {
	web.AddFuncs(template.FuncMap{
		// boardsListAttr: attributes for a column root; empty when the list has no colour.
		"boardsListAttr": func(c any) template.HTMLAttr { return colorVarAttr("--list-color", "data-colored", c) },
		// boardsBoardAttr: attributes for the board shell.
		"boardsBoardAttr": func(b Board) template.HTMLAttr {
			if b.Color == nil {
				return ""
			}
			return colorVarAttr("--board-color", "data-board-color", *b.Color)
		},
		// boardsTileFill: attributes for a tile root in fill mode.
		"boardsTileFill": func(t Tile, p *PageData) template.HTMLAttr {
			if p == nil {
				return ""
			}
			h := FillColor(p.Board.CardColorMode, t.Labels)
			if h == "" {
				return ""
			}
			return template.HTMLAttr(` data-card-fill="1" style="--card-color:` + h + `"`)
		},
		// boardsTileFilled: true when the tile is drawn as a filled card (fill mode and its first label has a colour).
		"boardsTileFilled": func(t Tile, p *PageData) bool { return p != nil && FillColor(p.Board.CardColorMode, t.Labels) != "" },
		// boardsChipLabels: in fill mode, the labels that still get a (neutral text) chip: every label except
		// the first one, whose colour is the card fill. Unnamed labels carry no text and are skipped.
		"boardsChipLabels": func(t Tile, p *PageData) []TileLabel { return ChipLabels(p.Board.CardColorMode, t.Labels) },
		"boardsPresets":    func() []Preset { return Presets },
		"boardsHexOf": func(c any) string {
			h, _ := ResolveColor(colorStr(c))
			return h
		},
	})
}
