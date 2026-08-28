package ulpengine

import "testing"

// parseLoose must admit a key for any line that strict parse() OR its loose
// extras path accepts. These cases pin the three regions that matter:
//   - strict-only: messy real creds (truncated JSON/cookie tails) that the
//     loose isLikelyJunk gate drops but strict keeps. losing these was the
//     straggler bug.
//   - loose-only: bare/IP host shapes the strict regex rejects.
//   - junk: rejected by both, must stay rejected by union too.
func TestParseLooseCoversStrictAndLoose(t *testing.T) {
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
			name:      "loose-only: ip host with port and trailing slash",
			line:      "103.181.181.122:8897/:admin:admin",
			strictOK:  false,
			looseOK:   true,
			unionOK:   true,
			wantHost:  "103.181.181.122:8897",
			wantLogin: "admin",
			wantPass:  "admin",
		},
		{
			name:      "loose-only: ip host with port and path",
			line:      "35.207.240.204:8080/auth/login:annotator.37@crowdworks.kr:acote_an_37",
			strictOK:  false,
			looseOK:   true,
			unionOK:   true,
			wantHost:  "35.207.240.204:8080",
			wantLogin: "annotator.37@crowdworks.kr",
			wantPass:  "acote_an_37",
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
			host, _, login, pass, ok := parse(tc.line)
			if !ok {
				host, _, login, pass, ok = parseLoose(tc.line)
			}
			if ok != tc.unionOK {
				t.Fatalf("strict-or-loose parse ok = %v, want %v", ok, tc.unionOK)
			}
			if !ok {
				return
			}
			if host != tc.wantHost || login != tc.wantLogin || pass != tc.wantPass {
				t.Errorf("strict-or-loose = (%q,%q,%q), want (%q,%q,%q)",
					host, login, pass, tc.wantHost, tc.wantLogin, tc.wantPass)
			}
		})
	}
}

// For any line strict accepts, the strict parser returns the canonical fields
// that ingest must preserve.
func TestParseStrictKeyParity(t *testing.T) {
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
		lh, _, ll, lp, lok := parse(line)
		if !lok {
			t.Errorf("strict parser rejected strict-accepted line %q", line)
			continue
		}
		if dedupKey(sh, sl, sp) != dedupKey(lh, ll, lp) {
			t.Errorf("key mismatch for %q: first=%q strict=%q",
				line, dedupKey(sh, sl, sp), dedupKey(lh, ll, lp))
		}
	}
}
