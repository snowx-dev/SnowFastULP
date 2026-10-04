// Package version exposes the build identifier embedded in each binary.
// release builds override via `-ldflags "-X .../version.String=<v>"`.
// MUST stay a var (not const) for -ldflags -X to work.
package version

import "strings"

// String is the version banner. edit only on a real source bump.
var String = "0.3"

// Compare returns -1, 0, or 1 if a is older, equal, or newer than b.
// Versions are dotted-numeric (e.g. "0.1.1"); missing trailing components count
// as 0, so "0.1" == "0.1.0". A prerelease suffix after '-' (e.g. "0.1.1-dev")
// ranks below the same base release, matching semver precedence. Prerelease
// identifiers are compared identifier by identifier: shared numeric prefixes
// (rc, beta, …) compare numerically so "rc10" > "rc2", purely numeric
// identifiers compare numerically, and anything else falls back to string
// compare. Non-numeric base components are compared by string as a last resort.
// Build metadata after '+' (e.g. "0.3.2+build1") is ignored for precedence,
// per semver §10 — it must never order a release above a newer numeric one
// (H-14: "0.3.2+build1" used to rank newer than "0.3.10", authorizing a
// downgrade). The '+' separator is cut before '-' because prerelease and
// build identifiers can each contain '-' but never '+'.
func Compare(a, b string) int {
	a, _, _ = strings.Cut(a, "+")
	b, _, _ = strings.Cut(b, "+")

	baseA, preA, _ := strings.Cut(a, "-")
	baseB, preB, _ := strings.Cut(b, "-")

	pa := strings.Split(baseA, ".")
	pb := strings.Split(baseB, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		na, sa := numAt(pa, i)
		nb, sb := numAt(pb, i)
		if sa != "" || sb != "" { // fall back to string compare for this field
			if sa != sb {
				return strings.Compare(sa, sb)
			}
			continue
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}

	// Equal base: a release (no prerelease) outranks a prerelease.
	switch {
	case preA == "" && preB == "":
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	default:
		return comparePrerelease(preA, preB)
	}
}

// comparePrerelease compares two prerelease suffixes (after the '-').
// Identifiers are split on '.'; each pair is compared with
// comparePrereleaseID. Fewer identifiers rank lower when one is a prefix of
// the other, matching semver precedence ("alpha" < "alpha.1").
func comparePrerelease(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		if i >= len(pa) {
			return -1
		}
		if i >= len(pb) {
			return 1
		}
		if c := comparePrereleaseID(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	return 0
}

// comparePrereleaseID compares one prerelease identifier pair. When both
// sides share a non-empty non-numeric prefix followed by digits ("rc2" vs
// "rc10") the prefixes must match and the digits decide numerically. Purely
// numeric identifiers ("1" vs "9") also compare numerically. Everything else
// — mixed shapes, different prefixes, empty — falls back to string compare.
func comparePrereleaseID(a, b string) int {
	pa, na := splitTrailingDigits(a)
	pb, nb := splitTrailingDigits(b)
	if pa != "" && pa == pb && (na != "" || nb != "") {
		return compareNumericTail(na, nb)
	}
	if isAllDigits(a) && isAllDigits(b) {
		return compareNumericTail(a, b)
	}
	return strings.Compare(a, b)
}

// splitTrailingDigits splits "rc10" into ("rc", "10"); a value with no
// trailing digits returns (s, ""). The prefix keeps its original case so
// prefix equality stays case-sensitive (matching the previous behavior
// where the whole string compared case-sensitively).
func splitTrailingDigits(s string) (string, string) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	return s[:i], s[i:]
}

// compareNumericTail compares digit strings numerically without allocation:
// longer is bigger; equal length compares bytewise (leading zeros make both
// orders degenerate to the same value only for identical strings, which the
// caller has already ruled out via prefix equality).
func compareNumericTail(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// isAllDigits reports whether s consists solely of ASCII digits (non-empty).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// numAt parses the i-th dotted component as an int. If the component is absent
// it's 0; if it's non-numeric, the raw string is returned for fallback compare.
func numAt(parts []string, i int) (int, string) {
	if i >= len(parts) {
		return 0, ""
	}
	n := 0
	for _, r := range parts[i] {
		if r < '0' || r > '9' {
			return 0, parts[i]
		}
		n = n*10 + int(r-'0')
	}
	return n, ""
}
