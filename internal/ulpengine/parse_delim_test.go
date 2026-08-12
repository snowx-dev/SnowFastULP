package ulpengine

import (
	"strings"
	"testing"
)

func TestNewDelimParserValidation(t *testing.T) {
	if _, err := NewDelimParser(""); err == nil {
		t.Fatal("empty delimiter must error")
	}
	for _, bad := range []string{":", "/", "a:b", "x/y", "|\n"} {
		if _, err := NewDelimParser(bad); err == nil {
			t.Fatalf("delimiter %q must error", bad)
		}
	}
	if _, err := NewDelimParser("|"); err != nil {
		t.Fatalf("valid delimiter errored: %v", err)
	}
}

func TestDelimParserExactThreeFields(t *testing.T) {
	p, err := NewDelimParser("|")
	if err != nil {
		t.Fatal(err)
	}
	host, url, login, password, ok := p.Parse("https://example.com/login|user@example.com|s3cret")
	if !ok {
		t.Fatal("valid 3-field line rejected")
	}
	if host != "example.com" || url != "https://example.com/login" || login != "user@example.com" || password != "s3cret" {
		t.Fatalf("got host=%q url=%q login=%q password=%q", host, url, login, password)
	}
}

func TestDelimParserRejectsWrongFieldCounts(t *testing.T) {
	p, _ := NewDelimParser("|")
	for _, line := range []string{
		"example.com|user",          // 2 fields
		"example.com|u|p|extra",     // 4 fields
		"example.com|u|p|with|pipe", // password contains sep -> reject
		"",                          // empty
	} {
		if _, _, _, _, ok := p.Parse(line); ok {
			t.Fatalf("line %q accepted, want reject", line)
		}
	}
}

func TestDelimParserRejectsEmptyFields(t *testing.T) {
	p, _ := NewDelimParser("|")
	for _, line := range []string{
		"example.com||pw",
		"example.com|user|",
		"https://example.com||pw",
		"|user|pw", // empty url/host also fails finishParse host-dot, but guard early
	} {
		if _, _, _, _, ok := p.Parse(line); ok {
			t.Fatalf("line %q accepted, want reject", line)
		}
	}
}

func TestDelimParserEmptyFieldsDoNotRoundTripPoison(t *testing.T) {
	// Defense-in-depth: even if Parse ever returned ok, FormatRecordStable
	// must not be fed empty login/password from DelimParser. Documented
	// regression from review: example.com::pw parses false via parseUnion.
	p, _ := NewDelimParser("|")
	line := "example.com||pw"
	if _, _, _, _, ok := p.Parse(line); ok {
		t.Fatal("empty login must reject before FormatRecordStable")
	}
	h, _, l, pw, ok := parseUnion("example.com::pw")
	if ok {
		t.Fatalf("sanity: parseUnion must reject empty-field ULP, got %q/%q/%q", h, l, pw)
	}
}

func TestDelimParserDedupKeyParity(t *testing.T) {
	// A custom-delimited line and its colon-ULP twin must dedup identically.
	p, _ := NewDelimParser("|")
	custom := "www.example.com/path|user|pw"
	builtin := "www.example.com/path:user:pw"
	ka, okA := DedupKeyWith(p, custom)
	kb, okB := DedupKeyForLine(builtin, false)
	if !okA || !okB {
		t.Fatalf("okA=%v okB=%v", okA, okB)
	}
	if ka != kb {
		t.Fatalf("dedup keys differ: custom=%#x builtin=%#x", ka, kb)
	}
}

func TestDelimParserHygieneStillApplies(t *testing.T) {
	p, _ := NewDelimParser(";")
	// >64 char password must reject via finishParse
	long := "example.com;user;" + strings.Repeat("x", 65)
	if _, _, _, _, ok := p.Parse(long); ok {
		t.Fatal("over-long password accepted")
	}
	// host without a dot must reject (finishParse)
	if _, _, _, _, ok := p.Parse("localhost;user;pw"); ok {
		t.Fatal("host without dot accepted")
	}
}

func TestParseWithNilIsStrictBuiltin(t *testing.T) {
	// nil parser = strict builtin (zero-value safety for tests)
	if _, _, _, _, ok := ParseWith(nil, "example.com:u:p"); !ok {
		t.Fatal("strict line rejected with nil parser")
	}
	if _, _, _, _, ok := ParseWith(nil, "203.0.113.5:8080:admin:p"); ok {
		t.Fatal("loose-only line accepted with nil (strict) parser")
	}
}
