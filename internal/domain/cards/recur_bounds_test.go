package cards

import (
	"github.com/crueber/weave/internal/ops"
	"testing"
	"time"
)

// oldWeekly is the previous brute-force implementation, kept as the reference.
func oldWeekly(prev time.Time, r *Recurrence) time.Time {
	n := max(r.Interval, 1)
	for i := 1; i <= 7*n+7; i++ {
		d := prev.AddDate(0, 0, i)
		week := (i + int(prev.Weekday())) / 7
		if week%n != 0 {
			continue
		}
		for _, w := range r.Weekdays {
			if int(d.Weekday()) == w {
				return d
			}
		}
	}
	return prev.AddDate(0, 0, 7*n)
}

func TestWeeklyNextDueMatchesReference(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	sets := [][]int{{0}, {1}, {6}, {1, 3}, {0, 2, 4, 6}, {5, 6}, {0, 1, 2, 3, 4, 5, 6}}
	for _, n := range []int{0, 1, 2, 3, 5, 13} {
		for _, wds := range sets {
			for day := 0; day < 14; day++ {
				prev := start.AddDate(0, 0, day)
				r := &Recurrence{Freq: "weekly", Interval: n, Weekdays: wds}
				if got, want := nextDue(prev, r), oldWeekly(prev, r); !got.Equal(want) {
					t.Fatalf("n=%d wds=%v prev=%s: got %s want %s", n, wds, prev.Format("2006-01-02"), got, want)
				}
			}
		}
	}
}

func TestHostileRecurrenceIsBounded(t *testing.T) {
	r := &Recurrence{Freq: "weekly", Interval: 1 << 40, Weekdays: []int{9}}
	start := time.Now()
	nextDue(time.Now(), r)
	if time.Since(start) > time.Second {
		t.Fatal("nextDue is not bounded")
	}
	f := seed(t)
	c := f.card(t, f.todo, "hostile")
	call[Card](t, f.e, "card.due.set", DueIn{ID: c.ID, DueDate: time.Now().UTC().AddDate(0, 0, 1).Format(time.RFC3339)})
	for _, bad := range []string{`{"freq":"weekly","interval":1001}`, `{"freq":"daily","interval":999999999}`, `{"freq":"daily","count":10001}`} {
		if _, err := ops.Typed[Card](f.e, "card.recurrence.set", RecurrenceIn{ID: c.ID, Recurrence: bad}); err == nil {
			t.Fatalf("%s must be rejected", bad)
		}
	}
	call[Card](t, f.e, "card.recurrence.set", RecurrenceIn{ID: c.ID, Recurrence: `{"freq":"weekly","interval":1000,"count":10000,"weekdays":[1]}`})
}
