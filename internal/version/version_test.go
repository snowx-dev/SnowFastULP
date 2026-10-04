package version

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.1", "0.1.1", 0},
		{"0.1", "0.1.0", 0},        // missing component == 0
		{"0.2", "0.1.9", 1},        // numeric, not lexical
		{"0.1.9", "0.1.10", -1},    // 9 < 10 numerically
		{"1.0.0", "0.9.9", 1},      // major dominates
		{"0.1.1-dev", "0.1.1", -1}, // prerelease ranks below release
		{"0.1.1", "0.1.1-dev", 1},
		{"0.1.1-dev", "0.1", 1},         // base 0.1.1 > 0.1 despite prerelease
		{"0.1", "0.1.1-dev", -1},        // mirror of above
		{"0.1.1-rc1", "0.1.1-rc2", -1},  // prerelease string order
		{"0.1.1-rc2", "0.1.1-rc10", -1}, // rc numbers compare numerically, not lexically
		{"0.1.1-rc10", "0.1.1-rc2", 1},
		{"0.1.1-rc2", "0.1.1-rc2", 0},
		{"0.1.1-beta2", "0.1.1-beta10", -1},  // other numeric prefixes too
		{"0.1.1-2", "0.1.1-10", -1},          // purely numeric identifiers
		{"0.1.1-alpha", "0.1.1-alpha.1", -1}, // fewer identifiers rank lower (semver)
		{"0.1.1-rc1", "0.1.1-dev2", 1},       // different prefixes: string fallback ("rc" > "dev")
		{"0.2-dev", "0.2", -1},               // dev suffix ranks below plain release
		{"0.10", "0.9", 1},                   // multi-digit minor: numeric, not lexical
		// H-14: SemVer §10 — build metadata (+…) MUST be ignored for
		// precedence. It used to leak into the base components and made
		// "0.3.2+build1" rank NEWER than "0.3.10", authorizing a downgrade.
		{"0.3.2+build1", "0.3.10", -1}, // the verified repro pair
		{"0.3.10", "0.3.2+build1", 1},
		{"1.0.0+build", "1.0.0", 0}, // metadata alone: equal precedence
		{"1.0.0", "1.0.0+build", 0},
		{"1.0.0+20130313144700", "1.0.0+beta", 0}, // metadata never compared
		{"1.0.0-alpha+build", "1.0.0-alpha", 0},   // metadata after prerelease
		{"1.0.0-alpha+build", "1.0.0", -1},        // prerelease still decides
		{"1.0.0+build-1", "1.0.0", 0},             // hyphen inside metadata
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("version.Compare(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}
