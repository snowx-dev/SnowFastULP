package ulpengine

import "testing"

// FormatRecordStable must pick a representation whose re-parse (parseStored,
// the regen/archive reader) reproduces the original dedup key, or drop the
// line. These cover the four outcomes: clean full form, verified full form,
// host-only rescue, and unrepresentable drop.
func TestFormatRecordStableChoosesRoundTrippableForm(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // expected written bytes; "" => dropped (ok=false)
	}{
		{"clean kept full", "https://a.example.com:user:pw1", "a.example.com:user:pw1"},
		{"port url kept after verify", "https://a.example.com:8080:user:pw1", "a.example.com:8080:user:pw1"},
		{"json tail kept full", `twitter.com:moraxd5:{"uid":"123","token"`, `twitter.com:moraxd5:{"uid":"123","token"`},
		{"colon url rescued by host-only", "user:pw1:https://clean.example.com/:weird:path", "clean.example.com:user:pw1"},
		// formerly dropped under parseUnion verify; parseStored recovers host form
		{"messy LPU kept via host form", `jurbzdm:astr.m@ou4eudeaeC:Estr@6438:https://om.fhttpiip-dual/:login.b@example.net:PassWord9:@Nv@g`, "om.fhttpiip-dual:jurbzdm:astr.m@ou4eudeaeC:Estr@6438"},
	}
	lf := newLineFormatter()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, url, login, password, ok := parse(tc.line)
			if !ok {
				t.Fatalf("setup: strict parse rejected %q", tc.line)
			}
			out, repr := lf.FormatRecordStable(host, url, login, password, false)
			if tc.want == "" {
				if repr {
					t.Fatalf("want dropped, got kept %q", out)
				}
				return
			}
			if !repr {
				t.Fatalf("want kept %q, got dropped", tc.want)
			}
			if string(out) != tc.want {
				t.Errorf("out = %q, want %q", out, tc.want)
			}
			wantKey := lf.HashKey(host, login, password)
			h2, _, l2, p2, ok2 := parseStored(string(out))
			if !ok2 || lf.HashKey(h2, l2, p2) != wantKey {
				t.Errorf("kept output %q does not round-trip to original key", out)
			}
		})
	}
}

// Property: for any parsed line, FormatRecordStable either drops it or yields
// bytes that parseStored re-parses to the same fields. This is the core
// integrity invariant that prevents regen stragglers.
func TestFormatRecordStableGuaranteesRoundTrip(t *testing.T) {
	corpus := []string{
		"https://a.example.com:user@mail.com:pw1",
		"user:pass:https://site.com",
		`twitter.com:moraxd5:{"uid":"123","token"`,
		`dash.cloudflare.com/sign-up:login.c@example.com:{"cc"`,
		"sub.host.co.uk:8443:bob:secret",
		"192.168.1.1:user:pass",
		"10.0.0.5:8080:admin:admin",
		"plain.example.com:joe:p@ss:word",
		"user:pw1:https://clean.example.com/:weird:path",
		`jurbzdm:astr.m@ou4eudeaeC:Estr@6438:https://om.fhttpiip-dual/:login.b@example.net:PassWord9:@Nv@g`,
	}
	lf := newLineFormatter()
	for _, line := range corpus {
		host, url, login, password, ok := parseUnion(line)
		if !ok {
			continue
		}
		out, repr := lf.FormatRecordStable(host, url, login, password, false)
		if !repr {
			continue // dropping is allowed; the guarantee only binds kept lines
		}
		h2, _, l2, p2, ok2 := parseStored(string(out))
		if !ok2 || h2 != host || l2 != login || p2 != password {
			t.Errorf("kept line %q -> %q fields mismatch (h=%q/%q l=%q/%q p=%q/%q)",
				line, out, h2, host, l2, login, p2, password)
		}
	}
}

func TestFormatRecordStableSpacedLoginRoundTrips(t *testing.T) {
	lf := newLineFormatter()
	host, url, login, password := "example.com", "example.com", "user with space", "pw"
	out, repr := lf.FormatRecordStable(host, url, login, password, false)
	if !repr {
		t.Fatal("spaced login must be representable via parseStored")
	}
	h2, _, l2, p2, ok := parseStored(string(out))
	if !ok || h2 != host || l2 != login || p2 != password {
		t.Fatalf("round-trip failed: out=%q h=%q l=%q p=%q", out, h2, l2, p2)
	}
}

func TestFormatRecordStableDigitLoginColonPassword(t *testing.T) {
	lf := newLineFormatter()
	host, url, login, password := "example.com", "example.com", "12345", "a:b:c"
	out, repr := lf.FormatRecordStable(host, url, login, password, false)
	if !repr {
		t.Fatal("digit login + colon password must be representable")
	}
	h2, _, l2, p2, ok := parseStored(string(out))
	if !ok || h2 != host || l2 != login || p2 != password {
		t.Fatalf("field round-trip failed: out=%q h=%q l=%q p=%q", out, h2, l2, p2)
	}
	// disambiguation form uses a trailing slash on urlPart
	if string(out) != "example.com/:12345:a:b:c" {
		t.Fatalf("out = %q, want example.com/:12345:a:b:c", out)
	}
}

func TestFormatRecordStableDigitLoginColonPasswordViaDelim(t *testing.T) {
	lf := newLineFormatter()
	p, err := NewDelimParser("|")
	if err != nil {
		t.Fatal(err)
	}
	host, url, login, password, ok := p.Parse("example.com|12345|a:b:c")
	if !ok {
		t.Fatal("DelimParser rejected digit login + colon password")
	}
	out, repr := lf.FormatRecordStable(host, url, login, password, false)
	if !repr {
		t.Fatal("Stable dropped delim digit+colon-password fields")
	}
	h2, _, l2, p2, ok := parseStored(string(out))
	if !ok || h2 != host || l2 != login || p2 != password {
		t.Fatalf("field round-trip failed: out=%q h=%q l=%q p=%q", out, h2, l2, p2)
	}
}

func TestFormatRecordStableDropsColonLoginFields(t *testing.T) {
	// Colon in login makes FormatRecord / HashKey field-ambiguous; even if a
	// caller bypasses finishParse, Stable must refuse to write.
	lf := newLineFormatter()
	out, repr := lf.FormatRecordStable("example.com", "example.com", "user:name", "pw", false)
	if repr {
		t.Fatalf("colon-login fields must be dropped, got %q", out)
	}
}

func TestColonAmbiguous(t *testing.T) {
	cases := map[string]bool{
		"":               false,
		"abc":            false,
		"a:b":            false,
		"a:b:c":          false,
		"a:b:c:d":        true,
		"h.com:8080:u:p": true,
	}
	for in, want := range cases {
		if got := colonAmbiguous([]byte(in)); got != want {
			t.Errorf("colonAmbiguous(%q) = %v, want %v", in, got, want)
		}
	}
}

// Clean two-colon lines still verify via parseStored (always-verify path).
func BenchmarkFormatRecordStableCleanLine(b *testing.B) {
	lf := newLineFormatter()
	host, url, login, password := "a.example.com", "https://a.example.com", "user", "pw1"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := lf.FormatRecordStable(host, url, login, password, false); !ok {
			b.Fatal("clean line unexpectedly dropped")
		}
	}
}
