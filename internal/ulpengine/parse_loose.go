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
		// host:port:login:password recognised via digit-only 2nd field
		if isAllDigits(parts[1]) {
			return finishLoose(parts[0]+":"+parts[1], parts[2], parts[3])
		}
		return "", "", "", "", false
	default:
		return "", "", "", "", false
	}
}

// parseUnion is the index/regen parser: it admits a key for any line that
// EITHER strict parse() OR loose parseLoose() would accept. strict runs first
// WITHOUT the isLikelyJunk gate (the gate is a loose-mode recall/precision
// heuristic, not an index-fidelity rule), so strict-only creds like
// host:user:{"uid":...} are indexed; the loose-only colon shapes then run
// behind the gate. This guarantees a part's sidecar can never miss a line the
// archive actually stored, regardless of the mode it was written/ingested in.
// Keys match parse() exactly for any line both accept (strict branch reuses it,
// and parseLoose itself runs strict-first), so dedup stays consistent.
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
