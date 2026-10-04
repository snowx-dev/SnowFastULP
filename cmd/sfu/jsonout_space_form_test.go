package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// D1: the space form of -json (-json FILE) must consume FILE as the
// stream target like the = form, instead of leaking FILE into the positional
// arguments. The bare form keeps meaning stdout.

func sfuSpaceFormInput(t *testing.T, dir string) string {
	t.Helper()
	input := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(input, []byte("https://example.com:user:pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return input
}

func runSFUSpaceForm(t *testing.T, dir, config string, args ...string) (string, int) {
	t.Helper()
	bin := buildSFUE2E(t)
	if config != "" {
		args = append([]string{"-config", config}, args...)
	} else {
		args = append([]string{"-config", filepath.Join(dir, "empty.toml")}, args...)
		if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sfu: %v\nstderr:\n%s", err, stderr.String())
	}
	return stderr.String(), code
}

// The space form writes the stream to the named file.
func TestJSONOutSpaceFormWritesFile(t *testing.T) {
	dir := t.TempDir()
	input := sfuSpaceFormInput(t, dir)
	out := filepath.Join(dir, "out.d")
	jsonl := filepath.Join(dir, "stats.jsonl")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check", "-json", jsonl,
		"-o", out+string(filepath.Separator), input)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	if snaps[0]["event"] != "start" {
		t.Fatalf("first stream event = %v, want start", snaps[0]["event"])
	}
}

// The hijack case: with a config input set, the space-form FILE must become
// the stream target, not the input path.
func TestJSONOutSpaceFormDoesNotHijackConfigInput(t *testing.T) {
	dir := t.TempDir()
	input := sfuSpaceFormInput(t, dir)
	out := filepath.Join(dir, "out.d")
	jsonl := filepath.Join(dir, "stats.jsonl")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "sfu.toml")
	if err := os.WriteFile(cfg, []byte("[sfu]\ninput = \""+input+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFUSpaceForm(t, dir, cfg,
		"-no-tui", "-no-update-check", "-json", jsonl,
		"-o", out+string(filepath.Separator))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	if snaps[0]["event"] != "start" {
		t.Fatalf("first stream event = %v, want start (FILE must be the stream, not the input)", snaps[0]["event"])
	}
}

// The bare form still means stdout: no file is created for it.
func TestJSONOutBareFormStillStdout(t *testing.T) {
	dir := t.TempDir()
	input := sfuSpaceFormInput(t, dir)
	out := filepath.Join(dir, "out.d")
	jsonl := filepath.Join(dir, "stats.jsonl")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check", "-json",
		"-o", out+string(filepath.Separator), input)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if _, err := os.Stat(jsonl); !os.IsNotExist(err) {
		t.Fatalf("bare -json must not create %s: %v", jsonl, err)
	}
}

// D6: an all-rejects run exits 4, which os.Exits and skips the deferred
// artifact close — the -debug-reject buffer must still flush, and the
// password>64 tag must be in the file.
func TestDebugRejectTaggedSurvivesNothingParsed(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, []byte("https://example.com:user:"+strings.Repeat("x", 65)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check", "-debug-reject", in)
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (nothing parsed)\nstderr:\n%s", code, stderr)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "sfu-rejected-*.txt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("reject files = %v (err %v), want exactly one", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[password>64] https://a") && !strings.Contains(string(data), "[password>64]") {
		t.Fatalf("reject file missing the tagged reject: %q", data)
	}
}

// A -json target that IS the input would be truncated by the stream
// before the input is read (the reviewer probe: the input destroyed, exit 4).
// Both argv spellings must refuse with exit 2 and leave the input
// byte-identical. Non-colliding targets still work (see the writes-file
// tests above).
func TestJSONOutTargetCollidingWithPositionalInputRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		targetArg []string
	}{
		{name: "space form"},
		{name: "equals form"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			clob := filepath.Join(dir, "clob.txt")
			content := []byte("https://example.com:user:pass\n")
			if err := os.WriteFile(clob, content, 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "out.d")
			if err := os.MkdirAll(out, 0o755); err != nil {
				t.Fatal(err)
			}
			var targetArg []string
			if tc.name == "space form" {
				targetArg = []string{"-json", clob}
			} else {
				targetArg = []string{"-json=" + clob}
			}
			args := append([]string{
				"-no-tui", "-no-update-check",
				"-o", out + string(filepath.Separator),
			}, targetArg...)
			args = append(args, clob)
			stderr, code := runSFUSpaceForm(t, dir, "", args...)
			if code != 2 {
				t.Fatalf("%s: exit = %d, want 2\nstderr:\n%s", tc.name, code, stderr)
			}
			if !strings.Contains(stderr, "overlaps input") {
				t.Fatalf("%s: stderr missing the collision reason:\n%s", tc.name, stderr)
			}
			got, err := os.ReadFile(clob)
			if err != nil || string(got) != string(content) {
				t.Fatalf("%s: input clobbered: %q (err %v)", tc.name, got, err)
			}
		})
	}
}

// The config-input case: the space-form FILE equals the [sfu].input file.
func TestJSONOutTargetCollidingWithConfigInputRefused(t *testing.T) {
	dir := t.TempDir()
	clob := filepath.Join(dir, "clob.txt")
	content := []byte("https://example.com:user:pass\n")
	if err := os.WriteFile(clob, content, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "sfu.toml")
	if err := os.WriteFile(cfg, []byte("[sfu]\ninput = \""+clob+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFUSpaceForm(t, dir, cfg,
		"-no-tui", "-no-update-check", "-json", clob)
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", code, stderr)
	}
	got, rerr := os.ReadFile(clob)
	if rerr != nil || string(got) != string(content) {
		t.Fatalf("config-input collision must leave the input byte-identical: %q (err %v)", got, rerr)
	}
}

// A target inside an input directory is refused even when it equals no
// collected file.
func TestJSONOutTargetInsideInputDirRefused(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(input, []byte("https://example.com:user:pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ins := filepath.Join(dir, "inputs")
	if err := os.MkdirAll(ins, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ins, "a.txt"), []byte("https://a.example:user:pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check",
		"-json", filepath.Join(ins, "stream.jsonl"), ins)
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "overlaps input directory") {
		t.Fatalf("stderr missing the input-directory reason:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(ins, "stream.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("target must not be created inside the input dir: %v", err)
	}
}
