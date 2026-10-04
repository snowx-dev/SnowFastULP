package sflog

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A corrupt PLAIN 7z fails our own member-CRC verifier (bodgit verifies only
// header digests), which used to be classified like a password rejection: the
// sweep then fully attempted every remaining candidate. A structural,
// password-independent checksum failure must stop the sweep at the first
// candidate.
func TestCorruptPlainSevenZipStopsSweepingCandidates(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found")
	}
	dir := t.TempDir()
	mustWrite(t, dir, "Passwords.txt",
		"URL: https://corrupt.example.com/login\nUSER: analyst\nPASS: secret\n")
	cmd := exec.Command(bin, "a", "-t7z", "-mx=0", "-bd", "plain.7z", "Passwords.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("7z create failed: %v\n%s", err, out)
	}
	path := filepath.Join(dir, "plain.7z")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip the first packed byte: the start header is 24 bytes (signature,
	// version, header CRC, then the 12-byte start header), and the packed
	// data begins at 24 + NextHeaderOffset. Copy-mode (-mx=0) members decode
	// whatever bytes they hold, so decoding still succeeds and only the
	// member CRC — checked by our own verifier — fails.
	packedBase := 24 + int(binary.LittleEndian.Uint32(data[12:16]))
	if packedBase >= len(data) {
		t.Fatalf("packed data offset %d beyond %d-byte archive", packedBase, len(data))
	}
	data[packedBase] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	incorrect := 0
	ec := extractCtx{
		passwords: []string{"", "wrong1", "wrong2"},
		display:   path,
		emit:      func(Credential) {},
		onIssue:   func(string, IssueKind, error) {},
		debug: func(format string, args ...any) {
			if strings.Contains(fmt.Sprintf(format, args...), "incorrect") {
				incorrect++
			}
		},
	}
	_, err = readArchiveCredentials(context.Background(), path, ec, 1<<20)
	if !errors.Is(err, errSevenZipMemberCRC) {
		t.Fatalf("readArchiveCredentials error = %v, want the structural member CRC failure", err)
	}
	if incorrect != 0 {
		t.Fatalf("sweep logged %d candidate rejection(s) for a plain structural CRC failure; want 0", incorrect)
	}
}
