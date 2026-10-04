package main

import (
	"os"
	"path/filepath"
	"testing"
)

// M-09: a dry-run consult (-odr) is read-only by contract. The orphan-workdir
// sweep used to MkdirAll the library unconditionally, and the -debug /
// ingest-debug / reject artifacts landed inside the consulted library when -o
// was absent. A preview must not create the library directory nor leave files
// inside it.
func TestDryRunSweepDoesNotCreateLibrary(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "library") // does not exist
	cfg := runConfig{LibraryDir: lib, DryRun: true}
	sweepOrphanWorkDirs(cfg)
	if _, err := os.Stat(lib); err == nil {
		t.Fatalf("dry-run sweep created the library directory %q", lib)
	}
}

func TestRealRunSweepStillPreparesLibrary(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "library") // does not exist
	cfg := runConfig{LibraryDir: lib}
	sweepOrphanWorkDirs(cfg)
	if fi, err := os.Stat(lib); err != nil || !fi.IsDir() {
		t.Fatalf("real-run sweep must prepare the library directory: %v", err)
	}
}

// M-09 (re-review defect): the ingest-side artifacts follow ingestDebugDir,
// which batch-4 left unguarded — a -odr -debug run MkdirAll'd the consulted
// library and wrote the ingest debug/reject artifacts inside it. Real runs
// keep the library location.
func TestDryRunIngestDebugDirExcludesLibrary(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "library")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	// -odr consults an existing library: -debug ingest artifacts must land
	// outside it (CWD fallback, mirroring issueLogDir), never inside.
	t.Chdir(t.TempDir())
	cfg := runConfig{LibraryDir: lib, Debug: true, DebugReject: true, DryRun: true}
	if got := ingestDebugDir(cfg); got != "." {
		t.Fatalf("dry-run ingestDebugDir = %q, want the CWD fallback", got)
	}
	elog := newIngestDebugLog(cfg)
	if elog != nil {
		_ = elog.Close()
	}
	rr := newIngestRejectRecorder(cfg)
	if rr != nil {
		_ = rr.Close()
	}
	matches, err := filepath.Glob(filepath.Join(".", "sfl_ingest_debug_*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d ingest debug logs in CWD (%v), want 1", len(matches), matches)
	}
	rejects, err := filepath.Glob(filepath.Join(".", "sfl_ingest_rejected_*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rejects) != 1 {
		t.Fatalf("got %d ingest reject artifacts in CWD (%v), want 1", len(rejects), rejects)
	}
	// Real ingest mode keeps the stable library location and artifacts
	// inside it.
	cfgReal := runConfig{LibraryDir: lib, Debug: true}
	if got := ingestDebugDir(cfgReal); got != lib {
		t.Fatalf("ingestDebugDir = %q, want the library dir in real ingest mode", got)
	}
}

// The strongest form of the M-09 defect: -odr against a NONEXISTENT library
// must not create it, even with -debug running the full ingest-artifact path.
func TestDryRunIngestDebugDoesNotCreateLibrary(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "library") // does not exist
	t.Chdir(t.TempDir())
	cfg := runConfig{LibraryDir: lib, Debug: true, DebugReject: true, DryRun: true}
	elog := newIngestDebugLog(cfg)
	if elog != nil {
		_ = elog.Close()
	}
	rr := newIngestRejectRecorder(cfg)
	if rr != nil {
		_ = rr.Close()
	}
	if _, err := os.Stat(lib); err == nil {
		t.Fatalf("dry-run ingest artifacts created the library directory %q", lib)
	}
}

// M-09 (re-review defect): with no explicit -tempdir the engine derives the
// shard-temp parent from the output path — the library dir — and MkdirAlls
// it, which alone created the library during a dry run. Dry-run must route
// the shard temp outside the consulted library; real runs and an explicit
// -tempdir keep the caller's value.
func TestIngestTempDirDryRunLeavesLibraryDefault(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "library")
	cfgDry := runConfig{LibraryDir: lib, DryRun: true}
	if got := ingestTempDir(cfgDry); got != os.TempDir() {
		t.Fatalf("dry-run ingestTempDir = %q, want the platform temp dir", got)
	}
	cfgReal := runConfig{LibraryDir: lib}
	if got := ingestTempDir(cfgReal); got != "" {
		t.Fatalf("real-run ingestTempDir = %q, want the engine default (%q)", got, "")
	}
	cfgExplicit := runConfig{LibraryDir: lib, DryRun: true, TempDir: filepath.Join(t.TempDir(), "shards")}
	if got := ingestTempDir(cfgExplicit); got != cfgExplicit.TempDir {
		t.Fatalf("explicit -tempdir overridden in dry-run: %q", got)
	}
}

func TestDryRunLogDirExcludesLibrary(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "library")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig{LibraryDir: lib, DryRun: true}
	if got := logDir(cfg); got == lib {
		t.Fatalf("dry-run logDir = library dir; diagnostics must not land inside a consulted library")
	}
	if got := logDir(cfg); got != "." {
		t.Fatalf("dry-run logDir without -o = %q, want CWD fallback", got)
	}
	// Real ingest mode keeps the stable library location.
	cfgReal := runConfig{LibraryDir: lib}
	if got := logDir(cfgReal); got != lib {
		t.Fatalf("logDir = %q, want the library dir in real ingest mode", got)
	}
	// An explicit -o output dir wins in both modes.
	cfgOut := runConfig{LibraryDir: lib, OutputDir: filepath.Join(dir, "out"), DryRun: true}
	if got := logDir(cfgOut); got != filepath.Join(dir, "out") {
		t.Fatalf("logDir = %q, want the -o output dir", got)
	}
}
