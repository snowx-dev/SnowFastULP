package sflog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bodgit/sevenzip"
)

// mustWrite is a small local test helper.
func mustWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedSevenZipNonCredentialMemberRejectsWrongPassword(t *testing.T) {
	path := filepath.Join("testdata", "7z-encrypted-info.7z")
	ec := extractCtx{
		passwords: []string{"", "wrong"},
		display:   path,
		emit:      func(Credential) {},
		onIssue:   func(string, IssueKind, error) {},
	}

	if _, err := readArchiveCredentials(context.Background(), path, ec, 1<<20); !errors.Is(err, errPasswordNotFound) {
		t.Fatalf("readArchiveCredentials error = %v, want password-not-found", err)
	}
}

func TestEncryptedSevenZipCopyModeRetriesPasswordCandidates(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found")
	}
	dir := t.TempDir()
	mustWrite(t, dir, "Passwords.txt",
		"URL: https://retry.example.com/login\nUSER: analyst\nPASS: secret\n")
	cmd := exec.Command(bin, "a", "-t7z", "-mx=0", "-psecret", "-mhe=off", "-bd",
		"encrypted.7z", "Passwords.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("7z create failed: %v\n%s", err, out)
	}

	var got []Credential
	ec := extractCtx{
		passwords: []string{"", "wrong", "secret"},
		display:   filepath.Join(dir, "encrypted.7z"),
		emit:      func(c Credential) { got = append(got, c) },
		onIssue:   func(string, IssueKind, error) {},
	}
	if _, err := readArchiveCredentials(context.Background(), ec.display, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if len(got) != 1 || got[0].Password != "secret" {
		t.Fatalf("credentials = %+v, want one credential with secret password", got)
	}
}

func TestPickSevenZipValidationFileTakesEarliestStreamPosition(t *testing.T) {
	early := &sevenzip.File{FileHeader: sevenzip.FileHeader{Name: "a-first.txt", UncompressedSize: 1 << 20}}
	late := &sevenzip.File{FileHeader: sevenzip.FileHeader{Name: "z-late-tiny.txt", UncompressedSize: 10}}
	// zr.File is in stream order; a later, smaller member must NOT win —
	// reaching it in a solid archive can cost a near-full decode.
	if got := pickSevenZipValidationFile([]*sevenzip.File{early, late}); got != early {
		t.Fatalf("validation file = %v, want the earliest qualifying member", got)
	}
	large := &sevenzip.File{FileHeader: sevenzip.FileHeader{Name: "large.txt", UncompressedSize: maxParseBuffer + 1}}
	if got := pickSevenZipValidationFile([]*sevenzip.File{large, early, late}); got != early {
		t.Fatalf("validation file = %v, want first qualifying member past an oversized head", got)
	}
	empty := &sevenzip.File{FileHeader: sevenzip.FileHeader{Name: "empty.txt", UncompressedSize: 0}}
	if got := pickSevenZipValidationFile([]*sevenzip.File{empty, early}); got != early {
		t.Fatalf("validation file = %v, want first qualifying member past an empty head", got)
	}
	if got := pickSevenZipValidationFile([]*sevenzip.File{large}); got != nil {
		t.Fatalf("validation file = %v, want nil for oversized-only archive", got)
	}
	if got := pickSevenZipValidationFile(nil); got != nil {
		t.Fatalf("validation file = %v, want nil for no files", got)
	}
}

func TestStartValidationHeartbeatEmitsAndStops(t *testing.T) {
	oldInterval := validationHeartbeatInterval
	validationHeartbeatInterval = 20 * time.Millisecond
	defer func() { validationHeartbeatInterval = oldInterval }()

	var mu sync.Mutex
	var lines []string
	ec := extractCtx{
		display: "big.7z",
		debug: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		},
	}
	stop := ec.startValidationHeartbeat(context.Background(), "a-first.txt")
	time.Sleep(90 * time.Millisecond)
	stop()
	mu.Lock()
	emitted := len(lines)
	mu.Unlock()
	if emitted == 0 {
		t.Fatal("no heartbeat lines while validation ran")
	}
	for _, ln := range lines {
		if !strings.Contains(ln, "still validating") || !strings.Contains(ln, "a-first.txt") || !strings.Contains(ln, "big.7z") {
			t.Fatalf("heartbeat line %q missing expected parts", ln)
		}
	}
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	afterStop := len(lines)
	mu.Unlock()
	if afterStop != emitted {
		t.Fatalf("heartbeat kept emitting after stop: %d -> %d", emitted, afterStop)
	}
	// Nil sink and double-stop must be safe no-ops.
	neut := (extractCtx{}).startValidationHeartbeat(context.Background(), "x")
	neut()
	neut()
	stop()
	stop()
}

func TestStartValidationHeartbeatStopsOnCtxCancel(t *testing.T) {
	oldInterval := validationHeartbeatInterval
	validationHeartbeatInterval = 20 * time.Millisecond
	defer func() { validationHeartbeatInterval = oldInterval }()

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	n := 0
	ec := extractCtx{
		display: "big.7z",
		debug:   func(format string, args ...any) { mu.Lock(); defer mu.Unlock(); n++ },
	}
	stop := ec.startValidationHeartbeat(ctx, "a-first.txt")
	time.Sleep(90 * time.Millisecond)
	cancel()
	stop() // must not double-close panic
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n == 0 {
		t.Fatal("no heartbeat lines before cancel")
	}
}

func TestEncryptedSevenZipRetryLeavesPlainCRCStructural(t *testing.T) {
	if isWrongPassword(fmt.Errorf("%w: member", errSevenZipMemberCRC)) {
		t.Fatal("plain member CRC mismatch must not be classified as a password error")
	}
}
