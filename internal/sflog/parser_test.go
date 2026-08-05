package sflog

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

// encodeUTF16 produces BOM-prefixed UTF-16 bytes (little- or big-endian) for s,
// matching how Windows stealer logs store Passwords.txt.
func encodeUTF16(s string, bigEndian bool) []byte {
	var buf bytes.Buffer
	put := binary.LittleEndian.PutUint16
	bom := []byte{0xff, 0xfe}
	if bigEndian {
		put = binary.BigEndian.PutUint16
		bom = []byte{0xfe, 0xff}
	}
	buf.Write(bom)
	var u [2]byte
	for _, r := range utf16.Encode([]rune(s)) {
		put(u[:], r)
		buf.Write(u[:])
	}
	return buf.Bytes()
}

// TestParseCredentialsDecodesEncodings proves a Windows-origin Passwords.txt is
// parsed identically whether it is plain UTF-8, UTF-8 with a BOM, or UTF-16
// LE/BE with a BOM (the formats RedLine/Vidar and Notepad emit).
func TestParseCredentialsDecodesEncodings(t *testing.T) {
	const body = "URL: https://portal.example.com/login\nUSER: alice@example.com\nPASS: s3cret\n"
	want := func(c Credential) bool {
		return c.URL == "https://portal.example.com/login" &&
			c.Username == "alice@example.com" && c.Password == "s3cret"
	}
	cases := map[string][]byte{
		"utf8":        []byte(body),
		"utf8-bom":    append([]byte{0xef, 0xbb, 0xbf}, []byte(body)...),
		"utf16le-bom": encodeUTF16(body, false),
		"utf16be-bom": encodeUTF16(body, true),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			creds, err := ParseCredentials(bytes.NewReader(raw), "Passwords.txt")
			if err != nil {
				t.Fatal(err)
			}
			if len(creds) != 1 || !want(creds[0]) {
				t.Fatalf("%s: got %d creds %+v", name, len(creds), creds)
			}
		})
	}
}

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

// TestFormatULPLineKeepsAndroidURL proves android:// pseudo-URLs are emitted
// verbatim (scheme + signing-cert hash + package), matching sfu's stripScheme
// which only drops http(s). The host-only -no-uri mode still reduces to the
// package name.
func TestFormatULPLineKeepsAndroidURL(t *testing.T) {
	cred := Credential{URL: "android://Zm9vYmFy@com.example.app/", Username: "u", Password: "p"}
	if got := FormatULPLine(cred, false); got != "android://Zm9vYmFy@com.example.app/:u:p" {
		t.Fatalf("android line = %q", got)
	}
	if got := FormatULPLine(cred, true); got != "com.example.app:u:p" {
		t.Fatalf("android no-uri line = %q", got)
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

// TestParseCredentialsLabelLessColonLines proves the colon-line fallback recovers
// creds from label-less Raccoon/StealC files (raw url:user:pass per line) that
// the labeled-block parser alone yields zero for.
func TestParseCredentialsLabelLessColonLines(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		"https://example.com/login:bob:hunter2",
		"https://other.com/signin:alice:password1",
		"android://Zm9v@com.example.app/:user:pw",
		"",
	}, "\n"))
	creds, err := ParseCredentials(body, "pws.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 3 {
		t.Fatalf("got %d creds, want 3: %+v", len(creds), creds)
	}
	want := map[string]Credential{
		"bob:hunter2":    {URL: "https://example.com/login", Username: "bob", Password: "hunter2", Source: "pws.txt"},
		"alice:password1": {URL: "https://other.com/signin", Username: "alice", Password: "password1", Source: "pws.txt"},
		"user:pw":         {URL: "android://Zm9v@com.example.app/", Username: "user", Password: "pw", Source: "pws.txt"},
	}
	for _, c := range creds {
		key := c.Username + ":" + c.Password
		w, ok := want[key]
		if !ok {
			t.Fatalf("unexpected cred: %+v", c)
		}
		if c.URL != w.URL || c.Source != w.Source {
			t.Errorf("cred %q:%q URL=%q want %q", c.Username, c.Password, c.URL, w.URL)
		}
	}
}

// TestParseCredentialsLabeledStillParsed proves the colon-line fallback does
// not change output for a labeled file (the 99% case): the labeled pass runs
// and the fallback never kicks in.
func TestParseCredentialsLabeledStillParsed(t *testing.T) {
	body := strings.NewReader("URL: https://a.example.com/\nUSER: bob\nPASS: pw1\n")
	creds, err := ParseCredentials(body, "Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].Username != "bob" || creds[0].Password != "pw1" {
		t.Fatalf("labeled file mis-parsed: %+v", creds)
	}
}

// TestParseCredentialsCRLFLabeledNoGarbage pins the CRLF regression: a
// CRLF-encoded labeled file must not leave a '\r' glued to values (which used
// to make "Login:\r" parse as val="\r" and emit garbage blocks). The output
// must be byte-identical to the LF-encoded version.
func TestParseCredentialsCRLFLabeledNoGarbage(t *testing.T) {
	lf := "URL: https://a.example.com/\nUSER: bob\nPASS: pw1\n"
	crlf := "URL: https://a.example.com/\r\nUSER: bob\r\nPASS: pw1\r\n"
	want, err := ParseCredentials(strings.NewReader(lf), "Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCredentials(strings.NewReader(crlf), "Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("CRLF cred count %d != LF %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CRLF cred[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// A bare "Login:\r\n" with no value must NOT set username to "\r".
	bare := "URL: https://b.example.com/\r\nLogin:\r\nPASS: pw\r\n"
	creds, err := ParseCredentials(strings.NewReader(bare), "Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range creds {
		if c.Username == "\r" || c.Password == "\r" || strings.ContainsRune(c.Username, '\r') {
			t.Fatalf("CRLF bare-value line leaked \\r into a cred: %+v", c)
		}
	}
}

// TestParseCredentialsLabeledBeatsFallback proves a mixed file (has labeled
// blocks AND stray colon lines) is parsed via the labeled path only — the
// fallback is gated on "labeled pass found nothing", so a file with any labeled
// creds never falls back to colon-line mode (no mixed-mode parsing).
func TestParseCredentialsLabeledBeatsFallback(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		"URL: https://a.example.com/",
		"USER: bob",
		"PASS: pw1",
		"",
		"https://stray.example.com:noise:shouldbeignored",
	}, "\n"))
	creds, err := ParseCredentials(body, "Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("expected 1 labeled cred (no fallback), got %d: %+v", len(creds), creds)
	}
}

// TestClassifyFieldAliases pins every alias in the alias maps to its expected
// class and guards the maps against the allAliases slice drifting out of sync.
// Add an alias => add it here too; the test fails until both agree.
func TestClassifyFieldAliases(t *testing.T) {
	// Every allAliases entry must classify to its listed class.
	for _, a := range allAliases {
		if got := classifyField(a.alias); got != a.class {
			t.Errorf("classifyField(%q) = %q, want %q", a.alias, got, a.class)
		}
	}
	// Every map entry must appear in allAliases (no orphan aliases).
	want := make(map[string]string, len(allAliases))
	for _, a := range allAliases {
		want[a.alias] = a.class
	}
	for key := range urlAlias {
		if _, ok := want[key]; !ok {
			t.Errorf("urlAlias has %q but allAliases does not", key)
		}
	}
	for key := range userAlias {
		if _, ok := want[key]; !ok {
			t.Errorf("userAlias has %q but allAliases does not", key)
		}
	}
	for key := range passAlias {
		if _, ok := want[key]; !ok {
			t.Errorf("passAlias has %q but allAliases does not", key)
		}
	}
	// A few representative new aliases must now parse as credentials in a block.
	newBlock := strings.NewReader(strings.Join([]string{
		"Email: alice@example.com",
		"Passwd: p@ss",
		"Website: https://new.example.com/login",
		"",
	}, "\n"))
	creds, err := ParseCredentials(newBlock, "Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].Username != "alice@example.com" ||
		creds[0].Password != "p@ss" || creds[0].URL != "https://new.example.com/login" {
		t.Fatalf("new-alias block mis-parsed: %+v", creds)
	}
}
