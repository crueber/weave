package server

import "time"

// SetLegacyRecheck shortens the re-authentication interval of `rooms=` streams for a test.
func SetLegacyRecheck(d time.Duration) (restore func()) {
	old := legacyRecheck
	legacyRecheck = d
	return func() { legacyRecheck = old }
}
