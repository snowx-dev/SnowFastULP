package selfupdate

import (
	"encoding/hex"
	"testing"
)

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
		{"0.1.1-dev", "0.1", 1},        // base 0.1.1 > 0.1 despite prerelease
		{"0.1", "0.1.1-dev", -1},       // mirror of above
		{"0.1.1-rc1", "0.1.1-rc2", -1}, // prerelease string order
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFetchSumsParsing(t *testing.T) {
	// Mirror the on-disk SHA256SUMS format (sha256sum output: "<hex>  <name>").
	manifest := "" +
		"aa" + "00" + "112233445566778899aabbccddeeff00112233445566778899aabbccddeeff  SnowFastULP-0.2-linux-amd64\n" +
		"bb112233445566778899aabbccddeeff00112233445566778899aabbccddeeff00  *SnowFastSearch-0.2-windows-amd64.exe\n" +
		"\n" + // blank line tolerated
		"# comment-ish short line\n"

	sums := parseSums([]byte(manifest))
	if len(sums) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(sums), sums)
	}
	want, _ := hex.DecodeString("aa00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if got := sums["SnowFastULP-0.2-linux-amd64"]; string(got) != string(want) {
		t.Errorf("linux digest mismatch: got %x", got)
	}
	// Leading '*' (binary-mode marker) must be stripped from the name.
	if _, ok := sums["SnowFastSearch-0.2-windows-amd64.exe"]; !ok {
		t.Errorf("windows entry not found (star-prefix not stripped?): %v", sums)
	}
}
