package ulpengine

import (
	"strings"
	"testing"
)

// FuzzParseLineStrictLoose feeds arbitrary lines through both parse modes.
// No oracle: the value is panic/hang detection on the parser paths that
// produced real-world bugs. Seeds mirror the known-good corpus from
// TestParseValid and the junk-shape lines from the loose false-positive
// findings.
func FuzzParseLineStrictLoose(f *testing.F) {
	for _, seed := range []string{
		"",
		":",
		"::",
		"https://foo.example.com/x:user@example.com:secret",
		"http://www.example.com:bob:token",
		"example.com:8080/path?x=1:alice:pw",
		"android://Zm9vYmFy@com.netflix.mediaclient/:user@gmail.com:secret",
		"android://com.spotify.music/:u2:pw2",
		"android://h@com.x/:user:a:b:c",
		"https://a.example.com:user:secret | Full Access →",
		"https://a.example.com:user:secret [Network]",
		"https://a.example.com:user:correct horse $ ",
		"示例.com:user:pw",
		"foo+bar.com:user:pw",
		"\x00\xff:user:password",
		":target=",
		"LegacyGeneric:target=site:user:pw",
		"https://a.example.com:user:" + strings.Repeat("z", maxParsedLineLen+1),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		// Both modes over the same input; results discarded (no oracle).
		ParseLine(line, false)
		ParseLine(line, true)
	})
}
