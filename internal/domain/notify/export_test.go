package notify

// DestinationHostsForTest exposes the hosts a shoutrrr URL can make the server connect to.
func DestinationHostsForTest(raw string) []string {
	var out []string
	for _, t := range destinationTargets(raw) {
		out = append(out, t.host)
	}
	return out
}
