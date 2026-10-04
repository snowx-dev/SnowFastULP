package ulpengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		// key must be the canonical one the library derives for this line:
		// host=url=the whole android URL, cert included
		if k, ok := DedupKeyForLine(c.in, false); !ok || k != lf.HashKey(host, login, password) {
			t.Fatalf("android key for %q = %#x,%v; want HashKey(fields)", c.in, k, ok)
		}
		// must round-trip through the regen/reparse path, else it'd be dropped
		out, repr := lf.FormatRecordStable(host, url, login, password, false)
		if !repr {
			t.Fatalf("android line %q has no stable representation", c.in)
		}
		if string(out) != c.in {
			t.Fatalf("android output = %q, want verbatim %q", out, c.in)
		}
		h2, _, l2, p2, ok2 := parseStored(string(out))
		if !ok2 || lf.HashKey(h2, l2, p2) != lf.HashKey(host, login, password) {
			t.Fatalf("android %q does not round-trip via parseStored", c.in)
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

func TestFinishParseRejectsColonInLogin(t *testing.T) {
	if _, _, _, _, ok := finishParse("example.com", "user:name", "pw"); ok {
		t.Fatal("login containing ':' must reject (HashKey field ambiguity)")
	}
	p, _ := NewDelimParser("|")
	if _, _, _, _, ok := p.Parse("example.com|user:name|pw"); ok {
		t.Fatal("DelimParser must reject colon in login via finishParse")
	}
}

// www. normalization must not eat legitimate single-label hosts, must stay
// case-insensitive so dedup keys match, and the original host must still be
// dotted before any stripping. Since P6-W12 the canonical host also folds
// hostname case, so WWW.Example.com and www.example.com report the same host.
func TestFinishParseWWWNormalization(t *testing.T) {
	cases := []struct {
		url      string
		wantOK   bool
		wantHost string
	}{
		{"http://www.ai:bob:token", true, "www.ai"},
		{"https://www.io:u:p", true, "www.io"},
		{"https://www.com:u:p", true, "www.com"},
		{"https://WWW.Example.com:u:p", true, "example.com"},
		{"https://wWw.Example.com:u:p", true, "example.com"},
		{"https://www.example.com:u:p", true, "example.com"},
		{"https://www:u:p", false, ""},
		{"https://www.:u:p", false, ""},
		{"http://www.localhost:u:p", false, ""},
		{"http://www.LOCALHOST:u:p", false, ""},
		{"http://www.localhost:8080:u:p", false, ""},
	}
	for _, c := range cases {
		host, _, _, _, ok := parse(c.url)
		if ok != c.wantOK || (ok && host != c.wantHost) {
			t.Errorf("parse(%q) = (ok=%v, host=%q), want (ok=%v, host=%q)",
				c.url, ok, host, c.wantOK, c.wantHost)
		}
	}
	// uppercase/lowercase www. variants must dedup identically
	ka, okA := DedupKeyForLine("https://WWW.Example.com:u:p", false)
	kb, okB := DedupKeyForLine("https://www.Example.com:u:p", false)
	if !okA || !okB || ka != kb {
		t.Errorf("www case variants must share a dedup key: %v %v %#x %#x", okA, okB, ka, kb)
	}
}

func TestFinishParseRejectsEmptyLoginPassword(t *testing.T) {
	if _, _, _, _, ok := finishParse("example.com", "", "pw"); ok {
		t.Fatal("empty login must reject")
	}
	if _, _, _, _, ok := finishParse("example.com", "user", ""); ok {
		t.Fatal("empty password must reject")
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
	a := dedupKeySum("a.example.com", "user", "pw")
	b := dedupKeySum("a.example.com", "user", "pw")
	c := dedupKeySum("a.example.com", "user2", "pw")
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

func TestParseCleansGenericAnnotationSuffix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "symbol-delimited text is preserved",
			in:   "https://a.example.com:user:secret | Full Access →",
			want: "secret | Full Access →",
		},
		{
			name: "bracketed text is preserved",
			in:   "https://a.example.com:user:secret [Network]",
			want: "secret [Network]",
		},
		{
			name: "ordinary password punctuation is preserved",
			in:   "https://a.example.com:user:secret|literal",
			want: "secret|literal",
		},
		{
			name: "lowercase pipe suffix is preserved",
			in:   "https://a.example.com:user:secret | literal",
			want: "secret | literal",
		},
		{
			name: "ordinary password spaces are preserved",
			in:   "https://a.example.com:user:correct horse battery staple",
			want: "correct horse battery staple",
		},
		{
			name: "password symbol is preserved and edge space is trimmed",
			in:   "https://a.example.com:user:correct horse $ ",
			want: "correct horse $",
		},
		{
			name: "annotation-like password is preserved",
			in:   "https://a.example.com:user:secret - Password",
			want: "secret - Password",
		},
		{
			name: "bracketed password is preserved",
			in:   "https://a.example.com:user:[Network]",
			want: "[Network]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, password, ok := parse(tc.in)
			if !ok {
				t.Fatalf("parse(%q) rejected; want password %q", tc.in, tc.want)
			}
			if password != tc.want {
				t.Fatalf("parse(%q) password = %q, want %q", tc.in, password, tc.want)
			}
		})
	}
}

func TestParseAcceptsLiteralPlaceholderPasswords(t *testing.T) {
	for _, in := range []string{
		"https://a.example.com:user:None",
		"https://a.example.com:user:null",
		"https://a.example.com:user:nil",
		"https://a.example.com:user:unknown",
		"https://a.example.com:user:n/a",
		"https://a.example.com:user:Unable to decrypt",
	} {
		if _, _, _, _, ok := parse(in); !ok {
			t.Fatalf("parse(%q) ok=false; literal password must be accepted", in)
		}
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

// RR-2.1: a ':' inside a scheme:// URL path/query/fragment stays URL bytes.
// The scanner must prefer the longest URL prefix that leaves a class-valid
// non-colon login plus a non-empty password, so /oauth:callback stays in the
// URL instead of shifting into login "callback" / password "user:pw".
func TestParseURLPathColonsStayURLBytes(t *testing.T) {
	const in = "https://example.com/oauth:callback:user:pw"
	host, url, login, password, ok := parse(in)
	if !ok {
		t.Fatalf("parse(%q) rejected; URL path colons must not shift credentials", in)
	}
	if host != "example.com" || url != "https://example.com/oauth:callback" ||
		login != "user" || password != "pw" {
		t.Fatalf("parse(%q) = host=%q url=%q login=%q password=%q",
			in, host, url, login, password)
	}
	// the documented field tuple must be the key DedupKeyForLine derives
	lf := newLineFormatter()
	k, ok := DedupKeyForLine(in, false)
	if !ok || k != lf.HashKey(host, login, password) {
		t.Fatalf("DedupKeyForLine(%q) = %#x,%v; want %#x", in, k, ok, lf.HashKey(host, login, password))
	}
}

// RR-2.2: example.com:80:user:p (port in host) and example.com/:80:user:p
// (port glued to a path) parse to different field tuples but the old
// colon-joined preimage collapsed both to the string "example.com:80:user:p",
// silently dropping one credential as a dup. The versioned, length-prefixed
// key must keep them distinct.
func TestDedupKeyDistinguishesPortFromPathColon(t *testing.T) {
	hA, _, lA, pA, okA := parse("example.com:80:user:p")
	hB, _, lB, pB, okB := parse("example.com/:80:user:p")
	if !okA || !okB {
		t.Fatalf("both forms must parse: okA=%v okB=%v", okA, okB)
	}
	if hA != "example.com:80" || lA != "user" || pA != "p" {
		t.Fatalf("port form fields = host=%q login=%q pw=%q", hA, lA, pA)
	}
	if hB != "example.com" || lB != "80" || pB != "user:p" {
		t.Fatalf("path form fields = host=%q login=%q pw=%q", hB, lB, pB)
	}
	kA, okA := DedupKeyForLine("example.com:80:user:p", false)
	kB, okB := DedupKeyForLine("example.com/:80:user:p", false)
	if !okA || !okB {
		t.Fatalf("both forms must key: okA=%v okB=%v", okA, okB)
	}
	if kA == kB {
		t.Fatalf("distinct field tuples must not share a key: %#x", kA)
	}
}

// The versioned, length-prefixed preimage must be unambiguous and must agree
// byte-for-byte between the stateless helper (DedupKeyForLine/DedupKeyWith)
// and the streaming lineFormatter.HashKey path.
func TestDedupKeyEncodingUnambiguousAndConsistent(t *testing.T) {
	lf := newLineFormatter()
	// field tuples the old colon join could not distinguish (both joined to
	// "example.com:80:user:p")
	if lf.HashKey("example.com:80", "user", "p") == lf.HashKey("example.com", "80:user", "p") {
		t.Fatal("length-prefixed fields must not collide where host:login:password did")
	}
	fields := [][3]string{
		{"a.example.com", "user", "pw"},
		{"example.com:8080", "12345", "a:b:c"},
		{strings.Repeat("h", 300), strings.Repeat("l", 700), strings.Repeat("p", 3000)},
	}
	for _, f := range fields {
		if dedupKeySum(f[0], f[1], f[2]) != lf.HashKey(f[0], f[1], f[2]) {
			t.Fatalf("dedupKeySum and HashKey disagree for %q/%q/%q", f[0], f[1], f[2])
		}
	}
}

// The ambiguous pair must not dedup through a .idx sidecar either: both keys
// survive a write+read round trip instead of one collapsing into the other.
func TestSidecarKeepsAmbiguousPairDistinct(t *testing.T) {
	ka, okA := DedupKeyForLine("example.com:80:user:p", false)
	kb, okB := DedupKeyForLine("example.com/:80:user:p", false)
	if !okA || !okB {
		t.Fatalf("both forms must key: okA=%v okB=%v", okA, okB)
	}
	if ka == kb {
		t.Fatalf("keys must differ before the sidecar round trip: %#x", ka)
	}
	arch := filepath.Join(t.TempDir(), "part.txt.zst")
	// v4 sidecars bind to the archive's identity at Commit: create it
	if err := os.WriteFile(arch, []byte("archive payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := newSidecarWriter(arch)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []uint64{ka, kb} {
		if err := w.WriteHash(k); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	var got []uint64
	err = streamSidecarKeys(sidecarPathForArchive(arch), func(k uint64) error {
		got = append(got, k)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("sidecar must hold both distinct keys, got %v", got)
	}
}
