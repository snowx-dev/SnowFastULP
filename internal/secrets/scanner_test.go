package secrets

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestScannerFindsAWSKey(t *testing.T) {
	sc, err := NewPool(2)
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
	sc, err := NewPool(4)
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
