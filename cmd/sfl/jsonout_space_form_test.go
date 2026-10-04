//go:build unix

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
// arguments (arity error, or — with a config input set — silently becoming
// THE input). The bare form keeps meaning stdout.

func sflSpaceFormFixture(t *testing.T) (dir, input, out, jsonl string) {
	t.Helper()
	dir = t.TempDir()
	input = filepath.Join(dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"), []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = filepath.Join(dir, "out.d")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, input, out, filepath.Join(dir, "stats.jsonl")
}

func runSFLSpaceForm(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	bin := buildSFLSub(t)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+filepath.Join(dir, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"TMPDIR="+filepath.Join(dir, "tmp"),
	)
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sfl: %v\nstderr:\n%s", err, stderr.String())
	}
	return stderr.String(), code
}

// The space form writes the stream to the named file.
func TestJSONOutSpaceFormWritesFile(t *testing.T) {
	dir, input, out, jsonl := sflSpaceFormFixture(t)
	_, code := runSFLSpaceForm(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-tui", "-no-update-check",
		"-json", jsonl, "-o", out+string(os.PathSeparator), input)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	snaps := readJSONL(t, jsonl)
	if snaps[0]["event"] != "start" {
		t.Fatalf("first stream event = %v, want start", snaps[0]["event"])
	}
}

// The hijack case from the findings: with a config input set, the space-form
// FILE must become the stream target, not the input path.
func TestJSONOutSpaceFormDoesNotHijackConfigInput(t *testing.T) {
	dir, input, out, jsonl := sflSpaceFormFixture(t)
	cfg := filepath.Join(dir, "sfl.toml")
	if err := os.WriteFile(cfg, []byte("[sfl]\ninput = \""+input+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No positional: the input comes from config; FILE is the stream target.
	_, code := runSFLSpaceForm(t, dir,
		"-config", cfg, "-no-tui", "-no-update-check",
		"-json", jsonl, "-o", out+string(os.PathSeparator))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	snaps := readJSONL(t, jsonl)
	if snaps[0]["event"] != "start" {
		t.Fatalf("first stream event = %v, want start (FILE must be the stream, not the input)", snaps[0]["event"])
	}
}

// The bare form still means stdout: no file is created for it.
func TestJSONOutBareFormStillStdout(t *testing.T) {
	dir, input, out, jsonl := sflSpaceFormFixture(t)
	stderr, code := runSFLSpaceForm(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-tui", "-no-update-check",
		"-json", "-o", out+string(os.PathSeparator), input)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if _, err := os.Stat(jsonl); !os.IsNotExist(err) {
		t.Fatalf("bare -json must not create %s: %v", jsonl, err)
	}
}
func TestJSONOutSpaceDashShowsUsageGuidance(t *testing.T) {
	dir, _, _, _ := sflSpaceFormFixture(t)
	stderr, code := runSFLSpaceForm(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-json", "-")
	if code != 2 {
		t.Fatalf("exit = %d, want usage exit 2\nstderr:\n%s", code, stderr)
	}
	for _, want := range []string{
		"-json - is ambiguous",
		"bare -json or -json=- for stdout",
		"-json=file:- for a literal file named -",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

func TestJSONOutEqualsDashAndLiteralFileDashRemainSupported(t *testing.T) {
	t.Run("equals dash is stdout", func(t *testing.T) {
		dir, input, out, _ := sflSpaceFormFixture(t)
		_, code := runSFLSpaceForm(t, dir,
			"-config", filepath.Join(dir, "empty.toml"), "-no-tui", "-no-update-check",
			"-json=-", "-o", out+string(os.PathSeparator), input)
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
	})
	t.Run("file colon dash is literal path", func(t *testing.T) {
		dir, input, out, _ := sflSpaceFormFixture(t)
		_, code := runSFLSpaceForm(t, dir,
			"-config", filepath.Join(dir, "empty.toml"), "-no-tui", "-no-update-check",
			"-json=file:-", "-o", out+string(os.PathSeparator), input)
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		data, err := os.ReadFile(filepath.Join(dir, "-"))
		if err != nil {
			t.Fatalf("read literal dash output: %v", err)
		}
		if !strings.Contains(string(data), `"event":"start"`) {
			t.Fatalf("literal dash output has no start event: %q", data)
		}
	})
}

// A -json target that IS the input would be truncated by the stream
// before the input is read. Both argv spellings must refuse with exit 2 and
// leave the input byte-identical (sfl's validateJSONOutTarget guards this).
func TestJSONOutTargetCollidingWithPositionalInputRefused(t *testing.T) {
	for _, tc := range []struct{ name string }{
		{name: "space form"},
		{name: "equals form"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _, out, _ := sflSpaceFormFixture(t)
			clob := filepath.Join(dir, "clob.txt")
			content := []byte("example.com:alice:pw\n")
			if err := os.WriteFile(clob, content, 0o644); err != nil {
				t.Fatal(err)
			}
			var targetArg []string
			if tc.name == "space form" {
				targetArg = []string{"-json", clob}
			} else {
				targetArg = []string{"-json=" + clob}
			}
			args := append([]string{
				"-config", filepath.Join(dir, "empty.toml"), "-no-tui", "-no-update-check",
				"-o", out + string(os.PathSeparator),
			}, targetArg...)
			args = append(args, clob)
			stderr, code := runSFLSpaceForm(t, dir, args...)
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

// The config-input case: the space-form FILE equals the input resolved from
// [sfl].input — a file inside the config input directory.
func TestJSONOutTargetCollidingWithConfigInputRefused(t *testing.T) {
	dir, input, out, _ := sflSpaceFormFixture(t)
	cfg := filepath.Join(dir, "sfl.toml")
	if err := os.WriteFile(cfg, []byte("[sfl]\ninput = \""+input+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clob := filepath.Join(input, "Passwords.txt")
	before, err := os.ReadFile(clob)
	if err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFLSpaceForm(t, dir,
		"-config", cfg, "-no-tui", "-no-update-check",
		"-json", clob, "-o", out+string(os.PathSeparator))
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", code, stderr)
	}
	after, rerr := os.ReadFile(clob)
	if rerr != nil || string(after) != string(before) {
		t.Fatalf("config-input collision must leave the input byte-identical: %q (err %v)", after, rerr)
	}
}
