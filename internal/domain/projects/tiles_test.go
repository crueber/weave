package projects

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crueber/weave/internal/domain/boards"
)

func cols(spec ...string) []boards.ColumnStat {
	var out []boards.ColumnStat
	for i, s := range spec { // "Name:cards" ; a leading ! marks a closed column
		var name string
		var n int
		typ := "active"
		if strings.HasPrefix(s, "!") {
			typ, s = "closed", s[1:]
		}
		j := strings.LastIndex(s, ":")
		name = s[:j]
		fmt.Sscan(s[j+1:], &n)
		out = append(out, boards.ColumnStat{ListID: fmt.Sprint(i), Name: name, Type: typ, Cards: n})
	}
	return out
}

func rowsText(rs []statRow) string {
	var p []string
	for _, r := range rs {
		p = append(p, fmt.Sprintf("%s=%d/%s", r.Label, r.Cards, r.Kind))
	}
	return strings.Join(p, " ")
}

func TestStatRowsBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []boards.ColumnStat
		want string
	}{
		{"3 columns: all, board order, zeros kept", cols("A:2", "B:0", "!Done:5"), "A=2/column B=0/column Done=5/done"},
		{"4 columns: all", cols("A:1", "B:0", "C:3", "!Done:0"), "A=1/column B=0/column C=3/column Done=0/done"},
		{"5 columns, one closed: done row + the 3 open columns that fit",
			cols("A:1", "B:4", "C:0", "D:2", "!Shipped:6"), "A=1/column B=4/column D=2/column Shipped=6/done"},
		{"5 open columns, nothing done: 3 largest + more",
			cols("A:1", "B:4", "C:3", "D:2", "E:9"), "B=4/column C=3/column E=9/column 2 more columns=3/more"},
		{"9 columns, two closed merged into Done, zero columns omitted",
			cols("Inbox:3", "Triage:0", "Backlog:12", "Ready:4", "Doing:6", "Review:0", "QA:1", "!Done:10", "!Won't do:2"),
			"Backlog=12/column Doing=6/column Done=12/done 3 more columns=8/more"},
		{"over the cap, closed columns empty: no Done row",
			cols("A:5", "B:0", "C:0", "D:1", "!Done:0"), "A=5/column D=1/column"},
	} {
		got := statRows(tc.in, maxStatRows)
		if s := rowsText(got); s != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, s, tc.want)
		}
		if len(got) > maxStatRows {
			t.Errorf("%s: %d rows", tc.name, len(got))
		}
	}
	// 14 columns never exceed the budget
	var many []string
	for i := 0; i < 14; i++ {
		many = append(many, fmt.Sprintf("Column with a long name %d:%d", i, i+1))
	}
	if got := statRows(cols(many...), maxStatRows); len(got) != 4 || got[3].Kind != "more" {
		t.Fatalf("14 columns: %s", rowsText(got))
	}
	// bars scale to the largest shown row; non-empty rows keep a sliver, empty ones none
	got := statRows(cols("A:100", "B:1", "C:0"), maxStatRows)
	if got[0].Width != "--w:100%" || got[1].Width != "--w:4%" || got[2].Width != "--w:0%" {
		t.Fatalf("widths %v %v %v", got[0].Width, got[1].Width, got[2].Width)
	}
}

func TestNewTileConditionalParts(t *testing.T) {
	now := time.Now()
	quiet := boards.BoardStats{Name: "Loom", Open: 3, Done: 1, Activity7d: make([]int, 7), LastActivityAt: now.Add(-60 * 24 * time.Hour).UTC().Format(time.RFC3339),
		PerColumn: cols("A:3", "!Done:1")}
	tl := newTile(quiet, now)
	if tl.Empty || tl.Spark != nil || tl.Ago == "" || tl.MoreMembers != 0 {
		t.Fatalf("quiet board: %+v", tl)
	}
	for _, no := range []string{"overdue", "due this week", "claimed", "listening", "member"} {
		if strings.Contains(tl.Summary, no) {
			t.Errorf("summary mentions %q for a zero count: %s", no, tl.Summary)
		}
	}
	busy := boards.BoardStats{Open: 42, Done: 11, Overdue: 3, DueSoon: 5, Claimed: 2, Listening: 2, Members: 6,
		MemberSample: []boards.StatMember{{ID: "a"}, {ID: "b"}, {ID: "c"}}, Activity7d: []int{0, 1, 0, 4, 2, 0, 8},
		LastActivityAt: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339), PerColumn: cols("A:42", "!Done:11")}
	tl = newTile(busy, now)
	if tl.Ago != "2h ago" || tl.MoreMembers != 3 || len(tl.Spark) != 7 || !tl.Spark[0].Zero || tl.Spark[6].H != "--h:100%" {
		t.Fatalf("busy board: %+v", tl)
	}
	if want := "42 open cards, 11 done, 3 overdue, 5 due this week, 2 cards claimed, 2 agents listening, 6 members, 15 changes in the last 7 days, active 2 hours ago"; tl.Summary != want {
		t.Fatalf("summary\n got %s\nwant %s", tl.Summary, want)
	}
	if e := newTile(boards.BoardStats{Activity7d: make([]int, 7)}, now); !e.Empty || e.Rows != nil {
		t.Fatalf("no cards: %+v", e)
	}
}

func TestTileCSS(t *testing.T) {
	b, err := os.ReadFile("../../web/static/projects_ui.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	for _, want := range []string{".projects-board{display:flex;flex-direction:column", "max-height:16rem", "minmax(min(18rem,100%),1fr)", "prefers-reduced-motion"} {
		if !strings.Contains(css, want) {
			t.Errorf("projects_ui.css missing %q", want)
		}
	}
}
