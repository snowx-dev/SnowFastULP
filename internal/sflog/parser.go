package sflog

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/snowx-dev/SnowFastULP/internal/textenc"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

type credBlock struct {
	url      string
	username string
	password string
}

func ParseCredentials(r io.Reader, source string) ([]Credential, error) {
	// Stealer logs are routinely UTF-16LE-BOM (RedLine/Vidar) or UTF-8-BOM;
	// decode to UTF-8 first so a Windows-origin Passwords.txt isn't parsed as
	// garbage (silent zero creds) or stripped of its first record. The label-less
	// colon-line fallback (Raccoon pws.txt / label-less StealC) needs to re-scan
	// the same bytes, so the decoded stream is buffered into a capped []byte and
	// each pass streams it via a bufio.Scanner over a bytes.Reader — one line in
	// memory at a time, no []string materialization of every line. The cap is a
	// per-file budget (password files are small, but a Vidar/Lumma aggregate can
	// be a few MiB; 16 MiB is generous and still bounds memory across workers).
	// Reading one byte past the cap detects overflow so a too-large member errors
	// out instead of being silently truncated.
	decoded := textenc.WrapReader(r)
	probe := io.LimitReader(decoded, maxParseBuffer+1)
	body, err := io.ReadAll(probe)
	if err != nil {
		return nil, err
	}
	if len(body) > maxParseBuffer {
		return nil, errParseBufferExceeded
	}
	br := bytes.NewReader(body)

	out, err := parseLabeled(br, source)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		// Labeled pass found nothing: try the label-less url:user:pass layout
		// (Raccoon pws.txt, some StealC passwords.txt) before giving up. The
		// fallback reuses the library's ULP line parser so the two pipelines
		// never disagree on what counts as a credential.
		br.Reset(body)
		out, err = parseColonLines(br, source)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// errParseBufferExceeded is returned by ParseCredentials when a decoded member
// exceeds maxParseBuffer, mirroring bufio.ErrTooLong from the pre-fallback
// scanner so engine.go reports it as a parse error / skipped file exactly
// as before.
var errParseBufferExceeded = errors.New("sflog: credential member exceeds parse buffer limit")

// maxParseBuffer is the per-file byte budget for the buffered re-scan. It bounds
// memory for the two-pass design (the body is held in memory, unlike the old
// single-pass streaming scanner). 16 MiB comfortably fits any real password file
// (a Vidar/Lumma aggregate is a few MiB) while bounding peak memory across
// parser workers.
const maxParseBuffer = 16 * 1024 * 1024

// maxScanLineLen is the per-line cap each pass enforces via sc.Buffer, matching
// the pre-fallback scanner's per-token ceiling so a single oversized line still
// errors (bufio.ErrTooLong) exactly as before.
const maxScanLineLen = 4 * 1024 * 1024

// parseLabeled runs the Key: value block scan, byte-identical to the pre-fallback
// ParseCredentials. It streams r one line at a time and returns every complete
// url+username+password block in encounter order. A scanner error (e.g. a line
// exceeding maxScanLineLen) is propagated to the caller.
func parseLabeled(r io.Reader, source string) ([]Credential, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxScanLineLen)

	var out []Credential
	var block credBlock
	flush := func() {
		if block.url != "" && block.username != "" && block.password != "" {
			out = append(out, Credential{
				URL: block.url, Username: block.username, Password: block.password, Source: source,
			})
		}
		block = credBlock{}
	}

	for sc.Scan() {
		raw := strings.TrimRight(sc.Text(), "\r\n")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || isSeparator(trimmed) {
			flush()
			continue
		}
		key, val, ok := splitField(raw)
		if !ok || val == "" {
			continue
		}
		switch classifyField(key) {
		case "url":
			// A second url-class field re-appearing signals a new record only
			// when the current one already has a user+password (url-first
			// layout). On an incomplete block it is a duplicate alias within
			// the same record (e.g. URL + Host), so keep the first.
			if block.url != "" {
				if block.username != "" && block.password != "" {
					flush()
					block.url = val
				}
			} else {
				block.url = val
			}
		case "username":
			if block.username != "" {
				flush()
			}
			block.username = val
		case "password":
			if block.password != "" {
				flush()
			}
			block.password = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}

// parseColonLines is the label-less fallback for Raccoon pws.txt / label-less
// StealC passwords.txt: one url:login:password per line, no field labels. It
// streams r one line at a time and reuses ulpengine.ParseLine (strict) so it
// agrees with the library parser on what is a credential. Lines that don't
// parse are skipped, matching the labeled path's "no ULP" behavior.
func parseColonLines(r io.Reader, source string) ([]Credential, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxScanLineLen)

	var out []Credential
	for sc.Scan() {
		line := sc.Text()
		if line == "" || isSeparator(strings.TrimSpace(line)) {
			continue
		}
		// Strict (loose=false) is intentional: the fallback targets label-less
		// url:login:password files (Raccoon pws.txt, StealC passwords.txt), which
		// are strict ULP shapes. -loose is not plumbed here because loose-only
		// shapes (host:port:user:pw, bare host:user:pw, LPU) are the labeled
		// extraction's job, not the fallback's. A -loose user with a Raccoon
		// pws.txt of host:port:user:pw lines gets 0 from this fallback by design.
		_, url, login, password, ok := ulpengine.ParseLine(line, false)
		if !ok {
			continue
		}
		// The labeled path stores the full URL it saw; ParseLine returns the
		// parsed url field (which carries scheme/path for android and LPU lines),
		// so store that for FormatULPLine to emit consistently. The dedup host
		// is discarded — finishParse already stripped it for the library key.
		out = append(out, Credential{
			URL:      url,
			Username:  login,
			Password:  password,
			Source:    source,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// splitField parses "Key: value". The key is fully trimmed; the value keeps its
// meaningful whitespace, dropping only the single conventional delimiter space
// after the colon so passwords with leading/trailing spaces survive.
func splitField(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(line[:i]))
	val = strings.TrimPrefix(line[i+1:], " ")
	return key, val, true
}

// fieldAliases maps a normalized key (lowercased, internal whitespace collapsed to
// single spaces) to its credential class. Add a new alias by appending to the
// matching map and to the allAliases slice below (the slice drives the parity
// test). Every entry must cite at least one external parser that attests it;
// aliases with no real-data sighting are marked "(attested only)" so the next
// auditor knows they came from cross-parser comparison, not from a log we hold.
//
// Sources: lexfo/stealer-parser, TreRB/stealerlogs, githubesson/processor,
// TSP68/tsp68-checker (see the audit in tmp/field_audit.py + the research
// notes). The local Vidar/Lumma sample (tmp/vidar_inspect, tmp/pr34_inspect)
// uses only soft/host/login/password; the additions below cover other strains
// (Raccoon, StealC, Rhadamanthys, MetaStealer, ...) attested by the sources.
//
// Deliberately excluded (single-parser, speculative, or false-positive prone):
//   pin, pincode, passcode — PINs are not web passwords (githubesson only).
//   phone, phonenumber, mobile — phone-as-login is debatable (githubesson only).
//   soft (as URL) — "soft" is the browser label, not the URL (TreRB only).
var (
	urlAlias = map[string]bool{
		"url":      true, // lexfo, TreRB, githubesson
		"ur1":      true, // lexfo leet-speak variant
		"host":      true, // lexfo, TreRB, githubesson
		"hostname":  true, // lexfo, TreRB, githubesson
		"website":  true, // TreRB, githubesson (attested only)
		"domain":   true, // githubesson (attested only)
		"site":     true, // githubesson (attested only)
		"link":     true, // githubesson (attested only)
		"uri":      true, // githubesson (attested only)
	}
	userAlias = map[string]bool{
		"user":       true, // lexfo, TreRB, githubesson
		"login":      true, // lexfo, TreRB, githubesson
		"username":   true, // lexfo, TreRB, githubesson
		"user login": true, // lexfo
		"u53rn4m3":  true, // lexfo leet-speak variant
		"email":     true, // TreRB, githubesson, TSP68 (attested only)
		"mail":      true, // githubesson (attested only)
		"account":   true, // githubesson (attested only)
		"acc":       true, // githubesson (attested only)
		"usr":       true, // TreRB (attested only)
		"user name":  true, // TreRB (attested only; spaced, missed before b/c the switch had no case)
	}
	passAlias = map[string]bool{
		"pass":          true, // lexfo, TreRB, githubesson
		"password":      true, // lexfo, TreRB, githubesson
		"user password": true, // lexfo
		"p455w0rd":      true, // lexfo leet-speak variant
		"passwd":        true, // TreRB, githubesson (attested only)
		"pwd":           true, // TreRB, githubesson (attested only)
	}
)

// allAliases drives TestClassifyFieldAliases: every (alias, class) pair must
// appear here so the parity test fails the moment a map gains an entry the
// slice doesn't list (and vice versa).
var allAliases = []struct {
	alias string
	class string
}{
	{"url", "url"}, {"ur1", "url"}, {"host", "url"}, {"hostname", "url"},
	{"website", "url"}, {"domain", "url"}, {"site", "url"}, {"link", "url"}, {"uri", "url"},
	{"user", "username"}, {"login", "username"}, {"username", "username"},
	{"user login", "username"}, {"u53rn4m3", "username"},
	{"email", "username"}, {"mail", "username"}, {"account", "username"},
	{"acc", "username"}, {"usr", "username"}, {"user name", "username"},
	{"pass", "password"}, {"password", "password"}, {"user password", "password"},
	{"p455w0rd", "password"}, {"passwd", "password"}, {"pwd", "password"},
}

func classifyField(key string) string {
	key = strings.Join(strings.Fields(key), " ")
	switch {
	case urlAlias[key]:
		return "url"
	case userAlias[key]:
		return "username"
	case passAlias[key]:
		return "password"
	default:
		return ""
	}
}

func isSeparator(line string) bool {
	if len(line) < 3 {
		return false
	}
	for _, r := range line {
		if r != '=' && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func FormatULPLine(c Credential, noURI bool) string {
	urlPart := normalizeURL(strings.TrimSpace(c.URL))
	host := hostFromURL(urlPart)
	if noURI {
		urlPart = host
	}
	var b strings.Builder
	b.Grow(len(urlPart) + len(c.Username) + len(c.Password) + 2)
	b.WriteString(urlPart)
	b.WriteByte(':')
	b.WriteString(c.Username)
	b.WriteByte(':')
	b.WriteString(c.Password)
	return b.String()
}

// normalizeURL drops the http(s) scheme so web URLs match the sfu line shape.
// Non-web schemes (android://, etc.) are kept verbatim, exactly like sfu's
// stripScheme, so the two pipelines emit identical lines for the same input.
func normalizeURL(s string) string {
	return stripWebScheme(s)
}

func stripWebScheme(s string) string {
	if len(s) >= 7 && strings.EqualFold(s[:7], "http://") {
		return s[7:]
	}
	if len(s) >= 8 && strings.EqualFold(s[:8], "https://") {
		return s[8:]
	}
	return s
}

func hostFromURL(s string) string {
	raw := s
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		return strings.TrimPrefix(u.Host, "www.")
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "www.")
}
