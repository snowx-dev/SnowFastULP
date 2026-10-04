package sflog

import (
	"strings"
	"testing"
)

func TestDiagnosticCountsUseSelectedSFLLayoutAndSpill(t *testing.T) {
	const ambiguous = "https://example.com/login:alice:pw:tail"
	body := strings.Repeat(ambiguous+"\n", 1000)
	var emitted int
	var counts AmbiguityCounts
	_, err := ParseCredentialsStreamWithDiagnostics(strings.NewReader(body), "source", t.TempDir(), false, true, func(c AmbiguityCounts) {
		counts = c
	}, func(Credential) error {
		emitted++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if emitted != 1000 {
		t.Fatalf("emitted %d credentials, want 1000", emitted)
	}
	if counts != (AmbiguityCounts{Total: 1000, PathOrPassword: 1000}) {
		t.Fatalf("ambiguity counts = %+v, want 1000 path witnesses", counts)
	}
}

func TestDiagnosticCountsIgnoreDiscardedSFLLayout(t *testing.T) {
	body := "URL: https://example.com/labeled\nUsername: alice\nPassword: safe\n\n" +
		"https://example.com/login:alice:pw:tail\n"
	var counts AmbiguityCounts
	var emitted int
	mixed, err := ParseCredentialsStreamWithDiagnostics(strings.NewReader(body), "source", t.TempDir(), false, true, func(c AmbiguityCounts) {
		counts = c
	}, func(Credential) error {
		emitted++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !mixed || emitted != 1 {
		t.Fatalf("mixed=%v emitted=%d, want mixed labeled winner with one record", mixed, emitted)
	}
	if counts.Total != 0 {
		t.Fatalf("discarded colon candidate contributed ambiguity counts: %+v", counts)
	}
}
