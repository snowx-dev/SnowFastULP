package secrets

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "s.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestStoreDedupsWithinRun(t *testing.T) {
	s := openTemp(t)
	f := Finding{RuleID: "np.aws.1", RuleName: "AWS Key", Secret: "AKIA...", SourcePath: "a!b"}
	s.Add(f)
	s.Add(f) // exact repeat this run
	st, err := s.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st.New != 1 || st.DupInRun != 1 {
		t.Fatalf("got New=%d DupInRun=%d, want 1/1", st.New, st.DupInRun)
	}
}

func TestStoreAccumulatesAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sqlite")
	f := Finding{RuleID: "np.aws.1", RuleName: "AWS Key", Secret: "AKIA...", SourcePath: "a!b"}

	s1, _ := Open(path)
	s1.Add(f)
	st1, _ := s1.Close()
	if st1.New != 1 {
		t.Fatalf("run1 New=%d, want 1", st1.New)
	}

	s2, _ := Open(path)
	s2.Add(f) // already in the DB from run 1
	st2, _ := s2.Close()
	if st2.New != 0 || st2.Existing != 1 {
		t.Fatalf("run2 New=%d Existing=%d, want 0/1", st2.New, st2.Existing)
	}
}

// A volume spanning several batches (plus a partial tail) must persist every
// unique finding exactly once — guards the transaction-batching writer.
func TestStorePersistsAcrossBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const n = secretsBatchSize*2 + 137 // two full batches + a partial flush
	for i := 0; i < n; i++ {
		s.Add(Finding{RuleID: "np.r", RuleName: "R", Secret: fmt.Sprintf("secret-%d", i), SourcePath: "p"})
	}
	st, err := s.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st.New != int64(n) {
		t.Fatalf("New = %d, want %d", st.New, n)
	}
	got := collect(t, path, QueryOpts{})
	if len(got) != n {
		t.Fatalf("query returned %d rows, want %d", len(got), n)
	}
}

func TestCapSecret(t *testing.T) {
	if got := capSecret("short"); got != "short" {
		t.Fatalf("short value should pass through, got %q", got)
	}
	long := strings.Repeat("A", maxSecretLen+500)
	if got := capSecret(long); len(got) != maxSecretLen {
		t.Fatalf("capped len = %d, want %d", len(got), maxSecretLen)
	}
	// A multi-byte rune straddling the cap must not be split (stays valid UTF-8).
	multibyte := strings.Repeat("A", maxSecretLen-1) + "é" + "tail"
	if got := capSecret(multibyte); !utf8.ValidString(got) {
		t.Fatalf("capSecret produced invalid UTF-8")
	}
}

func TestSanitizeSecret(t *testing.T) {
	// CR/LF/TAB collapse to spaces so a multi-line/tabbed match stays one record.
	got := sanitizeSecret("AKIA...\naws_secret = X\ty")
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("sanitizeSecret left a line/tab break in %q", got)
	}
	if got != "AKIA... aws_secret = X y" {
		t.Fatalf("unexpected sanitized value: %q", got)
	}
	// Length cap still applies after flattening.
	if got := sanitizeSecret(strings.Repeat("A", maxSecretLen+10)); len(got) != maxSecretLen {
		t.Fatalf("sanitizeSecret did not cap length: %d", len(got))
	}
}

// Distinct rules over the same string are kept apart (composite uniqueness).
func TestStoreKeepsSameSecretUnderDifferentRules(t *testing.T) {
	s := openTemp(t)
	s.Add(Finding{RuleID: "np.a.1", RuleName: "A", Secret: "x", SourcePath: "p"})
	s.Add(Finding{RuleID: "np.b.1", RuleName: "B", Secret: "x", SourcePath: "p"})
	st, err := s.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st.New != 2 {
		t.Fatalf("got New=%d, want 2", st.New)
	}
}
