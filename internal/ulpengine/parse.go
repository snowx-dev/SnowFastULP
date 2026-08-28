package ulpengine

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/cespare/xxhash/v2"
)

// strict url:login:password regex. tried first, falls through to LPU
// (login:password:scheme://url) hand-scan on miss. LPU isnt a regex b/c
// `.+`-between-anchors costs ~5x more per failed match than this one.
var ulpPattern = regexp.MustCompile(`(?i)^((?:\w+:\/\/)?(?:[a-z0-9\-]+\.)+[a-z]{2,}(?::\d+)?(?:[\/?#][^:]*)?):([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}|[a-zA-Z0-9._-]+):(.+)$`)

// extracts host, url, login, password. ULP first, then LPU. never errors.
func parse(line string) (host, url, login, password string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 || len(line) > maxParsedLineLen {
		return "", "", "", "", false
	}
	if !strings.Contains(line, ":") {
		return "", "", "", "", false
	}

	if h, u, l, p, ok := parseAndroid(line); ok {
		return h, u, l, p, ok
	}

	if idx := ulpPattern.FindStringSubmatchIndex(line); idx != nil && len(idx) >= 8 {
		url = line[idx[2]:idx[3]]
		login = line[idx[4]:idx[5]]
		password = line[idx[6]:idx[7]]
		return finishParse(url, login, password)
	}

	if url, login, password, ok := matchLPU(line); ok {
		return finishParse(url, login, password)
	}
	return "", "", "", "", false
}

// androidScheme is the Google Autofill / Smart Lock credential prefix seen in
// stealer logs: android://<base64 signing-cert hash>==@<package.name>/. The
// strict ULP host class can't express the base64 authority, so these creds
// (real email:password for com.instagram.android, com.netflix.mediaclient, ...)
// were being dropped at ingest.
const androidScheme = "android://"

// parseAndroid admits android://<authority>[/…]:login:password verbatim. host
// and url are the ENTIRE android URL (through the authority), so the dedup key
// is the whole line, cert included: an android cred stays distinct from its web
// twin and from the same login under a different signing cert. The authority
// carries no colon (base64 + '@' + dotted package), so the first colon past the
// scheme is the url/login boundary; login carries no colon either, and the
// password takes the remainder (which may itself contain colons). Reachable
// from parse(), and thus from parseLoose, so
// ingest, loose, and regen/round-trip all key these identically.
func parseAndroid(line string) (host, url, login, password string, ok bool) {
	if !strings.HasPrefix(line, androidScheme) {
		return "", "", "", "", false
	}
	rel := line[len(androidScheme):] // authority[/path]:login:password
	sep := strings.IndexByte(rel, ':')
	if sep <= 0 { // empty authority, or no login/password
		return "", "", "", "", false
	}
	url = line[:len(androidScheme)+sep]
	rest := rel[sep+1:]
	c := strings.IndexByte(rest, ':')
	if c <= 0 { // missing or empty login
		return "", "", "", "", false
	}
	login = rest[:c]
	if password = rest[c+1:]; password == "" {
		return "", "", "", "", false
	}
	return url, url, login, password, true
}

// login:password:scheme://url via byte scan. login class excludes `:` so the
// first `:` is always the login/password split and the first `://` always
// belongs to the URL
func matchLPU(line string) (url, login, password string, ok bool) {
	schemeColonIdx := strings.Index(line, "://")
	if schemeColonIdx <= 0 {
		return "", "", "", false
	}
	// walk back over scheme alpha chars to the ":" introducing the URL
	i := schemeColonIdx - 1
	for i >= 0 && isASCIIAlpha(line[i]) {
		i--
	}
	if i <= 0 || line[i] != ':' {
		return "", "", "", false
	}
	urlStart := i + 1
	url = line[urlStart:]
	lp := line[:i]

	cIdx := strings.IndexByte(lp, ':')
	if cIdx <= 0 {
		return "", "", "", false
	}
	login = lp[:cIdx]
	password = lp[cIdx+1:]
	if len(login) == 0 || len(password) == 0 || len(url) == 0 {
		return "", "", "", false
	}
	return url, login, password, true
}

// shared post-match hygiene for ULP and LPU
func finishParse(url, login, password string) (host, urlOut, loginOut, passwordOut string, ok bool) {
	if login == "" || password == "" {
		return "", "", "", "", false
	}
	// login must not contain ':' so FormatRecord / HashKey stay field-unambiguous
	// (password may contain colons).
	if strings.ContainsRune(login, ':') {
		return "", "", "", "", false
	}
	host = url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimPrefix(host, "www.")
	if !strings.ContainsRune(host, '.') {
		return "", "", "", "", false
	}
	if (strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "localhost")) && !strings.Contains(login, "@") {
		return "", "", "", "", false
	}
	if wrappedBraces(host) || wrappedBraces(login) || wrappedBraces(password) {
		return "", "", "", "", false
	}
	if strings.HasPrefix(login, "http://") || strings.HasPrefix(login, "https://") ||
		strings.HasPrefix(password, "http://") || strings.HasPrefix(password, "https://") {
		return "", "", "", "", false
	}
	if len(password) > 64 {
		return "", "", "", "", false
	}
	return host, url, login, password, true
}

// strict/loose dispatcher, keeps hot-path callsites uniform
func parseFor(line string, loose bool) (host, url, login, password string, ok bool) {
	if loose {
		return parseLoose(line)
	}
	return parse(line)
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func wrappedBraces(s string) bool {
	return strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")
}

// drops leading http:// or https:// (case-insensitive)
func stripScheme(u string) string {
	if len(u) >= 7 && strings.EqualFold(u[:7], "http://") {
		return u[7:]
	}
	if len(u) >= 8 && strings.EqualFold(u[:8], "https://") {
		return u[8:]
	}
	return u
}

// DedupKeyForLine returns the library's canonical dedup key (xxhash64 of
// host:login:password) for an already-formatted ULP line — the same key Ingest
// derives for that line. ok=false when the line doesn't parse (the library would
// reject it). This lets an upstream producer (sfl's extraction) pre-dedup on the
// exact key the library uses, so its "unique" count reconciles with what ingest
// actually adds instead of over-counting path-only variants.
func DedupKeyForLine(line string, loose bool) (uint64, bool) {
	host, _, login, password, ok := parseFor(line, loose)
	if !ok {
		return 0, false
	}
	return xxhash.Sum64String(dedupKey(host, login, password)), true
}

// ParseLine is the exported entry point for parsing a single ULP/LPU line into
// its (host, url, login, password) fields. It mirrors parseFor exactly and is
// the reuse seam for callers outside ulpengine (e.g. sflog's label-less
// colon-line fallback) so they never duplicate the ULP regex. loose=false uses
// the strict parser (the library default); loose=true admits the extra
// host:port:user:pw / bare host:user:pw / LPU shapes. ok=false when the line
// is not a credential.
func ParseLine(line string, loose bool) (host, url, login, password string, ok bool) {
	return parseFor(line, loose)
}

// host:login:password dedup key. hot path uses lineFormatter.HashKey instead
// to skip the alloc, this one stays for tests
func dedupKey(host, login, password string) string {
	var b strings.Builder
	b.Grow(len(host) + len(login) + len(password) + 2)
	b.WriteString(host)
	b.WriteByte(':')
	b.WriteString(login)
	b.WriteByte(':')
	b.WriteString(password)
	return b.String()
}

// reusable buffer + streaming digest for zero-alloc per-line formatting.
// one per goroutine, NOT safe for concurrent use. buffer returned by
// FormatRecord is reused on next call, caller must consume before reusing.
type lineFormatter struct {
	out    bytes.Buffer
	digest *xxhash.Digest
}

func newLineFormatter() *lineFormatter {
	lf := &lineFormatter{digest: xxhash.New()}
	lf.out.Grow(256)
	return lf
}

// returned slice is reused on next call, see lineFormatter doc
func (lf *lineFormatter) FormatRecord(host, url, login, password string, noURI bool) []byte {
	urlPart := stripScheme(url)
	if noURI {
		urlPart = host
	}
	// Digit login + colon in password collides with host:port:user:pass on the
	// wire (example.com:12345:a:b:c). Append '/' so the stored form is
	// example.com/:12345:a:b:c — finishParse still yields host example.com.
	if allDigits(login) && strings.ContainsRune(password, ':') &&
		!strings.ContainsAny(urlPart, "/?#") {
		urlPart += "/"
	}
	lf.out.Reset()
	lf.out.Grow(len(urlPart) + len(login) + len(password) + 2)
	lf.out.WriteString(urlPart)
	lf.out.WriteByte(':')
	lf.out.WriteString(login)
	lf.out.WriteByte(':')
	lf.out.WriteString(password)
	return lf.out.Bytes()
}

// FormatRecordStable returns the bytes to write for a parsed record, choosing a
// representation that re-parses (via parseStored, the regen/archive reader)
// back to the same fields (host, login, password). It prefers the full url
// form, falls back to host:login:password, and reports ok=false when neither
// round-trips so the caller can drop the line. Without this, a stored line can
// fail to re-parse on sidecar regen, leaving its key out of the index ->
// re-ingest straggler.
//
// Verification is field-faithful (not HashKey-only): HashKey concatenates with
// ':' and cannot distinguish login "user:name" from login "user" + password
// "name:…". Every candidate is verified — including clean ≤2-colon lines.
func (lf *lineFormatter) FormatRecordStable(host, url, login, password string, noURI bool) ([]byte, bool) {
	out := lf.FormatRecord(host, url, login, password, noURI)
	if lf.roundTrips(out, host, login, password) {
		return out, true
	}
	if !noURI {
		outHost := lf.FormatRecord(host, url, login, password, true)
		if lf.roundTrips(outHost, host, login, password) {
			return outHost, true
		}
	}
	return nil, false
}

// FormatRecordStableLine is FormatRecordStable + '\n', for newline-terminated
// sinks. ok=false means the record has no round-trippable representation.
func (lf *lineFormatter) FormatRecordStableLine(host, url, login, password string, noURI bool) ([]byte, bool) {
	if _, ok := lf.FormatRecordStable(host, url, login, password, noURI); !ok {
		return nil, false
	}
	lf.out.WriteByte('\n')
	return lf.out.Bytes(), true
}

// roundTrips reports whether serialized re-parses via parseStored to the same
// host/login/password fields that were formatted.
func (lf *lineFormatter) roundTrips(serialized []byte, host, login, password string) bool {
	h, _, l, p, ok := parseStored(string(serialized))
	return ok && h == host && l == login && p == password
}

// xxhash64(host:login:password) via streaming digest, 0 allocs
func (lf *lineFormatter) HashKey(host, login, password string) uint64 {
	lf.digest.Reset()
	_, _ = lf.digest.WriteString(host)
	_, _ = lf.digest.WriteString(":")
	_, _ = lf.digest.WriteString(login)
	_, _ = lf.digest.WriteString(":")
	_, _ = lf.digest.WriteString(password)
	return lf.digest.Sum64()
}
