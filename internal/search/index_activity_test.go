package search_test

import (
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestIndexFocusNameFrameScan(t *testing.T) {
	m := &search.Metrics{}
	m.BeginFrameScan("monster.zst")
	if got := m.IndexFocusName(); got != "monster.zst" {
		t.Fatalf("focus = %q, want monster.zst", got)
	}
	m.EndFrameScan("monster.zst")
	if got := m.IndexFocusName(); got != "" {
		t.Fatalf("focus after end = %q, want empty", got)
	}
}
