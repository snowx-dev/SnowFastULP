package ulpengine

import "testing"

// parseLoose must admit a key for any line that strict parse() OR its loose
// extras path accepts. These cases pin the three regions that matter:
//   - strict-parseable messy real creds (truncated JSON/cookie tails): loose
//     must keep everything strict keeps, so the strict attempt runs before the
//     junk filter (the old order dropped them and lost credentials).
//   - loose-only: bare/IP host shapes the strict parser rejects.
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
			name:      "json-tail password (strict and loose agree)",
			line:      `twitter.com:moraxd5:{"uid":"7178515064324310021","token"`,
			strictOK:  true,
			looseOK:   true,
			unionOK:   true,
			wantHost:  "twitter.com",
			wantLogin: "moraxd5",
			wantPass:  `{"uid":"7178515064324310021","token"`,
		},
		{
			name:      "open-brace cookie tail (strict and loose agree)",
			line:      `dash.cloudflare.com/sign-up:login.c@example.com:{"cc"`,
			strictOK:  true,
			looseOK:   true,
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
			name:      "shared strict fallback: ip host with port",
			line:      "10.0.0.5:8080:admin:admin",
			strictOK:  true,
			looseOK:   true,
			unionOK:   true,
			wantHost:  "10.0.0.5:8080",
			wantLogin: "admin",
			wantPass:  "admin",
		},
		{
			name:      "shared strict fallback: ip host with port and trailing slash",
			line:      "103.181.181.122:8897/:admin:admin",
			strictOK:  true,
			looseOK:   true,
			unionOK:   true,
			wantHost:  "103.181.181.122:8897",
			wantLogin: "admin",
			wantPass:  "admin",
		},
		{
			name:      "shared strict fallback: ip host with port and path",
			line:      "35.207.240.204:8080/auth/login:annotator.37@crowdworks.kr:acote_an_37",
			strictOK:  true,
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
// that ingest must preserve. Lines both modes accept must key identically
// under strict and loose; strict-only lines (loose isLikelyJunk drops them)
// are pinned to their exact fields plus a stable round-trip through the
// stored representation, instead of a meaningless strict-vs-strict compare.
func TestParseStrictKeyParity(t *testing.T) {
	cases := []struct {
		line      string
		wantHost  string
		wantLogin string
		wantPass  string
	}{
		{"https://a.example.com:user@mail.com:pw1", "a.example.com", "user@mail.com", "pw1"},
		{"user:pass:https://site.com", "site.com", "user", "pass"},
		{`twitter.com:moraxd5:{"uid":"123","token"`, "twitter.com", "moraxd5", `{"uid":"123","token"`},
		{"sub.host.co.uk:8443:bob:secret", "sub.host.co.uk:8443", "bob", "secret"},
	}
	lf := newLineFormatter()
	for _, tc := range cases {
		sh, su, sl, sp, sok := parse(tc.line)
		if !sok {
			t.Fatalf("test setup: strict rejected %q", tc.line)
		}
		lh, _, ll, lp, lok := parseLoose(tc.line)
		if lok {
			// accepted by both modes: loose must admit the same canonical key
			if sh != lh || sl != ll || sp != lp {
				t.Errorf("strict/loose field drift for %q: strict=(%q,%q,%q) loose=(%q,%q,%q)",
					tc.line, sh, sl, sp, lh, ll, lp)
			}
			if dedupKeySum(sh, sl, sp) != dedupKeySum(lh, ll, lp) {
				t.Errorf("key mismatch for %q: strict=%#x loose=%#x",
					tc.line, dedupKeySum(sh, sl, sp), dedupKeySum(lh, ll, lp))
			}
			continue
		}
		// strict-only: exact fields, and the stored form must round-trip back
		// through the archive reader to the same fields
		if sh != tc.wantHost || sl != tc.wantLogin || sp != tc.wantPass {
			t.Errorf("strict-only %q = (%q,%q,%q), want (%q,%q,%q)",
				tc.line, sh, sl, sp, tc.wantHost, tc.wantLogin, tc.wantPass)
		}
		out, repr := lf.FormatRecordStable(sh, su, sl, sp, false)
		if !repr {
			t.Fatalf("strict-only %q has no round-trippable stored form", tc.line)
		}
		rh, _, rl, rp, rok := parseStored(string(out))
		if !rok || rh != sh || rl != sl || rp != sp {
			t.Errorf("stored round-trip drift for %q: got (ok=%v, %q,%q,%q), want (%q,%q,%q)",
				tc.line, rok, rh, rl, rp, sh, sl, sp)
		}
	}
}
