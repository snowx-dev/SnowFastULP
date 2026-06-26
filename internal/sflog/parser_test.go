package sflog

import (
	"strings"
	"testing"
)

func TestParseCredentialsHandlesAliasesAndOutOfOrderFields(t *testing.T) {
	input := strings.NewReader(`Browser: Chrome
Login: alice@example.com
Host: https://portal.example.com/login
Password: a:b:c

`)
	creds, err := ParseCredentials(input, "Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("got %d creds: %+v", len(creds), creds)
	}
	got := creds[0]
	if got.URL != "https://portal.example.com/login" {
		t.Fatalf("URL = %q", got.URL)
	}
	if got.Username != "alice@example.com" {
		t.Fatalf("Username = %q", got.Username)
	}
	if got.Password != "a:b:c" {
		t.Fatalf("Password = %q", got.Password)
	}
}

func TestParseCredentialsHandlesRedLineSeparators(t *testing.T) {
	input := strings.NewReader(`URL: https://a.example.com/
USER: bob
PASS: pw1
===============
HOSTNAME: b.example.com
Username: carol
USER PASSWORD: pw2
`)
	creds, err := ParseCredentials(input, "passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Fatalf("got %d creds: %+v", len(creds), creds)
	}
	if creds[0].Username != "bob" || creds[1].Username != "carol" {
		t.Fatalf("creds = %+v", creds)
	}
}

func TestParseCredentialsURLLastNoSeparator(t *testing.T) {
	input := strings.NewReader("Login:u1\nPassword:p1\nURL:a.com\nLogin:u2\nPassword:p2\nURL:b.com\n")
	creds, err := ParseCredentials(input, "passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Fatalf("got %d creds: %+v", len(creds), creds)
	}
	if creds[0] != (Credential{URL: "a.com", Username: "u1", Password: "p1", Source: "passwords.txt"}) {
		t.Fatalf("cred0 = %+v", creds[0])
	}
	if creds[1] != (Credential{URL: "b.com", Username: "u2", Password: "p2", Source: "passwords.txt"}) {
		t.Fatalf("cred1 = %+v", creds[1])
	}
}

func TestParseCredentialsKeepsFirstURLAliasWithinRecord(t *testing.T) {
	input := strings.NewReader("URL: a.com\nHost: mirror.example.com\nUSER: u\nPASS: p\n")
	creds, err := ParseCredentials(input, "passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].URL != "a.com" {
		t.Fatalf("creds = %+v", creds)
	}
}

func TestParseCredentialsPreservesPasswordWhitespace(t *testing.T) {
	input := strings.NewReader("URL: a.com\nUSER: u\nPASS:  pa ss \n")
	creds, err := ParseCredentials(input, "passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].Password != " pa ss " {
		t.Fatalf("password = %q", creds[0].Password)
	}
}

func TestFormatULPLineNormalizesAndroidURL(t *testing.T) {
	cred := Credential{URL: "android://Zm9vYmFy@com.example.app/", Username: "u", Password: "p"}
	if got := FormatULPLine(cred, false); got != "com.example.app:u:p" {
		t.Fatalf("android line = %q", got)
	}
}

func TestFormatULPLineMatchesSFUShape(t *testing.T) {
	cred := Credential{
		URL:      "https://www.example.com/login?x=1",
		Username: "user",
		Password: "pass",
	}
	if got := FormatULPLine(cred, false); got != "www.example.com/login?x=1:user:pass" {
		t.Fatalf("full URL line = %q", got)
	}
	if got := FormatULPLine(cred, true); got != "example.com:user:pass" {
		t.Fatalf("no-uri line = %q", got)
	}
}
