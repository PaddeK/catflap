package homelab

import (
	"strconv"
	"strings"
	"time"
)

// ParseDuration is time.ParseDuration plus whole days ("7d"), the unit
// invite lifetimes and request windows are usually thought of in.
func ParseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if days, err := strconv.Atoi(n); err == nil && days >= 0 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}

// HumanDuration formats e.g. 24h as "24 hours" and 168h as "7 days".
func HumanDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0 && d >= 48*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	case d%time.Hour == 0 && d > 0:
		return plural(int(d/time.Hour), "hour")
	case d%time.Minute == 0 && d > 0:
		return plural(int(d/time.Minute), "minute")
	}
	return d.String()
}
