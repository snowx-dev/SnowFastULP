package ulpengine

import (
	"strings"
	"testing"
)

func TestSharedParserHarmonizedGrammar(t *testing.T) {
	tests := []struct {
		name, line, host, login, password string
	}{
		{"trim field edges", "  https://example.com/path \t: user : pw  ", "example.com", "user", "pw"},
		{"internal-space login", "example.com:Tinni Roy:pw", "example.com", "Tinni Roy", "pw"},
		{"space-bearing email-like login", "example.com:mario-RK9@ hot mail.com:pw", "example.com", "mario-RK9@ hot mail.com", "pw"},
		{"leading-plus login", "example.com:+8801318421196:pw", "example.com", "+8801318421196", "pw"},
		{"P1 host port", "1.2.3.4:8080:admin:pw", "1.2.3.4:8080", "admin", "pw"},
		{"P2 host port path", "103.181.181.122:8897/:admin:pw", "103.181.181.122:8897", "admin", "pw"},
		{"P3 password colons", "example.com:user:pa:ss", "example.com", "user", "pa:ss"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, _, login, password, ok := ParseLine(tc.line, false)
			if !ok {
				t.Fatalf("ParseLine(%q) rejected", tc.line)
			}
			if host != tc.host || login != tc.login || password != tc.password {
				t.Fatalf("ParseLine(%q) = host=%q login=%q password=%q, want %q %q %q", tc.line, host, login, password, tc.host, tc.login, tc.password)
			}
		})
	}
}

func TestSharedParserHarmonizedRejectsRemain(t *testing.T) {
	for _, line := range []string{
		"127.0.0.1:8080:admin:pw",
		"localhost.local:admin:pw",
		"example.com:user$name:pw",
		"example.com:user:{pw}",
		"example.com:user:" + strings.Repeat("p", 65),
	} {
		if _, _, _, _, ok := ParseLine(line, false); ok {
			t.Fatalf("ParseLine(%q) accepted; hygiene must remain enforced", line)
		}
	}
}

func TestHarmonizedRecordsRoundTrip(t *testing.T) {
	for _, line := range []string{
		"  https://example.com/path : Tinni Roy : pw  ",
		"example.com:+8801318421196:pw",
		"1.2.3.4:8080:admin:pw",
		"103.181.181.122:8897/:admin:pw",
	} {
		host, url, login, password, ok := ParseLine(line, false)
		if !ok {
			t.Fatalf("ParseLine(%q) rejected", line)
		}
		lf := newLineFormatter()
		stored, stable := lf.FormatRecordStable(host, url, login, password, false)
		if !stable {
			t.Fatalf("newly accepted record %q has no stable representation", line)
		}
		h2, _, l2, p2, ok := parseStored(string(stored))
		if !ok || h2 != host || l2 != login || p2 != password {
			t.Fatalf("round trip %q -> %q = (%q,%q,%q,%v), want (%q,%q,%q,true)", line, stored, h2, l2, p2, ok, host, login, password)
		}
	}
}
