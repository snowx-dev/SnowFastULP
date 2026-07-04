package ulpengine

import (
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
)

// formatRecord formats the final output line. noURI drops path/query, keeps
// host:port. The dedup key is always host:login:password so flipping noURI
// never adds visual dupes. Test-only convenience mirroring the production
// lineFormatter.FormatRecord output shape.
func formatRecord(host, url, login, password string, noURI bool) string {
	urlPart := stripScheme(url)
	if noURI {
		urlPart = host
	}
	var b strings.Builder
	b.Grow(len(urlPart) + len(login) + len(password) + 2)
	b.WriteString(urlPart)
	b.WriteByte(':')
	b.WriteString(login)
	b.WriteByte(':')
	b.WriteString(password)
	return b.String()
}

func TestParseValid(t *testing.T) {
	cases := []struct {
		in          string
		host        string
		url         string
		login       string
		password    string
		strippedURL string
	}{
		{
			in:          "https://foo.example.com/x:user@example.com:secret",
			host:        "foo.example.com",
			url:         "https://foo.example.com/x",
			login:       "user@example.com",
			password:    "secret",
			strippedURL: "foo.example.com/x",
		},
		{
			in:          "http://www.example.com:bob:token",
			host:        "example.com",
			url:         "http://www.example.com",
			login:       "bob",
			password:    "token",
			strippedURL: "www.example.com",
		},
		{
			// :port stays part of host, stripScheme variant emits
			in:          "example.com:8080/path?x=1:alice:pw",
			host:        "example.com:8080",
			url:         "example.com:8080/path?x=1",
			login:       "alice",
			password:    "pw",
			strippedURL: "example.com:8080/path?x=1",
		},
	}
	for _, c := range cases {
		host, url, login, password, ok := parse(c.in)
		if !ok {
			t.Fatalf("parse(%q) ok=false, want true", c.in)
		}
		if host != c.host || url != c.url || login != c.login || password != c.password {
			t.Fatalf("parse(%q) = %q,%q,%q,%q; want %q,%q,%q,%q",
				c.in, host, url, login, password,
				c.host, c.url, c.login, c.password)
		}
		if got := stripScheme(url); got != c.strippedURL {
			t.Fatalf("stripScheme(%q) = %q, want %q", url, got, c.strippedURL)
		}
	}
}

func TestParseRejects(t *testing.T) {
	rejects := []string{
		"",
		"no-colon-line",
		"only-one:colon",
		"localhost:bob:pw",                    // localhost w/o @ in login
		"127.0.0.1:bob:pw",                    // 127. w/o @ in login
		"https://a.example.com:{user}:secret", // braces
		"https://a.example.com:user:{pw}",     // braces
		"https://a.example.com:user:http://evil.com",            // pw has http://
		"https://a.example.com:user:https://bad",                // pw has https://
		"https://a.example.com:user:" + strings.Repeat("z", 65), // pw > 64
	}
	for _, in := range rejects {
		if _, _, _, _, ok := parse(in); ok {
			t.Errorf("parse(%q) ok=true, want false", in)
		}
	}
}

func TestParseAndroidCredentials(t *testing.T) {
	cases := []struct {
		in                    string
		host, login, password string
	}{
		{
			in:       "android://Zm9vYmFy@com.netflix.mediaclient/:user@gmail.com:secret",
			host:     "android://Zm9vYmFy@com.netflix.mediaclient/",
			login:    "user@gmail.com",
			password: "secret",
		},
		{
			// [NOT_SAVED] placeholder is a non-empty password -> kept
			in:       "android://Zm9vYmFy@com.pinterest/:login.a@example.com:[NOT_SAVED]",
			host:     "android://Zm9vYmFy@com.pinterest/",
			login:    "login.a@example.com",
			password: "[NOT_SAVED]",
		},
		{
			// cert-less form
			in:       "android://com.spotify.music/:u2:pw2",
			host:     "android://com.spotify.music/",
			login:    "u2",
			password: "pw2",
		},
		{
			// password may contain colons; login never does
			in:       "android://h@com.x/:user:a:b:c",
			host:     "android://h@com.x/",
			login:    "user",
			password: "a:b:c",
		},
	}
	lf := newLineFormatter()
	for _, c := range cases {
		host, url, login, password, ok := parse(c.in)
		if !ok {
			t.Fatalf("parse(%q) ok=false, want true", c.in)
		}
		if host != c.host || url != c.host || login != c.login || password != c.password {
			t.Fatalf("parse(%q) = host=%q url=%q login=%q pw=%q; want host=url=%q login=%q pw=%q",
				c.in, host, url, login, password, c.host, c.login, c.password)
		}
		// key is the whole line: xxhash(host:login:password) == xxhash(in)
		if got, want := lf.HashKey(host, login, password), xxhash.Sum64String(c.in); got != want {
			t.Fatalf("android key for %q = %#x, want whole-line %#x", c.in, got, want)
		}
		// must round-trip through the regen/reparse path, else it'd be dropped
		out, repr := lf.FormatRecordStable(host, url, login, password, false)
		if !repr {
			t.Fatalf("android line %q has no stable representation", c.in)
		}
		if string(out) != c.in {
			t.Fatalf("android output = %q, want verbatim %q", out, c.in)
		}
		h2, _, l2, p2, ok2 := parseUnion(string(out))
		if !ok2 || lf.HashKey(h2, l2, p2) != lf.HashKey(host, login, password) {
			t.Fatalf("android %q does not round-trip via parseUnion", c.in)
		}
	}
}

func TestParseAndroidRejects(t *testing.T) {
	for _, in := range []string{
		"android://",              // nothing after scheme
		"android://com.x/",        // no login:password
		"android://com.x/:user",   // no password
		"android://:user:pw",      // empty authority
		"android://com.x/::pw",    // empty login
		"android://com.x/:user:",  // empty password
		"notandroid://com.x/:u:p", // wrong scheme
	} {
		if _, _, _, _, ok := parse(in); ok {
			t.Errorf("parse(%q) ok=true, want false", in)
		}
	}
}

func TestParseAcceptsMaxPassword(t *testing.T) {
	in := "https://a.example.com:user:" + strings.Repeat("z", 64)
	if _, _, _, _, ok := parse(in); !ok {
		t.Fatalf("parse with 64-byte password should be valid")
	}
}

func TestParseStripsTrailingNewline(t *testing.T) {
	in := "https://a.example.com:user@example.com:secret\r\n"
	host, _, _, password, ok := parse(in)
	if !ok || host != "a.example.com" || password != "secret" {
		t.Fatalf("parse should strip trailing CRLF: host=%q password=%q ok=%v", host, password, ok)
	}
}

func TestDedupKeyDistinguishesOnlyOnHostLoginPassword(t *testing.T) {
	a := dedupKey("a.example.com", "user", "pw")
	b := dedupKey("a.example.com", "user", "pw")
	c := dedupKey("a.example.com", "user2", "pw")
	if a != b {
		t.Fatal("identical inputs must produce identical keys")
	}
	if a == c {
		t.Fatal("different login must produce different key")
	}
}

func TestParseLineTooLong(t *testing.T) {
	in := "https://a.example.com:user:" + strings.Repeat("z", 4096)
	if _, _, _, _, ok := parse(in); ok {
		t.Fatalf("parse should reject lines > 4096 bytes")
	}
}

// DedupKeyForLine must yield the exact key Ingest derives for the same line, so
// an upstream producer can pre-dedup on the library's canonical key.
func TestDedupKeyForLineMatchesLibraryKey(t *testing.T) {
	lf := newLineFormatter()
	line := "www.example.com/login?next=1:user@host.com:secret"
	host, _, login, password, ok := parse(line)
	if !ok {
		t.Fatalf("setup: %q should parse", line)
	}
	want := lf.HashKey(host, login, password)
	got, ok := DedupKeyForLine(line, false)
	if !ok || got != want {
		t.Fatalf("DedupKeyForLine(%q) = %#x,%v; want %#x", line, got, ok, want)
	}
}

// Same host:login:password, differing only by www/path/query, must share one
// key — this is exactly the collapse that made sfl's "unique" over-count.
func TestDedupKeyForLineCollapsesPathVariants(t *testing.T) {
	a, okA := DedupKeyForLine("www.example.com/a?x=1:u:p", false)
	b, okB := DedupKeyForLine("example.com/b:u:p", false)
	if !okA || !okB {
		t.Fatalf("both should parse: okA=%v okB=%v", okA, okB)
	}
	if a != b {
		t.Fatalf("path-only variants should share a key: %#x vs %#x", a, b)
	}
}

func TestDedupKeyForLineRejectsUnparsable(t *testing.T) {
	if k, ok := DedupKeyForLine("not a ulp line", false); ok {
		t.Fatalf("garbage line should not yield a key, got %#x", k)
	}
}

func TestFormatRecord(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		url      string
		login    string
		password string
		noURI    bool
		want     string
	}{
		{
			name:     "default keeps stripped url including path",
			host:     "aaa.bbb.com",
			url:      "https://aaa.bbb.com/bunch/ofthings/here",
			login:    "john@gmail.com",
			password: "password123",
			noURI:    false,
			want:     "aaa.bbb.com/bunch/ofthings/here:john@gmail.com:password123",
		},
		{
			name:     "no-uri replaces url with bare host",
			host:     "aaa.bbb.com",
			url:      "https://aaa.bbb.com/bunch/ofthings/here",
			login:    "john@gmail.com",
			password: "password123",
			noURI:    true,
			want:     "aaa.bbb.com:john@gmail.com:password123",
		},
		{
			name:     "no-uri preserves port in host",
			host:     "x.example.com:8080",
			url:      "x.example.com:8080/path?q=1",
			login:    "alice",
			password: "pw",
			noURI:    true,
			want:     "x.example.com:8080:alice:pw",
		},
		{
			name:     "default emits scheme-less url even when no-uri off",
			host:     "x.example.com",
			url:      "http://www.x.example.com",
			login:    "bob",
			password: "tok",
			noURI:    false,
			want:     "www.x.example.com:bob:tok",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatRecord(c.host, c.url, c.login, c.password, c.noURI)
			if got != c.want {
				t.Fatalf("formatRecord = %q, want %q", got, c.want)
			}
		})
	}
}
