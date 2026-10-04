package fdlimit_test

import (
	"runtime"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/fdlimit"
)

func TestMaxOpenFiles(t *testing.T) {
	v, ok := fdlimit.MaxOpenFiles()
	if runtime.GOOS == "windows" {
		if ok {
			t.Fatalf("expected ok=false on windows, got %d", v)
		}
		return
	}
	if !ok {
		t.Skip("platform reports no fd accounting")
	}
	if v <= 0 {
		t.Fatalf("expected positive limit, got %d", v)
	}
}
