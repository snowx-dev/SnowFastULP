package history

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenameNoReplaceNeverOverwritesLateReplacement(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "payload")
	target := filepath.Join(dir, "source")
	if err := os.WriteFile(staged, []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := renameNoReplace(staged, target); err == nil {
		t.Fatal("renameNoReplace overwrote an existing replacement")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "replacement" {
		t.Fatalf("replacement = %q, %v", got, err)
	}
	if got, err := os.ReadFile(staged); err != nil || string(got) != "staged" {
		t.Fatalf("staged payload = %q, %v", got, err)
	}
}
