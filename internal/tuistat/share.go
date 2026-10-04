package tuistat

import "fmt"

// ShareParen formats n as a share of of for final-summary partitions.
// Returns "" when of <= 0, n < 0, n > of, n == of, or when the one-decimal
// display would round to 100.0% / 0.0% (near-whole and near-zero noise).
func ShareParen(n, of int64) string {
	if of <= 0 || n < 0 || n > of || n == of {
		return ""
	}
	pct := 100 * float64(n) / float64(of)
	shown := fmt.Sprintf("%.1f", pct)
	if shown == "100.0" || shown == "0.0" {
		return ""
	}
	return " (" + shown + "%)"
}
