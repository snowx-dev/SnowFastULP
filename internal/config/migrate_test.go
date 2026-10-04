package config

// Internal test file (deliberate one-file deviation from the directory's
// config_test convention) so the drift test can read the unexported
// exampleTOML and the tests can inspect the per-path migration state
// directly.

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// migrationEnv isolates the config path (SNOWFAST_CONFIG) and the migration
// state dir (XDG_DATA_HOME / AppData, per installctx_test.go) into temp dirs.
func migrationEnv(t *testing.T) (cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.toml")
	t.Setenv("SNOWFAST_CONFIG", cfgPath)
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", t.TempDir()) // state dir seam, per installctx_test.go:17-21
	} else {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
	}
	return cfgPath
}

func stateFileFor(t *testing.T, cfgPath string) string {
	t.Helper()
	sp := statePathFor(cfgPath)
	if sp == "" {
		t.Fatal("migration state path unresolvable")
	}
	return sp
}

func writeStateVer(t *testing.T, cfgPath, ver string) {
	t.Helper()
	sp := stateFileFor(t, cfgPath)
	if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp, []byte("config="+cfgPath+"\nversion="+ver+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stateVersion(t *testing.T, cfgPath string) (string, bool) {
	t.Helper()
	return readStateVersion(stateFileFor(t, cfgPath))
}

func writeLegacyStamp(t *testing.T, ver string) {
	t.Helper()
	sp := legacyStampPath()
	if sp == "" {
		t.Fatal("legacy stamp path unresolvable")
	}
	if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp, []byte("version="+ver+"\nlast_run=2000-01-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func legacyStampVersion(t *testing.T) (string, bool) {
	t.Helper()
	sp := legacyStampPath()
	if sp == "" {
		t.Fatal("legacy stamp path unresolvable")
	}
	return readStampVersion(sp)
}

func backupsFor(t *testing.T, cfgPath string) []string {
	t.Helper()
	baks, err := filepath.Glob(filepath.Join(filepath.Dir(cfgPath), filepath.Base(cfgPath)+".*.bak*"))
	if err != nil {
		t.Fatal(err)
	}
	return baks
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureMigratedSkipsSymlinkedConfig(t *testing.T) {
	cfgPath := migrationEnv(t)
	targetPath := filepath.Join(filepath.Dir(cfgPath), "target.toml")
	targetBytes := []byte("[sfu]\nworkers = 4\nzst = true\n")
	writeFile(t, targetPath, targetBytes)
	writeStateVer(t, cfgPath, "0.1")
	if err := os.Symlink(targetPath, cfgPath); err != nil {
		t.Skipf("create config symlink: %v", err)
	}

	before, err := os.Lstat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var notice bytes.Buffer
	EnsureMigrated(nil, "0.2", &notice)

	after, err := os.Lstat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode()&os.ModeSymlink == 0 || !os.SameFile(before, after) {
		t.Fatalf("config symlink was replaced: before=%v after=%v", before.Mode(), after.Mode())
	}
	if got, err := os.Readlink(cfgPath); err != nil {
		t.Fatal(err)
	} else if got != targetPath {
		t.Errorf("config symlink target = %q, want %q", got, targetPath)
	}
	gotTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotTarget, targetBytes) {
		t.Errorf("symlink target bytes changed: got %q, want %q", gotTarget, targetBytes)
	}
	if got, ok := stateVersion(t, cfgPath); !ok || got != "0.1" {
		t.Errorf("migration state = %q, %v; want prior version 0.1", got, ok)
	}
	if got := backupsFor(t, cfgPath); len(got) != 0 {
		t.Errorf("migration created backups: %v", got)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(cfgPath), filepath.Base(cfgPath)+".migrate-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Errorf("migration created temporary files: %v", temps)
	}
	if got := notice.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "migration skipped") || !strings.Contains(got, "symlink") || !strings.Contains(got, "migrate the target") || !strings.Contains(got, "replace the link deliberately") {
		t.Errorf("unexpected symlink migration notice: %q", got)
	}

	// A profile already recorded at this version needs no migration and must
	// not print the symlink warning on every normal invocation.
	writeStateVer(t, cfgPath, "0.2")
	notice.Reset()
	EnsureMigrated(nil, "0.2", &notice)
	if notice.Len() != 0 {
		t.Errorf("current symlink profile should be silent, got %q", notice.String())
	}
}

func TestEnsureMigratedMigratesOnVersionChange(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.1")
	old := "[sfu]\nworkers = 4\nzst = true\n"
	writeFile(t, cfgPath, []byte(old))

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	migrated, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(migrated)
	for _, want := range []string{"[sfu]", "# workers", "# zst", "workers = 4", "zst = true"} {
		if !strings.Contains(s, want) {
			t.Errorf("migrated config missing %q:\n%s", want, s)
		}
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Errorf("expected exactly one notice line, got %d: %q", n, buf.String())
	}
	if !strings.Contains(buf.String(), "migrated config to 0.2") || !strings.Contains(buf.String(), "backup:") {
		t.Errorf("unexpected notice: %q", buf.String())
	}
	if v, ok := stateVersion(t, cfgPath); !ok || v != "0.2" {
		t.Errorf("state = %q, ok=%v; want 0.2", v, ok)
	}
	loaded, err := Load(cfgPath, true)
	if err != nil {
		t.Fatalf("migrated config does not load: %v\n%s", err, s)
	}
	if loaded.SFU.Workers == nil || *loaded.SFU.Workers != 4 {
		t.Errorf("loaded.SFU.Workers = %v, want 4", loaded.SFU.Workers)
	}
}

func TestEnsureMigratedNoopOnSameVersion(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.2")
	old := []byte("[sfu]\nworkers = 4\n")
	writeFile(t, cfgPath, old)

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Errorf("config was rewritten:\n%s", got)
	}
	if baks := backupsFor(t, cfgPath); len(baks) != 0 {
		t.Errorf("unexpected backups: %v", baks)
	}
	if buf.Len() != 0 {
		t.Errorf("expected silence, got %q", buf.String())
	}
}

func TestEnsureMigratedFirstRunDoesNotMigrate(t *testing.T) {
	cfgPath := migrationEnv(t)
	old := []byte("[sfu]\nworkers = 4\n")
	writeFile(t, cfgPath, old)

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Errorf("first run must not migrate, config was rewritten:\n%s", got)
	}
	if baks := backupsFor(t, cfgPath); len(baks) != 0 {
		t.Errorf("unexpected backups: %v", baks)
	}
	if v, ok := stateVersion(t, cfgPath); !ok || v != "0.2" {
		t.Errorf("state = %q, ok=%v; want 0.2", v, ok)
	}
	if buf.Len() != 0 {
		t.Errorf("expected silence, got %q", buf.String())
	}
}

func TestEnsureMigratedMissingConfigRecordsNoState(t *testing.T) {
	t.Run("default path", func(t *testing.T) {
		base := t.TempDir()
		if runtime.GOOS == "windows" {
			t.Setenv("AppData", base)
		} else {
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "cfg"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
		}
		def, err := DefaultPath()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		EnsureMigrated(nil, "0.2", &buf)
		if buf.Len() != 0 {
			t.Errorf("expected silence, got %q", buf.String())
		}
		if _, ok := stateVersion(t, def); ok {
			t.Error("missing config must not record migration state")
		}
		if _, err := os.Stat(filepath.Join(base, "cfg", "snowfast", "config.toml")); !os.IsNotExist(err) {
			t.Errorf("migration created a config file: %v", err)
		}
	})
	t.Run("explicit missing path", func(t *testing.T) {
		migrationEnv(t)
		var buf bytes.Buffer
		EnsureMigrated([]string{"-config", "/nonexistent/x.toml"}, "0.2", &buf)
		if buf.Len() != 0 {
			t.Errorf("expected silence, got %q", buf.String())
		}
		if _, ok := stateVersion(t, "/nonexistent/x.toml"); ok {
			t.Error("missing config must not record migration state")
		}
	})
}

func TestEnsureMigratedDropsDeprecatedKeys(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.1")
	old := "stray = 1\n[sfu]\nworkers = 2\nworokers = 3\n"
	writeFile(t, cfgPath, []byte(old))

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "workers = 2") {
		t.Errorf("kept value lost:\n%s", s)
	}
	if strings.Contains(s, "worokers") || strings.Contains(s, "stray") {
		t.Errorf("deprecated keys survived:\n%s", s)
	}
	if !strings.Contains(buf.String(), "dropped: [] stray, [sfu] worokers") {
		t.Errorf("notice missing dropped keys: %q", buf.String())
	}
	if baks := backupsFor(t, cfgPath); len(baks) != 1 {
		t.Errorf("backups = %v, want exactly 1", baks)
	}
}

func TestEnsureMigratedInvalidTOMLSkips(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.1")
	bad := []byte("not [ valid toml")
	writeFile(t, cfgPath, bad)

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bad) {
		t.Errorf("invalid config was rewritten:\n%s", got)
	}
	if baks := backupsFor(t, cfgPath); len(baks) != 0 {
		t.Errorf("unexpected backups: %v", baks)
	}
	if !strings.Contains(buf.String(), "skipping migration") {
		t.Errorf("expected a skipping-migration warning, got %q", buf.String())
	}
	if v, ok := stateVersion(t, cfgPath); !ok || v != "0.1" {
		t.Errorf("state = %q, ok=%v; want unchanged 0.1 (no version=0.2 written)", v, ok)
	}
}

func TestEnsureMigratedBackupPreservesOriginalBytes(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.1")
	old := []byte("[sfu]\nworkers = 4\n")
	writeFile(t, cfgPath, old)
	if err := os.Chmod(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	baks := backupsFor(t, cfgPath)
	if len(baks) != 1 {
		t.Fatalf("backups = %v, want exactly 1", baks)
	}
	got, err := os.ReadFile(baks[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Errorf("backup bytes differ from original:\n%s", got)
	}
	// os.Chmod on windows only toggles the read-only bit, so the perm
	// assertion is meaningful on unix filesystems only.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(baks[0])
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("backup mode = %v, want -rw-------", info.Mode().Perm())
		}
	}
}

func TestEmbeddedExampleMatchesRootExample(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.toml.example"))
	if err != nil {
		t.Fatalf("read root example: %v", err)
	}
	if !bytes.Equal(raw, exampleTOML) {
		t.Fatal("internal/config/example.toml drifted from config.toml.example; run: make example-sync")
	}
}

func TestEnsureMigratedPathResolution(t *testing.T) {
	migrationEnv(t) // state dir seam only; SNOWFAST_CONFIG is overridden below
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.toml")
	pathB := filepath.Join(dir, "b.toml")
	t.Setenv("SNOWFAST_CONFIG", pathB)
	writeStateVer(t, pathA, "0.1")
	oldA := []byte("[sfu]\nworkers = 4\n")
	oldB := []byte("[sfu]\nworkers = 5\n")
	writeFile(t, pathA, oldA)
	writeFile(t, pathB, oldB)

	var buf bytes.Buffer
	EnsureMigrated([]string{"-config", pathA}, "0.2", &buf)

	gotA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotA), "workers = 4") || !strings.Contains(string(gotA), "# workers") {
		t.Errorf("config A (argv) not migrated:\n%s", gotA)
	}
	gotB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotB, oldB) {
		t.Errorf("config B (env) must stay untouched:\n%s", gotB)
	}
	baksA := backupsFor(t, pathA)
	if len(baksA) != 1 {
		t.Fatalf("backups for A = %v, want exactly 1", baksA)
	}
	if baksB := backupsFor(t, pathB); len(baksB) != 0 {
		t.Errorf("backups for B = %v, want 0", baksB)
	}
	if !strings.Contains(buf.String(), baksA[0]) {
		t.Errorf("notice does not name A's backup %q: %q", baksA[0], buf.String())
	}
}

func TestEnsureMigratedIdempotentSecondRun(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.1")
	writeFile(t, cfgPath, []byte("[sfu]\nworkers = 4\n"))

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)
	first := backupsFor(t, cfgPath)
	if len(first) != 1 {
		t.Fatalf("backups after migration = %v, want exactly 1", first)
	}

	// Simulate the state write having failed after the commit: the next run
	// re-derives identical merged bytes and must skip silently.
	writeStateVer(t, cfgPath, "0.1")
	var buf2 bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf2)

	if baks := backupsFor(t, cfgPath); len(baks) != 1 {
		t.Errorf("backups after second run = %v, want still exactly 1", baks)
	}
	if buf2.Len() != 0 {
		t.Errorf("expected silent no-op, got %q", buf2.String())
	}
	if v, ok := stateVersion(t, cfgPath); !ok || v != "0.2" {
		t.Errorf("state = %q, ok=%v; want 0.2", v, ok)
	}
}

func TestEnsureMigratedResultLoadsThroughProductionPath(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.1")
	writeFile(t, cfgPath, []byte("[sfs]\nj = 3\n"))

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	loaded, err := Load(cfgPath, true)
	if err != nil {
		t.Fatalf("migrated config does not load: %v", err)
	}
	j := 0
	if err := loaded.ApplySFS(Visited{}, SFSFlags{J: &j}); err != nil {
		t.Fatalf("ApplySFS: %v", err)
	}
	if j != 3 {
		t.Errorf("merged -j = %d, want 3 (from migrated [sfs] j)", j)
	}
}

func TestEnsureMigratedPreservesHistory(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.1")
	writeFile(t, cfgPath, []byte("[history]\nenabled = true\npath = \"~/snowfast-history.db\"\n"))

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	loaded, err := Load(cfgPath, true)
	if err != nil {
		t.Fatalf("migrated config does not load: %v", err)
	}
	if loaded.History.Enabled == nil || !*loaded.History.Enabled {
		t.Fatalf("history.enabled = %v, want true", loaded.History.Enabled)
	}
	if loaded.History.Path != "~/snowfast-history.db" {
		t.Fatalf("history.path = %q, want preserved value", loaded.History.Path)
	}
}

func TestEnsureMigratedPerProfileIndependent(t *testing.T) {
	migrationEnv(t) // state dir seam only
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.toml")
	pathB := filepath.Join(dir, "b.toml")
	// Both profiles baseline at 0.1; the binary is now 0.2.
	writeStateVer(t, pathA, "0.1")
	writeStateVer(t, pathB, "0.1")
	oldA := []byte("[sfu]\nworkers = 4\n")
	oldB := []byte("[sfu]\nworkers = 5\n")
	writeFile(t, pathA, oldA)
	writeFile(t, pathB, oldB)

	var bufA bytes.Buffer
	EnsureMigrated([]string{"-config", pathA}, "0.2", &bufA)

	// Running A must not mark B done: B still migrates on its own run.
	var bufB bytes.Buffer
	EnsureMigrated([]string{"-config", pathB}, "0.2", &bufB)

	for _, tc := range []struct {
		name, path string
		old        []byte
		buf        *bytes.Buffer
	}{
		{"A", pathA, oldA, &bufA},
		{"B", pathB, oldB, &bufB},
	} {
		got, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "# workers") {
			t.Errorf("profile %s not migrated:\n%s", tc.name, got)
		}
		if v, ok := stateVersion(t, tc.path); !ok || v != "0.2" {
			t.Errorf("profile %s state = %q, ok=%v; want 0.2", tc.name, v, ok)
		}
		if baks := backupsFor(t, tc.path); len(baks) != 1 {
			t.Errorf("profile %s backups = %v, want exactly 1 (independent backup/state)", tc.name, baks)
		}
		if !strings.Contains(tc.buf.String(), "migrated config to 0.2") {
			t.Errorf("profile %s notice missing: %q", tc.name, tc.buf.String())
		}
	}
}

func TestEnsureMigratedFirstEverCustomProfileNotMigrated(t *testing.T) {
	cfgPath := migrationEnv(t)
	old := []byte("[sfu]\nworkers = 4\n")
	writeFile(t, cfgPath, old)

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Errorf("first-ever custom profile must not migrate, config rewritten:\n%s", got)
	}
	if baks := backupsFor(t, cfgPath); len(baks) != 0 {
		t.Errorf("unexpected backups: %v", baks)
	}
	if v, ok := stateVersion(t, cfgPath); !ok || v != "0.2" {
		t.Errorf("state = %q, ok=%v; want 0.2 baseline", v, ok)
	}
	if buf.Len() != 0 {
		t.Errorf("expected silence, got %q", buf.String())
	}
}

func TestEnsureMigratedLegacyStampImportedForDefaultPathOnly(t *testing.T) {
	t.Run("default path imports legacy stamp and migrates", func(t *testing.T) {
		base := t.TempDir()
		if runtime.GOOS == "windows" {
			t.Setenv("AppData", base)
		} else {
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "cfg"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
		}
		writeLegacyStamp(t, "0.1")
		def, err := DefaultPath()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(def), 0o755); err != nil {
			t.Fatal(err)
		}
		old := []byte("[sfu]\nworkers = 4\n")
		writeFile(t, def, old)

		var buf bytes.Buffer
		EnsureMigrated(nil, "0.2", &buf)

		got, err := os.ReadFile(def)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "# workers") {
			t.Errorf("default config not migrated from legacy stamp:\n%s", got)
		}
		if v, ok := stateVersion(t, def); !ok || v != "0.2" {
			t.Errorf("state = %q, ok=%v; want 0.2", v, ok)
		}
	})
	t.Run("custom profile ignores legacy stamp", func(t *testing.T) {
		cfgPath := migrationEnv(t)
		writeLegacyStamp(t, "0.1")
		old := []byte("[sfu]\nworkers = 4\n")
		writeFile(t, cfgPath, old)

		var buf bytes.Buffer
		EnsureMigrated(nil, "0.2", &buf)

		got, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, old) {
			t.Errorf("custom profile must not import the legacy global stamp, config rewritten:\n%s", got)
		}
		if baks := backupsFor(t, cfgPath); len(baks) != 0 {
			t.Errorf("unexpected backups: %v", baks)
		}
		// First observation records its own baseline at the running version.
		if v, ok := stateVersion(t, cfgPath); !ok || v != "0.2" {
			t.Errorf("state = %q, ok=%v; want 0.2 baseline (not imported 0.1)", v, ok)
		}
	})
}

func TestEnsureMigratedStateRecordsReadablePath(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.1")
	writeFile(t, cfgPath, []byte("[sfu]\nworkers = 4\n"))

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	data, err := os.ReadFile(stateFileFor(t, cfgPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "config="+cfgPath+"\n") {
		t.Errorf("state file does not record readable config path:\n%s", data)
	}
}

func TestEnsureMigratedSkipsDowngrade(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "9.9")
	old := []byte("[sfu]\nworkers = 4\n")
	writeFile(t, cfgPath, old)

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Errorf("downgrade rewrote the config:\n%s", got)
	}
	if baks := backupsFor(t, cfgPath); len(baks) != 0 {
		t.Errorf("unexpected backups: %v", baks)
	}
	if v, ok := stateVersion(t, cfgPath); !ok || v != "9.9" {
		t.Errorf("state = %q, ok=%v; want 9.9 untouched", v, ok)
	}
	if buf.Len() != 0 {
		t.Errorf("expected silence, got %q", buf.String())
	}
}

func TestEnsureMigratedDowngradeLegacyStamp(t *testing.T) {
	base := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", base)
	} else {
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "cfg"))
		t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	}
	writeLegacyStamp(t, "9.9")
	def, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(def), 0o755); err != nil {
		t.Fatal(err)
	}
	old := []byte("[sfu]\nworkers = 4\n")
	writeFile(t, def, old)

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	got, err := os.ReadFile(def)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Errorf("downgrade rewrote the config:\n%s", got)
	}
	if baks := backupsFor(t, def); len(baks) != 0 {
		t.Errorf("unexpected backups: %v", baks)
	}
	// No state write on a gated downgrade: the legacy stamp keeps 9.9 and no
	// per-profile state file appears.
	if v, ok := stateVersion(t, def); ok {
		t.Errorf("downgrade must record no state, got version=%q", v)
	}
	if buf.Len() != 0 {
		t.Errorf("expected silence, got %q", buf.String())
	}
}

func TestEnsureMigratedMixedVersionNoThrash(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.1")
	old := []byte("[sfu]\nworkers = 4\n")
	writeFile(t, cfgPath, old)

	// First run (0.2 > 0.1) migrates; the alternation must not thrash.
	var buf1 bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf1)
	migrated, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if baks := backupsFor(t, cfgPath); len(baks) != 1 {
		t.Fatalf("backups after first run = %v, want exactly 1", baks)
	}
	if v, ok := stateVersion(t, cfgPath); !ok || v != "0.2" {
		t.Fatalf("state = %q, ok=%v; want 0.2", v, ok)
	}

	// Alternating binaries must never thrash the config: the newer run finds
	// identical merged bytes (no rewrite, no backup, silent state advance to
	// the newest seen version), the older run is gated outright.
	for i, tc := range []struct {
		ver, wantState string
	}{
		{"0.2", "0.2"}, // fast path: state already 0.2
		{"0.3", "0.3"}, // newer: bytes equal, state advances silently
		{"0.2", "0.3"}, // gated downgrade: nothing recorded
	} {
		var buf bytes.Buffer
		EnsureMigrated(nil, tc.ver, &buf)
		if baks := backupsFor(t, cfgPath); len(baks) != 1 {
			t.Errorf("round %d (%s): backups = %v, want still exactly 1", i, tc.ver, baks)
		}
		got, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, migrated) {
			t.Errorf("round %d (%s): config bytes changed:\n%s", i, tc.ver, got)
		}
		if v, ok := stateVersion(t, cfgPath); !ok || v != tc.wantState {
			t.Errorf("round %d (%s): state = %q, ok=%v; want %q", i, tc.ver, v, ok, tc.wantState)
		}
		if buf.Len() != 0 {
			t.Errorf("round %d (%s): expected silence, got %q", i, tc.ver, buf.String())
		}
	}
}

func TestEnsureMigratedLeavesEmptyConfig(t *testing.T) {
	cfgPath := migrationEnv(t)
	writeStateVer(t, cfgPath, "0.1")
	writeFile(t, cfgPath, []byte{})

	var buf bytes.Buffer
	EnsureMigrated(nil, "0.2", &buf)

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty config was rewritten:\n%q", got)
	}
	if baks := backupsFor(t, cfgPath); len(baks) != 0 {
		t.Errorf("unexpected backups: %v", baks)
	}
	if v, ok := stateVersion(t, cfgPath); !ok || v != "0.2" {
		t.Errorf("state = %q, ok=%v; want 0.2 recorded", v, ok)
	}
	if buf.Len() != 0 {
		t.Errorf("expected silence, got %q", buf.String())
	}
}

func TestEnsureMigratedLeavesCommentsOnlyConfig(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		cfgPath := migrationEnv(t)
		writeStateVer(t, cfgPath, "0.1")
		old := []byte("# snowfast config\n\n# nothing active here\n\n# [sfu]\n# workers = 4\n")
		writeFile(t, cfgPath, old)

		var buf bytes.Buffer
		EnsureMigrated(nil, "0.2", &buf)

		got, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, old) {
			t.Errorf("comments-only config was rewritten:\n%q", got)
		}
		if baks := backupsFor(t, cfgPath); len(baks) != 0 {
			t.Errorf("unexpected backups: %v", baks)
		}
		if v, ok := stateVersion(t, cfgPath); !ok || v != "0.2" {
			t.Errorf("state = %q, ok=%v; want 0.2 recorded", v, ok)
		}
		if buf.Len() != 0 {
			t.Errorf("expected silence, got %q", buf.String())
		}
	})
	t.Run("BOM", func(t *testing.T) {
		cfgPath := migrationEnv(t)
		writeStateVer(t, cfgPath, "0.1")
		old := append([]byte("\xef\xbb\xbf"), []byte("# BOM'd comment header\n\n# workers = 4\n")...)
		writeFile(t, cfgPath, old)

		var buf bytes.Buffer
		EnsureMigrated(nil, "0.2", &buf)

		got, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, old) {
			t.Errorf("BOM'd comments-only config was rewritten:\n%q", got)
		}
		if baks := backupsFor(t, cfgPath); len(baks) != 0 {
			t.Errorf("unexpected backups: %v", baks)
		}
		if v, ok := stateVersion(t, cfgPath); !ok || v != "0.2" {
			t.Errorf("state = %q, ok=%v; want 0.2 recorded", v, ok)
		}
		if buf.Len() != 0 {
			t.Errorf("expected silence, got %q", buf.String())
		}
	})
}

// TestBackupPathForFilenameShape pins the backup filename contract:
// config file name + "." + UTC timestamp in the 20060102T150405Z layout +
// ".bak" suffix at the very end (e.g. config.toml.20260928T154233Z.bak).
func TestBackupPathForFilenameShape(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	got := backupPathFor(cfgPath)

	// Collision suffix (-2, -3, …) now lands before the .bak suffix
	// (config.toml.<ts>-2.bak); HasSuffix already covers both shapes.
	trimmed := got
	if idx := strings.LastIndex(trimmed, ".bak"); idx >= 0 {
		trimmed = trimmed[:idx+4]
	}
	ts := strings.TrimSuffix(strings.TrimPrefix(trimmed, filepath.Join(dir, "config.toml.")), ".bak")
	if _, err := time.Parse("20060102T150405Z", ts); err != nil {
		t.Fatalf("backup name %q: suffix %q is not a 20060102T150405Z UTC timestamp: %v", got, ts, err)
	}
	if !strings.HasSuffix(got, ".bak") {
		t.Errorf("backup name %q must end in .bak", got)
	}
	if !strings.HasPrefix(got, filepath.Join(dir, "config.toml.")) {
		t.Errorf("backup name %q must start with the config path plus a dot", got)
	}
}
