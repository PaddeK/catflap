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
