package notify

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/server"
)

// Event-stream retention: replayable envelopes are kept for at least 30 days
// (default 90, WEAVE_EVENT_RETENTION_DAYS, floor 30). Activity rows themselves
// are never deleted; only the replay payload is dropped.
func retentionDays() int {
	n := 90
	if v, err := strconv.Atoi(os.Getenv("WEAVE_EVENT_RETENTION_DAYS")); err == nil && v > 0 {
		n = v
	}
	if n < 30 {
		n = 30
	}
	return n
}

func init() {
	server.Hooks = append(server.Hooks, func(s *server.Server) error {
		go func() {
			t := time.NewTicker(6 * time.Hour)
			defer t.Stop()
			for {
				if n, err := events.Purge(context.Background(), s.DB, time.Duration(retentionDays())*24*time.Hour); err != nil {
					log.Println("events: purge:", err)
				} else if n > 0 {
					log.Printf("events: dropped %d replay payloads older than %d days", n, retentionDays())
				}
				<-t.C
			}
		}()
		return nil
	})
}
