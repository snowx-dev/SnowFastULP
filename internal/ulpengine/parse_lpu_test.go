package ulpengine

import (
	"strings"
	"testing"
)

// login:password:scheme://url. matchLPU fallback must extract the same
// fields strict would from the equivalent ULP-order line
func TestParseLPUAccepted(t *testing.T) {
	cases := []struct {
		in       string
		host     string
		login    string
		password string
	}{
		{
			in:       "alice@example.com:hunter2A:https://www.shop.example.com/create_account.php",
			host:     "shop.example.com",
			login:    "alice@example.com",
			password: "hunter2A",
		},
		{
			in:       "BobUser01:Secret2024:https://forum.example.org/login",
			host:     "forum.example.org",
			login:    "BobUser01",
			password: "Secret2024",
		},
		{
			in:       "carol@example.net:PassPhrase01:https://auth.example.com/signin/authorize",
			host:     "auth.example.com",
			login:    "carol@example.net",
			password: "PassPhrase01",
		},
	}
	for _, c := range cases {
		host, _, login, password, ok := parse(c.in)
		if !ok {
			t.Errorf("parse(%q) ok=false, want true", c.in)
			continue
		}
		if host != c.host || login != c.login || password != c.password {
			t.Errorf("parse(%q) = host=%q login=%q password=%q; want host=%q login=%q password=%q",
				c.in, host, login, password, c.host, c.login, c.password)
		}
	}
}

// LPU-shape lines rejected by finishParse hygiene rules
func TestParseLPURejectedByHygiene(t *testing.T) {
	rejects := []string{
		"user:" + strings.Repeat("p", 65) + ":https://example.com", // pw > 64
		"user:{wrapped}:https://example.com",                       // wrapped braces
		"user:pass:https://localhost",                              // localhost w/o @ in login
	}
	for _, in := range rejects {
		if _, _, _, _, ok := parse(in); ok {
			t.Errorf("parse(%q) ok=true, want false (hygiene)", in)
		}
	}
}

// parseLoose adds host:port:user:pw, bare host:user:pw, IP-prefixed dumps
func TestParseLooseExtraShapes(t *testing.T) {
	cases := []struct {
		in       string
		host     string
		login    string
		password string
	}{
		{
			in:       "ftp.example.com:21:90210:Pa55word!@",
			host:     "ftp.example.com:21",
			login:    "90210",
			password: "Pa55word!@",
		},
		{
			// 192.0.2.0/24 = TEST-NET-1, never routable
			in:       "192.0.2.10:21:test user:hunter2A",
			host:     "192.0.2.10:21",
			login:    "test user",
			password: "hunter2A",
		},
		{
			in:       "example.org/forum/signup/:Zoë:p4ssword",
			host:     "example.org",
			login:    "Zoë",
			password: "p4ssword",
		},
		{
			// LPU works in loose too
			in:       "alice@example.com:hunter2A:https://www.shop.example.com/create_account.php",
			host:     "shop.example.com",
			login:    "alice@example.com",
			password: "hunter2A",
		},
	}
	for _, c := range cases {
		host, _, login, password, ok := parseLoose(c.in)
		if !ok {
			t.Errorf("parseLoose(%q) ok=false, want true", c.in)
			continue
		}
		if host != c.host || login != c.login || password != c.password {
			t.Errorf("parseLoose(%q) = host=%q login=%q password=%q; want host=%q login=%q password=%q",
				c.in, host, login, password, c.host, c.login, c.password)
		}
	}
}

// junk filter, would otherwise pass naive 2-colon rule. covers markers
// {", LegacyGeneric:, \t\t1:, :PasswordText, :PasswordResetRequestForm,
// :Username, :LoginID. synthetic, no real samples shipped
func TestParseLooseJunkFilters(t *testing.T) {
	junk := []string{
		`0123456789abcdef01234567:{"SerializedData":"{\"access_token\":\"x\"}"}:oauth.example.com`,
		`LegacyGeneric:target=Example:App:Info:something`,
		`//www.example.com/foo/:Username Display Name:PasswordResetRequestForm[email] user@example.org`,
		"//site.example\t\t1:LoginID\t\t5550101234\t\t1:PasswordText testuser:1",
	}
	for _, in := range junk {
		if _, _, _, _, ok := parseLoose(in); ok {
			t.Errorf("parseLoose(%q) ok=true, want false (junk filter)", in)
		}
	}
}

// parseLoose must not regress vs strict on canonical ULP inputs
func TestParseLooseStrictCompatible(t *testing.T) {
	cases := []struct {
		in   string
		host string
	}{
		{"https://foo.example.com/x:user@example.com:secret", "foo.example.com"},
		{"http://www.example.com:bob:token", "example.com"},
		{"example.com:8080/path?x=1:alice:pw", "example.com:8080"},
	}
	for _, c := range cases {
		host, _, _, _, ok := parseLoose(c.in)
		if !ok {
			t.Errorf("parseLoose(%q) ok=false, want true (strict-compatible)", c.in)
			continue
		}
		if host != c.host {
			t.Errorf("parseLoose(%q) host=%q, want %q", c.in, host, c.host)
		}
	}
}

func TestIsAllDigits(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     true,
		"1234":  true,
		"12a":   false,
		"-1":    false,
		" 1":    false,
		"00000": true,
	}
	for s, want := range cases {
		if got := isAllDigits(s); got != want {
			t.Errorf("isAllDigits(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestSplitNColon(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want []string
	}{
		{"a:b:c", 3, []string{"a", "b", "c"}},
		{"a:b:c", 2, []string{"a", "b:c"}},
		{"a:b:c:d", 3, []string{"a", "b", "c:d"}},
		{"abc", 3, []string{"abc"}},
		{"", 3, []string{""}},
	}
	for _, c := range cases {
		got := splitNColon(c.in, c.n)
		if len(got) != len(c.want) {
			t.Errorf("splitNColon(%q,%d) = %v, want %v", c.in, c.n, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitNColon(%q,%d)[%d] = %q, want %q", c.in, c.n, i, got[i], c.want[i])
			}
		}
	}
}
