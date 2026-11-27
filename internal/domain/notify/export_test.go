package notify

import (
	"context"

	"github.com/crueber/weave/internal/ops"
)

// PlannedForTest is one planned notification (user, type, data).
type PlannedForTest struct {
	UserID, Type, BoardID string
	Data                  map[string]any
}

// PlanForTest runs the notification planner for an event without writing anything.
func PlanForTest(e *ops.Env, ev ops.Event, actor string) []PlannedForTest {
	var out []PlannedForTest
	plans, _, _ := planNotifications(e, ev, actor)
	for _, p := range plans {
		out = append(out, PlannedForTest{p.UserID, p.Type, p.BoardID, p.Data})
	}
	return out
}

// DestinationHostsForTest exposes the hosts a shoutrrr URL can make the server connect to.
func DestinationHostsForTest(raw string) []string {
	var out []string
	targets, _ := destinationTargets(raw)
	for _, t := range targets {
		out = append(out, t.host)
	}
	return out
}

// CheckDestinationForTest exposes checkDestination for unit tests.
func CheckDestinationForTest(ctx context.Context, raw string) error {
	return checkDestination(ctx, raw)
}
