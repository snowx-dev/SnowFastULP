package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/secrets"
)

func TestOfferSecretsExportDefaultNo(t *testing.T) {
	dir := t.TempDir()
	matches := []secrets.Match{{Secret: "AKIA1"}, {Secret: "AKIA2"}}
	var out bytes.Buffer
	path, err := offerSecretsExport(matches, dir, strings.NewReader("\n"), &out)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if path != "" {
		t.Fatalf("declined reply should not write, got %q", path)
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Fatalf("expected skipped note, got %q", out.String())
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "sfs_secrets_*.txt")); len(files) != 0 {
		t.Fatalf("no file should be created on default-no, got %v", files)
	}
}

func TestOfferSecretsExportYes(t *testing.T) {
	dir := t.TempDir()
	matches := []secrets.Match{
		{Secret: "AKIA1"},
		{Secret: "AKIA1"}, // dup
		{Secret: "ghp_token"},
		{Secret: ""}, // empty dropped
	}
	var out bytes.Buffer
	path, err := offerSecretsExport(matches, dir, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if path == "" {
		t.Fatal("expected a path for yes reply")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 unique secrets, got %d: %q", len(lines), body)
	}
	if !strings.Contains(string(body), "AKIA1") || !strings.Contains(string(body), "ghp_token") {
		t.Fatalf("export missing secrets: %q", body)
	}
	if !strings.Contains(out.String(), "exported 2 secrets") {
		t.Fatalf("expected exported summary, got %q", out.String())
	}
}

func TestOfferSecretsExportEmptyMatchesNoop(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	path, err := offerSecretsExport(nil, dir, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if path != "" {
		t.Fatalf("no matches should not write, got %q", path)
	}
	if out.Len() != 0 {
		t.Fatalf("no prompt expected for zero matches, got %q", out.String())
	}
}
