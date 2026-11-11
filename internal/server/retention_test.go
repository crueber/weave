package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/crueber/weave/internal/events"
)

func TestRetentionKeepsRecentAndNeverReusesIDs(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Retention")
	e.card(bd.a, "old")
	e.card(bd.a, "recent")
	// age the first 40 days (inside the 90 day default), then purge at a 30 day window: both stay
	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := e.d.Exec(`UPDATE actions SET created_at=? WHERE card_id IS NOT NULL AND seq=(SELECT MAX(seq)-1 FROM actions)`, old); err != nil {
		t.Fatal(err)
	}
	if n, _ := events.Purge(context.Background(), e.d, 90*24*time.Hour); n != 0 {
		t.Fatalf("purged %d within retention", n)
	}
	rows, _ := events.Query(context.Background(), e.d, events.Filter{Boards: []string{bd.id}, Types: []string{"card.created"}})
	if len(rows) != 2 {
		t.Fatalf("replayable %d, want 2", len(rows))
	}
	// beyond the window the payload goes but the sequence keeps counting
	if n, _ := events.Purge(context.Background(), e.d, 30*24*time.Hour); n == 0 {
		t.Fatal("nothing purged past the window")
	}
	rows, _ = events.Query(context.Background(), e.d, events.Filter{Boards: []string{bd.id}, Types: []string{"card.created"}})
	if len(rows) != 1 {
		t.Fatalf("replayable after purge %d, want 1", len(rows))
	}
	last := events.LastSeq(context.Background(), e.d)
	e.card(bd.a, "after")
	if now := events.LastSeq(context.Background(), e.d); now != last+1 {
		t.Fatalf("seq %d after %d", now, last)
	}
}
