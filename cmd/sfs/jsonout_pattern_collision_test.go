package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// H-04: -json must refuse a target that names the -f patterns file, the
// config file, or any per-pattern output -f mode is about to create — and it
// must refuse BEFORE allocatePatternFiles truncates anything. Both review
// proofs ran the whole binary: Proof A destroyed the patterns file and exited
// 0; Proof B let the stream and the hit writer share one generated file.

// TestJSONOutRefusesPatternsFileTarget is review Proof A: the stream target
// is the -f patterns file itself. The patterns file must survive byte-identical
// and the invocation must be refused with the usage code before anything is
// opened or truncated.
func TestJSONOutRefusesPatternsFileTarget(t *testing.T) {
	dir := t.TempDir()
	root := sfsJSONOutRoot(t, dir)
	patterns := filepath.Join(dir, "patterns.txt")
	if err := os.WriteFile(patterns, []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runSFSCapture(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-txt", "-f", patterns, "-o", filepath.Join(dir, "out"),
		"-json", patterns, root)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "overlaps") {
		t.Fatalf("stderr missing collision reason:\n%s", stderr)
	}
	data, err := os.ReadFile(patterns)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "needle\n" {
		t.Fatalf("patterns file destroyed: %q", data)
	}
}

// TestJSONOutRefusesGeneratedPatternFileTarget is review Proof B: the stream
// target is the predictable per-pattern output name <slug>_<YYYYMMDD-HHMM>.txt
// inside the -o directory. The stamp comes from the run's own start time, so
// the test tries the current and next minute; any run whose stamp matched must
// be refused with the usage code and must leave no per-pattern file behind.
func TestJSONOutRefusesGeneratedPatternFileTarget(t *testing.T) {
	dir := t.TempDir()
	root := sfsJSONOutRoot(t, dir)
	patterns := filepath.Join(dir, "patterns.txt")
	if err := os.WriteFile(patterns, []byte("foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	now := time.Now()
	stamps := []string{
		now.Format("20060102-1504"),
		now.Add(time.Minute).Format("20060102-1504"),
	}
	refused := false
	for _, stamp := range stamps {
		target := filepath.Join(outDir, "foo_"+stamp+".txt")
		_, stderr, code := runSFSCapture(t, dir,
			"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
			"-txt", "-f", patterns, "-o", outDir,
			"-json", target, root)
		if code == 0 {
			// The stamp rolled over between now and the run: this spelling
			// was not a prospective output name, so the run was legitimate.
			continue
		}
		if code != 2 || !strings.Contains(stderr, "overlaps") {
			t.Fatalf("exit = %d, want 2 with collision reason\nstderr:\n%s", code, stderr)
		}
		if _, err := os.Stat(target); err == nil {
			t.Fatalf("per-pattern target %s was created/truncated by the refused run", target)
		}
		entries, err := os.ReadDir(outDir)
		if err == nil && len(entries) > 0 {
			t.Fatalf("refused run left per-pattern files behind: %v", entries)
		}
		refused = true
		break
	}
	if !refused {
		t.Fatal("no candidate stamp matched the run's stamp; collision never exercised")
	}
}

// TestJSONOutRefusesConfigFileTarget: the stream target is the config file
// the run loaded from. The config file must survive byte-identical.
func TestJSONOutRefusesConfigFileTarget(t *testing.T) {
	dir := t.TempDir()
	root := sfsJSONOutRoot(t, dir)
	cfg := filepath.Join(dir, "sfs.toml")
	if err := os.WriteFile(cfg, []byte("[sfs]\nj = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runSFSCapture(t, dir,
		"-config", cfg, "-no-update-check",
		"-txt", "-o", filepath.Join(dir, "out.txt"),
		"-json", cfg, root, "needle")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "overlaps") {
		t.Fatalf("stderr missing collision reason:\n%s", stderr)
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[sfs]\nj = 2\n" {
		t.Fatalf("config file destroyed: %q", data)
	}
}

// Unit coverage for the prospective-name computation and the preflight check
// itself, with an explicit stamp so the minute-rollover ambiguity of the e2e
// runs does not apply here.
func TestProspectivePatternFilePaths(t *testing.T) {
	paths := prospectivePatternFilePaths("/o", []string{"a.b", "a:b"}, time.Date(2026, 9, 28, 3, 36, 0, 0, time.UTC))
	// Both terms slug to the same base, so the second allocation takes the
	// _2 suffix; the candidate set must contain both spellings.
	want := map[string]bool{
		"/o/a_b_20260928-0336.txt":   true,
		"/o/a_b_20260928-0336_2.txt": true,
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for _, p := range paths {
		if !want[p] {
			t.Fatalf("unexpected candidate %q (want subset of %v)", p, want)
		}
	}
}

func TestRejectJSONOutPreflightCollisions(t *testing.T) {
	dir := t.TempDir()
	patterns := filepath.Join(dir, "patterns.txt")
	if err := os.WriteFile(patterns, []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Existing input, exact spelling.
	if err := rejectJSONOutPreflightCollisions(patterns, patterns); err == nil {
		t.Fatal("expected collision for identical spelling")
	}
	// Existing input through a hardlink alias: SameFile must catch it even
	// though the spellings never converge.
	alias := filepath.Join(dir, "alias.txt")
	if err := os.Link(patterns, alias); err != nil {
		t.Fatal(err)
	}
	if err := rejectJSONOutPreflightCollisions(alias, patterns); err == nil {
		t.Fatal("expected collision for hardlink alias")
	}
	// Not-yet-created candidate (the Proof B shape): the target names the
	// very file -f mode would create.
	prospective := filepath.Join(dir, "out", "foo_20260928-0336.txt")
	if err := rejectJSONOutPreflightCollisions(prospective, prospective); err == nil {
		t.Fatal("expected collision for prospective candidate")
	}
	// Distinct paths stay clear.
	if err := rejectJSONOutPreflightCollisions(filepath.Join(dir, "stats.jsonl"), patterns); err != nil {
		t.Fatalf("distinct target rejected: %v", err)
	}
	// Empty target and stdout are never collisions.
	if err := rejectJSONOutPreflightCollisions("", patterns); err != nil {
		t.Fatalf("empty target rejected: %v", err)
	}
	if err := rejectJSONOutPreflightCollisions("-", patterns); err != nil {
		t.Fatalf("stdout target rejected: %v", err)
	}
}

// The target's final component may itself be a symlink whose referent does
// not exist yet (the dangling shape: the per-pattern output -f mode is about
// to create). CanonicalProspective and SameFile both pass on such a link —
// the canonical spelling of the target is the link path itself — but the
// stream's create follows the link and lands on the referent, destroying the
// prospective output the moment allocation opens it. The referent chain must
// be compared against the protected set.
func TestRejectJSONOutPreflightCollisionsDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "out", "foo_20260928-0336.txt")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(candidate, link); err != nil {
		t.Fatal(err)
	}
	if err := rejectJSONOutPreflightCollisions(link, candidate); err == nil {
		t.Fatal("dangling symlink onto a prospective per-pattern output must be refused")
	}

	// An existing referent is already caught by the canonical compare (Stat
	// follows the link); the chain check must not weaken that.
	existing := filepath.Join(dir, "patterns.txt")
	if err := os.WriteFile(existing, []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	liveLink := filepath.Join(dir, "link-live")
	if err := os.Symlink(existing, liveLink); err != nil {
		t.Fatal(err)
	}
	if err := rejectJSONOutPreflightCollisions(liveLink, existing); err == nil {
		t.Fatal("symlink onto an existing protected file must be refused")
	}

	// A dangling referent outside the protected set stays clear.
	other := filepath.Join(dir, "elsewhere.txt")
	otherLink := filepath.Join(dir, "link-other")
	if err := os.Symlink(other, otherLink); err != nil {
		t.Fatal(err)
	}
	if err := rejectJSONOutPreflightCollisions(otherLink, candidate); err != nil {
		t.Fatalf("unrelated dangling referent rejected: %v", err)
	}
}

// The non-f target check must also follow the chain: a dangling link from
// outside the search root landing inside it (pollution the scan then sees)
// and a dangling link spelling the -o output file are both refused.
func TestRejectJSONOutTargetCollisionsDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "scan")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	intoRoot := filepath.Join(dir, "link-root")
	if err := os.Symlink(filepath.Join(root, "new.txt"), intoRoot); err != nil {
		t.Fatal(err)
	}
	if err := rejectJSONOutTargetCollisions(intoRoot, "", root); err == nil {
		t.Fatal("dangling symlink landing inside the search root must be refused")
	}
	outFile := filepath.Join(dir, "hits.txt")
	ontoOut := filepath.Join(dir, "link-out")
	if err := os.Symlink(outFile, ontoOut); err != nil {
		t.Fatal(err)
	}
	if err := rejectJSONOutTargetCollisions(ontoOut, outFile, root); err == nil {
		t.Fatal("dangling symlink onto the -o output must be refused")
	}
	// A dangling referent outside root and off the -o file stays clear.
	clear := filepath.Join(dir, "link-clear")
	if err := os.Symlink(filepath.Join(dir, "stats.jsonl"), clear); err != nil {
		t.Fatal(err)
	}
	if err := rejectJSONOutTargetCollisions(clear, outFile, root); err != nil {
		t.Fatalf("unrelated dangling referent rejected: %v", err)
	}
}

// End-to-end shape of the second review's Proof: `ln -s out/secret_STAMP.txt
// LINK; sfs -f pat.txt -o out/ -json LINK scan/` must be refused with the
// usage code — not exit 0 with the hit output truncated into interleaved
// stream fragments. As in TestJSONOutRefusesGeneratedPatternFileTarget, a
// stamp mismatch means the spelled name was never a prospective candidate and
// the run is legitimate.
func TestJSONOutRefusesDanglingSymlinkOntoGeneratedPatternFile(t *testing.T) {
	dir := t.TempDir()
	root := sfsJSONOutRoot(t, dir)
	patterns := filepath.Join(dir, "patterns.txt")
	if err := os.WriteFile(patterns, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	refused := false
	for _, stamp := range []string{
		now.Format("20060102-1504"),
		now.Add(time.Minute).Format("20060102-1504"),
	} {
		referent := filepath.Join(outDir, "secret_"+stamp+".txt")
		link := filepath.Join(dir, "link_"+stamp)
		if err := os.Symlink(referent, link); err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runSFSCapture(t, dir,
			"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
			"-txt", "-f", patterns, "-o", outDir,
			"-json", link, root)
		if code == 0 {
			// Stamp rolled over: the spelled name was not a prospective
			// per-pattern output, so the run was legitimate.
			continue
		}
		if code != 2 || !strings.Contains(stderr, "overlaps") {
			t.Fatalf("exit = %d, want 2 with collision reason\nstderr:\n%s", code, stderr)
		}
		if _, err := os.Lstat(referent); !os.IsNotExist(err) {
			t.Fatalf("referent %s was created/truncated by the refused run", referent)
		}
		if entries, err := os.ReadDir(outDir); err == nil && len(entries) > 0 {
			t.Fatalf("refused run left per-pattern files behind: %v", entries)
		}
		refused = true
		break
	}
	if !refused {
		t.Fatal("no candidate stamp matched the run's stamp; collision never exercised")
	}
}

// The same chain escape in non-f mode: a dangling link from outside the
// search root landing inside it would have the stream create a file the scan
// then sees mid-write. The link must be recognized as overlapping the root.
func TestJSONOutRefusesDanglingSymlinkIntoSearchRoot(t *testing.T) {
	dir := t.TempDir()
	root := sfsJSONOutRoot(t, dir)
	hits := filepath.Join(dir, "hits.txt")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(root, "smuggled.txt"), link); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runSFSCapture(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-txt", "-o", hits, "-json", link, root, "needle")
	if code != 2 || !strings.Contains(stderr, "overlaps") {
		t.Fatalf("exit = %d, want 2 with collision reason\nstderr:\n%s", code, stderr)
	}
	if _, err := os.Lstat(filepath.Join(root, "smuggled.txt")); !os.IsNotExist(err) {
		t.Fatal("the refused run created a file inside the search root through the link")
	}
}
