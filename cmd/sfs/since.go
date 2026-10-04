package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// maxSinceDays bounds the leading "<n>d" component so the multiplication
// below and the addition of the ParseDuration remainder cannot overflow
// time.Duration (int64 nanoseconds is ~292 years, i.e. 106751 days); an
// unchecked product wraps into a small positive duration that silently
// searches the wrong window.
const maxSinceDays = int((1<<63 - 1) / int64(24*time.Hour))

// parseSince parses an age window for the -since flag, e.g. "7d", "12h",
// "90m", or a combo like "1d6h". Go's time.ParseDuration has no day unit, so a
// leading <int>d is handled here and the remainder (if any) handed to ParseDuration.
func parseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	var days time.Duration
	rest := s
	if i := strings.IndexByte(s, 'd'); i > 0 {
		if n, err := strconv.Atoi(s[:i]); err == nil {
			if n > maxSinceDays || n < -maxSinceDays {
				return 0, fmt.Errorf("invalid -since %q: duration out of range (use e.g. 7d, 12h, 90m)", s)
			}
			days = time.Duration(n) * 24 * time.Hour
			rest = s[i+1:]
		}
	}

	total := days
	if rest != "" {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("invalid -since %q (use e.g. 7d, 12h, 90m)", s)
		}
		if d > 0 && total > math.MaxInt64-time.Duration(d) ||
			d < 0 && total < math.MinInt64-time.Duration(d) {
			return 0, fmt.Errorf("invalid -since %q: duration out of range (use e.g. 7d, 12h, 90m)", s)
		}
		total += d
	}
	if total <= 0 {
		return 0, fmt.Errorf("invalid -since %q: must be a positive duration", s)
	}
	return total, nil
}
