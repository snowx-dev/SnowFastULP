package ulpengine

import "testing"

// FormatRecordStable must pick a representation whose re-parse (parseUnion, the
// regen parser) reproduces the original dedup key, or drop the line. These
// cover the four outcomes: clean full form, verified full form, host-only
// rescue, and unrepresentable drop.
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
		{"unrepresentable dropped", `jurbzdm:astr.m@ou4eudeaeC:Estr@6438:https://om.fhttpiip-dual/:abdell.zouad@gmail.co:NellaAde9:@Nv@g`, ""},
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
			// the kept form must re-parse to the original key.
			wantKey := lf.HashKey(host, login, password)
			h2, _, l2, p2, ok2 := parseUnion(string(out))
			if !ok2 || lf.HashKey(h2, l2, p2) != wantKey {
				t.Errorf("kept output %q does not round-trip to original key", out)
			}
		})
	}
}

// Property: for any parsed line, FormatRecordStable either drops it or yields
// bytes that parseUnion re-parses to the same key. This is the core integrity
// invariant that prevents regen stragglers.
func TestFormatRecordStableGuaranteesRoundTrip(t *testing.T) {
	corpus := []string{
		"https://a.example.com:user@mail.com:pw1",
		"user:pass:https://site.com",
		`twitter.com:moraxd5:{"uid":"123","token"`,
		`dash.cloudflare.com/sign-up:holik@gmail.com:{"cc"`,
		"sub.host.co.uk:8443:bob:secret",
		"192.168.1.1:user:pass",
		"10.0.0.5:8080:admin:admin",
		"plain.example.com:joe:p@ss:word",
		"user:pw1:https://clean.example.com/:weird:path",
		`jurbzdm:astr.m@ou4eudeaeC:Estr@6438:https://om.fhttpiip-dual/:abdell.zouad@gmail.co:NellaAde9:@Nv@g`,
	}
	lf := newLineFormatter()
	for _, line := range corpus {
		host, url, login, password, ok := parseUnion(line)
		if !ok {
			continue
		}
		wantKey := lf.HashKey(host, login, password)
		out, repr := lf.FormatRecordStable(host, url, login, password, false)
		if !repr {
			continue // dropping is allowed; the guarantee only binds kept lines
		}
		h2, _, l2, p2, ok2 := parseUnion(string(out))
		if !ok2 || lf.HashKey(h2, l2, p2) != wantKey {
			t.Errorf("kept line %q -> %q does not round-trip to its key", line, out)
		}
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

// the clean two-colon hot path must not allocate (no verifying re-parse).
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
