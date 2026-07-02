package secrets

import (
	"path/filepath"
	"testing"
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
