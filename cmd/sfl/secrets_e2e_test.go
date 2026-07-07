//go:build secrets

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/secrets"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

// TestSecretsEndToEnd drives the whole pipeline: a real Engine extracts a zip
// (credential member + a config carrying secrets) with the CLI's own sink wired
// in, and we assert both that credentials are emitted and that secrets land in
// the SQLite store, then that a second run accumulates instead of re-adding.
func TestSecretsEndToEnd(t *testing.T) {
	in := t.TempDir()
	buildZip(t, filepath.Join(in, "log.zip"), map[string]string{
		"Passwords.txt": "URL: https://site\nUSER: alice\nPASS: hunter2\n",
		"config.env": "aws_access_key_id = AKIAIOSFODNN7EXAMPLE\n" +
			"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n" +
			"token: ghp_1234567890abcdefghijklmnopqrstuvwx12\n",
	})
	dbPath := filepath.Join(t.TempDir(), "secrets.sqlite")

	run := func() (sflog.ExtractStats, secrets.Stats) {
		t.Helper()
		sink, closeFn, err := buildSecretSink(dbPath, 2, secrets.RuleFilter{})
		if err != nil {
			t.Fatalf("buildSecretSink: %v", err)
		}
		e := &sflog.Engine{
			Workers:      2,
			Passwords:    []string{""},
			SecretSink:   sink,
			SecretMaxLen: 1 << 20,
		}
		var buf bytes.Buffer
		stats, _, rerr := e.Run(context.Background(), in, &buf)
		if rerr != nil {
			t.Fatalf("Run: %v", rerr)
		}
		sst, cerr := closeFn()
		if cerr != nil {
			t.Fatalf("close secrets: %v", cerr)
		}
		return stats, sst
	}

	stats, sst := run()
	if stats.Emitted < 1 {
		t.Fatalf("expected credentials emitted, got %d", stats.Emitted)
	}
	if sst.New < 2 {
		t.Fatalf("expected >=2 new secrets (AWS + GitHub), got %d", sst.New)
	}

	_, sst2 := run()
	if sst2.New != 0 || sst2.Existing < 2 {
		t.Fatalf("second run should accumulate: new=%d existing=%d", sst2.New, sst2.Existing)
	}
}

// TestSecretsDedupSkipsIdenticalMembers proves the content-hash dedup: a zip
// whose members carry byte-identical secret content is scanned once, the rest
// skipped, yet the secrets still land in the store (from the one scan) exactly
// as if every copy had been scanned.
func TestSecretsDedupSkipsIdenticalMembers(t *testing.T) {
	in := t.TempDir()
	const secretBody = "aws_access_key_id = AKIAIOSFODNN7EXAMPLE\n" +
		"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n" +
		"token: ghp_1234567890abcdefghijklmnopqrstuvwx12\n"
	files := map[string]string{"Passwords.txt": "URL: https://s\nUSER: u\nPASS: p\n"}
	const copies = 6
	for i := 0; i < copies; i++ {
		// Distinct member names, identical content -> same digest -> one scan.
		files[filepath.Join("victim"+string(rune('A'+i)), "creds.env")] = secretBody
	}
	buildZip(t, filepath.Join(in, "dupes.zip"), files)

	sink, closeFn, err := buildSecretSink(filepath.Join(t.TempDir(), "s.sqlite"), 4, secrets.RuleFilter{})
	if err != nil {
		t.Fatalf("buildSecretSink: %v", err)
	}
	e := &sflog.Engine{Workers: 4, Passwords: []string{""}, SecretSink: sink, SecretMaxLen: 1 << 20}
	var buf bytes.Buffer
	if _, _, rerr := e.Run(context.Background(), in, &buf); rerr != nil {
		t.Fatalf("Run: %v", rerr)
	}
	sst, cerr := closeFn()
	if cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	// copies-1 of the identical .env members must be skipped as duplicate content.
	if sst.Deduped < copies-1 {
		t.Fatalf("expected >=%d deduped, got %d", copies-1, sst.Deduped)
	}
	// The secrets still made it in from the single scan of that content.
	if sst.New < 2 {
		t.Fatalf("expected >=2 new secrets from the one scanned copy, got %d", sst.New)
	}
}

func buildZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}
