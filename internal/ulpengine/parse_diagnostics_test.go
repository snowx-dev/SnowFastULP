package ulpengine

import (
	"strings"
	"testing"
)

func TestParseWithAmbiguityPathWitnessIsAdmissible(t *testing.T) {
	line := "https://example.com/login:alice:pw:tail"
	host, url, login, password, ok := parseFor(line, false)
	if !ok {
		t.Fatal("base parser rejected witness fixture")
	}
	gotHost, gotURL, gotLogin, gotPassword, gotOK, kind := parseWithAmbiguity(line, false)
	if !gotOK || gotHost != host || gotURL != url || gotLogin != login || gotPassword != password {
		t.Fatalf("diagnostic parser changed selected result: got %q %q %q %q %v, want %q %q %q %q true", gotHost, gotURL, gotLogin, gotPassword, gotOK, host, url, login, password)
	}
	if kind != AmbiguityPathOrPassword {
		t.Fatalf("ambiguity = %v, want path_or_password", kind)
	}
	if _, _, _, _, valid := ValidateFields(url, login, password, false); !valid {
		t.Fatal("selected tuple failed independent strict field validation")
	}
	altURL, altLogin, altPassword := "https://example.com/login", "alice", "pw:tail"
	if _, _, _, _, valid := ValidateFields(altURL, altLogin, altPassword, false); !valid {
		t.Fatal("witness tuple failed independent strict field validation")
	}
}

func TestParseWithAmbiguityPortWitnessIsAdmissible(t *testing.T) {
	line := "example.com:12345:alice:pw:tail"
	host, url, login, password, ok := parseFor(line, false)
	if !ok {
		t.Fatal("base parser rejected port witness fixture")
	}
	gotHost, gotURL, gotLogin, gotPassword, gotOK, kind := parseWithAmbiguity(line, false)
	if !gotOK || gotHost != host || gotURL != url || gotLogin != login || gotPassword != password {
		t.Fatalf("diagnostic parser changed selected result: got %q %q %q %q %v, want %q %q %q %q true", gotHost, gotURL, gotLogin, gotPassword, gotOK, host, url, login, password)
	}
	if kind != AmbiguityPortOrLogin {
		t.Fatalf("ambiguity = %v, want port_or_login", kind)
	}
	if _, _, _, _, valid := ValidateFields(url, login, password, false); !valid {
		t.Fatal("selected tuple failed independent strict field validation")
	}
	altURL, altLogin, altPassword := "example.com", "12345", "alice:pw:tail"
	if _, _, _, _, valid := ValidateFields(altURL, altLogin, altPassword, false); !valid {
		t.Fatal("witness tuple failed independent strict field validation")
	}
}

func TestParseWithAmbiguityUniqueRejectedAndBudget(t *testing.T) {
	for _, line := range []string{
		"https://example.com/path:alice:pw",
		"example.org:normal:sentinel_private_password",
	} {
		_, _, _, _, ok, _ := parseWithAmbiguity(line, false)
		if !ok {
			t.Fatalf("unique control rejected: %q", line)
		}
		_, _, _, _, _, kind := parseWithAmbiguity(line, false)
		if kind != AmbiguityNone {
			t.Errorf("unique control %q has ambiguity status %v", line, kind)
		}
	}

	line := "https://example.com/path" + strings.Repeat(":", ambiguityCandidateBudget+2) + "alice:pw"
	_, _, _, _, ok, kind := parseWithAmbiguity(line, false)
	if !ok {
		t.Fatal("budget control rejected")
	}
	if kind != AmbiguityUnknown {
		t.Fatalf("budget status = %v, want unknown", kind)
	}
}

func TestParseWithAmbiguityPreservesRejectedFallback(t *testing.T) {
	// The strict scanner's first apparent split fails common checks, after which
	// the existing fallback chain still gets the final say.
	line := "https://example.com/path::alice:pw"
	host, url, login, password, ok := parseFor(line, false)
	gotHost, gotURL, gotLogin, gotPassword, gotOK, _ := parseWithAmbiguity(line, false)
	if host != gotHost || url != gotURL || login != gotLogin || password != gotPassword || ok != gotOK {
		t.Fatalf("diagnostic changed fallback result: base=(%q,%q,%q,%q,%v), diagnostic=(%q,%q,%q,%q,%v)", host, url, login, password, ok, gotHost, gotURL, gotLogin, gotPassword, gotOK)
	}
}

// TestRepairWhitespaceInvariantPathOrPassword pins review finding 4: a
// whitespace byte inside a strict field must not suppress the diagnostic
// witness. Every variant below selects the same grammar as the clean line and
// must report the same path_or_password witness.
func TestRepairWhitespaceInvariantPathOrPassword(t *testing.T) {
	base := "https://example.com/login:alice:pw:tail"
	baseHost, baseURL, baseLogin, basePassword, baseOK := parseFor(base, false)
	if !baseOK {
		t.Fatal("base parser rejected witness fixture")
	}
	for _, line := range []string{
		base,
		"https://example.com/login:alice :pw:tail",
		"https://example.com/login:alice\t:pw:tail",
		"https://example.com/login: alice:pw:tail",
		"https://example.com/login :alice:pw:tail",
	} {
		host, url, login, password, ok := parseFor(line, false)
		if !ok {
			t.Fatalf("whitespace variant rejected: %q", line)
		}
		gotHost, gotURL, gotLogin, gotPassword, gotOK, kind := parseWithAmbiguity(line, false)
		if !gotOK || gotHost != host || gotURL != url || gotLogin != login || gotPassword != password {
			t.Fatalf("diagnostic parser changed selected result for %q: got %q %q %q %q %v, want %q %q %q %q true", line, gotHost, gotURL, gotLogin, gotPassword, gotOK, host, url, login, password)
		}
		if kind != AmbiguityPathOrPassword {
			t.Errorf("variant %q: ambiguity = %v, want path_or_password", line, kind)
		}
	}
	// Whitespace confined to the login segment is trimmed by the selected
	// parser, so those variants share the base selected tuple exactly.
	for _, line := range []string{
		"https://example.com/login:alice :pw:tail",
		"https://example.com/login:alice\t:pw:tail",
	} {
		host, url, login, password, ok := parseFor(line, false)
		if !ok || host != baseHost || url != baseURL || login != baseLogin || password != basePassword {
			t.Fatalf("variant %q selected tuple = (%q,%q,%q,%q,%v), want base (%q,%q,%q,%q,true)", line, host, url, login, password, ok, baseHost, baseURL, baseLogin, basePassword)
		}
	}
}

// TestRepairDistinctWitnessPinnedAsLiteralFields pins the distinct witness
// identity (example.com, https://example.com/login, alice, pw:tail) as literal
// labeled fields: it must pass the same strict admission as the selected
// tuple, independent of any candidate-loop output agreement.
func TestRepairDistinctWitnessPinnedAsLiteralFields(t *testing.T) {
	witnessURL, witnessLogin, witnessPassword := "https://example.com/login", "alice", "pw:tail"
	host, urlOut, loginOut, passwordOut, ok := ValidateFields(witnessURL, witnessLogin, witnessPassword, false)
	if !ok {
		t.Fatal("distinct witness tuple rejected as literal fields")
	}
	if host != "example.com" || urlOut != witnessURL || loginOut != witnessLogin || passwordOut != witnessPassword {
		t.Fatalf("literal witness fields = (%q,%q,%q,%q), want (%q,%q,%q,%q)", host, urlOut, loginOut, passwordOut, "example.com", witnessURL, witnessLogin, witnessPassword)
	}
	// The pinned identity is genuinely distinct from the selected tuple.
	base := "https://example.com/login:alice:pw:tail"
	_, baseURL, baseLogin, basePassword, baseOK := parseFor(base, false)
	if !baseOK {
		t.Fatal("base parser rejected witness fixture")
	}
	if baseURL == witnessURL && baseLogin == witnessLogin && basePassword == witnessPassword {
		t.Fatal("witness identity equals the selected tuple; fixture is not ambiguous")
	}
}

// TestRepairPortWitnessWithWhitespace keeps the port/login witness alive when
// whitespace rides inside the login segment.
func TestRepairPortWitnessWithWhitespace(t *testing.T) {
	for _, line := range []string{
		"example.com:12345:alice:pw:tail",
		"example.com:12345: alice:pw:tail",
		"example.com:12345:alice :pw:tail",
		"https://example.com:12345:alice:pw:tail",
		"https://example.com:12345: alice:pw:tail",
	} {
		host, url, login, password, ok := parseFor(line, false)
		if !ok {
			t.Fatalf("port witness variant rejected: %q", line)
		}
		gotHost, gotURL, gotLogin, gotPassword, gotOK, kind := parseWithAmbiguity(line, false)
		if !gotOK || gotHost != host || gotURL != url || gotLogin != login || gotPassword != password {
			t.Fatalf("diagnostic parser changed selected result for %q: got %q %q %q %q %v, want %q %q %q %q true", line, gotHost, gotURL, gotLogin, gotPassword, gotOK, host, url, login, password)
		}
		if kind != AmbiguityPortOrLogin {
			t.Errorf("variant %q: ambiguity = %v, want port_or_login", line, kind)
		}
	}
}

// TestRepairWhitespaceControlsInvalidAndRejected covers the unchanged edges:
// padded unique controls stay unique, invalid alternates stay silent even when
// normalized, rejected inputs stay rejected without an ambiguity claim, and
// colon-heavy whitespace lines exhaust the budget into AmbiguityUnknown.
func TestRepairWhitespaceControlsInvalidAndRejected(t *testing.T) {
	for _, line := range []string{
		"  https://example.com/path:alice:pw  ",
		"\thttps://example.com/path:alice:pw\n",
	} {
		_, _, _, _, ok, kind := parseWithAmbiguity(line, false)
		if !ok {
			t.Fatalf("padded unique control rejected: %q", line)
		}
		if kind != AmbiguityNone {
			t.Errorf("padded unique control %q has ambiguity status %v", line, kind)
		}
	}
	for _, line := range []string{
		"https://example.com/path: $bad:pw:tail",
		"https://example.com/path:$bad :pw:tail",
	} {
		_, _, _, _, ok, kind := parseWithAmbiguity(line, false)
		if !ok {
			t.Fatalf("invalid-alternate control rejected: %q", line)
		}
		if kind != AmbiguityNone {
			t.Errorf("invalid alternate %q produced witness %v", line, kind)
		}
	}
	tooLong := "https://example.com/login:alice:" + strings.Repeat("p", 70)
	for _, line := range []string{tooLong, " " + tooLong + " ", "\t" + tooLong} {
		_, _, _, _, ok, kind := parseWithAmbiguity(line, false)
		if ok {
			t.Fatalf("over-cap input unexpectedly accepted: %q", line)
		}
		if kind != AmbiguityNone {
			t.Errorf("rejected input %q reported ambiguity %v", line, kind)
		}
	}
	line := "https://example.com/path" + strings.Repeat(":a|b", ambiguityCandidateBudget+2) + ":alice:pw"
	_, _, _, _, ok, kind := parseWithAmbiguity(line, false)
	if !ok {
		t.Fatal("whitespace budget control rejected")
	}
	if kind != AmbiguityUnknown {
		t.Fatalf("budget status = %v, want unknown", kind)
	}
}
