package config

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed" // required for //go:embed (no other use)
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/snowx-dev/SnowFastULP/internal/atomicfs"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

// exampleTOML is the embedded new-format template. MUST stay byte-identical to
// the repo-root config.toml.example (enforced by TestEmbeddedExampleMatchesRootExample).
//
//go:embed example.toml
var exampleTOML []byte

const (
	runStampName = "run.stamp" // legacy global stamp, imported once per default config
	stateDirName = "config-state"
	utf8BOM      = "\ufeff"
)

var (
	headerRe = regexp.MustCompile(`^\[(sfu|sfs|sfl|history)\]\s*$`)
	// same shape as internal/config/example_parse_test.go:27
	keyRe = regexp.MustCompile(`^(\s*)#\s*([A-Za-z_][A-Za-z0-9_]*)(\s*=)`)
)

// EnsureMigrated runs the first-run-after-update config migration. argv is
// os.Args[1:]; ver is the running version (version.String); notices/warnings
// are written as plain lines to w (nil = silent). It never returns an error
// and never aborts startup: every failure degrades to leaving the existing
// config untouched.
//
// Migration state is tracked per resolved config path (a versioned state file
// under DefaultDataDir keyed by SHA-256 of the cleaned absolute path), so
// each profile migrates independently. A missing config records no state.
// The legacy global run.stamp is imported only for the default config path,
// then never consulted again. Migration is forward-only: it runs when the
// running version is newer than the stored (or legacy-imported) version. An
// older binary leaves a newer-migrated config untouched and records nothing,
// so the state keeps the newest version it has seen.
func EnsureMigrated(argv []string, ver string, w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	if ver == "" {
		return
	}
	cfgPath, _, err := ResolveConfigPath(argv)
	if err != nil {
		return // Load will surface the resolution error as it does today
	}
	sp := statePathFor(cfgPath)
	var cur string
	haveState := false
	if sp != "" {
		cur, haveState = readStateVersion(sp)
		if !haveState {
			// Legacy import: the old global run.stamp counts as prior state
			// for the default config path only, so existing installs do not
			// re-migrate once. Custom profiles establish their own baseline.
			if isDefaultConfigPath(cfgPath) {
				if legacy, ok := readStampVersion(legacyStampPath()); ok {
					cur, haveState = legacy, true
				}
			}
		}
	}
	if haveState && cur == ver {
		return // fast path: this profile already ran this version
	}
	if haveState && version.Compare(cur, ver) > 0 {
		return // older binary: never rewrite a newer-migrated config; state keeps the newest version
	}
	if info, err := os.Lstat(cfgPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintf(w, "snowfast: config migration skipped because config path %q is a symlink; migrate the target or replace the link deliberately\n", cfgPath)
		return
	}
	oldBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		// Missing or unreadable config: record no state; Load surfaces the
		// real error next (a missing config must never advance the stamp).
		return
	}
	if info, err := os.Stat(cfgPath); err != nil || info.IsDir() {
		return // Load produces the authoritative error
	}
	if !haveState {
		// First observation of this profile: record the current version and
		// do not migrate (first-run safety, matching the old fresh-stamp
		// behavior). With an unresolvable data dir there is nowhere to record
		// state, so this profile is left untouched: recordState is skipped
		// and the function returns without migrating.
		if sp != "" {
			recordState(sp, cfgPath, ver, w)
		}
		return
	}
	if !hasActiveTOML(oldBytes) {
		recordState(sp, cfgPath, ver, w)
		return // no settings worth preserving: leave the config untouched
	}
	var all map[string]any
	if _, err := toml.Decode(string(oldBytes), &all); err != nil {
		fmt.Fprintf(w, "snowfast: config parse failed, skipping migration: %v\n", err)
		return // no state update: retry after the user fixes the file
	}
	pending, dropped := classifyOld(all, templateKeys(exampleTOML))
	merged, _, err := mergeTemplate(exampleTOML, pending)
	if err != nil {
		fmt.Fprintf(w, "snowfast: config parse failed, skipping migration: %v\n", err)
		return
	}
	if bytes.Equal(merged, oldBytes) {
		recordState(sp, cfgPath, ver, w) // already migrated (or nothing to change): silent
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(cfgPath), filepath.Base(cfgPath)+".migrate-*")
	if err != nil {
		return // read-only dir etc.; silent skip
	}
	tmpName := tmp.Name()
	mode := os.FileMode(0o644)
	if info, err := os.Stat(cfgPath); err == nil {
		mode = info.Mode().Perm()
	}
	if _, err := tmp.Write(merged); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	tmp.Close()
	os.Chmod(tmpName, mode)
	if _, err := Load(tmpName, true); err != nil { // validate via the production path
		os.Remove(tmpName)
		fmt.Fprintf(w, "snowfast: migrated config failed validation, skipping migration: %v\n", err)
		return // no stamp update: retry after the user fixes the file
	}
	backup := backupPathFor(cfgPath) // "<base>.<20060102T150405Z ts>.bak", -2/-3… on collision
	if err := copyFile(backup, cfgPath, mode); err != nil {
		os.Remove(tmpName)
		fmt.Fprintf(w, "snowfast: could not write config backup, skipping migration: %v\n", err)
		return
	}
	if err := atomicfs.Rename(tmpName, cfgPath); err != nil {
		os.Remove(tmpName)
		fmt.Fprintf(w, "snowfast: could not replace config, skipping migration: %v\n", err)
		return
	}
	msg := fmt.Sprintf("snowfast: migrated config to %s (backup: %s)", ver, backup)
	if len(dropped) > 0 {
		msg += "; dropped: " + strings.Join(dropped, ", ")
	}
	fmt.Fprintln(w, msg)
	recordState(sp, cfgPath, ver, w)
}

// recordState writes the per-profile state file, reporting failures as a
// warning (never an error): the next run re-derives the migration and skips
// on identical merged bytes. The recorded version is forward-only: ver is
// written only when it is not older than the stored version, so a stale
// record from a concurrent older run cannot clobber the newest version the
// config has been migrated to.
func recordState(sp, cfgPath, ver string, w io.Writer) {
	if sp == "" {
		return
	}
	if cur, ok := readStateVersion(sp); ok && version.Compare(cur, ver) > 0 {
		return // forward-only bookkeeping: never record an older version
	}
	if err := writeState(sp, cfgPath, ver); err != nil {
		fmt.Fprintf(w, "snowfast: could not record migration state: %v\n", err)
	}
}

// absConfigPath returns the cleaned absolute form of a config path. Relative
// paths resolve against the process CWD, matching CLI-flag semantics.
func absConfigPath(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(cwd, p))
}

// isDefaultConfigPath reports whether cfgPath resolves to the platform
// default config location (the only path that may import the legacy stamp).
func isDefaultConfigPath(cfgPath string) bool {
	def, err := DefaultPath()
	if err != nil {
		return false
	}
	return absConfigPath(cfgPath) == absConfigPath(def)
}

// statePathFor returns the per-profile migration state file path: a versioned
// state directory under the snowfast data dir, keyed by the SHA-256 of the
// cleaned absolute config path, or "" when the data dir cannot be resolved.
func statePathFor(cfgPath string) string {
	dir, err := DefaultDataDir()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(absConfigPath(cfgPath)))
	return filepath.Join(dir, stateDirName, hex.EncodeToString(sum[:])+".state")
}

// legacyStampPath returns the legacy global run.stamp path in the snowfast
// data dir, or "" when the data dir cannot be resolved. It is imported only
// for the default config path.
func legacyStampPath() string {
	dir, err := DefaultDataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, runStampName)
}

// readStampVersion returns the recorded legacy-stamp version, ok=false when
// the stamp is missing, unreadable, corrupt, or carries no non-empty version=
// line. readStateVersion shares the format.
func readStampVersion(p string) (string, bool) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	data = bytes.TrimPrefix(data, []byte(utf8BOM))
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if val, ok := strings.CutPrefix(line, "version="); ok {
			return strings.TrimSpace(val), true
		}
	}
	return "", false
}

// readStateVersion returns the recorded version of a per-profile state file,
// ok=false when the file is missing, unreadable, corrupt, or carries no
// non-empty version= line.
func readStateVersion(p string) (string, bool) {
	return readStampVersion(p)
}

// writeState records the profile's config path, version, and a last_run
// timestamp atomically-enough: MkdirAll 0o755 then os.WriteFile 0o644. Torn
// writes degrade to "corrupt state" = first-run semantics, which is safe.
func writeState(p, cfgPath, ver string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	content := "config=" + cfgPath + "\nversion=" + ver + "\nlast_run=" + time.Now().UTC().Format(time.RFC3339) + "\n"
	return os.WriteFile(p, []byte(content), 0o644)
}

// hasActiveTOML reports whether b contains any active TOML content: a line
// that is neither blank nor a comment (BOM-tolerant). A config with no
// active keys carries no settings worth preserving — leave it untouched.
// Line-level check: a bare [section] header still counts as active (and so
// still migrates); a multi-line string cannot hide from this because its
// key = """ opener line is itself active.
func hasActiveTOML(b []byte) bool {
	b = bytes.TrimPrefix(b, []byte(utf8BOM))
	for _, line := range bytes.Split(b, []byte("\n")) {
		t := strings.TrimSpace(string(line))
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return true
	}
	return false
}

// templateKeys returns the documented key set of the template:
// "section.key" for every commented key line under an active [section] header.
func templateKeys(tmpl []byte) map[string]bool {
	known := make(map[string]bool)
	section := ""
	for _, ln := range strings.Split(string(tmpl), "\n") {
		if m := headerRe.FindStringSubmatch(ln); m != nil {
			section = m[1]
			continue
		}
		if m := keyRe.FindStringSubmatch(ln); m != nil {
			known[section+"."+m[2]] = true
		}
	}
	return known
}

// classifyOld splits decoded old-config values into per-section pending
// values whose "section.key" is documented, and sorted dropped names
// ("[section] key"). Top-level scalars and unknown sections are dropped.
func classifyOld(all map[string]any, known map[string]bool) (map[string]map[string]any, []string) {
	pending := make(map[string]map[string]any)
	var dropped []string
	for k, v := range all {
		sub, ok := v.(map[string]any)
		if !ok {
			dropped = append(dropped, "[] "+k) // already ignored by Load; odd bracket form is intentional
			continue
		}
		for sk, sv := range sub {
			if known[k+"."+sk] {
				if pending[k] == nil {
					pending[k] = make(map[string]any)
				}
				pending[k][sk] = sv
			} else {
				dropped = append(dropped, "["+k+"] "+sk)
			}
		}
	}
	sort.Strings(dropped)
	return pending, dropped
}

// encodeValue renders one "key = value" TOML line-chunk via toml.NewEncoder.
func encodeValue(key string, val any) (string, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(map[string]any{key: val}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// mergeTemplate returns the template with each pending value inserted as an
// active line right after its commented key line. Pending keys with no
// commented line in the template (theoretically impossible: both derive from
// the same bytes) come back as a sorted remainder. An encode error yields
// (nil, nil, err); the caller skips migration like a parse failure.
func mergeTemplate(tmpl []byte, pending map[string]map[string]any) ([]byte, []string, error) {
	lines := strings.Split(string(tmpl), "\n")
	out := make([]string, 0, len(lines))
	section := ""
	for _, ln := range lines {
		if m := headerRe.FindStringSubmatch(ln); m != nil {
			section = m[1]
			out = append(out, ln)
			continue
		}
		if m := keyRe.FindStringSubmatch(ln); m != nil {
			if val, ok := pending[section][m[2]]; ok {
				chunk, err := encodeValue(m[2], val)
				if err != nil {
					return nil, nil, err
				}
				out = append(out, ln)
				out = append(out, strings.Split(strings.TrimSuffix(chunk, "\n"), "\n")...)
				delete(pending[section], m[2])
				continue
			}
		}
		out = append(out, ln)
	}
	var remainder []string
	for sec, keys := range pending {
		for k := range keys {
			remainder = append(remainder, "["+sec+"] "+k)
		}
	}
	sort.Strings(remainder)
	return []byte(strings.Join(out, "\n")), remainder, nil
}

// backupPathFor picks "<base>.<UTC ts>.bak", appending -2, -3, … before the
// .bak suffix while the name is taken. Lstat makes dangling symlinks count as
// occupied; O_EXCL keeps the final pick race-safe.
func backupPathFor(cfgPath string) string {
	return backupPathForAt(cfgPath, time.Now().UTC())
}

func backupPathForAt(cfgPath string, now time.Time) string {
	dir, base := filepath.Dir(cfgPath), filepath.Base(cfgPath)
	ts := now.Format("20060102T150405Z")
	stem := filepath.Join(dir, base+"."+ts)
	cand := stem + ".bak"
	for i := 2; ; i++ {
		if _, err := os.Lstat(cand); os.IsNotExist(err) {
			return cand
		}
		cand = fmt.Sprintf("%s-%d.bak", stem, i)
	}
}

// copyFile byte-copies src to dst with the given mode. O_EXCL keeps the
// backupPathFor collision loop race-free; on any error dst is removed.
func copyFile(dst, src string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}
