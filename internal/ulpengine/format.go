package ulpengine

import "fmt"

// humanBytes formats a byte count for debug logs. Mirrors the identical helper
// in cmd/sfu's TUI; kept local so the engine never depends on the command's
// display code (which would create an import cycle).
func humanBytes(n int64) string {
	if n < 0 {
		return "0 B"
	}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	u := 0
	// roll to next unit at 1000 (not 1024) so display never exceeds 8 chars
	for v >= 1000 && u < len(units)-1 {
		v /= 1024
		u++
	}
	return fmt.Sprintf("%.1f %s", v, units[u])
}
