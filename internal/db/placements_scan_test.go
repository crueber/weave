package db

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The columns cards.board_id/list_id/position/prev_list_id/list_changed_at mirror the home placement (migration 0404):
// writers update card_placements. The guard trigger rejects a direct write at run time, but only on paths a test
// executes, and some old helpers discarded errors. This scan fails on any Go source (tests included) that writes one
// of those columns through an UPDATE of cards, or hands the cards table to the generic position helpers
// (boards.PlaceBetween / Rebalance write `position` by table name). Only the trigger tests may do it on purpose.

// placementScanAllow: file -> why it may write the mirrored columns directly.
var placementScanAllow = map[string]string{
	"internal/db/migrate_placements_test.go": "proves the guard rejects direct writes",
	"internal/db/placements_scan_test.go":    "the detector's own positive examples",
}

var (
	updateCardsSet = regexp.MustCompile(`(?i)UPDATE\s+cards\s+SET\b`)
	whereKw        = regexp.MustCompile(`(?i)\bWHERE\b`)
	mirroredAssign = regexp.MustCompile(`(?i)\b(board_id|list_id|position|prev_list_id|list_changed_at)\s*=`)
	genericCards   = regexp.MustCompile(`\b(PlaceBetween|Rebalance)\([^;\n]*"cards"`)
)

// mirroredWrites returns a description of each offending statement in src.
func mirroredWrites(src string) []string {
	var out []string
	for _, loc := range updateCardsSet.FindAllStringIndex(src, -1) {
		span := src[loc[1]:min(len(src), loc[1]+1200)] // the SET list may be assembled over several string pieces
		if w := whereKw.FindStringIndex(span); w != nil {
			span = span[:w[0]]
		}
		if m := mirroredAssign.FindStringSubmatch(span); m != nil {
			out = append(out, "UPDATE cards SET ... "+m[1]+"=")
		}
	}
	for _, m := range genericCards.FindAllString(src, -1) {
		out = append(out, m)
	}
	return out
}

func TestMirroredColumnsDetector(t *testing.T) {
	for src, want := range map[string]int{
		"`UPDATE cards SET is_closed=? WHERE list_id=? AND is_draft=0`":                                                       0,
		"`UPDATE cards SET updated_at=? WHERE id=?`":                                                                          0,
		"`UPDATE card_placements SET list_id=?,position=? WHERE card_id=?`":                                                   0,
		"`UPDATE cards SET list_id=?,board_id=?,position=? WHERE id=?`":                                                       1,
		"`UPDATE cards\n\t\tSET position=? WHERE id=?`":                                                                       1,
		"`UPDATE cards SET list_id=?,updated_at=?,position=CASE id`+strings.Repeat(\" WHEN ? THEN ?\", n)+` END WHERE id IN`": 1,
		"`update cards set prev_list_id = NULL`":                                                                              1,
		`pos := PlaceBetween(ctx, d, "cards", "list_id=?", args, a, b)`:                                                       1,
		`pos := PlaceBetween(ctx, d, "labels", "board_id=?", args, a, b)`:                                                     0,
	} {
		if got := len(mirroredWrites(src)); got != want {
			t.Errorf("%q: %d findings, want %d", src, got, want)
		}
	}
}

func TestNoDirectWritesOfMirroredCardColumns(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	seen := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			seen++
			if placementScanAllow[rel] != "" {
				return nil
			}
			for _, f := range mirroredWrites(string(b)) {
				bad = append(bad, rel+": "+f)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if seen < 100 {
		t.Fatalf("scanned only %d Go files; wrong root %s?", seen, root)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s: cards.board_id/list_id/position/prev_list_id/list_changed_at mirror the home placement; update card_placements instead (docs/DESIGN.md \"Placements\")", b)
	}
}
