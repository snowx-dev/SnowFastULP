package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseSince parses an age window for the --since flag, e.g. "7d", "12h",
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
			days = time.Duration(n) * 24 * time.Hour
			rest = s[i+1:]
		}
	}

	total := days
	if rest != "" {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("invalid --since %q (use e.g. 7d, 12h, 90m)", s)
		}
		total += d
	}
	if total <= 0 {
		return 0, fmt.Errorf("invalid --since %q: must be a positive duration", s)
	}
	return total, nil
}
