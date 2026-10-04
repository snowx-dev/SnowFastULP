package history_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

func TestDeleteStagedRestoresSameSizeRewriteDetectedByRehash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = history.DeleteStaged(context.Background(), []string{path}, []history.Candidate{candidate}, func(staged []history.StagedPath) error {
		info, statErr := os.Stat(staged[0].Payload)
		if statErr != nil {
			return statErr
		}
		if writeErr := os.WriteFile(staged[0].Payload, []byte("rewritte"), 0o600); writeErr != nil {
			return writeErr
		}
		return os.Chtimes(staged[0].Payload, info.ModTime(), info.ModTime())
	})
	if err == nil {
		t.Fatal("DeleteStaged accepted rewritten staged content")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("staged source was not restored: %v", readErr)
	}
	if string(got) != "rewritte" {
		t.Fatalf("restored content = %q", got)
	}
}

func TestDeleteStagedPreservesQuarantineWhenRestoreWouldOverwriteReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var quarantine string
	_, err = history.DeleteStaged(context.Background(), []string{path}, []history.Candidate{candidate}, func(staged []history.StagedPath) error {
		quarantine = staged[0].Payload
		if writeErr := os.WriteFile(quarantine, []byte("rewritte"), 0o600); writeErr != nil {
			return writeErr
		}
		return os.WriteFile(path, []byte("replacement"), 0o600)
	})
	if err == nil || !strings.Contains(err.Error(), "staged data preserved") {
		t.Fatalf("error = %v, want clear preserved-data error", err)
	}
	if _, statErr := os.Stat(quarantine); statErr != nil {
		t.Fatalf("quarantine was not preserved: %v", statErr)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != "replacement" {
		t.Fatalf("replacement = %q, %v", got, readErr)
	}
}

func TestDeleteStagedMultipartKeepsReplacementVolume(t *testing.T) {
	parts := multipartFixture(t, t.TempDir())
	candidate, err := history.FingerprintMultipart(context.Background(), "archive", parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := history.DeleteStaged(context.Background(), parts, []history.Candidate{candidate}, func([]history.StagedPath) error {
		return os.WriteFile(parts[0], []byte("replacement"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %#v, want both original volumes", removed)
	}
	got, err := os.ReadFile(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "replacement" {
		t.Fatalf("replacement volume = %q", got)
	}
	if _, err := os.Stat(parts[1]); !os.IsNotExist(err) {
		t.Fatalf("processed second volume still exists: %v", err)
	}
}

func TestDeleteStagedKeepsReplacementCreatedAtOriginalPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("processed"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}

	removed, err := history.DeleteStaged(context.Background(), []string{path}, []history.Candidate{candidate}, func([]history.StagedPath) error {
		return os.WriteFile(path, []byte("replacement"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != path {
		t.Fatalf("removed = %#v, want original path", removed)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "replacement" {
		t.Fatalf("replacement content = %q", got)
	}
}

func TestDeleteStagedDoesNotDeleteReplacedPrivateContainer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(path, []byte("processed"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}

	var container, preservedContainer, payloadBase string
	_, err = history.DeleteStaged(context.Background(), []string{path}, []history.Candidate{candidate}, func(staged []history.StagedPath) error {
		container = staged[0].Container
		payloadBase = filepath.Base(staged[0].Payload)
		preservedContainer = container + ".preserved"
		if renameErr := os.Rename(container, preservedContainer); renameErr != nil {
			return renameErr
		}
		if mkdirErr := os.Mkdir(container, 0o700); mkdirErr != nil {
			return mkdirErr
		}
		return os.WriteFile(filepath.Join(container, payloadBase), []byte("processed"), 0o600)
	})
	if err == nil || !strings.Contains(err.Error(), "container was replaced") {
		t.Fatalf("error = %v, want replaced-container error", err)
	}
	got, readErr := os.ReadFile(filepath.Join(container, payloadBase))
	if readErr != nil || string(got) != "processed" {
		t.Fatalf("replacement payload = %q, %v; replacement container was deleted", got, readErr)
	}
	got, readErr = os.ReadFile(filepath.Join(preservedContainer, payloadBase))
	if readErr != nil || string(got) != "processed" {
		t.Fatalf("staged payload = %q, %v; original staged data was not preserved", got, readErr)
	}
}
