package ulpengine

import "strings"

// AmbiguityKind identifies a bounded alternate interpretation from the
// debug-only raw parser. A witness records uncertainty, not a preferred tuple.
type AmbiguityKind uint8

const (
	AmbiguityNone AmbiguityKind = iota
	AmbiguityPathOrPassword
	AmbiguityPortOrLogin
	// AmbiguityUnknown means the diagnostic budget was exhausted; it is not
	// counted as an ambiguity and must not be treated as proven unique.
	AmbiguityUnknown
)

// ParseLineWithDiagnostics preserves ParseLine's tuple and admission decision
// while returning at most one admissible alternate-identity witness.
func ParseLineWithDiagnostics(line string, loose bool) (host, url, login, password string, ok bool, ambiguity AmbiguityKind) {
	return parseWithAmbiguity(line, loose)
}

type diagnosticParser struct {
	loose   bool
	metrics *Metrics
}

func (p diagnosticParser) Parse(line string) (host, url, login, password string, ok bool) {
	host, url, login, password, ok, kind := parseWithAmbiguity(line, p.loose)
	if ok && p.metrics != nil {
		switch kind {
		case AmbiguityPathOrPassword:
			p.metrics.AmbiguityTotal.Add(1)
			p.metrics.AmbiguityPathPassword.Add(1)
		case AmbiguityPortOrLogin:
			p.metrics.AmbiguityTotal.Add(1)
			p.metrics.AmbiguityPortLogin.Add(1)
		}
	}
	return host, url, login, password, ok
}

// parseWithAmbiguity keeps the selected parser result byte-for-byte intact and
// performs bounded witness analysis only for accepted strict-ULP records.
// Callers must avoid it unless diagnostics are enabled.
func parseWithAmbiguity(line string, loose bool) (host, url, login, password string, ok bool, ambiguity AmbiguityKind) {
	host, url, login, password, ok = parseFor(line, loose)
	if !ok {
		return host, url, login, password, false, AmbiguityNone
	}

	line = strings.TrimSpace(strings.TrimRight(line, "\r\n"))
	if len(line) < 5 || len(line) > maxParsedLineLen || strings.HasPrefix(line, androidScheme) {
		return host, url, login, password, true, AmbiguityNone
	}

	// Confirm strict ULP was the selected grammar. Other parser branches,
	// including a strict scanner result rejected by finishParse, are untouched.
	// finishParse sees the identical TrimSpace normalization parseCore applies
	// to strict scanner fields (parse.go), so a whitespace byte riding in a
	// field cannot fail the selected-tuple comparison before witness analysis.
	rawURL, rawLogin, rawPassword, strictOK := parseStrictULP(line)
	if !strictOK {
		return host, url, login, password, true, AmbiguityNone
	}
	parsedHost, parsedURL, parsedLogin, parsedPassword, admitted := finishParse(
		strings.TrimSpace(rawURL), strings.TrimSpace(rawLogin), strings.TrimSpace(rawPassword))
	if !admitted || parsedHost != host || parsedURL != url || parsedLogin != login || parsedPassword != password {
		return host, url, login, password, true, AmbiguityNone
	}
	return host, url, login, password, true, strictULPAmbiguity(line, host, login, password)
}

const ambiguityCandidateBudget = 64

func strictULPAmbiguity(line string, selectedHost, selectedLogin, selectedPassword string) AmbiguityKind {
	schemeEnd := schemePrefixLen(line)
	hostEnd, portEnd, pathStart := scanURLHead(line, schemeEnd)
	if hostEnd < 0 {
		return AmbiguityNone
	}

	// For scheme URLs, candidate URL boundaries are colons inside the path.
	// Visit adjacent delimiter pairs once and stop at a fixed budget; this
	// bounds adversarial colon-heavy inputs without claiming uniqueness after
	// an incomplete scan.
	incomplete := false
	if schemeEnd > 0 && pathStart >= 0 {
		checked := 0
		previous := -1
		for i := pathStart; i < len(line); i++ {
			if line[i] != ':' {
				continue
			}
			if previous >= 0 {
				checked++
				if checked > ambiguityCandidateBudget {
					incomplete = true
					break
				}
				if kind := admissibleAlternate(line, previous, i, selectedHost, selectedLogin, selectedPassword); kind != AmbiguityNone {
					return kind
				}
			}
			previous = i
		}
	}

	// A strict numeric port wins before the same scanner's host-only boundary.
	// Check that host-only ULP candidate independently under strict admission.
	if portEnd >= 0 && hostEnd+1 < len(line) && line[hostEnd] == ':' {
		next := strings.IndexByte(line[hostEnd+1:], ':')
		if next >= 0 {
			next += hostEnd + 1
			candidateLogin, candidatePassword, valid := splitLoginPassword(line, hostEnd+1, next)
			if valid {
				candidateHost, _, normalizedLogin, normalizedPassword, admitted := finishParse(strings.TrimSpace(line[:hostEnd]), candidateLogin, candidatePassword)
				if admitted && distinctCredential(candidateHost, normalizedLogin, normalizedPassword, selectedHost, selectedLogin, selectedPassword) {
					return AmbiguityPortOrLogin
				}
			}
		}
	}
	if incomplete {
		return AmbiguityUnknown
	}
	return AmbiguityNone
}

func admissibleAlternate(line string, urlEnd, loginEnd int, selectedHost, selectedLogin, selectedPassword string) AmbiguityKind {
	login, password, valid := splitLoginPassword(line, urlEnd+1, loginEnd)
	if !valid {
		return AmbiguityNone
	}
	host, _, login, password, admitted := finishParse(strings.TrimSpace(line[:urlEnd]), login, password)
	if admitted && distinctCredential(host, login, password, selectedHost, selectedLogin, selectedPassword) {
		return AmbiguityPathOrPassword
	}
	return AmbiguityNone
}

func distinctCredential(host, login, password, selectedHost, selectedLogin, selectedPassword string) bool {
	return host != selectedHost || login != selectedLogin || password != selectedPassword
}
