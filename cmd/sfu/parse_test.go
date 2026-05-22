package main

import (
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	cases := []struct {
		in          string
		host        string
		url         string
		login       string
		password    string
		strippedURL string
	}{
		{
			in:          "https://foo.example.com/x:user@example.com:secret",
			host:        "foo.example.com",
			url:         "https://foo.example.com/x",
			login:       "user@example.com",
			password:    "secret",
			strippedURL: "foo.example.com/x",
		},
		{
			in:          "http://www.example.com:bob:token",
			host:        "example.com",
			url:         "http://www.example.com",
			login:       "bob",
			password:    "token",
			strippedURL: "www.example.com",
		},
		{
			// :port stays part of host, stripScheme variant emits
			in:          "example.com:8080/path?x=1:alice:pw",
			host:        "example.com:8080",
			url:         "example.com:8080/path?x=1",
			login:       "alice",
			password:    "pw",
			strippedURL: "example.com:8080/path?x=1",
		},
	}
	for _, c := range cases {
		host, url, login, password, ok := parse(c.in)
		if !ok {
			t.Fatalf("parse(%q) ok=false, want true", c.in)
		}
		if host != c.host || url != c.url || login != c.login || password != c.password {
			t.Fatalf("parse(%q) = %q,%q,%q,%q; want %q,%q,%q,%q",
				c.in, host, url, login, password,
				c.host, c.url, c.login, c.password)
		}
		if got := stripScheme(url); got != c.strippedURL {
			t.Fatalf("stripScheme(%q) = %q, want %q", url, got, c.strippedURL)
		}
	}
}

func TestParseRejects(t *testing.T) {
	rejects := []string{
		"",
		"no-colon-line",
		"only-one:colon",
		"localhost:bob:pw",                    // localhost w/o @ in login
		"127.0.0.1:bob:pw",                    // 127. w/o @ in login
		"https://a.example.com:{user}:secret", // braces
		"https://a.example.com:user:{pw}",     // braces
		"https://a.example.com:user:http://evil.com",            // pw has http://
		"https://a.example.com:user:https://bad",                // pw has https://
		"https://a.example.com:user:" + strings.Repeat("z", 65), // pw > 64
	}
	for _, in := range rejects {
		if _, _, _, _, ok := parse(in); ok {
			t.Errorf("parse(%q) ok=true, want false", in)
		}
	}
}

func TestParseAcceptsMaxPassword(t *testing.T) {
	in := "https://a.example.com:user:" + strings.Repeat("z", 64)
	if _, _, _, _, ok := parse(in); !ok {
		t.Fatalf("parse with 64-byte password should be valid")
	}
}

func TestParseStripsTrailingNewline(t *testing.T) {
	in := "https://a.example.com:user@example.com:secret\r\n"
	host, _, _, password, ok := parse(in)
	if !ok || host != "a.example.com" || password != "secret" {
		t.Fatalf("parse should strip trailing CRLF: host=%q password=%q ok=%v", host, password, ok)
	}
}

func TestDedupKeyDistinguishesOnlyOnHostLoginPassword(t *testing.T) {
	a := dedupKey("a.example.com", "user", "pw")
	b := dedupKey("a.example.com", "user", "pw")
	c := dedupKey("a.example.com", "user2", "pw")
	if a != b {
		t.Fatal("identical inputs must produce identical keys")
	}
	if a == c {
		t.Fatal("different login must produce different key")
	}
}

func TestParseLineTooLong(t *testing.T) {
	in := "https://a.example.com:user:" + strings.Repeat("z", 4096)
	if _, _, _, _, ok := parse(in); ok {
		t.Fatalf("parse should reject lines > 4096 bytes")
	}
}

func TestFormatRecord(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		url      string
		login    string
		password string
		noURI    bool
		want     string
	}{
		{
			name:     "default keeps stripped url including path",
			host:     "aaa.bbb.com",
			url:      "https://aaa.bbb.com/bunch/ofthings/here",
			login:    "john@gmail.com",
			password: "password123",
			noURI:    false,
			want:     "aaa.bbb.com/bunch/ofthings/here:john@gmail.com:password123",
		},
		{
			name:     "no-uri replaces url with bare host",
			host:     "aaa.bbb.com",
			url:      "https://aaa.bbb.com/bunch/ofthings/here",
			login:    "john@gmail.com",
			password: "password123",
			noURI:    true,
			want:     "aaa.bbb.com:john@gmail.com:password123",
		},
		{
			name:     "no-uri preserves port in host",
			host:     "x.example.com:8080",
			url:      "x.example.com:8080/path?q=1",
			login:    "alice",
			password: "pw",
			noURI:    true,
			want:     "x.example.com:8080:alice:pw",
		},
		{
			name:     "default emits scheme-less url even when no-uri off",
			host:     "x.example.com",
			url:      "http://www.x.example.com",
			login:    "bob",
			password: "tok",
			noURI:    false,
			want:     "www.x.example.com:bob:tok",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatRecord(c.host, c.url, c.login, c.password, c.noURI)
			if got != c.want {
				t.Fatalf("formatRecord = %q, want %q", got, c.want)
			}
		})
	}
}
