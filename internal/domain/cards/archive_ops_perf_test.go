//go:build !race

// Timing guards run in normal builds; SQLite instrumentation under -race is not representative.
package cards

import (
	"testing"
	"time"
)

// The archive page reads one 50-row page from a board with 10,000 archived placements. Setup is one transaction and
// emits no events; only card.archive.list is timed. The hard bound is deliberately generous for loaded CI machines,
// while the logged measurement makes the plan's <150 ms goal visible.
func TestArchiveListTenKTiming(t *testing.T) {
	f := seed(t)
	const n = 10_000
	ids := archiveViewBatch(t, f.e, f.board, f.todo, f.arch, "perf", "Archived", "2026-01-01T00:00:00Z", n)
	var setupEvents int
	_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE card_id GLOB 'perf-*'`).Scan(&setupEvents)
	if setupEvents != 0 {
		t.Fatalf("fixture emitted %d events for 10,000 archived cards", setupEvents)
	}

	start := time.Now()
	out := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Limit: 50})
	elapsed := time.Since(start)
	t.Logf("card.archive.list with %d archived placements, limit 50: %s", n, elapsed)
	if elapsed >= time.Second {
		t.Fatalf("archive list took %s, want <1s", elapsed)
	}
	if out.Total != len(ids) || len(out.Items) != 50 || out.NextCursor == "" {
		t.Fatalf("total=%d rows=%d nextCursor=%q", out.Total, len(out.Items), out.NextCursor)
	}
}
