package projects

import (
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/crueber/weave/internal/domain/boards"
)

// Board tiles on the project page ("Pulse card"): a headline open count, at most maxStatRows per-column bars,
// chips that exist only when their count is > 0, members, a 7-day activity sparkline and "active 2h ago".
// The numbers come from boards.Stats (the projects domain never reads cards or lists itself). Everything here is
// computed once per page load; the tiles never re-sort or re-render while the page is open.

// maxStatRows is the fixed budget of per-column rows on a tile, however many columns the board has.
const maxStatRows = 4

// statRow is one mini bar on a tile.
type statRow struct {
	Label  string
	Cards  int
	Kind   string       // column | done (closed columns) | more (the merged rest, muted)
	Merged int          // columns merged into a done/more row
	Width  template.CSS // --w:NN% of the largest shown row (server-generated integer)
}

// sparkBar is one day of the activity sparkline: a height (--h:NN%, server-generated) or a flat zero tick.
type sparkBar struct {
	H    template.CSS
	Zero bool
}

// boardTile is the view model of one board tile.
type boardTile struct {
	Empty       bool // no cards at all: name + "No cards yet" only
	Open, Done  int
	Rows        []statRow
	Overdue     int
	DueSoon     int
	Claimed     int
	Listening   int
	Avatars     []boards.StatMember
	MoreMembers int
	Members     int
	Spark       []sparkBar // 7 bars, oldest first; nil when there was no activity in the last 7 days
	SparkTitle  string
	Ago         string // "2h ago"; empty when the board never had activity
	AgoLong     string // "2 hours ago" for the screen-reader sentence
	At          string // RFC3339 for <time datetime>
	Summary     string // visually hidden sentence with the same facts as the visual parts
}

// statRows applies the row budget. With <= max columns every column shows in board order (zero counts too).
// With more: all closed columns merge into one "Done" row (if it holds cards), the largest open columns (kept in
// board order) fill the remaining rows, and the rest merge into one muted "N more columns" row; zero-card columns
// are left out. Bars are scaled to the largest shown row.
func statRows(cols []boards.ColumnStat, max int) []statRow {
	var rows []statRow
	if len(cols) <= max {
		for _, c := range cols {
			k := "column"
			if c.Type == "closed" {
				k = "done"
			}
			rows = append(rows, statRow{Label: c.Name, Cards: c.Cards, Kind: k})
		}
		return scaleRows(rows)
	}
	done, closedCols, doneCols := 0, 0, 0
	doneName := ""
	var open []int // indexes of open columns with cards, board order
	for i, c := range cols {
		switch {
		case c.Type == "closed":
			closedCols++
			doneName = c.Name
			done += c.Cards
			if c.Cards > 0 {
				doneCols++
			}
		case c.Cards > 0:
			open = append(open, i)
		}
	}
	slots := max
	if done > 0 {
		slots--
	}
	keep := map[int]bool{}
	var rest []int
	if len(open) <= slots {
		for _, i := range open {
			keep[i] = true
		}
	} else {
		bySize := append([]int(nil), open...)
		sort.SliceStable(bySize, func(a, b int) bool { return cols[bySize[a]].Cards > cols[bySize[b]].Cards })
		for _, i := range bySize[:slots-1] {
			keep[i] = true
		}
		for _, i := range open {
			if !keep[i] {
				rest = append(rest, i)
			}
		}
	}
	for _, i := range open {
		if keep[i] {
			rows = append(rows, statRow{Label: cols[i].Name, Cards: cols[i].Cards, Kind: "column"})
		}
	}
	if done > 0 {
		label := "Done"
		if closedCols == 1 && strings.TrimSpace(doneName) != "" {
			label = doneName
		}
		rows = append(rows, statRow{Label: label, Cards: done, Kind: "done", Merged: doneCols})
	}
	if len(rest) > 0 {
		n := 0
		for _, i := range rest {
			n += cols[i].Cards
		}
		rows = append(rows, statRow{Label: plural(len(rest), "more column", "more columns"), Cards: n, Kind: "more", Merged: len(rest)})
	}
	return scaleRows(rows)
}

func scaleRows(rows []statRow) []statRow {
	top := 0
	for _, r := range rows {
		top = max(top, r.Cards)
	}
	for i := range rows {
		w := 0
		if top > 0 && rows[i].Cards > 0 {
			w = max(rows[i].Cards*100/top, 4) // a non-empty column always shows a sliver
		}
		rows[i].Width = template.CSS(fmt.Sprintf("--w:%d%%", w))
	}
	return rows
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// ago renders a relative time: short ("2h ago") for the tile, long ("2 hours ago") for screen readers.
func ago(at string, now time.Time) (short, long string) {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return "", ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now", "just now"
	case d < time.Hour:
		n := int(d.Minutes())
		return fmt.Sprintf("%dm ago", n), plural(n, "minute", "minutes") + " ago"
	case d < 24*time.Hour:
		n := int(d.Hours())
		return fmt.Sprintf("%dh ago", n), plural(n, "hour", "hours") + " ago"
	case d < 30*24*time.Hour:
		n := int(d.Hours() / 24)
		return fmt.Sprintf("%dd ago", n), plural(n, "day", "days") + " ago"
	}
	if t.Year() == now.Year() {
		s := "on " + t.Local().Format("Jan 2")
		return s, s
	}
	s := "on " + t.Local().Format("Jan 2, 2006")
	return s, s
}

// newTile builds a tile from a board's stats.
func newTile(s boards.BoardStats, now time.Time) *boardTile {
	t := &boardTile{Open: s.Open, Done: s.Done, Overdue: s.Overdue, DueSoon: s.DueSoon, Claimed: s.Claimed, Listening: s.Listening, Members: s.Members}
	t.Ago, t.AgoLong = ago(s.LastActivityAt, now)
	t.At = s.LastActivityAt
	if s.Cards() == 0 {
		t.Empty = true
		return t
	}
	t.Rows = statRows(s.PerColumn, maxStatRows)
	t.Avatars = s.MemberSample
	t.MoreMembers = max(s.Members-len(s.MemberSample), 0)
	top, total := 0, 0
	for _, n := range s.Activity7d {
		top, total = max(top, n), total+n
	}
	if total > 0 {
		for _, n := range s.Activity7d {
			if n == 0 {
				t.Spark = append(t.Spark, sparkBar{Zero: true})
				continue
			}
			t.Spark = append(t.Spark, sparkBar{H: template.CSS(fmt.Sprintf("--h:%d%%", max(n*100/top, 15)))})
		}
		t.SparkTitle = plural(total, "change", "changes") + " in the last 7 days"
	}
	// The sentence a screen reader hears after the board name (the visual parts are aria-hidden).
	parts := []string{plural(s.Open, "open card", "open cards")}
	if s.Done > 0 {
		parts = append(parts, fmt.Sprintf("%d done", s.Done))
	}
	if s.Overdue > 0 {
		parts = append(parts, fmt.Sprintf("%d overdue", s.Overdue))
	}
	if s.DueSoon > 0 {
		parts = append(parts, fmt.Sprintf("%d due this week", s.DueSoon))
	}
	if s.Claimed > 0 {
		parts = append(parts, plural(s.Claimed, "card claimed", "cards claimed"))
	}
	if s.Listening > 0 {
		parts = append(parts, plural(s.Listening, "agent listening", "agents listening"))
	}
	if s.Members > 0 {
		parts = append(parts, plural(s.Members, "member", "members"))
	}
	if t.SparkTitle != "" {
		parts = append(parts, t.SparkTitle)
	}
	if t.AgoLong != "" {
		parts = append(parts, "active "+t.AgoLong)
	}
	t.Summary = strings.Join(parts, ", ")
	return t
}
