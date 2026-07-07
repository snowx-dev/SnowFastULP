package ulpengine

import "testing"

// parseUnion is the index/regen parser: it must admit a key for any line that
// strict parse() OR loose parseLoose() accepts, so a sidecar can never miss a
// stored line. These cases pin the three regions that matter:
//   - strict-only: messy real creds (truncated JSON/cookie tails) that the
//     loose isLikelyJunk gate drops but strict keeps. losing these was the
//     straggler bug.
//   - loose-only: bare/IP host shapes the strict regex rejects.
//   - junk: rejected by both, must stay rejected by union too.
func TestParseUnionCoversStrictAndLoose(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		strictOK  bool
		looseOK   bool
		unionOK   bool
		wantHost  string
		wantLogin string
		wantPass  string
	}{
		{
			name:      "clean ulp (both)",
			line:      "https://a.example.com:user@mail.com:pw1",
			strictOK:  true,
			looseOK:   true,
			unionOK:   true,
			wantHost:  "a.example.com",
			wantLogin: "user@mail.com",
			wantPass:  "pw1",
		},
		{
			name:      "strict-only: truncated json tail",
			line:      `twitter.com:moraxd5:{"uid":"7178515064324310021","token"`,
			strictOK:  true,
			looseOK:   false,
			unionOK:   true,
			wantHost:  "twitter.com",
			wantLogin: "moraxd5",
			wantPass:  `{"uid":"7178515064324310021","token"`,
		},
		{
			name:      "strict-only: open-brace cookie tail",
			line:      `dash.cloudflare.com/sign-up:login.c@example.com:{"cc"`,
			strictOK:  true,
			looseOK:   false,
			unionOK:   true,
			wantHost:  "dash.cloudflare.com",
			wantLogin: "login.c@example.com",
			wantPass:  `{"cc"`,
		},
		{
			name:      "loose-only: bare ip host (3 field)",
			line:      "192.168.1.1:user:pass",
			strictOK:  false,
			looseOK:   true,
			unionOK:   true,
			wantHost:  "192.168.1.1",
			wantLogin: "user",
			wantPass:  "pass",
		},
		{
			name:      "loose-only: ip host with port (4 field)",
			line:      "10.0.0.5:8080:admin:admin",
			strictOK:  false,
			looseOK:   true,
			unionOK:   true,
			wantHost:  "10.0.0.5:8080",
			wantLogin: "admin",
			wantPass:  "admin",
		},
		{
			name:     "junk: wrapped json object, no creds",
			line:     `{"session":"abc","exp":"y"}`,
			strictOK: false,
			looseOK:  false,
			unionOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, ok := parse(tc.line); ok != tc.strictOK {
				t.Errorf("strict parse ok = %v, want %v", ok, tc.strictOK)
			}
			if _, _, _, _, ok := parseLoose(tc.line); ok != tc.looseOK {
				t.Errorf("loose parse ok = %v, want %v", ok, tc.looseOK)
			}
			host, _, login, pass, ok := parseUnion(tc.line)
			if ok != tc.unionOK {
				t.Fatalf("union parse ok = %v, want %v", ok, tc.unionOK)
			}
			if !ok {
				return
			}
			if host != tc.wantHost || login != tc.wantLogin || pass != tc.wantPass {
				t.Errorf("union = (%q,%q,%q), want (%q,%q,%q)",
					host, login, pass, tc.wantHost, tc.wantLogin, tc.wantPass)
			}
		})
	}
}

// for any line strict accepts, union must return byte-identical fields (and
// thus the same dedup key) so the index built by regen matches what ingest
// would compute. parseLoose also runs strict-first, so this transitively
// covers loose/union key parity on shared lines.
func TestParseUnionKeyParityWithStrict(t *testing.T) {
	lines := []string{
		"https://a.example.com:user@mail.com:pw1",
		"user:pass:https://site.com",
		`twitter.com:moraxd5:{"uid":"123","token"`,
		"sub.host.co.uk:8443:bob:secret",
	}
	for _, line := range lines {
		sh, _, sl, sp, sok := parse(line)
		if !sok {
			t.Fatalf("test setup: strict rejected %q", line)
		}
		uh, _, ul, up, uok := parseUnion(line)
		if !uok {
			t.Errorf("union rejected strict-accepted line %q", line)
			continue
		}
		if dedupKey(sh, sl, sp) != dedupKey(uh, ul, up) {
			t.Errorf("key mismatch for %q: strict=%q union=%q",
				line, dedupKey(sh, sl, sp), dedupKey(uh, ul, up))
		}
	}
}
