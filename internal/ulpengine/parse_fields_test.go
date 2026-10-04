package ulpengine

import (
	"strings"
	"testing"
)

func TestT01LabeledFieldsPreserveColonBoundaries(t *testing.T) {
	stableFormatter := NewStableFormatter()
	for _, loose := range []bool{false, true} {
		for _, noURI := range []bool{false, true} {
			for _, tc := range []struct {
				name, url, noURIURL, login, password, host, wantLine string
			}{
				{"colon_path", "https://example.com/oauth:callback", "example.com", "alice", "pw", "example.com", "example.com:alice:pw"},
				{"numeric_login", "example.com", "example.com", "12345", "a:b:c", "example.com", "example.com/:12345:a:b:c"},
				{"numeric_login_port", "https://example.com:8080", "example.com:8080", "12345", "a:b:c", "example.com:8080", "example.com:8080/:12345:a:b:c"},
			} {
				t.Run(tc.name+map[bool]string{false: "/strict", true: "/loose"}[loose]+map[bool]string{false: "/uri", true: "/no-uri"}[noURI], func(t *testing.T) {
					validateURL := tc.url
					if noURI {
						validateURL = tc.noURIURL
					}
					host, url, login, password, ok := ValidateFields(validateURL, tc.login, tc.password, loose)
					if !ok {
						t.Fatal("ValidateFields rejected valid supplied fields")
					}
					if host != tc.host || login != tc.login || password != tc.password {
						t.Fatalf("ValidateFields = (%q, %q, %q), want host/login/password (%q, %q, %q)", host, login, password, tc.host, tc.login, tc.password)
					}
					line, representable := stableFormatter.FormatRecordStable(host, url, login, password, noURI)
					if !representable {
						t.Fatal("stable formatting marked valid fields unrepresentable")
					}
					if line != tc.wantLine {
						t.Fatalf("stable line = %q, want %q", line, tc.wantLine)
					}
					h, _, l, p, parsed := parseStored(line)
					if !parsed || h != host || l != login || p != password {
						t.Fatalf("stored parse = (%q, %q, %q, %v), want (%q, %q, %q, true)", h, l, p, parsed, host, login, password)
					}
				})
			}
		}
	}
}

func TestT01StrictLabeledFieldsRetainComponentConstraints(t *testing.T) {
	for _, tc := range []struct {
		name, url, login, password string
	}{
		{"empty_login", "https://example.com", "", "pw"},
		{"colon_login", "https://example.com", "alice:admin", "pw"},
		{"strict_login_class", "https://example.com", "$alice", "pw"},
		{"password_65_bytes", "https://example.com", "alice", strings.Repeat("p", 65)},
		{"empty_password", "https://example.com", "alice", ""},
		{"braced_host", "https://{example}.com", "alice", "pw"},
		{"url_login", "https://example.com", "https://evil.example", "pw"},
		{"braced_password", "https://example.com", "alice", "{pw}"},
		{"url_password", "https://example.com", "alice", "https://evil.example"},
		{"invalid_url", "https://example.c", "alice", "pw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, ok := ValidateFields(tc.url, tc.login, tc.password, false); ok {
				t.Fatal("ValidateFields admitted invalid strict field component")
			}
		})
	}
}

func TestT01LooseLabeledFieldsKeepLooseLoginClass(t *testing.T) {
	host, _, login, password, ok := ValidateFields("https://example.com", "$alice", "pw", true)
	if !ok || host != "example.com" || login != "$alice" || password != "pw" {
		t.Fatalf("loose supplied fields = (%q, %q, %q, %v), want (example.com, $alice, pw, true)", host, login, password, ok)
	}
	for _, tc := range []struct{ login, password string }{
		{"", "pw"},
		{"alice:admin", "pw"},
		{"alice", strings.Repeat("p", 65)},
		{"alice", "{pw}"},
		{"alice", "https://evil.example"},
	} {
		if _, _, _, _, ok := ValidateFields("https://example.com", tc.login, tc.password, true); ok {
			t.Errorf("loose supplied fields admitted invalid components login=%q password=%q", tc.login, tc.password)
		}
	}
}

func TestT01LooseOnlyFieldsValidateComponentsIndependently(t *testing.T) {
	for _, tc := range []struct {
		url, login, password string
	}{
		{"example.c:8080/path", "$alice", "pw"},
		{"https://example.com/oauth:callback", "$alice", "pw"},
		{"https://example.com", "$alice", "a:b"},
	} {
		if _, _, login, password, ok := ValidateFields(tc.url, tc.login, tc.password, true); !ok || login != tc.login || password != tc.password {
			t.Errorf("loose supplied fields (%q, %q, %q) = (%q, %q, %v)", tc.url, tc.login, tc.password, login, password, ok)
		}
	}
	for _, url := range []string{"not-a-host", "example.com:port/path"} {
		if _, _, _, _, ok := ValidateFields(url, "$alice", "pw", true); ok {
			t.Errorf("loose supplied fields admitted invalid URL component %q", url)
		}
	}
}

func TestT01AndroidFieldsRetainRawAndNoURIContracts(t *testing.T) {
	longPassword := strings.Repeat("p", 65)
	for _, loose := range []bool{false, true} {
		for _, password := range []string{longPassword, "{pw}", "https://evil.example"} {
			host, url, login, gotPassword, ok := ValidateFields("android://cert==@com.example.app/", "alice", password, loose)
			if !ok || host != url || host != "android://cert==@com.example.app/" || login != "alice" || gotPassword != password {
				t.Errorf("Android supplied fields (loose=%v, password=%q) = (%q, %q, %q, %q, %v)", loose, password, host, url, login, gotPassword, ok)
			}
		}
		if _, _, _, _, ok := ValidateFields("com.example.app", "alice", longPassword, loose); ok {
			t.Errorf("NoURI package projection admitted 65-byte web password (loose=%v)", loose)
		}
	}
	for _, url := range []string{"android://", "android://:cert"} {
		if _, _, _, _, ok := ParseLine(url+":alice:pw", false); ok {
			t.Errorf("raw Android control %q was admitted", url)
		}
		if _, _, _, _, ok := ValidateFields(url, "alice", "pw", false); ok {
			t.Errorf("malformed Android URL component %q was admitted", url)
		}
	}
}
func TestT01LooseLabeledFieldsRetainJunkFilter(t *testing.T) {
	if _, _, _, _, ok := ValidateFields("https://example.com", "$alice", `{"`, true); ok {
		t.Fatal("loose-only junk field bypassed the existing junk filter")
	}
}

func TestT01StableFormatReturnsOwnedString(t *testing.T) {
	formatter := NewStableFormatter()
	line, ok := formatter.FormatRecordStable("example.com", "https://example.com", "alice", "first", false)
	if !ok || line != "example.com:alice:first" {
		t.Fatalf("first stable format = (%q, %v)", line, ok)
	}
	second, ok := formatter.FormatRecordStable("other.example", "https://other.example", "bob", "second", false)
	if !ok || second != "other.example:bob:second" {
		t.Fatalf("second stable format = (%q, %v)", second, ok)
	}
	if line != "example.com:alice:first" {
		t.Fatalf("first stable string changed after another formatter call: %q", line)
	}
}

func TestT01StableFormatReportsUnrepresentableFields(t *testing.T) {
	if line, ok := NewStableFormatter().FormatRecordStable("example", "example", "alice", "pw", false); ok || line != "" {
		t.Fatalf("unrepresentable fields = (%q, %v), want (empty, false)", line, ok)
	}
}

func TestT01StrictIPv4PortFallbackFields(t *testing.T) {
	const raw = "127.0.0.1:8080:alice@example.com:pw"
	if _, _, _, _, ok := ParseLine(raw, false); !ok {
		t.Fatal("strict raw IPv4 port fallback control was rejected")
	}
	for _, url := range []string{"127.0.0.1:8080", "127.0.0.1:8080/auth"} {
		host, _, login, password, ok := ValidateFields(url, "alice@example.com", "pw", false)
		if !ok || host != strings.TrimSuffix(url, "/auth") || login != "alice@example.com" || password != "pw" {
			t.Errorf("strict IPv4 port fields (%q) = (%q, %q, %q, %v)", url, host, login, password, ok)
		}
	}
	if _, _, _, _, ok := ValidateFields("127.0.0.1", "alice@example.com", "pw", false); ok {
		t.Fatal("strict supplied fields admitted bare IPv4 without the raw fallback port shape")
	}
}

func TestT01FieldValidationRetainsParsedLineByteLimit(t *testing.T) {
	baseURL := "https://example.com/"
	payloadBytes := maxParsedLineLen - len(stripScheme(baseURL)) - len("alice") - len("pw") - 2
	atLimit := baseURL + strings.Repeat("x", payloadBytes)
	if _, _, _, _, ok := ValidateFields(atLimit, "alice", "pw", false); !ok {
		t.Fatal("field line at maxParsedLineLen was rejected")
	}
	if _, _, _, _, ok := ValidateFields(atLimit+"x", "alice", "pw", false); ok {
		t.Fatal("field line above maxParsedLineLen was admitted")
	}
}

// Repairs review finding 2: restore the strict P3 admission of parseColonFallback
// for already-separated fields — bare fallback host + strict-valid login +
// colon-bearing password (the four-or-more-field prerequisite) — so raw-decoded
// credentials stop dropping records the baseline stores.
func TestRepairStrictP3BareIPColonPasswordFields(t *testing.T) {
	for _, tc := range []struct {
		name, url, login, password, host string
	}{
		{"bare_ip_colon_password", "103.181.181.122", "alice", "a:b", "103.181.181.122"},
		{"doc_range_ip_colon_password", "192.0.2.1", "alice", "a:b", "192.0.2.1"},
		{"colon_heavy_password", "103.1.2.3", "alice", "pw:extra", "103.1.2.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Baseline truth: the strict raw parser accepts the joined line.
			rawHost, _, rawLogin, rawPassword, ok := ParseLine(tc.url+":"+tc.login+":"+tc.password, false)
			if !ok || rawHost != tc.host || rawLogin != tc.login || rawPassword != tc.password {
				t.Fatalf("raw strict parse = (%q, %q, %q, %v), want (%q, %q, %q, true)", rawHost, rawLogin, rawPassword, ok, tc.host, tc.login, tc.password)
			}
			host, url, login, password, ok := ValidateFields(tc.url, tc.login, tc.password, false)
			if !ok || host != tc.host || url != tc.url || login != tc.login || password != tc.password {
				t.Fatalf("ValidateFields = (%q, %q, %q, %q, %v), want (%q, %q, %q, %q, true)", host, url, login, password, ok, tc.host, tc.url, tc.login, tc.password)
			}
		})
	}
}

// Strict three-field bare-IP records stay rejected (deliberate loose-only
// extension); loose keeps its existing acceptance.
func TestRepairStrictBareIPSimplePasswordStillRejected(t *testing.T) {
	if _, _, _, _, ok := ValidateFields("103.1.2.3", "alice", "pw", false); ok {
		t.Fatal("strict ValidateFields admitted a three-field bare-IP record")
	}
	if _, _, _, _, ok := ParseLine("103.1.2.3:alice:pw", false); ok {
		t.Fatal("strict raw parse admitted a three-field bare-IP record")
	}
	if _, _, _, _, ok := ParseLine("103.1.2.3:alice:pw", true); !ok {
		t.Fatal("loose raw parse lost its existing bare-IP acceptance")
	}
}

// The fallback URL-component rule validates the COMPLETE port/path suffix: a
// bare-IP port/query or port/fragment tail is rejected, while valid path forms
// (including /path?query and /path#fragment) stay admitted.
func TestRepairStrictFallbackPortSuffixRule(t *testing.T) {
	for _, url := range []string{"103.1.2.3:8080?x", "103.1.2.3:8080#frag"} {
		if _, _, _, _, ok := ValidateFields(url, "alice", "pw", false); ok {
			t.Errorf("strict ValidateFields admitted bare-IP suffix fragment %q", url)
		}
	}
	for _, tc := range []struct{ url, host string }{
		{"103.1.2.3:8080", "103.1.2.3:8080"},
		{"103.1.2.3:8080/path", "103.1.2.3:8080"},
		{"103.1.2.3:8080/path?query", "103.1.2.3:8080"},
		{"103.1.2.3:8080/path#fragment", "103.1.2.3:8080"},
	} {
		host, _, login, password, ok := ValidateFields(tc.url, "alice", "pw", false)
		if !ok || host != tc.host || login != "alice" || password != "pw" {
			t.Errorf("strict ValidateFields (%q) = (%q, %q, %q, %v), want (%q, alice, pw, true)", tc.url, host, login, password, ok, tc.host)
		}
	}
}

// Negative controls for the restored P3 shape: login classes, fallback-host
// shapes, the 64-byte password cap, and the parsed-line byte limit must keep
// binding when the password carries colons.
func TestRepairStrictP3NegativeControls(t *testing.T) {
	for _, tc := range []struct {
		name, url, login, password string
	}{
		{"empty_login", "103.181.181.122", "", "a:b"},
		{"colon_login", "103.181.181.122", "alice:admin", "a:b"},
		{"invalid_login_class", "103.181.181.122", "al|ice", "a:b"},
		{"host_three_labels", "10.0.0", "alice", "a:b"},
		{"host_five_labels", "1.2.3.4.5", "alice", "a:b"},
		{"host_path_suffix", "103.1.2.3/path", "alice", "a:b"},
		{"password_cap_66", "103.181.181.122", "alice", strings.Repeat("p", 32) + ":" + strings.Repeat("p", 33)},
	} {
		if _, _, _, _, ok := ValidateFields(tc.url, tc.login, tc.password, false); ok {
			t.Errorf("negative control %s was admitted", tc.name)
		}
	}
	baseURL := "https://example.com/"
	payloadBytes := maxParsedLineLen - len(stripScheme(baseURL)) - len("alice") - len("a:b") - 2
	atLimit := baseURL + strings.Repeat("x", payloadBytes)
	if _, _, _, _, ok := ValidateFields(atLimit, "alice", "a:b", false); !ok {
		t.Fatal("field line at maxParsedLineLen was rejected")
	}
	if _, _, _, _, ok := ValidateFields(atLimit+"x", "alice", "a:b", false); ok {
		t.Fatal("field line above maxParsedLineLen was admitted")
	}
}

// Repair, review finding 3: isLikelyJunkFields must equal isLikelyJunk over
// the logical URL:login:password view, including the two inserted colons.
// A junk pattern that begins with a delimiter (e.g. ":target=" formed by the
// password "target=x") lies outside any single field and must still reject
// the loose-only admission — but only after strict admission failed; the
// strict lane is untouched.
func TestRepairLooseJunkBoundaryParity(t *testing.T) {
	// Defect fixture: loose labeled example.com / $alice / target=x.
	if _, _, _, _, ok := ValidateFields("example.com", "$alice", "target=x", true); ok {
		t.Fatal("loose ValidateFields admitted password target=x; serialized record contains :target= and must be junk-rejected")
	}
	// Strict-admitted control: same text, strict component admission succeeds
	// before any loose filter, so it must retain acceptance.
	if _, _, _, _, ok := ValidateFields("example.com", "alice", "target=x", false); !ok {
		t.Fatal("strict ValidateFields rejected alice/target=x; strict success must precede the loose junk filter")
	}
	// Ordinary loose Unicode/$ logins with opaque non-junk passwords.
	for _, login := range []string{"$alice", "ünïcødé", "user|pipe"} {
		if _, _, _, _, ok := ValidateFields("example.com", login, "pässwörd", true); !ok {
			t.Fatalf("loose ValidateFields rejected login %q with opaque password", login)
		}
	}
}

// TestRepairLooseJunkFieldsMatchesLogicalSerialization pins exact equivalence:
// isLikelyJunkFields(url, login, password) == isLikelyJunk(url+":"+login+":"+password).
func TestRepairLooseJunkFieldsMatchesLogicalSerialization(t *testing.T) {
	type triple struct{ url, login, password string }
	cases := []triple{
		{"example.com", "$alice", "target=x"},
		{"", "", ""},
		{"example.com", "", ""},
		{"", "alice", ""},
		{"", "", "target=x"},
		{"LegacyGeneric:example.com", "alice", "pw"},
		{"example.com", "alice", `{"k":"v"}`},
		{"example.com", "alice", `":"`},
		{"example.com", "alice", "target="},
		{"example.com", "alice", "a:target=b:c"},
		{"example.com", "alice", "x\ty"},
		{"example.com", "alice", "x\t\t1:y"},
		{"example.com", "alice", "x\\t\\t1:y"},
		{"example.com", "alice", "line1\t\t1:end"},
		{"example.com", "alice", "pw:PasswordText "},
		{"example.com", "alice", "pw:PasswordResetRequestForm"},
		{"example.com", "alice", "user:Username x"},
		{"example.com", "alice", "pw:LoginID"},
		{"example.com", "alice", "PasswordText "},
		{"example.com", "alice", "LoginID"},
		{"a:b", "c:d", "e:f"},
		{":::", ":::", ":::"},
		{"example.com", "alice", "plainpw"},
	}
	for _, tc := range cases {
		logical := tc.url + ":" + tc.login + ":" + tc.password
		want := isLikelyJunk(logical)
		if got := isLikelyJunkFields(tc.url, tc.login, tc.password); got != want {
			t.Errorf("isLikelyJunkFields(%q,%q,%q)=%t; want isLikelyJunk(%q)=%t", tc.url, tc.login, tc.password, got, logical, want)
		}
	}
}

// StrictLaneAdmitted is exported for sfl's emission lane gate; pin its
// contract: the shared strict predicate, trimmed fields, and the android
// exception (android admissions always keep the full dual-fidelity check).
func TestStrictLaneAdmittedContract(t *testing.T) {
	cases := []struct {
		name, url, login, password string
		want                       bool
	}{
		{"dotted host", "example.com", "alice", "pw", true},
		{"scheme URL with path", "https://example.com/path", "alice", "pw", true},
		{"bare-IP colon password (strict P3)", "103.181.181.122", "alice", "a:b", true},
		{"bare-IP simple password (loose only)", "103.181.181.122", "alice", "pw", false},
		{"port/query tail rejected", "103.1.2.3:8080?x", "alice", "pw", false},
		{"empty login rejected", "example.com", "", "pw", false},
		{"android never strict-lane", "android://com.example.app", "alice", "pw", false},
		{"untrimmed equals trimmed", "  example.com  ", "  alice  ", " pw ", true},
	}
	for _, tc := range cases {
		if got := StrictLaneAdmitted(tc.url, tc.login, tc.password); got != tc.want {
			t.Errorf("%s: StrictLaneAdmitted(%q,%q,%q)=%t, want %t",
				tc.name, tc.url, tc.login, tc.password, got, tc.want)
		}
	}
	// Parity: outside the android exception the exported helper must agree
	// with the strict predicate ValidateFields applies (same inputs, both
	// trimmed) so the two call sites cannot drift.
	for _, tc := range cases {
		if strings.HasPrefix(tc.url, "android://") {
			continue
		}
		want := strictLanePredicate(strings.TrimSpace(tc.url), strings.TrimSpace(tc.login), strings.TrimSpace(tc.password))
		if got := StrictLaneAdmitted(tc.url, tc.login, tc.password); got != want {
			t.Errorf("%s: helper=%t, shared predicate=%t", tc.name, got, want)
		}
	}
}
