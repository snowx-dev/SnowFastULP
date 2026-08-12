package ulpengine

import (
	"testing"
)

func TestParseStoredRoundTripFormatRecord(t *testing.T) {
	lf := newLineFormatter()
	cases := []struct {
		name                   string
		host, url, login, pass string
	}{
		{"plain", "example.com", "example.com", "user", "pw"},
		{"https path", "example.com", "https://example.com/login", "alice", "s3cret"},
		{"port", "a.example.com:8080", "https://a.example.com:8080/x", "user", "pw1"},
		{"space login", "example.com", "example.com", "user name", "pw"},
		{"dollar login", "example.com", "example.com", "kalai123$s", "pw"},
		{"unicode login", "example.com", "example.com", "Zoë", "pw"},
		{"colon password", "example.com", "example.com", "user", "a:b:c"},
		{"digit login colon password", "example.com", "example.com", "12345", "a:b:c"},
		{"json password", "twitter.com", "twitter.com", "moraxd5", `{"uid":"123","token"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// colon-in-login must already be rejected by finishParse before format
			if _, _, _, _, ok := finishParse(tc.url, tc.login, tc.pass); !ok {
				t.Fatalf("setup finishParse rejected fields")
			}
			out := lf.FormatRecord(tc.host, tc.url, tc.login, tc.pass, false)
			h, _, l, p, ok := parseStored(string(out))
			if !ok {
				t.Fatalf("parseStored rejected FormatRecord output %q", out)
			}
			if h != tc.host || l != tc.login || p != tc.pass {
				t.Fatalf("fields mismatch: got h=%q l=%q p=%q want h=%q l=%q p=%q (out=%q)",
					h, l, p, tc.host, tc.login, tc.pass, out)
			}
		})
	}
}

func TestParseStoredAcceptsSpacedLogin(t *testing.T) {
	line := "example.com:user with space:pw"
	h, _, l, p, ok := parseStored(line)
	if !ok {
		t.Fatal("parseStored must accept spaced login on trusted archive lines")
	}
	if h != "example.com" || l != "user with space" || p != "pw" {
		t.Fatalf("got host=%q login=%q pass=%q", h, l, p)
	}
	// strict parse rejects; that is fine for raw ingest
	if _, _, _, _, ok := parse(line); ok {
		t.Fatal("setup: strict parse unexpectedly accepted spaced login")
	}
}

func TestParseStoredKeepsStrictJSONTail(t *testing.T) {
	line := `twitter.com:moraxd5:{"uid":"7178515064324310021","token"`
	h, _, l, p, ok := parseStored(line)
	if !ok {
		t.Fatal("parseStored must keep strict-only JSON password tails (no junk gate)")
	}
	if h != "twitter.com" || l != "moraxd5" {
		t.Fatalf("got host=%q login=%q pass=%q", h, l, p)
	}
}

func TestParseStoredAndroid(t *testing.T) {
	line := "android://com.example.app/:user:secret"
	h, u, l, p, ok := parseStored(line)
	if !ok {
		t.Fatal("parseStored must accept android lines")
	}
	if h == "" || u == "" || l != "user" || p != "secret" {
		t.Fatalf("got host=%q url=%q login=%q pass=%q", h, u, l, p)
	}
}

func TestParseStoredKeyParityWithParseUnion(t *testing.T) {
	lf := newLineFormatter()
	corpus := []string{
		"https://a.example.com:user@mail.com:pw1",
		`twitter.com:moraxd5:{"uid":"7178515064324310021","token"`,
		`dash.cloudflare.com/sign-up:login.c@example.com:{"cc"`,
		"https://a.example.com:8080:user:pw1",
		"a.example.com:8080:user:pw1",
		"103.181.181.122:8897/:admin:admin",
		"103.181.181.122:8897:admin:admin",
		"example.com:user:pw",
		"www.example.com/path:user:secret",
		"android://com.x.app/:alice:pw",
		"user:pass:https://site.com",
		"example.com:user name:pw", // union via loose extras
		"example.com:kalai123$s:pw",
	}
	for _, line := range corpus {
		uh, _, ul, up, uok := parseUnion(line)
		sh, _, sl, sp, sok := parseStored(line)
		if !uok {
			continue // parity only binds when union accepts
		}
		if !sok {
			t.Errorf("parseUnion ok but parseStored rejected %q", line)
			continue
		}
		if lf.HashKey(uh, ul, up) != lf.HashKey(sh, sl, sp) {
			t.Errorf("key mismatch on %q\n  union  h=%q l=%q p=%q\n  stored h=%q l=%q p=%q",
				line, uh, ul, up, sh, sl, sp)
		}
	}
}

func TestParseStoredRejectsColonInLogin(t *testing.T) {
	// finishParse is the library boundary; parseStored only sees fields that
	// already passed it (or android with the same ban).
	if _, _, _, _, ok := finishParse("example.com", "user:name", "pw"); ok {
		t.Fatal("finishParse must reject colon in login")
	}
}

func TestIsStoredURLPrefix(t *testing.T) {
	for _, s := range []string{"example.com", "a.example.com:8080", "a.example.com:8080/x", "1.2.3.4:8897/", "example.com/"} {
		if !isStoredURLPrefix(s) {
			t.Errorf("isStoredURLPrefix(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "nodot", ":8080", "example.com:abc", "example.com:8080x", "example.com/:12345"} {
		if isStoredURLPrefix(s) {
			t.Errorf("isStoredURLPrefix(%q) = true, want false", s)
		}
	}
}

func TestParseStoredPortURLStillLongest(t *testing.T) {
	h, _, l, p, ok := parseStored("a.example.com:8080:user:pw")
	if !ok {
		t.Fatal("port URL rejected")
	}
	if h != "a.example.com:8080" || l != "user" || p != "pw" {
		t.Fatalf("got host=%q login=%q pass=%q", h, l, p)
	}
}

func TestParseStoredSlashDisambiguatesDigitLogin(t *testing.T) {
	h, _, l, p, ok := parseStored("example.com/:12345:a:b:c")
	if !ok {
		t.Fatal("slash-disambiguated line rejected")
	}
	if h != "example.com" || l != "12345" || p != "a:b:c" {
		t.Fatalf("got host=%q login=%q pass=%q", h, l, p)
	}
}
