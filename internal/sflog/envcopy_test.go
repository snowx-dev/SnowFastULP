package sflog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsEnvCopyCandidate(t *testing.T) {
	positive := []string{
		".env", ".env.local", "foo.env", "id_rsa", "secrets.json",
		"credentials.json", "wallet.dat", "config.pem", "key.key",
		"my-apikeys.json", "appsettings.Development.json",
	}
	for _, p := range positive {
		if !isEnvCopyCandidate(p) {
			t.Errorf("%q should be an env copy candidate", p)
		}
	}
	negative := []string{
		"id_rsa.pub", "id_ed25519.pub", "config", "Passwords.txt",
		"readme.md", "package.json", "image.png", "app.py", "debug.log",
	}
	for _, p := range negative {
		if isEnvCopyCandidate(p) {
			t.Errorf("%q should not be an env copy candidate", p)
		}
	}
}

func TestIsLogContextFile(t *testing.T) {
	if !isLogContextFile("Information.txt") {
		t.Fatal("Information.txt should be context")
	}
	if isLogContextFile("Passwords.txt") {
		t.Fatal("Passwords.txt should not be context")
	}
}

func TestPathUnderAnchor(t *testing.T) {
	cases := []struct {
		path, anchor string
		want         bool
	}{
		{"secrets.json", ".", true},
		{"Information.txt", ".", true},
		{"VictimA/foo.ovpn", ".", false},
		{"VictimA/foo.ovpn", "VictimA", true},
		{"VictimA/deep/foo.ovpn", "VictimA", true},
		{"Logs_1 July/VE[abc]/Vpns/foo.ovpn", "Logs_1 July/VE[abc]", true},
		{"Logs_1 July/IN[def]/information.txt", "Logs_1 July/VE[abc]", false},
		{"Batch/Logs_1 July/VE[abc]/foo.ovpn", "Batch/Logs_1 July/VE[abc]", true},
	}
	for _, tc := range cases {
		if got := pathUnderAnchor(tc.path, tc.anchor); got != tc.want {
			t.Errorf("pathUnderAnchor(%q, %q) = %v, want %v", tc.path, tc.anchor, got, tc.want)
		}
	}
}

func TestMarkedContextAnchorsAndCopyAllowed(t *testing.T) {
	envMembers := []string{
		"VictimA/profile.ovpn",
		"Logs_1 July/VE[abc]/Vpns/tunnel.ovpn",
		"Batch/Logs_1 July/VE[abc]/Vpns/tunnel.ovpn",
	}
	contextMembers := []string{
		"VictimA/information.txt",
		"VictimB/information.txt",
		"Logs_1 July/VE[abc]/information.txt",
		"Logs_1 July/IN[def]/information.txt",
		"Batch/Logs_1 July/VE[abc]/information.txt",
		"Batch/Logs_1 July/IN[def]/information.txt",
		"VictimA/DungeonCheckerGmail/info.txt",
	}
	marked := markedContextAnchors(envMembers, contextMembers)

	allowed := map[string]bool{
		"VictimA/information.txt":                    true,
		"VictimB/information.txt":                    false,
		"Logs_1 July/VE[abc]/information.txt":        true,
		"Logs_1 July/IN[def]/information.txt":        false,
		"Batch/Logs_1 July/VE[abc]/information.txt":  true,
		"Batch/Logs_1 July/IN[def]/information.txt":  false,
		"VictimA/DungeonCheckerGmail/info.txt":       true,
	}
	for member, want := range allowed {
		if got := contextCopyAllowed(member, marked); got != want {
			t.Errorf("contextCopyAllowed(%q) = %v, want %v (marked=%v)", member, got, want, marked)
		}
	}

	rootMarked := markedContextAnchors([]string{"secrets.json"}, []string{
		"VictimA/information.txt", "VictimB/information.txt", "Information.txt",
	})
	rootAllowed := map[string]bool{
		"Information.txt":         true,
		"VictimA/information.txt": false,
		"VictimB/information.txt": false,
	}
	for member, want := range rootAllowed {
		if got := contextCopyAllowed(member, rootMarked); got != want {
			t.Errorf("root contextCopyAllowed(%q) = %v, want %v", member, got, want)
		}
	}
}

func TestContextAnchorDir(t *testing.T) {
	if got := contextAnchorDir("Logs_1 July/VE[abc]/information.txt"); got != "Logs_1 July/VE[abc]" {
		t.Fatalf("contextAnchorDir = %q", got)
	}
	if got := contextAnchorDir("Information.txt"); got != "." {
		t.Fatalf("root context anchor = %q, want .", got)
	}
}

func TestSafeRelPath(t *testing.T) {
	got := safeRelPath(`../evil/../../.env`)
	if got == ".." || got == "" {
		t.Fatalf("safeRelPath = %q, want sanitized path", got)
	}
	if got := memberRelDest("/run/media/disk/logs/VictimA/bundle.zip", `Users/dev/.env`); got != filepath.Join("Users", "dev", ".env") {
		t.Fatalf("memberRelDest top-level = %q, want Users/dev/.env", got)
	}
	if got := memberRelDest("outer.zip!inner.zip", `foo/.env`); got != filepath.Join("inner.zip", "foo", ".env") {
		t.Fatalf("memberRelDest nested = %q", got)
	}
}

func TestEnvVictimPrefix(t *testing.T) {
	cases := map[string]string{
		"VictimA/profile.ovpn":                                              "VictimA",
		"vidar_20260708_MZ_197.218.76.87/Files/Desktop/seed.txt":            "vidar_20260708_MZ_197.218.76.87",
		"Logs_1 July/VE[abc]/Vpns/tunnel.ovpn":                              filepath.Join("Logs_1 July", "VE[abc]"),
		"Batch/Logs_1 July/VE[abc]/Vpns/tunnel.ovpn":                        filepath.Join("Batch", "Logs_1 July", "VE[abc]"),
		"VictimA/information.txt":                                           "VictimA",
		"secrets.json":                                                      "",
		"VictimA/config.env":                                                "VictimA",
	}
	for path, want := range cases {
		if got := envVictimPrefix(path); got != want {
			t.Errorf("envVictimPrefix(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestFlushArchiveContextDefersUntilClose(t *testing.T) {
	root := filepath.Join(t.TempDir(), "env")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()

	pending := []envPending{{
		relDest: "VictimA/information.txt", data: []byte("pc\n"), memberName: "VictimA/information.txt",
	}}
	copier.FlushArchiveContext("/logs/mega.zip", []string{"VictimA/profile.ovpn"}, pending)

	if _, err := os.Stat(filepath.Join(root, "mega.zip", "VictimA", "information.txt")); !os.IsNotExist(err) {
		t.Fatal("context should not be written before Close")
	}

	// Simulate successful env write then close.
	copier.recordWrittenArchiveEnv("/logs/mega.zip", "VictimA/profile.ovpn")
	es := copier.Close()
	if es.ContextCopied != 1 {
		t.Fatalf("context = %d, want 1 after Close", es.ContextCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "mega.zip", "VictimA", "information.txt")); err != nil {
		t.Fatalf("victim context missing after Close: %v", err)
	}
}

func TestAppendPendingContextCap(t *testing.T) {
	state := &archiveEnvState{}
	copier := NewEnvCopier(t.TempDir(), nil, defaultEnvCopyMaxLen)
	p := envPending{data: []byte("x"), memberName: "a/information.txt"}
	if !appendPendingContext(state, p, copier) {
		t.Fatal("first append should succeed")
	}
	state.pendingContextBytes = maxPendingContextBytes
	if appendPendingContext(state, envPending{data: []byte("y")}, copier) {
		t.Fatal("should reject over byte cap")
	}
}

func TestEnqueueFileSkipsOverCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.env")
	if err := os.WriteFile(path, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	copier := NewEnvCopier(t.TempDir(), nil, 512)
	copier.Start()
	copier.EnqueueFile(dir, path, false)
	es := copier.Close()
	if es.SkippedOverCap != 1 {
		t.Fatalf("SkippedOverCap = %d, want 1", es.SkippedOverCap)
	}
}

func TestCopyFileSkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.env")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks not supported:", err)
	}
	dest := filepath.Join(dir, "out.env")
	if err := copyFile(link, dest); err == nil {
		t.Fatal("expected error copying symlink")
	}
}
