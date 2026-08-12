package ulpengine

import (
	"sort"
	"strings"
)

// parseStored is the trusted-archive / regen reader. It recovers
// (host, url, login, password) from a line that FormatRecord (or an older
// ingest path) wrote into a library .zst. Unlike parseUnion it never applies
// isLikelyJunk — archive lines are trusted — and it can re-read logins that
// ulpPattern rejects (spaces, $, unicode) as long as finishParse accepts them.
//
// Order:
//  1. parseAndroid (unchanged scheme)
//  2. strict parse() (key-identical to today for the common case)
//  3. FormatRecord inverse (urlPart may contain :port[/path]; login is [^:]+;
//     password is the remainder and may contain ':')
func parseStored(line string) (host, url, login, password string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" || len(line) > maxParsedLineLen {
		return "", "", "", "", false
	}
	if h, u, l, p, ok := parseAndroid(line); ok {
		// android bypasses finishParse historically; still ban ':' in login
		// for HashKey field identity (same rule as finishParse).
		if l == "" || p == "" || strings.ContainsRune(l, ':') {
			return "", "", "", "", false
		}
		return h, u, l, p, true
	}
	if h, u, l, p, ok := parse(line); ok {
		return h, u, l, p, true
	}
	return formatRecordInverse(line)
}

// formatRecordInverse undoes FormatRecord's stripScheme(url):login:password
// layout. login must not contain ':'; urlPart may include :port and /path.
// Candidates are tried longest-url first through finishParse so a longer
// prefix that fails hygiene can fall through to a shorter valid split.
func formatRecordInverse(line string) (host, url, login, password string, ok bool) {
	type cand struct {
		url, login, pass string
	}
	var cands []cand
	for i := 0; i < len(line); i++ {
		if line[i] != ':' {
			continue
		}
		urlPart := line[:i]
		rest := line[i+1:]
		j := strings.IndexByte(rest, ':')
		if j <= 0 {
			continue // empty login or no password separator
		}
		candLogin := rest[:j]
		candPass := rest[j+1:]
		if candLogin == "" || candPass == "" {
			continue
		}
		if !isStoredURLPrefix(urlPart) {
			continue
		}
	cands = append(cands, cand{urlPart, candLogin, candPass})
	}
	sort.Slice(cands, func(i, j int) bool {
		return len(cands[i].url) > len(cands[j].url)
	})
	for _, c := range cands {
		if h, u, l, p, ok := finishParse(c.url, c.login, c.pass); ok {
			return h, u, l, p, true
		}
	}
	return "", "", "", "", false
}

// isStoredURLPrefix reports whether s looks like a FormatRecord urlPart:
// host-ish text with a dotted hostname, optional :digits port, optional /?# path.
// Colons are allowed only in the authority (before the first /?#); after a path
// starts, no further ':' — so example.com/:12345 is not a valid urlPart.
func isStoredURLPrefix(s string) bool {
	if s == "" {
		return false
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		if strings.ContainsRune(s[i:], ':') {
			return false
		}
	}
	host := s
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if host == "" || !strings.ContainsRune(host, '.') {
		return false
	}
	// optional :port — if a colon remains in the authority, it must be digits only
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		port := host[i+1:]
		if port == "" || !allDigits(port) {
			return false
		}
		// bare ":8080" with no hostname before colon
		if i == 0 {
			return false
		}
	}
	return true
}

func allDigits(s string) bool {
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
