package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/mrothroc/mailpipe/pkg/policy"
)

// parseSince reads a --since value: a positive duration back from now ("36h",
// or whole days as "2d"), a date taken from its local midnight ("2026-10-23"),
// or an RFC 3339 time.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, now.Location()); err == nil {
		return t, nil
	}
	d, err := policy.ParseAge(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q: want a positive duration (24h, 2d), a date (2026-10-23) or a time (2026-10-23T09:00:00Z)", s)
	}
	return now.Add(-d), nil
}
