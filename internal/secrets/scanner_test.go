//go:build secrets

package secrets

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestScannerFindsAWSKey(t *testing.T) {
	sc, err := NewPool(2, RuleFilter{})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer sc.Close()
	fs, err := sc.Scan(context.Background(),
		[]byte("aws_access_key_id = AKIAIOSFODNN7EXAMPLE\naws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"), "log!aws.txt")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(fs) == 0 {
		t.Fatal("expected at least one finding")
	}
	if fs[0].SourcePath != "log!aws.txt" || fs[0].RuleID == "" || fs[0].Secret == "" {
		t.Fatalf("unexpected finding: %+v", fs[0])
	}
}

// Concurrent Scan calls must not race on Titus scratch memory.
func TestScannerPoolConcurrent(t *testing.T) {
	sc, err := NewPool(4, RuleFilter{})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer sc.Close()
	content := []byte(strings.Repeat("token: ghp_1234567890abcdefghijklmnopqrstuvwx12\n", 3))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sc.Scan(context.Background(), content, "p"); err != nil {
				t.Errorf("Scan: %v", err)
			}
		}()
	}
	wg.Wait()
}

// A non-empty RuleFilter is applied at pool build: only surviving rule IDs can
// produce findings. Allow np.aws.* restricts to AWS rules; deny np.aws.1 then
// drops the AWS API Key rule specifically.
func TestScannerPoolFilterDropsRules(t *testing.T) {
	sc, err := NewPool(2, RuleFilter{Allow: []string{"np.aws.*"}, Deny: []string{"np.aws.1"}})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer sc.Close()
	// GitHub PAT (np.github.*) + AWS key (np.aws.1) + AWS secret (np.aws.2).
	content := []byte("ghp_1234567890abcdefghijklmnopqrstuvwx12\n" +
		"aws_access_key_id = AKIAIOSFODNN7EXAMPLE\n" +
		"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	fs, err := sc.Scan(context.Background(), content, "p")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range fs {
		if !strings.HasPrefix(f.RuleID, "np.aws.") {
			t.Fatalf("allow np.aws.* leaked non-aws finding %q", f.RuleID)
		}
		if f.RuleID == "np.aws.1" {
			t.Fatalf("deny np.aws.1 leaked: %q", f.RuleID)
		}
	}
}

// A filter that matches no rules fails at pool build with a clear error.
func TestScannerPoolFilterMatchesNoRules(t *testing.T) {
	if _, err := NewPool(1, RuleFilter{Allow: []string{"no.such.*"}}); err == nil {
		t.Fatal("NewPool should error when filter matches no rules")
	}
}
