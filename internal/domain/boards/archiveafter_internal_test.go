package boards

import (
	"strings"
	"testing"
)

// The auto-archive confirm and the 409 name the cards that stay over the limit, claimed and recurring.
func TestArchiveConfirmNamesSkippedCards(t *testing.T) {
	d := archiveConfirm(ListArchivePreview{Name: "Done", Cards: 10, Keep: 3, WillArchive: 5, SkippedClaimed: 1, SkippedRecurring: 1}, "sheet")
	if !strings.Contains(d.Body, "1 card over the limit stay until an agent's claim") || !strings.Contains(d.Body, "1 recurring card over the limit stays: recurring cards are not auto-archived.") {
		t.Fatalf("confirm body: %s", d.Body)
	}
	d = archiveConfirm(ListArchivePreview{Name: "Done", Cards: 10, Keep: 3, WillArchive: 7}, "sheet")
	if strings.Contains(d.Body, "recurring") || strings.Contains(d.Body, "claim") {
		t.Fatalf("nothing skipped, nothing named: %s", d.Body)
	}
	if n := skippedNote(archivePlan{SkippedClaimed: 2, SkippedRecurring: 3}); n != "; 2 over the limit stay while claimed, 3 recurring cards stay over the limit" {
		t.Fatalf("409 note: %q", n)
	}
	if skippedNote(archivePlan{}) != "" {
		t.Fatal("empty note expected")
	}
}
