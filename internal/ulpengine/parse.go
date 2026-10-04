package ulpengine

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"unicode/utf8"

	"github.com/cespare/xxhash/v2"
)

// extracts host, url, login, password. ULP first, then LPU. never errors.
func parse(line string) (host, url, login, password string, ok bool) {
	// The shared grammar owns edge cleanup. Trim the record terminator first,
	// then surrounding whitespace so URL-leading and password-trailing noise
	// cannot create distinct identities. Individual fields are trimmed again
	// in finishParseReason for login/password delimiter whitespace.
	line = strings.TrimSpace(strings.TrimRight(line, "\r\n"))
	return parseCore(line)
}

// parseCore parses one edge-trimmed record.
func parseCore(line string) (host, url, login, password string, ok bool) {
	if len(line) == 0 || len(line) > maxParsedLineLen {
		return "", "", "", "", false
	}
	if !strings.Contains(line, ":") {
		return "", "", "", "", false
	}

	if h, u, l, p, ok := parseAndroid(line); ok {
		return h, u, l, p, ok
	}

	if url, login, password, ok := parseStrictULP(line); ok {
		return finishParse(strings.TrimSpace(url), strings.TrimSpace(login), strings.TrimSpace(password))
	}

	// Shared deterministic fallback for colon-heavy stealer output. P1/P2
	// recover host:port[/path]:login:password; P3 recovers a left-anchored
	// host:login:password whose password itself contains colons. Hygiene and
	// the password cap still run in finishParse.
	if url, login, password, ok := parseColonFallback(line); ok {
		if h, u, l, p, accepted := finishParse(url, login, password); accepted {
			return h, u, l, p, true
		}
	}

	if url, login, password, ok := matchLPU(line); ok {
		return finishParse(url, login, password)
	}
	return "", "", "", "", false
}

// parseColonFallback applies the approved ordered P1/P2/P3 recovery rules.
// It runs only after the normal strict ULP scan, so established unambiguous
// records keep their existing split.
func parseColonFallback(line string) (url, login, password string, ok bool) {
	parts := strings.Split(line, ":")
	// These are fallbacks for 4+-field ambiguity only. Three-field bare-IP
	// records remain a deliberate loose-mode extension.
	if len(parts) < 4 {
		return "", "", "", false
	}
	field1 := strings.TrimSpace(parts[0])
	if !validFallbackHost(field1) {
		return "", "", "", false
	}

	// P1/P2: dotted host + numeric port, optionally followed by /path.
	if _, _, portOK := splitPortPath(strings.TrimSpace(parts[1])); portOK {
		candidateLogin := strings.TrimSpace(parts[2])
		if validLoginClass(candidateLogin) {
			return field1 + ":" + strings.TrimSpace(parts[1]), candidateLogin,
				strings.TrimSpace(strings.Join(parts[3:], ":")), true
		}
	}

	// P3: left-anchored host/login; the password owns every later colon.
	candidateLogin := strings.TrimSpace(parts[1])
	if !validLoginClass(candidateLogin) {
		return "", "", "", false
	}
	return field1, candidateLogin, strings.TrimSpace(strings.Join(parts[2:], ":")), true
}

func validFallbackHost(host string) bool {
	parts := strings.Split(host, ".")
	ipv4Like := len(parts) == 4
	for _, part := range parts {
		if part == "" {
			ipv4Like = false
			break
		}
		for i := range part {
			if part[i] < '0' || part[i] > '9' {
				ipv4Like = false
				break
			}
		}
	}
	if ipv4Like {
		return true
	}
	hostEnd, portEnd, pathStart := scanURLHead(host, 0)
	return hostEnd == len(host) && portEnd < 0 && pathStart < 0
}

// androidScheme is the Google Autofill / Smart Lock credential prefix seen in
// stealer logs: android://<base64 signing-cert hash>==@<package.name>/. The
// strict ULP host class can't express the base64 authority, so these creds
// (real email:password for com.instagram.android, com.netflix.mediaclient, ...)
// were being dropped at ingest.
const androidScheme = "android://"

// parseAndroid admits android://<authority>[/…]:login:password verbatim. host
// and url are the ENTIRE android URL (through the authority), so the dedup key
// spans the whole line, cert included: an android cred stays distinct from its
// web twin and from the same login under a different signing cert. The
// authority carries no colon (base64 + '@' + dotted package), so the first
// colon past the scheme is the url/login boundary; login carries no colon
// either, and the password takes the remainder (which may itself contain
// colons). Reachable from parse(), and thus from parseLoose, so ingest,
// loose, and regen/round-trip all key these identically.
func parseAndroid(line string) (host, url, login, password string, ok bool) {
	if !strings.HasPrefix(line, androidScheme) {
		return "", "", "", "", false
	}
	rel := line[len(androidScheme):] // authority[/path]:login:password
	sep := strings.IndexByte(rel, ':')
	if sep <= 0 { // empty authority, or no login/password
		return "", "", "", "", false
	}
	url = strings.TrimSpace(line[:len(androidScheme)+sep])
	rest := rel[sep+1:]
	c := strings.IndexByte(rest, ':')
	if c <= 0 { // missing or empty login
		return "", "", "", "", false
	}
	login = strings.TrimSpace(rest[:c])
	password = strings.TrimSpace(rest[c+1:])
	if password == "" || !validLoginClass(login) {
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
	url = strings.TrimSpace(line[urlStart:])
	lp := line[:i]

	cIdx := strings.IndexByte(lp, ':')
	if cIdx <= 0 {
		return "", "", "", false
	}
	login = strings.TrimSpace(lp[:cIdx])
	password = strings.TrimSpace(lp[cIdx+1:])
	if password == "" || url == "" || !validLoginClass(login) {
		return "", "", "", false
	}
	return url, login, password, true
}

// parseStrictULP byte-scans the strict ULP shape
// [scheme://]host[:port][path]:login:password and returns the raw url/login/
// password split for finishParse. It replaces the old ulpPattern boundary
// decision. Colons belong to the URL when they are the port separator or sit
// inside the path/query/fragment; every other colon is the login separator,
// and the password takes the remainder (it may contain colons).
//
// Colon-boundary policy:
//   - With a scheme:// URL, colons in the path/query/fragment are URL bytes
//     and the LONGEST URL prefix that still leaves a class-valid non-colon
//     login plus a non-empty password wins. Walking candidate colons
//     right-to-left finds it directly. This keeps
//     https://example.com/oauth:callback:user:pw intact as
//     URL /oauth:callback + login "user" + password "pw" instead of shifting
//     the first path colon into login/password.
//   - Without a scheme the bare-host prefix is not trusted as a full URL, so
//     the regex-era boundary is preserved: the path ends at its first colon
//     (example.com/:80:user:p keeps login "80", password "user:p"), then the
//     :port form, then the bare-host form are tried in that order — exactly
//     the old regex's greedy-backtracking preference.
//
// The scan is single-pass over the line; login-candidate segments between
// consecutive colons are disjoint, so no candidate loop can go quadratic.
func parseStrictULP(line string) (url, login, password string, ok bool) {
	schemeEnd := schemePrefixLen(line)
	hostEnd, portEnd, pathStart := scanURLHead(line, schemeEnd)
	if hostEnd < 0 {
		return "", "", "", false
	}

	if pathStart >= 0 {
		if schemeEnd > 0 {
			// longest URL first: try each path colon right-to-left
			next := -1
			for i := len(line) - 1; i >= pathStart; i-- {
				if line[i] != ':' {
					continue
				}
				if next > i {
					if l, p, ok := splitLoginPassword(line, i+1, next); ok {
						return line[:i], l, p, true
					}
				}
				next = i
			}
		} else {
			// schemeless: only the first path colon is a URL boundary
			c := strings.IndexByte(line[pathStart:], ':')
			if c >= 0 {
				c += pathStart
				if d := strings.IndexByte(line[c+1:], ':'); d >= 0 {
					if l, p, ok := splitLoginPassword(line, c+1, c+1+d); ok {
						return line[:c], l, p, true
					}
				}
			}
		}
	}

	// URL truncated at the port: the remainder must be :login:password
	if portEnd >= 0 && portEnd < len(line) && line[portEnd] == ':' {
		if d := strings.IndexByte(line[portEnd+1:], ':'); d >= 0 {
			if l, p, ok := splitLoginPassword(line, portEnd+1, portEnd+1+d); ok {
				return line[:portEnd], l, p, true
			}
		}
	}
	// bare-host URL: the colon right after the host introduces the login
	if hostEnd < len(line) && line[hostEnd] == ':' {
		if d := strings.IndexByte(line[hostEnd+1:], ':'); d >= 0 {
			if l, p, ok := splitLoginPassword(line, hostEnd+1, hostEnd+1+d); ok {
				return line[:hostEnd], l, p, true
			}
		}
	}
	return "", "", "", false
}

// schemePrefixLen returns the length of a leading \w+:// scheme, 0 when
// absent (same class as the regex group it replaces).
func schemePrefixLen(line string) int {
	i := 0
	for i < len(line) && isWordByte(line[i]) {
		i++
	}
	if i > 0 && i+3 <= len(line) && line[i:i+3] == "://" {
		return i + 3
	}
	return 0
}

// scanURLHead locates the pieces of [scheme://]host[:port][path…] starting at
// pos: the end of the dotted hostname, the end of an optional :digits port,
// and the start of an optional /?# path. hostEnd < 0 when line[pos:] has no
// dotted hostname with a >=2-letter TLD.
//
// Host labels admit non-ASCII bytes (IDN hosts like 示例.com appear verbatim in
// stealer logs) plus '+' and '*': real capture hostnames carry them, and
// dropping them at parse time is silent ingest-layer data loss. The label run
// still must end in a >=2-letter ASCII TLD (unchanged), so garbage dotted
// tokens that lack one keep failing strict parse exactly as before.
func scanURLHead(line string, pos int) (hostEnd, portEnd, pathStart int) {
	hostEnd, portEnd, pathStart = -1, -1, -1
	// host: (label '.')+ tld(alpha >= 2), case-insensitive
	i := pos
	lastDot := -1
	for i < len(line) {
		c := lowerByte(line[i])
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' ||
			c == '+' || c == '*' || line[i] >= utf8.RuneSelf {
			i++
			continue
		}
		if c == '.' {
			if i == pos || line[i-1] == '.' {
				return // empty or doubled label
			}
			lastDot = i
			i++
			continue
		}
		break
	}
	if lastDot < 0 {
		return
	}
	j := lastDot + 1
	for j < len(line) && isASCIIAlpha(line[j]) {
		j++
	}
	if j-lastDot-1 < 2 {
		return
	}
	// Permit delimiter whitespace after the URL field; it is removed before
	// finishParse stores the record. Only ASCII space/tab are delimiter noise.
	for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
		j++
	}
	// The TLD must run into a char that can continue a URL (port, path, or
	// the login colon); anything else cannot form the host+suffix grammar.
	if j < len(line) && line[j] != ':' && line[j] != '/' && line[j] != '?' && line[j] != '#' {
		return
	}
	hostEnd = j
	if hostEnd < len(line) && line[hostEnd] == ':' {
		k := hostEnd + 1
		for k < len(line) && line[k] >= '0' && line[k] <= '9' {
			k++
		}
		if k > hostEnd+1 {
			portEnd = k
		}
	}
	after := hostEnd
	if portEnd >= 0 {
		after = portEnd
	}
	if after < len(line) && (line[after] == '/' || line[after] == '?' || line[after] == '#') {
		pathStart = after
	}
	return
}

// splitLoginPassword validates the login/password tail that starts at start,
// where nextColon ends the login: login must be class-valid and non-empty,
// password non-empty (it may contain colons).
func splitLoginPassword(line string, start, nextColon int) (login, password string, ok bool) {
	if start >= nextColon || nextColon+1 >= len(line) {
		return "", "", false
	}
	login = strings.TrimSpace(line[start:nextColon])
	password = strings.TrimSpace(line[nextColon+1:])
	if password == "" || !validLoginClass(login) {
		return "", "", false
	}
	return login, password, true
}

// validLoginClass accepts the established bare-login and dotted-email forms,
// plus the approved real-world extensions: leading '+' and internal ASCII
// spaces. Space-bearing logins stay bounded to the same harmless identifier
// bytes (plus one '@'); '/', '|', '$', braces, controls and non-ASCII bytes
// remain rejected. Email-shaped logins without spaces retain the strict dotted
// domain validation.
func validLoginClass(login string) bool {
	if login == "" {
		return false
	}
	if strings.ContainsRune(login, ' ') {
		if login[0] == ' ' || login[len(login)-1] == ' ' || strings.Count(login, "@") > 1 {
			return false
		}
		for i := range login {
			c := login[i]
			if c != ' ' && c != '@' && c != '+' && !isLoginBareByte(c) {
				return false
			}
		}
		return true
	}

	at := strings.IndexByte(login, '@')
	if at < 0 {
		for i := range login {
			if login[i] != '+' && !isLoginBareByte(login[i]) {
				return false
			}
		}
		return true
	}
	if at != strings.LastIndexByte(login, '@') {
		return false
	}
	local, domain := login[:at], login[at+1:]
	if local == "" || domain == "" {
		return false
	}
	for i := range local {
		c := local[i]
		if !isASCIIAlpha(c) && !(c >= '0' && c <= '9') && c != '_' && c != '.' && c != '-' {
			return false
		}
	}
	// domain = [a-zA-Z0-9.-]+ '.' [a-zA-Z]{2,}
	dot := strings.LastIndexByte(domain, '.')
	if dot <= 0 || len(domain)-dot-1 < 2 {
		return false
	}
	for i := range domain {
		c := domain[i]
		if !isASCIIAlpha(c) && !(c >= '0' && c <= '9') && c != '.' && c != '-' {
			return false
		}
	}
	for i := dot + 1; i < len(domain); i++ {
		if !isASCIIAlpha(domain[i]) {
			return false
		}
	}
	return true
}

func isWordByte(c byte) bool {
	return c == '_' || isASCIIAlpha(c) || (c >= '0' && c <= '9')
}

func isLoginBareByte(c byte) bool {
	c = lowerByte(c)
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
}

func lowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// shared post-match hygiene for ULP and LPU
func finishParse(url, login, password string) (host, urlOut, loginOut, passwordOut string, ok bool) {
	host, urlOut, loginOut, passwordOut, ok, _ = finishParseReason(url, login, password)
	return host, urlOut, loginOut, passwordOut, ok
}

// ValidateFields applies the selected builtin lane's admission rules to already
// separated URL, login, and password fields. It never interprets a colon in
// one field as a credential boundary. url must be the URL component selected
// for output (including any caller-side NoURI projection).
func ValidateFields(url, login, password string, loose bool) (host, urlOut, loginOut, passwordOut string, ok bool) {
	url = strings.TrimSpace(url)
	login = strings.TrimSpace(login)
	password = strings.TrimSpace(password)
	if !withinParsedFieldLimit(url, login, password) {
		return "", "", "", "", false
	}
	if strings.HasPrefix(url, androidScheme) {
		payload := url[len(androidScheme):]
		if payload == "" || strings.ContainsRune(payload, ':') || login == "" || password == "" || !validLoginClass(login) {
			return "", "", "", "", false
		}
		return url, url, login, password, true
	}
	strict := strictLanePredicate(url, login, password)
	if !loose && !strict {
		return "", "", "", "", false
	}
	if loose && !strict && (isLikelyJunkFields(url, login, password) || !looseURLComponent(url)) {
		return "", "", "", "", false
	}
	return finishParse(url, login, password)
}

// strictLanePredicate is the strict admission condition ValidateFields applies
// to already-separated fields. ValidateFields and StrictLaneAdmitted share
// this one predicate so the two cannot drift.
func strictLanePredicate(url, login, password string) bool {
	if strictURLComponent(url) && validLoginClass(login) {
		return true
	}
	// P3 mirror of parseColonFallback: a bare fallback host URL with a
	// strict-valid login and a password owning later colons is the
	// four-or-more-field prerequisite the strict raw fallback admits
	// (url:login:password:…); a colon-free password stays a strict
	// three-field bare-IP record and remains rejected.
	return validFallbackHost(url) && validLoginClass(login) && strings.Contains(password, ":")
}

// StrictLaneAdmitted reports whether the given already-separated fields would
// be admitted through the STRICT component path of ValidateFields — the exact
// `strict` condition ValidateFields computes, via the shared
// strictLanePredicate. Records admitted via the androidScheme branch bypass
// the strict component check inside ValidateFields and are therefore reported
// as NOT strict-lane (they keep the full dual-fidelity emission check — a
// deliberately safe default).
func StrictLaneAdmitted(url, login, password string) bool {
	url = strings.TrimSpace(url)
	login = strings.TrimSpace(login)
	password = strings.TrimSpace(password)
	if strings.HasPrefix(url, androidScheme) {
		return false
	}
	return strictLanePredicate(url, login, password)
}

func withinParsedFieldLimit(url, login, password string) bool {
	remaining := maxParsedLineLen
	for _, size := range [...]int{len(stripScheme(url)), len(login), len(password), 2} {
		if size > remaining {
			return false
		}
		remaining -= size
	}
	return true
}

// strictURLComponent applies the URL-head rules used by the strict byte scanner
// to a URL field without scanning its path for credential separators.
func strictURLComponent(url string) bool {
	pos := schemePrefixLen(url)
	hostEnd, portEnd, pathStart := scanURLHead(url, pos)
	if hostEnd < 0 {
		return strictFallbackURLComponent(url)
	}
	end := hostEnd
	if portEnd >= 0 {
		end = portEnd
	}
	if end == len(url) {
		return true
	}
	return pathStart >= 0 && pathStart == end
}

// strictFallbackURLComponent applies the bare fallback host rules
// (validFallbackHost + a numeric port) to a URL field whose dotted-TLD scan
// failed — IPv4-like hosts without a TLD. The COMPLETE port/path suffix is
// validated: after the port only a path (starting with '/') or end-of-field is
// allowed, so a bare-IP port/query or port/fragment tail like 103.1.2.3:8080?x
// is rejected exactly like the strict raw parser's splitPortPath rule, while
// valid /path?query and /path#fragment suffixes stay admitted.
func strictFallbackURLComponent(url string) bool {
	urlPart := stripScheme(url)
	colon := strings.IndexByte(urlPart, ':')
	if colon <= 0 || !validFallbackHost(urlPart[:colon]) {
		return false
	}
	_, _, ok := splitPortPath(urlPart[colon+1:])
	return ok
}

// rejectTagPasswordTooLong names the 64-char password cap in -debug-reject
// lines and the per-reason counters: the only finishParse rule consumers
// need to bisect for (a real stealer-log line otherwise indistinguishable
// from generic malformed).
const rejectTagPasswordTooLong = "password>64"

// finishParseReason is finishParse with the reject reason surfaced for
// diagnostics. ok=true carries no reason; ok=false carries
// rejectTagPasswordTooLong only when the sole failure is the password cap —
// every other rule stays an untagged generic reject. Parse decisions are
// identical to finishParse.
func finishParseReason(url, login, password string) (host, urlOut, loginOut, passwordOut string, ok bool, reason string) {
	if login == "" || password == "" {
		return "", "", "", "", false, ""
	}
	// login must not contain ':' so FormatRecord / parseStored round-trips stay
	// field-faithful (password may contain colons).
	if strings.ContainsRune(login, ':') {
		return "", "", "", "", false, ""
	}
	host = url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	// The original host must be dotted before any www. normalization. www. is
	// stripped case-insensitively only when the remainder is still a dotted
	// hostname, preserving legitimate single-label hosts such as www.ai,
	// www.io, and www.com. Uppercase WWW. strips identically to lowercase so
	// the two spellings dedup to the same key.
	if !strings.ContainsRune(host, '.') {
		return "", "", "", "", false, ""
	}
	hygieneHost := host
	if len(host) >= 4 && strings.EqualFold(host[:4], "www.") {
		rest := host[4:]
		hygieneHost = rest
		if strings.ContainsRune(rest, '.') {
			host = rest
		}
	}
	localhostPrefix := len(hygieneHost) >= len("localhost") &&
		strings.EqualFold(hygieneHost[:len("localhost")], "localhost")
	if (strings.HasPrefix(hygieneHost, "127.") || localhostPrefix) && !strings.Contains(login, "@") {
		return "", "", "", "", false, ""
	}
	if wrappedBraces(host) || wrappedBraces(login) || wrappedBraces(password) {
		return "", "", "", "", false, ""
	}
	if strings.HasPrefix(login, "http://") || strings.HasPrefix(login, "https://") ||
		strings.HasPrefix(password, "http://") || strings.HasPrefix(password, "https://") {
		return "", "", "", "", false, ""
	}
	if len(password) > 64 {
		return "", "", "", "", false, rejectTagPasswordTooLong
	}
	// P6-W12: the hostname folds to lowercase for the canonical host/dedup
	// key only; the original url keeps its spelling for output formatting.
	return canonicalHost(host), url, login, password, true, ""
}

// rejectTag classifies a builtin-parser rejection for diagnostics: the short
// reason tag carried by -debug-reject lines and the per-reason counters. ""
// is the generic case — no specific rule owns the reject. It re-parses the
// line through the same parse order parseCore uses, so call it only on the
// reject path (rejects are rare; the accept path stays untouched). Parse
// decisions never depend on this: it is diagnostic plumbing only.
func rejectTag(line string) string {
	if len(line) == 0 || len(line) > maxParsedLineLen || !strings.Contains(line, ":") {
		return ""
	}
	if url, login, password, ok := parseStrictULP(line); ok {
		return finishRejectTag(url, login, password)
	}
	if url, login, password, ok := matchLPU(line); ok {
		return finishRejectTag(url, login, password)
	}
	return ""
}

// finishRejectTag reports the finishParse reject reason for an
// already-matched raw split, or "" when the split parses cleanly.
func finishRejectTag(url, login, password string) string {
	_, _, _, _, ok, reason := finishParseReason(url, login, password)
	if !ok {
		return reason
	}
	return ""
}

// canonicalHost folds the hostname portion of a parsed host to lowercase for
// the versioned dedup key: DNS names are case-insensitive, so Example.COM and
// example.com are one credential. A numeric port survives verbatim —
// net.SplitHostPort splits it when the shape matches (host:port); the parser's
// dotted-host grammar can also produce shapes SplitHostPort rejects (no port,
// glued suffixes, extra colons) and those fall back to folding the whole
// string, which is safe because digits carry no case. Only ASCII letters fold;
// percent/binary bytes are never touched.
func canonicalHost(host string) string {
	if h, port, err := net.SplitHostPort(host); err == nil && h != "" {
		return asciiLowerHost(h) + ":" + port
	}
	return asciiLowerHost(host)
}

// asciiLowerHost folds ASCII A-Z to a-z without allocating when nothing folds.
func asciiLowerHost(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := []byte(s)
	for i := range b {
		b[i] = lowerByte(b[i])
	}
	return string(b)
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

// DedupKeyForLine returns the library's canonical dedup key for an
// already-formatted ULP line — the same key Ingest derives for that line.
// ok=false when the line doesn't parse (the library would reject it). This
// lets an upstream producer (sfl's extraction) pre-dedup on the exact key the
// library uses, so its "unique" count reconciles with what ingest actually
// adds instead of over-counting path-only variants.
func DedupKeyForLine(line string, loose bool) (uint64, bool) {
	host, _, login, password, ok := parseFor(line, loose)
	if !ok {
		return 0, false
	}
	return dedupKeySum(host, login, password), true
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

// dedupKeyPreimageVersion separates the versioned, length-prefixed key
// preimage from the legacy colon-joined one (never read back; exists so the
// preimage is self-describing and future layouts cannot alias it).
const dedupKeyPreimageVersion = byte(1)

// dedupKeyMaxEncoded bounds the encoded preimage: host/login/password
// partition one parsed line (<= maxParsedLineLen bytes) plus one version byte
// and three uvarints.
const dedupKeyMaxEncoded = maxParsedLineLen + 1 + 3*binary.MaxVarintLen64

// appendDedupKeyFields appends the canonical dedup-key preimage for
// (host, login, password): one version byte, then each field as a uvarint
// length prefix followed by its bytes. The encoding is domain-separated and
// delimiter-free, so distinct field tuples always produce distinct preimages
// — the old host:login:password colon join collided across parses, e.g.
// example.com:80:user:p (port in host) and example.com/:80:user:p (port
// glued to the path) both joined to "example.com:80:user:p" and one distinct
// credential was silently dropped as a dup. Sole encoding helper for both
// dedup paths: lineFormatter.HashKey (streaming digest) and dedupKeySum.
func appendDedupKeyFields(dst []byte, host, login, password string) []byte {
	dst = append(dst, dedupKeyPreimageVersion)
	dst = binary.AppendUvarint(dst, uint64(len(host)))
	dst = append(dst, host...)
	dst = binary.AppendUvarint(dst, uint64(len(login)))
	dst = append(dst, login...)
	dst = binary.AppendUvarint(dst, uint64(len(password)))
	dst = append(dst, password...)
	return dst
}

// dedupKeySum hashes the encoded fields without a formatter — the stateless
// twin of lineFormatter.HashKey (same bytes, same hash), used by
// DedupKeyForLine / DedupKeyWith outside the per-goroutine formatter path.
// The stack buffer is sized for the guaranteed bound; append still handles
// growth defensively for out-of-band callers.
func dedupKeySum(host, login, password string) uint64 {
	var buf [dedupKeyMaxEncoded]byte
	return xxhash.Sum64(appendDedupKeyFields(buf[:0], host, login, password))
}

// reusable buffer + streaming digest for zero-alloc per-line formatting.
// one per goroutine, NOT safe for concurrent use. buffer returned by
// FormatRecord is reused on next call, caller must consume before reusing.
type lineFormatter struct {
	out    bytes.Buffer
	digest *xxhash.Digest
	keyBuf []byte // scratch for the encoded dedup-key preimage
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
// Verification is field-faithful (not key-only): the encoded key is
// unambiguous, but a serialized line that re-parses to different fields would
// silently change identity on regen. Every candidate is verified — including
// clean ≤2-colon lines.
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

// StableFormatter is a reusable worker-local stable formatter. Its result is
// an owned string and remains valid after subsequent calls; instances are not
// safe for concurrent use.
type StableFormatter struct {
	formatter *lineFormatter
}

// NewStableFormatter creates a reusable formatter for stable record output.
func NewStableFormatter() *StableFormatter {
	return &StableFormatter{formatter: newLineFormatter()}
}

// FormatRecordStable returns an owned stable record string and whether the
// record can be represented without changing host, login, or password.
func (f *StableFormatter) FormatRecordStable(host, url, login, password string, noURI bool) (string, bool) {
	out, ok := f.formatter.FormatRecordStable(host, url, login, password, noURI)
	if !ok {
		return "", false
	}
	return string(out), true
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

// xxhash64 over the versioned, length-prefixed (host, login, password)
// preimage via streaming digest, 0 allocs steady-state (keyBuf grows once).
// Same bytes and hash as the stateless dedupKeySum helper.
func (lf *lineFormatter) HashKey(host, login, password string) uint64 {
	lf.keyBuf = appendDedupKeyFields(lf.keyBuf[:0], host, login, password)
	lf.digest.Reset()
	_, _ = lf.digest.Write(lf.keyBuf)
	return lf.digest.Sum64()
}

// HashKeyStrong is HashKey plus a strong 128-bit identity of the same
// preimage (SHA-256 condensed to 16 bytes). Fast-path dedup stores the
// identity beside each 64-bit key so an exact xxHash64 collision between two
// distinct credentials no longer drops one (review C-02). Steady-state cost
// is one SHA-256 over the preimage per accepted line.
func (lf *lineFormatter) HashKeyStrong(host, login, password string) (uint64, [16]byte) {
	lf.keyBuf = appendDedupKeyFields(lf.keyBuf[:0], host, login, password)
	lf.digest.Reset()
	_, _ = lf.digest.Write(lf.keyBuf)
	return lf.digest.Sum64(), strongKeyID(lf.keyBuf)
}
