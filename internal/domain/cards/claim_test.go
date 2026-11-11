package cards

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// agents adds n agent users with editor access to b1 and returns an Env for each.
func (f *fx) agents(t *testing.T, n int) []*ops.Env {
	t.Helper()
	var out []*ops.Env
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("ag-%d", i)
		mustExec(t, f.e, `INSERT INTO users(id,email,name,role,is_agent,created_at) VALUES(?,?,?,'worker',1,?)`, uid, uid+"@agents.invalid", fmt.Sprintf("Agent %d", i), db.Now())
		mustExec(t, f.e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,'b1',?,'editor',?)`, "m-"+uid, uid, db.Now())
		out = append(out, testutil.As(f.e, &ops.Principal{UserID: uid, Role: "worker", Name: fmt.Sprintf("Agent %d", i), IsAgent: true}))
	}
	return out
}

func TestClaimConcurrentExactlyOneWins(t *testing.T) {
	f := seed(t)
	c := f.card(t, f.todo, "contested")
	ags := f.agents(t, 12)
	var wg sync.WaitGroup
	wins, conflicts := make(chan string, 12), make(chan struct{}, 12)
	start := make(chan struct{})
	for _, a := range ags {
		wg.Add(1)
		go func(a *ops.Env) {
			defer wg.Done()
			<-start
			_, err := ops.Typed[Card](a, "card.claim", ClaimIn{ID: c.ID, ListID: f.doing})
			switch {
			case err == nil:
				wins <- a.User.UserID
			case err.(*ops.Error).Status == 409:
				conflicts <- struct{}{}
			default:
				t.Errorf("unexpected error %v", err)
			}
		}(a)
	}
	close(start)
	wg.Wait()
	if len(wins) != 1 || len(conflicts) != 11 {
		t.Fatalf("wins=%d conflicts=%d, want 1 and 11", len(wins), len(conflicts))
	}
	winner := <-wins
	got := call[Card](t, f.e, "card.get", GetIn{ID: c.ID})
	if got.ClaimedBy == nil || *got.ClaimedBy != winner || got.ClaimedUntil == nil || got.ListID != f.doing {
		t.Fatalf("card after claim %+v", got)
	}
	if len(got.MemberIDs) != 1 || got.MemberIDs[0] != winner {
		t.Fatalf("winner must be the only member: %v", got.MemberIDs)
	}
	var claimed int
	_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE card_id=? AND type='cardClaim'`, c.ID).Scan(&claimed)
	if claimed != 1 {
		t.Fatalf("exactly one claim event expected, got %d", claimed)
	}
}

func TestClaimLeaseRenewReleaseAndExpiry(t *testing.T) {
	f := seed(t)
	c := f.card(t, f.todo, "lease")
	a := f.agents(t, 2)
	if _, err := ops.Typed[Card](a[0], "card.claim", ClaimIn{ID: c.ID, TTLSeconds: 5}); err == nil {
		t.Fatal("ttl below 10s must be rejected")
	}
	got := call[Card](t, a[0], "card.claim", ClaimIn{ID: c.ID, TTLSeconds: 60, Note: "mine"})
	if got.ClaimedBy == nil || *got.ClaimedByName != "Agent 0" || got.ClaimNote == nil || *got.ClaimNote != "mine" {
		t.Fatalf("claim %+v", got)
	}
	if _, err := ops.Typed[Card](a[1], "card.claim", ClaimIn{ID: c.ID}); err == nil || err.(*ops.Error).Status != 409 {
		t.Fatalf("second claimer must get 409, got %v", err)
	}
	if _, err := ops.Typed[Card](a[1], "card.renew", RenewIn{ID: c.ID}); err == nil || err.(*ops.Error).Status != 409 {
		t.Fatalf("non-holder renew must 409, got %v", err)
	}
	if _, err := ops.Typed[Card](a[1], "card.release", ReleaseIn{ID: c.ID}); err == nil || err.(*ops.Error).Status != 409 {
		t.Fatalf("non-holder release must 409, got %v", err)
	}
	// the same holder may re-claim (extends) and renew
	call[Card](t, a[0], "card.renew", RenewIn{ID: c.ID, TTLSeconds: 600})
	// viewers cannot claim
	if _, err := ops.Typed[Card](f.viewer, "card.claim", ClaimIn{ID: c.ID}); err == nil {
		t.Fatal("viewer must not claim")
	}
	// a manager may force-release
	call[Card](t, f.admin, "card.release", ReleaseIn{ID: c.ID})
	if got = call[Card](t, f.e, "card.get", GetIn{ID: c.ID}); got.ClaimedBy != nil {
		t.Fatalf("still claimed after release: %+v", got)
	}

	// expiry: an expired lease counts as unclaimed everywhere and the sweeper reports it
	call[Card](t, a[0], "card.claim", ClaimIn{ID: c.ID, TTLSeconds: 60})
	mustExec(t, f.e, `UPDATE cards SET claimed_until=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), c.ID)
	if got = call[Card](t, f.e, "card.get", GetIn{ID: c.ID}); got.ClaimedBy != nil {
		t.Fatalf("expired lease must read as unclaimed: %+v", got)
	}
	n, err := SweepClaims(context.Background(), f.e.DB, f.e.Hub, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("sweep n=%d err=%v", n, err)
	}
	var reason string
	_ = f.e.DB.QueryRow(`SELECT json_extract(envelope,'$.data.reason') FROM actions WHERE card_id=? AND etype='card.released' ORDER BY seq DESC LIMIT 1`, c.ID).Scan(&reason)
	if reason != "lease_expired" {
		t.Fatalf("release reason %q", reason)
	}
	if n, _ := SweepClaims(context.Background(), f.e.DB, f.e.Hub, time.Now()); n != 0 {
		t.Fatal("second sweep must find nothing")
	}
	// a takeover of a lapsed lease between sweeps still reports the lapse
	call[Card](t, a[0], "card.claim", ClaimIn{ID: c.ID, TTLSeconds: 60})
	mustExec(t, f.e, `UPDATE cards SET claimed_until=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), c.ID)
	call[Card](t, a[1], "card.claim", ClaimIn{ID: c.ID})
	var lapses int
	_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE card_id=? AND etype='card.released'`, c.ID).Scan(&lapses)
	if lapses != 3 {
		t.Fatalf("expected 3 release events, got %d", lapses)
	}
	// a fresh claim is not touched by a sweep
	if n, _ := SweepClaims(context.Background(), f.e.DB, f.e.Hub, time.Now()); n != 0 {
		t.Fatal("sweep released a live lease")
	}
}

func TestClaimMoveFailureLeavesCardUnclaimed(t *testing.T) {
	f := seed(t)
	c := f.card(t, f.todo, "x")
	a := f.agents(t, 1)[0]
	if _, err := ops.Typed[Card](a, "card.claim", ClaimIn{ID: c.ID, ListID: f.arch}); err == nil {
		t.Fatal("moving a claim to the archive list must be rejected")
	}
	if _, err := ops.Typed[Card](a, "card.claim", ClaimIn{ID: c.ID, ListID: "nope"}); err == nil {
		t.Fatal("unknown list must be rejected")
	}
	if got := call[Card](t, f.e, "card.get", GetIn{ID: c.ID}); got.ClaimedBy != nil {
		t.Fatalf("rejected claim left a lease: %+v", got)
	}
}

func TestCardNextClaimsDistinctCardsConcurrently(t *testing.T) {
	f := seed(t)
	const cards = 6
	for i := 0; i < cards; i++ {
		f.card(t, f.todo, fmt.Sprintf("job%d", i))
	}
	closed := f.card(t, f.todo, "closed one")
	call[Card](t, f.e, "card.update", UpdateIn{ID: closed.ID})
	mustExec(t, f.e, `UPDATE cards SET is_closed=1 WHERE id=?`, closed.ID)

	// peek does not claim
	peek := call[*events.CardSnap](t, f.e, "card.next", NextIn{ListID: f.todo})
	if peek == nil || peek.Name != "job0" || peek.ClaimedBy != nil {
		t.Fatalf("peek %+v", peek)
	}
	if _, err := ops.Typed[*events.CardSnap](f.viewer, "card.next", NextIn{ListID: f.todo, Claim: true}); err == nil {
		t.Fatal("viewer must not claim via next")
	}

	ags := f.agents(t, 10) // more agents than cards
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]string{}
	empty := 0
	for _, a := range ags {
		wg.Add(1)
		go func(a *ops.Env) {
			defer wg.Done()
			s, err := ops.Typed[*events.CardSnap](a, "card.next", NextIn{ListID: f.todo, Claim: true, MoveTo: f.doing})
			if err != nil {
				t.Errorf("next: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if s == nil {
				empty++
				return
			}
			if prev, dup := got[s.ID]; dup {
				t.Errorf("card %s handed to %s and %s", s.ID, prev, a.User.UserID)
			}
			got[s.ID] = a.User.UserID
		}(a)
	}
	wg.Wait()
	if len(got) != cards || empty != 4 {
		t.Fatalf("claimed %d cards (empty=%d), want %d and 4", len(got), empty, cards)
	}
	for id := range got {
		if c := call[Card](t, f.e, "card.get", GetIn{ID: id}); c.ListID != f.doing || c.ClaimedBy == nil {
			t.Fatalf("card %s not moved/claimed: %+v", id, c)
		}
	}
	if s := call[*events.CardSnap](t, f.e, "card.next", NextIn{ListID: f.todo}); s != nil {
		t.Fatalf("closed card must not be offered: %+v", s)
	}
}

func TestCardListFiltersAndFullGet(t *testing.T) {
	f := seed(t)
	a := f.agents(t, 1)[0]
	c1, c2, c3 := f.card(t, f.todo, "alpha thing"), f.card(t, f.todo, "beta"), f.card(t, f.doing, "gamma thing")
	call[Card](t, f.e, "card.label.add", LabelIn{ID: c1.ID, LabelID: f.labelA})
	call[Card](t, a, "card.claim", ClaimIn{ID: c2.ID})
	ids := func(in ListCardsIn) string {
		in.BoardID = "b1"
		out := call[ListOut](t, f.e, "card.list", in)
		s := ""
		for _, c := range out.Items {
			s += c.Name[:1]
		}
		return s
	}
	for _, tc := range []struct {
		in   ListCardsIn
		want string
	}{
		{ListCardsIn{}, "abg"},
		{ListCardsIn{ListID: f.doing}, "g"},
		{ListCardsIn{LabelID: f.labelA}, "a"},
		{ListCardsIn{MemberID: "ag-0"}, "b"},
		{ListCardsIn{Claimed: "true"}, "b"},
		{ListCardsIn{Claimed: "false"}, "ag"},
		{ListCardsIn{Claimed: "any"}, "abg"},
		{ListCardsIn{Q: "thing"}, "ag"},
		{ListCardsIn{Q: "100%"}, ""},
	} {
		if got := ids(tc.in); got != tc.want {
			t.Errorf("%+v -> %q, want %q", tc.in, got, tc.want)
		}
	}
	p1 := call[ListOut](t, f.e, "card.list", ListCardsIn{BoardID: "b1", Limit: 2})
	if len(p1.Items) != 2 || p1.NextCursor == "" {
		t.Fatalf("page 1 %+v", p1)
	}
	p2 := call[ListOut](t, f.e, "card.list", ListCardsIn{BoardID: "b1", Limit: 2, Cursor: p1.NextCursor})
	if len(p2.Items) != 1 || p2.NextCursor != "" || p2.Items[0].ID != c3.ID {
		t.Fatalf("page 2 %+v", p2)
	}
	if _, err := ops.Typed[ListOut](f.e, "card.list", ListCardsIn{BoardID: "b1", Claimed: "maybe"}); err == nil {
		t.Fatal("bad claimed value must be rejected")
	}

	// full=true pulls tasks, comments, attachments, custom fields and activity
	if _, err := ops.CallMap(f.e, "comment.create", map[string]any{"cardId": c1.ID, "text": "hello"}); err != nil {
		t.Fatal(err)
	}
	full := call[Card](t, f.e, "card.get", GetIn{ID: c1.ID, Full: true})
	if full.Comments == nil || full.Tasks == nil || full.Attachments == nil || full.CustomFields == nil || full.Activity == nil {
		t.Fatalf("full get missing sections: %+v", full)
	}
	plain := call[Card](t, f.e, "card.get", GetIn{ID: c1.ID})
	if plain.Comments != nil || plain.Activity != nil {
		t.Fatalf("plain get must not include extras")
	}
	if _, err := ops.Typed[Card](f.stranger, "card.get", GetIn{ID: c1.ID, Full: true}); err == nil {
		t.Fatal("stranger must not read a card")
	}
}
