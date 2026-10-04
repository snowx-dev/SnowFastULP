package sflog

import (
	"bufio"
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

// ParseCredentialsStream parses r one line at a time in strict mode and emits
// every credential in encounter order. Production callers that expose parser
// mode use ParseCredentialsStreamWithMode.
func ParseCredentialsStream(r io.Reader, source, tempDir string, emit func(Credential) error) (mixed bool, err error) {
	return ParseCredentialsStreamWithMode(r, source, tempDir, false, emit)
}

// ParseCredentialsStreamWithMode parses with the same shared strict/loose
// grammar selected by sfu. Both scans (labeled Key: value blocks and the
// label-less ULP fallback) run over every line, and the labeled scan wins when
// it found anything. Each scan lands in a bounded credSink and only the winner
// is replayed at EOF.
//
// The returned mixed flag is true when BOTH scans found records, so callers
// preserve the source and withhold history completion.
func ParseCredentialsStreamWithMode(r io.Reader, source, tempDir string, loose bool, emit func(Credential) error) (mixed bool, err error) {
	return ParseCredentialsStreamWithDiagnostics(r, source, tempDir, loose, false, nil, emit)
}

// ParseCredentialsStreamWithDiagnostics runs bounded raw ambiguity checks when requested.
func ParseCredentialsStreamWithDiagnostics(r io.Reader, source, tempDir string, loose, diagnostics bool, onAmbiguity func(AmbiguityCounts), emit func(Credential) error) (mixed bool, err error) {
	ls := &labeledScan{source: source, sink: newCredSink(tempDir, source)}
	var ambiguity *AmbiguityCounts
	if diagnostics {
		ambiguity = &AmbiguityCounts{}
	}
	cs := &colonScan{source: source, sink: newCredSink(tempDir, source), loose: loose, diagnostics: diagnostics, ambiguity: ambiguity}
	sc := bufio.NewScanner(textenc.WrapReader(r))
	sc.Buffer(make([]byte, 64*1024), maxScanLineLen)
	for sc.Scan() {
		line := sc.Text()
		if err := ls.line(line); err != nil {
			ls.sink.discard()
			cs.sink.discard()
			return false, err
		}
		if err := cs.line(line); err != nil {
			ls.sink.discard()
			cs.sink.discard()
			return false, err
		}
	}
	if err := sc.Err(); err != nil {
		ls.sink.discard()
		cs.sink.discard()
		return false, err
	}
	if err := ls.flush(); err != nil {
		ls.sink.discard()
		cs.sink.discard()
		return false, err
	}
	// Review H-20: labeled precedence is kept and the output is unchanged,
	// but when both scans found records the losing scan's credentials are
	// valid lines that get discarded. Report the source as mixed so the
	// caller flags HadIssue (-del keeps it) and withholds history
	// completion.
	mixed = ls.sink.found() && cs.sink.found()
	winner, loser := ls.sink, cs.sink
	selectedColon := !ls.sink.found()
	if selectedColon {
		winner, loser = cs.sink, ls.sink
	}
	if selectedColon && onAmbiguity != nil && cs.ambiguity != nil && cs.ambiguity.Total != 0 {
		onAmbiguity(*cs.ambiguity)
	}
	defer loser.discard()
	return mixed, winner.replay(emit)
}

// ParseCredentials keeps the collect-everything form for callers that want a
// slice (tests, and seams that still speak the slice interface). Production
// members stream through ParseCredentialsStream instead.
func ParseCredentials(r io.Reader, source string) ([]Credential, error) {
	creds, _, err := ParseCredentialsChecked(r, source)
	return creds, err
}

// ParseCredentialsChecked is ParseCredentials plus the mixed-format signal:
// mixed=true when the source contained BOTH labeled Key: value blocks and
// valid label-less url:user:password lines. The output is unchanged (labeled
// precedence), but a mixed source has discarded valid credentials, so the
// caller must flag HadIssue and withhold history completion (review H-20).
func ParseCredentialsChecked(r io.Reader, source string) (creds []Credential, mixed bool, err error) {
	mixed, err = ParseCredentialsStream(r, source, "", func(c Credential) error {
		creds = append(creds, c)
		return nil
	})
	return creds, mixed, err
}

// errParseBufferExceeded is retained for the sevenzip validation-member path
// (readSevenZipMembers caps its pre-password probe), which still needs a
// bounded consume; the credential parser itself no longer fails on member
// size — it streams.
var errParseBufferExceeded = errors.New("sflog: credential member exceeds parse buffer limit")

// maxParseBuffer is the byte budget for the sevenzip validation probe
// (archive.go): a small member is force-decoded up front so a wrong password
// fails on its CRC check instead of decoding the whole archive.
const maxParseBuffer = 16 * 1024 * 1024

// maxScanLineLen is the per-line cap the streaming parse enforces via
// sc.Buffer, matching the pre-streaming scanner's per-token ceiling so a
// single pathological line still errors (bufio.ErrTooLong) and the failure is
// recorded per member — the member's siblings and the rest of the archive are
// unaffected.
const maxScanLineLen = 4 * 1024 * 1024

// labeledScan is the streaming Key: value block scanner (URL/USER/PASS and
// alias blocks). It streams r one line at a time and returns every complete
// url+username+password block in encounter order.
type labeledScan struct {
	source string
	block  credBlock
	sink   *credSink
}

// line consumes one raw line; trailing \r (CRLF files) is stripped exactly as
// the pre-streaming pass did before the trimmed/splitField handling.
func (ls *labeledScan) line(raw string) error {
	raw = strings.TrimRight(raw, "\r\n")
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || isSeparator(trimmed) {
		return ls.flush()
	}
	key, val, ok := splitField(raw)
	if !ok || val == "" {
		return nil
	}
	switch classifyField(key) {
	case "url":
		// A second url-class field re-appearing signals a new record only
		// when the current one already has a user+password (url-first
		// layout). On an incomplete block it is a duplicate alias within
		// the same record (e.g. URL + Host), so keep the first.
		if ls.block.url != "" {
			if ls.block.username != "" && ls.block.password != "" {
				ls.flush()
				ls.block.url = val
			}
		} else {
			ls.block.url = val
		}
	case "username":
		if ls.block.username != "" {
			if err := ls.flush(); err != nil {
				return err
			}
		}
		ls.block.username = val
	case "password":
		if ls.block.password != "" {
			if err := ls.flush(); err != nil {
				return err
			}
		}
		ls.block.password = val
	}
	return nil
}

func (ls *labeledScan) flush() error {
	defer func() { ls.block = credBlock{} }()
	if ls.block.url != "" && ls.block.username != "" && ls.block.password != "" {
		return ls.sink.add(Credential{
			URL: ls.block.url, Username: ls.block.username, Password: ls.block.password, Source: ls.source,
		})
	}
	return nil
}

// colonScan is the label-less fallback for Raccoon pws.txt / label-less
// StealC passwords.txt: one url:login:password per line, no field labels. It
// reuses ulpengine.ParseLine (strict) so it agrees with the library parser on
// what is a credential. Lines that don't parse are skipped, matching the
// labeled path's "no ULP" behavior.
type colonScan struct {
	source      string
	sink        *credSink
	loose       bool
	diagnostics bool
	ambiguity   *AmbiguityCounts
}

// line consumes one raw line (with any trailing \r still attached; ParseLine
// trims it itself).
func (cs *colonScan) line(line string) error {
	if line == "" || isSeparator(strings.TrimSpace(line)) {
		return nil
	}
	// Use the same selected built-in grammar as sfu. This is not a second sfl
	// grammar: ParseLine owns every accept/reject decision for label-less data.
	var ambiguity ulpengine.AmbiguityKind
	var url, login, password string
	var ok bool
	if cs.diagnostics {
		_, url, login, password, ok, ambiguity = ulpengine.ParseLineWithDiagnostics(line, cs.loose)
	} else {
		_, url, login, password, ok = ulpengine.ParseLine(line, cs.loose)
	}
	if !ok {
		return nil
	}
	if err := cs.sink.add(Credential{URL: url, Username: login, Password: password, Source: cs.source}); err != nil {
		return err
	}
	switch ambiguity {
	case ulpengine.AmbiguityPathOrPassword:
		cs.ambiguity.Total++
		cs.ambiguity.PathOrPassword++
	case ulpengine.AmbiguityPortOrLogin:
		cs.ambiguity.Total++
		cs.ambiguity.PortOrLogin++
	}
	return nil
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
//
//	pin, pincode, passcode — PINs are not web passwords (githubesson only).
//	phone, phonenumber, mobile — phone-as-login is debatable (githubesson only).
//	soft (as URL) — "soft" is the browser label, not the URL (TreRB only).
var (
	urlAlias = map[string]bool{
		"url":      true, // lexfo, TreRB, githubesson
		"ur1":      true, // lexfo leet-speak variant
		"host":     true, // lexfo, TreRB, githubesson
		"hostname": true, // lexfo, TreRB, githubesson
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
		"u53rn4m3":   true, // lexfo leet-speak variant
		"email":      true, // TreRB, githubesson, TSP68 (attested only)
		"mail":       true, // githubesson (attested only)
		"account":    true, // githubesson (attested only)
		"acc":        true, // githubesson (attested only)
		"usr":        true, // TreRB (attested only)
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
	urlPart := credentialURL(c, noURI)
	var b strings.Builder
	b.Grow(len(urlPart) + len(c.Username) + len(c.Password) + 2)
	b.WriteString(urlPart)
	b.WriteByte(':')
	b.WriteString(c.Username)
	b.WriteByte(':')
	b.WriteString(c.Password)
	return b.String()
}

func credentialURL(c Credential, noURI bool) string {
	urlPart := normalizeURL(strings.TrimSpace(c.URL))
	if noURI {
		return hostFromURL(urlPart)
	}
	return urlPart
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
