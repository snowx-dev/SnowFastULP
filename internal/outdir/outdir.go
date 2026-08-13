// Package outdir validates and prepares output directory paths shared by the
// sfu and sfl CLIs. The three helpers cover the full -o / -od / -odr lifecycle:
//
//   - ResolveDir validates the raw flag value (empty = CWD; -od/-odr are always
//     directories; -o requires a dir hint so -o cleaned.txt is rejected).
//   - IsDirHint is the dir-shape test ResolveDir uses for -o.
//   - EnsureReady rejects a path that exists and is not a directory, and when
//     create is true (non-dry-run) creates missing parents with MkdirAll.
//
// All three are pure filesystem helpers; no CLI coupling, so they unit-test
// without running main.
package outdir

import (
	"fmt"
	"os"
	"strings"
)

// ResolveDir validates an output dir flag value. Empty returns ".", autoMkdir=true.
// -od/-odr are always directories (autoMkdir=true, no dir-hint check).
// -o requires a dir hint (trailing separator or existing directory) so file-path
// muscle memory like "-o cleaned.txt" is rejected.
func ResolveDir(flagName, userOut string) (dir string, autoMkdir bool, err error) {
	userOut = strings.TrimSpace(userOut)
	if userOut == "" {
		return ".", true, nil
	}
	switch flagName {
	case "-od", "-odr":
		return userOut, true, nil
	}
	if !IsDirHint(userOut) {
		return "", false, fmt.Errorf(
			"%s must be a directory (trailing %q or existing directory); got %q — use e.g. %s ./out/",
			flagName, string(os.PathSeparator), userOut, flagName)
	}
	return userOut, true, nil
}

// IsDirHint reports whether p looks like a directory: trailing separator, or
// stats as an existing directory. Stat errors fall through to false. Used for
// -o only; -od/-odr bypass this check.
func IsDirHint(p string) bool {
	if strings.HasSuffix(p, "/") || strings.HasSuffix(p, string(os.PathSeparator)) {
		return true
	}
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return true
	}
	return false
}

// EnsureReady rejects a path that exists and is not a directory. When create is
// true (non-dry-run), missing parents are created with MkdirAll. The Stat guard
// runs for both dry-run and non-dry-run so -odr against an existing file is
// rejected before the pipeline starts.
func EnsureReady(flagName, absDir string, create bool) error {
	if fi, err := os.Stat(absDir); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s path exists and is not a directory: %s", flagName, absDir)
	}
	if create {
		if err := os.MkdirAll(absDir, 0o755); err != nil {
			return fmt.Errorf("create output dir: %w", err)
		}
	}
	return nil
}
