package history_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintFileIsPathIndependent(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	writeFixture(t, first, "same bytes")
	writeFixture(t, second, "same bytes")

	a, err := history.FingerprintFile(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.FingerprintFile(context.Background(), second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("identical bytes produced different identities: %+v != %+v", a.ID, b.ID)
	}
	if a.Paths[0] != filepath.Clean(first) || !filepath.IsAbs(a.Paths[0]) {
		t.Fatalf("path = %q, want absolute cleaned %q", a.Paths[0], first)
	}
}

func TestFingerprintFileDifferentBytesSameSize(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	writeFixture(t, first, "abc")
	writeFixture(t, second, "xyz")

	a, err := history.FingerprintFile(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.FingerprintFile(context.Background(), second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID.Size != b.ID.Size || a.ID.Hash == b.ID.Hash {
		t.Fatalf("identities = %+v, %+v; want equal size and different hashes", a.ID, b.ID)
	}
}

func TestFingerprintFileEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	writeFixture(t, path, "")
	candidate, err := history.FingerprintFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.ID.Size != 0 {
		t.Fatalf("size = %d, want 0", candidate.ID.Size)
	}
	if len(candidate.Snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(candidate.Snapshots))
	}
}

func TestFingerprintFileCanceled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.txt")
	writeFixture(t, path, strings.Repeat("x", 1024))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := history.FingerprintFile(ctx, path, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestValidateDetectsChangedOrReplacedFile(t *testing.T) {
	for _, name := range []string{"changed", "replaced"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.txt")
			writeFixture(t, path, "original")
			candidate, err := history.FingerprintFile(context.Background(), path, nil)
			if err != nil {
				t.Fatal(err)
			}

			if name == "changed" {
				writeFixture(t, path, "changed-size")
			} else {
				replacement := filepath.Join(filepath.Dir(path), "replacement.txt")
				writeFixture(t, replacement, "original")
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := history.Validate(candidate); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("Validate() error = %v, want changed-source error containing path", err)
			}
		})
	}
}

func TestValidateRehashesSameSizeRewriteWithRestoredMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.txt")
	writeFixture(t, path, "original")
	candidate, err := history.FingerprintFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, "rewritte")
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := history.Validate(candidate); err == nil {
		t.Fatal("Validate accepted same-size rewritten content with restored mtime")
	}
}

func TestValidateAllContextCancelReturnsPromptly(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	writeFixture(t, first, "first")
	writeFixture(t, second, "second")
	a, err := history.FingerprintFile(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.FingerprintFile(context.Background(), second, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = history.ValidateAllContext(ctx, []history.Candidate{a, b}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateAllContext() error = %v, want context.Canceled", err)
	}
}

func TestValidateAllContextReportsProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	writeFixture(t, path, strings.Repeat("x", 2<<20))
	candidate, err := history.FingerprintFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sawPath string
	var sawDone, sawTotal int64
	if err := history.ValidateAllContext(context.Background(), []history.Candidate{candidate}, func(p string, done, total int64) {
		sawPath, sawDone, sawTotal = p, done, total
	}); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(sawPath) != "big.txt" || sawDone != 2<<20 || sawTotal != 2<<20 {
		t.Fatalf("progress = %q %d/%d, want big.txt %d/%d", sawPath, sawDone, sawTotal, 2<<20, 2<<20)
	}
}

func TestValidateAllReportsFirstChangedSource(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	writeFixture(t, first, "first")
	writeFixture(t, second, "second")
	a, err := history.FingerprintFile(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.FingerprintFile(context.Background(), second, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, first, "changed first")
	writeFixture(t, second, "changed second")

	err = history.ValidateAllContext(context.Background(), []history.Candidate{a, b}, nil)
	if err == nil || !strings.Contains(err.Error(), first) {
		t.Fatalf("ValidateAllContext() error = %v, want first path %q", err, first)
	}
	if strings.Contains(err.Error(), second) {
		t.Fatalf("ValidateAllContext() error = %v, unexpectedly reports second path", err)
	}
}

func multipartFixture(t *testing.T, dir string) []string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(dir, "archive.001"), filepath.Join(dir, "archive.002")}
	writeFixture(t, paths[0], "first part")
	writeFixture(t, paths[1], "second part")
	return paths
}

func TestFingerprintMultipartIsPathIndependent(t *testing.T) {
	partsA := multipartFixture(t, filepath.Join(t.TempDir(), "a"))
	partsB := multipartFixture(t, filepath.Join(t.TempDir(), "b"))
	a, err := history.FingerprintMultipart(context.Background(), "archive", partsA, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.FingerprintMultipart(context.Background(), "archive", partsB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("same logical multipart identity differs: %+v != %+v", a.ID, b.ID)
	}
}

func TestFingerprintMultipartOrderAndAssemblyAffectIdentity(t *testing.T) {
	parts := multipartFixture(t, t.TempDir())
	base, err := history.FingerprintMultipart(context.Background(), "archive", parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := history.FingerprintMultipart(context.Background(), "archive", []string{parts[1], parts[0]}, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherAssembly, err := history.FingerprintMultipart(context.Background(), "other", parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base.ID == reversed.ID {
		t.Fatal("reversing multipart parts did not change identity")
	}
	if base.ID == otherAssembly.ID {
		t.Fatal("changing assembly label did not change identity")
	}
}

func TestFingerprintMultipartPartChangesIdentity(t *testing.T) {
	parts := multipartFixture(t, t.TempDir())
	before, err := history.FingerprintMultipart(context.Background(), "archive", parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, parts[1], "changed raw")
	after, err := history.FingerprintMultipart(context.Background(), "archive", parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.ID == after.ID {
		t.Fatal("changing one part did not change identity")
	}
}

func TestFingerprintMultipartSizeSnapshotsAndValidation(t *testing.T) {
	parts := multipartFixture(t, t.TempDir())
	candidate, err := history.FingerprintMultipart(context.Background(), "archive", parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantSize := int64(len("first part") + len("second part"))
	if candidate.ID.Size != wantSize {
		t.Fatalf("size = %d, want %d", candidate.ID.Size, wantSize)
	}
	if len(candidate.Paths) != 2 || len(candidate.Snapshots) != 2 {
		t.Fatalf("paths/snapshots = %d/%d, want 2/2", len(candidate.Paths), len(candidate.Snapshots))
	}
	writeFixture(t, parts[1], "changed second part")
	if err := history.Validate(candidate); err == nil || !strings.Contains(err.Error(), parts[1]) {
		t.Fatalf("Validate() error = %v, want changed second part", err)
	}
}

func TestValidateMultipartRehashesSameSizeRewriteWithRestoredMtime(t *testing.T) {
	parts := multipartFixture(t, t.TempDir())
	candidate, err := history.FingerprintMultipart(context.Background(), "archive", parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, parts[1], "rewrit part")
	if err := os.Chtimes(parts[1], info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := history.Validate(candidate); err == nil {
		t.Fatal("Validate accepted same-size rewritten multipart content with restored mtime")
	}
}

func TestFingerprintMultipartRequiresTwoParts(t *testing.T) {
	parts := multipartFixture(t, t.TempDir())
	if _, err := history.FingerprintMultipart(context.Background(), "archive", parts[:1], nil); err == nil {
		t.Fatal("FingerprintMultipart accepted one part")
	}
}
