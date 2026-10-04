package sflog

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sourceKind classifies a discovered file so callers can route it: archives go
// to the extractor, password files to the credential parser, and env/key files
// and Telegram tdata folders to the -env copier.
type sourceKind int

const (
	sourceArchive  sourceKind = iota // archive or split/volume part
	sourcePassword                   // credential dump (see isPasswordFile)
	sourceEnv                        // env/key file for -env copy
	sourceTelegram                   // Telegram tdata folder for -env whole-tree copy
)

// classifySource maps a path to its sourceKind. It returns ok=false for files
// that should be skipped entirely (the default when envExtra is off).
func classifySource(path string, envExtra bool) (sourceKind, bool) {
	switch {
	case isArchiveFile(path) || isSplitArchivePart(path) || isOldStyleRarPart(path):
		return sourceArchive, true
	case isPasswordFile(path):
		return sourcePassword, true
	case envExtra && isEnvCopyCandidate(path):
		return sourceEnv, true
	default:
		return 0, false
	}
}

// walkSources visits root once, reporting each discovered source and its kind to
// onFound. A single pass (vs. one walk per source kind) halves the up-front scan
// time on large trees and lets callers stream discovery progress. A single-file
// root is reported directly without walking. When envExtra is set, env/key files
// are reported as sourceEnv and Telegram tdata folders as sourceTelegram.
func walkSources(root string, envExtra bool, onFound func(path string, kind sourceKind)) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if kind, ok := classifySource(root, envExtra); ok {
			onFound(root, kind)
		}
		return nil
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// A Telegram tdata folder is copied as a whole tree under -env.
			// Emit it as its own source and skip the interior so the flat -env
			// path does not also walk key_datas / maps0 / cache trees one file
			// at a time (double copy + wasted I/O on potentially huge caches).
			if envExtra && isTelegramTdataDir(path) {
				onFound(path, sourceTelegram)
				return filepath.SkipDir
			}
			return nil
		}
		if kind, ok := classifySource(path, envExtra); ok {
			onFound(path, kind)
		}
		return nil
	})
}

func discoverPasswordFiles(root string) ([]SourceFile, error) {
	var files []SourceFile
	err := walkSources(root, false, func(path string, kind sourceKind) {
		if kind == sourcePassword {
			files = append(files, SourceFile{Path: path})
		}
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// logGroupKey maps a discovered source onto its "log" unit: one top-level
// subfolder under the input root, or the source itself when it sits directly
// under the root (loose file or archive). A single-file input is one log.
// Mirrors the -del grouping so counts and deletion agree.
func logGroupKey(absRoot string, rootIsDir bool, path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.Clean(abs)
	if !rootIsDir {
		return absRoot
	}
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil {
		return abs
	}
	if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
		return filepath.Join(absRoot, rel[:i])
	}
	return abs
}

func isPasswordFile(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	switch filepath.Ext(name) {
	case ".txt", ".log":
	default:
		return false
	}
	if strings.Contains(name, "passwordcracker") {
		return false
	}
	base := strings.TrimSuffix(strings.TrimSuffix(name, ".txt"), ".log")
	switch base {
	case "passwords", "all passwords", "password list", "_allpasswords_list", "pws":
		// "pws" is Raccoon's credential dump; the rest are the common
		// aggregate names emitted by RedLine/Vidar/Lumma/StealC/Meta.
		return true
	}
	if strings.Contains(name, "password") || strings.Contains(name, "logins") {
		// "logins" catches non-RedLine families whose dump is named
		// Logins_<Browser>.txt (HESOYAM) or <profile>_logins.txt (Firefox
		// exports) and never contains the "password" token.
		return true
	}
	// Some families drop per-browser dumps into a Passwords/ or Logins/
	// directory with browser-only filenames (Chrome.txt, Chrome_Default[..].txt),
	// so the name carries no credential token — key off the parent directory.
	// HESOYAM even ships a decoy root Passwords.txt (Telegram advert, zero
	// creds) while the real credentials live in Passwords/<Browser>.txt.
	switch strings.ToLower(filepath.Base(filepath.Dir(path))) {
	case "logins", "passwords":
		return true
	}
	return false
}
