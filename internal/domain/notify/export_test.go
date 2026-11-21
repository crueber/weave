package notify

import "github.com/crueber/weave/internal/ops"

// PlannedForTest is one planned notification (user, type, data).
type PlannedForTest struct {
	UserID, Type string
	Data         map[string]any
}

// PlanForTest runs the notification planner for an event without writing anything.
func PlanForTest(e *ops.Env, ev ops.Event, actor string) []PlannedForTest {
	var out []PlannedForTest
	for _, p := range planNotifications(e, ev, actor) {
		out = append(out, PlannedForTest{p.UserID, p.Type, p.Data})
	}
	return out
}

// DestinationHostsForTest exposes the hosts a shoutrrr URL can make the server connect to.
func DestinationHostsForTest(raw string) []string {
	var out []string
	for _, t := range destinationTargets(raw) {
		out = append(out, t.host)
	}
	return out
}
