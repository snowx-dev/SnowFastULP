package ulpengine

import (
	"strings"
	"testing"
)

// TestHostCaseCanonicalInDedupKey: hostname DNS case must not split one
// credential into distinct library keys — Example.COM, example.com, and their
// port variants share a key.
func TestHostCaseCanonicalInDedupKey(t *testing.T) {
	// URL form, strict parse
	upper := mustKey(t, "https://Example.COM/OAuth:Login:Pass", false)
	lower := mustKey(t, "https://example.com/OAuth:Login:Pass", false)
	if upper != lower {
		t.Fatalf("URL host case split the key: %d != %d", upper, lower)
	}

	// host:port form (loose grammar), case variants share the key
	upperPort := mustKey(t, "Example.COM:8080:Login:Pass", true)
	lowerPort := mustKey(t, "example.com:8080:Login:Pass", true)
	if upperPort != lowerPort {
		t.Fatalf("host:port case split the key: %d != %d", upperPort, lowerPort)
	}

	// www. case folds identically
	wwwUpper := mustKey(t, "https://WWW.Example.COM/:Login:Pass", false)
	wwwLower := mustKey(t, "https://www.example.com/:Login:Pass", false)
	if wwwUpper != wwwLower {
		t.Fatalf("www host case split the key: %d != %d", wwwUpper, wwwLower)
	}
}

// TestHostCaseLoginPasswordCaseDistinct: only the hostname folds; login and
// password case remain distinct credentials.
func TestHostCaseLoginPasswordCaseDistinct(t *testing.T) {
	host := "example.com"
	cases := [][2]string{
		{"User", "Pass"},
		{"user", "pass"},
		{"USER", "Pass"},
		{"User", "PASSWORD"},
	}
	seen := map[uint64]string{}
	for _, c := range cases {
		k := dedupKeySum(host, c[0], c[1])
		if prev, dup := seen[k]; dup {
			t.Fatalf("login/password case collapsed: %q and %q share key %d", prev, c, k)
		}
		seen[k] = c[0] + ":" + c[1]
	}
}

// TestHostCaseFormattedOutputPreservesOriginalURL: canonicalization is a
// dedup-key/host normalization — the parsed url keeps its original spelling
// and noURI=false output shows it verbatim.
func TestHostCaseFormattedOutputPreservesOriginalURL(t *testing.T) {
	line := "https://Example.COM/OAuth:callback:user:pw"
	host, url, _, _, ok := ParseLine(line, false)
	if !ok {
		t.Fatal("parse failed")
	}
	if host != "example.com" {
		t.Fatalf("canonical host = %q, want example.com", host)
	}
	if url != "https://Example.COM/OAuth:callback" {
		t.Fatalf("url lost original case: %q", url)
	}
	lf := newLineFormatter()
	out := string(lf.FormatRecord(host, url, "callback", "user:pw", false))
	if !strings.Contains(out, "Example.COM/OAuth:callback") {
		t.Fatalf("formatted output rewrote the URL case: %q", out)
	}
}

// TestHostCaseAndroidKeyUnchanged: android keys span the whole line (signing
// cert hash is base64, case-significant) and bypass host canonicalization.
func TestHostCaseAndroidKeyUnchanged(t *testing.T) {
	line := "android://AbCdEf==@com.instagram.android/.:user:pw"
	host, _, login, _, ok := ParseLine(line, false)
	if !ok {
		t.Fatal("android line should parse")
	}
	if host != "android://AbCdEf==@com.instagram.android/." {
		t.Fatalf("android host altered: %q", host)
	}
	if login != "user" || !strings.Contains(line, "AbCdEf==") {
		t.Fatalf("android fields altered: %q %q", host, login)
	}
}

func mustKey(t *testing.T, line string, loose bool) uint64 {
	t.Helper()
	k, ok := DedupKeyForLine(line, loose)
	if !ok {
		t.Fatalf("DedupKeyForLine(%q) not ok", line)
	}
	return k
}
