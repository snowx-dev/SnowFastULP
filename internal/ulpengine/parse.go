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

// formats final output line. noURI drops path/query, keeps host:port.
// dedup key is always host:login:password so flipping noURI never adds
// visual dupes
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
	lf.out.Reset()
	lf.out.Grow(len(urlPart) + len(login) + len(password) + 2)
	lf.out.WriteString(urlPart)
	lf.out.WriteByte(':')
	lf.out.WriteString(login)
	lf.out.WriteByte(':')
	lf.out.WriteString(password)
	return lf.out.Bytes()
}

// FormatRecord + '\n' for sinks that want newline-terminated input
func (lf *lineFormatter) FormatRecordLine(host, url, login, password string, noURI bool) []byte {
	_ = lf.FormatRecord(host, url, login, password, noURI)
	lf.out.WriteByte('\n')
	return lf.out.Bytes()
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
