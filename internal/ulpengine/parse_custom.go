package ulpengine

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/cespare/xxhash/v2"
)

// LineParser is the input-parsing seam for ingest. Implementations are
// immutable after construction and safe for concurrent use by shard workers.
//
// Custom parsers only change how raw input lines become (host, url, login,
// password). Output is still formatted by lineFormatter into canonical colon
// ULP. Sidecar regen and FormatRecordStable verification use parseStored
// (FormatRecord inverse), so looser custom logins that pass finishParse remain
// indexable in -od libraries.
type LineParser interface {
	Parse(line string) (host, url, login, password string, ok bool)
}

// builtinParser preserves the existing strict/loose behavior.
type builtinParser struct{ loose bool }

func (p builtinParser) Parse(line string) (host, url, login, password string, ok bool) {
	return parseFor(line, p.loose)
}

// defaultParser picks strict or strict+loose like parseFor did inline before.
func defaultParser(loose bool) LineParser { return builtinParser{loose: loose} }

// ParseWith runs p over line. p==nil is a strict parse (safe zero value for
// tests that never set a parser).
func ParseWith(p LineParser, line string) (host, url, login, password string, ok bool) {
	if p == nil {
		return parseFor(line, false)
	}
	return p.Parse(line)
}

// DedupKeyWith is DedupKeyForLine against an explicit parser.
func DedupKeyWith(p LineParser, line string) (uint64, bool) {
	host, _, login, password, ok := ParseWith(p, line)
	if !ok {
		return 0, false
	}
	return xxhash.Sum64String(dedupKey(host, login, password)), true
}

// ---- delimiter mode -------------------------------------------------------

// DelimParser splits each line on a fixed separator into exactly
// url<sep>login<sep>password. Any other field count is a reject; a password
// containing the separator therefore rejects instead of misparsing.
type DelimParser struct {
	sep string
}

// NewDelimParser validates the separator. It must be non-empty and must not
// contain ':' (ambiguity with ULP), '/' (ambiguity with URLs) or a newline.
func NewDelimParser(sep string) (LineParser, error) {
	if sep == "" {
		return nil, fmt.Errorf("delimiter must not be empty")
	}
	if strings.ContainsAny(sep, ":/\r\n") {
		return nil, fmt.Errorf("delimiter %q must not contain ':', '/' or a newline", sep)
	}
	return DelimParser{sep: sep}, nil
}

func (p DelimParser) Parse(line string) (host, url, login, password string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" || len(line) > maxParsedLineLen {
		return "", "", "", "", false
	}
	parts := strings.Split(line, p.sep)
	if len(parts) != 3 {
		return "", "", "", "", false
	}
	// empty fields would serialize to host::pw / host:user: which parseStored
	// cannot re-read — reject here (RegexRulesParser already requires non-empty).
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", "", false
	}
	return finishParse(parts[0], parts[1], parts[2])
}

// ---- regexp-file mode ------------------------------------------------------

// RegexRulesParser tries each user-supplied pattern top to bottom; the first
// pattern whose named groups url (or host), login and password all capture
// non-empty values imports the line. A line never becomes multiple
// credentials. No match = reject.
type RegexRulesParser struct {
	rules []*regexp.Regexp
}

// NewRegexRulesParser compiles a rules file: one regexp per line, blank lines
// and '#' comments skipped. Every pattern must define named groups
// "login" and "password", plus "url" or "host".
func NewRegexRulesParser(path string) (LineParser, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var rules []*regexp.Regexp
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		re, err := regexp.Compile(text)
		if err != nil {
			return nil, 0, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if err := validateRuleGroups(re); err != nil {
			return nil, 0, fmt.Errorf("line %d: %w", lineNo, err)
		}
		rules = append(rules, re)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	if len(rules) == 0 {
		return nil, 0, fmt.Errorf("rules file %s has no usable patterns", path)
	}
	return &RegexRulesParser{rules: rules}, len(rules), nil
}

func validateRuleGroups(re *regexp.Regexp) error {
	names := map[string]bool{}
	for _, n := range re.SubexpNames() {
		if n != "" {
			names[n] = true
		}
	}
	if !names["login"] || !names["password"] {
		return fmt.Errorf("pattern must define named groups (?P<login>…) and (?P<password>…)")
	}
	if !names["url"] && !names["host"] {
		return fmt.Errorf("pattern must define a named group (?P<url>…) or (?P<host>…)")
	}
	return nil
}

func (p *RegexRulesParser) Parse(line string) (host, url, login, password string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" || len(line) > maxParsedLineLen {
		return "", "", "", "", false
	}
	for _, re := range p.rules {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var u, h, l, pw string
		for i, name := range re.SubexpNames() {
			if i == 0 || name == "" || i >= len(m) {
				continue
			}
			switch name {
			case "url":
				u = m[i]
			case "host":
				h = m[i]
			case "login":
				l = m[i]
			case "password":
				pw = m[i]
			}
		}
		if u == "" && h != "" {
			u = h
		}
		if u == "" || l == "" || pw == "" {
			continue
		}
		if host, urlOut, loginOut, passwordOut, ok := finishParse(u, l, pw); ok {
			return host, urlOut, loginOut, passwordOut, true
		}
	}
	return "", "", "", "", false
}
