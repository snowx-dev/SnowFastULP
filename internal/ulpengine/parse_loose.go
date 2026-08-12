package ulpengine

import (
	"strings"
)

// high-recall counterpart to parse(). accepts strict shapes PLUS:
//   - host:port:login:password (ftp/ssh/mail dumps)
//   - bare host:login:password (no scheme, eg `undefined:user:pw`)
//
// not the 90x speedup a naive 2-colon rule would give. wins come from:
//  1. rejects fail ~5x faster (matchLPU + byte scans, no regex backtrack)
//  2. ~5-10% more credentials recovered
//
// isLikelyJunk pre-filter keeps the new admit surface bounded.
func parseLoose(line string) (host, url, login, password string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 5 || len(line) > maxParsedLineLen {
		return "", "", "", "", false
	}
	if isLikelyJunk(line) {
		return "", "", "", "", false
	}

	// strict first, ~95% of inputs land here. running strict first means
	// loose-mode output bytes match strict-mode for any line both agree on
	if h, u, l, p, ok := parse(line); ok {
		return h, u, l, p, true
	}

	return looseExtrasTrimmed(line)
}

// looseExtrasTrimmed handles the loose-only colon shapes (bare
// host:login:password, host:port:login:password). Caller guarantees the line
// is already TrimRight'd, within length bounds, and not isLikelyJunk.
func looseExtrasTrimmed(line string) (host, url, login, password string, ok bool) {
	first := strings.IndexByte(line, ':')
	if first <= 0 {
		return "", "", "", "", false
	}
	if last := strings.LastIndexByte(line, ':'); last == first {
		return "", "", "", "", false
	}

	parts := splitNColon(line, 5)
	switch len(parts) {
	case 3:
		// bare host:login:password, regex rejects b/c login class excludes
		// `:` and url group needs a TLD-ish pattern
		return finishLoose(parts[0], parts[1], parts[2])
	case 4:
		// host:port:login:password recognised via a leading digit run in the
		// 2nd field, optionally followed by /path (e.g. "8080/auth/login").
		// Real stealer logs glue the URL path onto the port when the host is a
		// bare IP, so the all-digits check alone dropped lines like
		// "103.181.181.122:8897/:admin:admin". The port+path is recombined
		// into the url so finishParse still strips the path to a host for the
		// dedup key.
		if port, rest, ok := splitPortPath(parts[1]); ok {
			return finishLoose(parts[0]+":"+port+rest, parts[2], parts[3])
		}
		return "", "", "", "", false
	default:
		return "", "", "", "", false
	}
}

// parseUnion is the raw-ingest index helper: strict OR loose (with junk gate on
// the loose extras path). Sidecar regen and FormatRecordStable verification use
// parseStored instead — trusted archive lines must not be junk-filtered.
func parseUnion(line string) (host, url, login, password string, ok bool) {
	if h, u, l, p, ok := parse(line); ok {
		return h, u, l, p, true
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 5 || len(line) > maxParsedLineLen {
		return "", "", "", "", false
	}
	if isLikelyJunk(line) {
		return "", "", "", "", false
	}
	return looseExtrasTrimmed(line)
}

// finishParse w/ url=="" treated as "use host as url"
func finishLoose(url, login, password string) (host, urlOut, loginOut, passwordOut string, ok bool) {
	if url == "" || login == "" || password == "" {
		return "", "", "", "", false
	}
	return finishParse(url, login, password)
}

// non-credential shapes the 2-colon rule would admit. cheap substring scans,
// well under 100ns. covers JSON blobs, windows cred mgr exports,
// openbullet form metadata, inline form-field labels.
func isLikelyJunk(line string) bool {
	if strings.Contains(line, `{"`) || strings.Contains(line, `":"`) {
		return true
	}
	if strings.HasPrefix(line, "LegacyGeneric:") || strings.Contains(line, ":target=") {
		return true
	}
	if strings.Contains(line, "\\t\\t1:") || strings.Contains(line, "\t\t1:") {
		return true
	}
	if strings.Contains(line, ":PasswordText ") ||
		strings.Contains(line, ":PasswordResetRequestForm") ||
		strings.Contains(line, ":Username ") ||
		strings.Contains(line, ":LoginID") {
		return true
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// splitPortPath splits a "port" or "port/path..." field (the 2nd colon group of
// a host:port:login:password line) into a leading digit run (the port) and
// the remainder (the path, possibly empty, including its leading "/").
// ok=false when the field doesn't start with at least one digit, or when the
// digit run is followed by anything other than "/" or end-of-field, so a
// non-numeric 2nd field like "user" (in host:user:pw:extra) stays rejected
// exactly as before. A bare digit run returns rest="" (no path).
func splitPortPath(field string) (port, rest string, ok bool) {
	i := 0
	for i < len(field) && field[i] >= '0' && field[i] <= '9' {
		i++
	}
	if i == 0 {
		return "", "", false
	}
	rest = field[i:]
	if rest != "" && rest[0] != '/' {
		return "", "", false
	}
	return field[:i], rest, true
}

// colon-only strings.SplitN, alloc-light at billions-of-lines scale
func splitNColon(s string, n int) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for i := 0; i < n-1; i++ {
		idx := strings.IndexByte(s, ':')
		if idx < 0 {
			out = append(out, s)
			return out
		}
		out = append(out, s[:idx])
		s = s[idx+1:]
	}
	out = append(out, s)
	return out
}
