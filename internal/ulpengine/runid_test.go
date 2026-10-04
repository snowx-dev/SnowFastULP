package ulpengine

import (
	"strings"
	"testing"
)

// run id length + crockford alphabet only, opsec guarantee
func TestNewRunIDShape(t *testing.T) {
	const N = 256
	seen := make(map[string]struct{}, N)
	for i := 0; i < N; i++ {
		id, err := NewRunID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != runIDLen {
			t.Fatalf("id %q length = %d, want %d", id, len(id), runIDLen)
		}
		for _, c := range id {
			if !strings.ContainsRune(crockfordAlphabet, c) {
				t.Fatalf("id %q contains non-alphabet char %q", id, c)
			}
		}
		seen[id] = struct{}{}
	}
	// 256 draws from 30-bit space, virtually certain to be unique
	if len(seen) < N-1 {
		t.Fatalf("got %d unique ids of %d, entropy degraded", len(seen), N)
	}
}
