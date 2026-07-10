package sflog

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sourceKind classifies a discovered file so callers can route it: archives go
// to the extractor, password files to the credential parser, and everything
// else (only surfaced when scanExtra is set) to the secret scanner.
type sourceKind int

const (
	sourceArchive  sourceKind = iota // archive or split/volume part
	sourcePassword                   // credential dump (see isPasswordFile)
	sourceEnv                        // env/key file for -env copy
	sourceOther                      // any other file, reported only when scanExtra
)

// classifySource maps a path to its sourceKind. It returns ok=false for files
// that should be skipped entirely (the default when scanExtra/envExtra are off,
// or a non-allowlisted file when they are on).
func classifySource(path string, scanExtra, envExtra bool) (sourceKind, bool) {
	switch {
	case isArchiveFile(path) || isSplitArchivePart(path):
		return sourceArchive, true
	case isPasswordFile(path):
		return sourcePassword, true
	case envExtra && isEnvCopyCandidate(path):
		return sourceEnv, true
	case scanExtra && isSecretScanCandidate(path):
		return sourceOther, true
	default:
		return 0, false
	}
}

// secretScanExts is the -secrets allowlist of file extensions worth handing to
// the secret scanner: the common carriers of API keys, tokens, private keys and
// credentials. Matched case-insensitively (callers lower the name). Deliberately
// excludes encrypted stores (.kdbx/.jks/.p12) the regex scanner can't read and
// media/binaries where any hit would be noise.
var secretScanExts = map[string]bool{
	// text / documents
	".txt": true, ".text": true, ".md": true, ".rtf": true, ".log": true,
	".csv": true, ".tsv": true, ".doc": true, ".docx": true, ".pdf": true, ".odt": true,
	// config / env
	".env": true, ".ini": true, ".cfg": true, ".conf": true, ".config": true,
	".toml": true, ".yaml": true, ".yml": true, ".json": true, ".xml": true,
	".properties": true,
	// keys / certs (text-encoded)
	".pem": true, ".key": true, ".crt": true, ".cer": true, ".asc": true,
	".ppk": true, ".ovpn": true,
	// scripts / source (frequent hardcoded-secret carriers)
	".sh": true, ".bash": true, ".zsh": true, ".ps1": true, ".bat": true, ".cmd": true,
	".py": true, ".rb": true, ".php": true, ".js": true, ".ts": true,
	".java": true, ".go": true, ".sql": true,
}

// secretScanNames is the -secrets allowlist of well-known secret-bearing files
// that carry no ordinary extension (bare names and dotfiles), matched on the
// full lowercased base name.
var secretScanNames = map[string]bool{
	"credentials": true, "config": true,
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
	".npmrc": true, ".netrc": true, ".pgpass": true, ".htpasswd": true,
	".git-credentials": true, ".pypirc": true, ".dockercfg": true,
}

// isSecretScanCandidate reports whether path is on the -secrets allowlist: a
// known secret-bearing extension, a well-known credential filename, or a .env
// variant (.env, .env.local, .env.production, ...). It gates every side scan so
// a -secrets run reads only files likely to hold secrets instead of every byte
// on disk / in an archive.
func isSecretScanCandidate(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	if secretScanNames[name] {
		return true
	}
	// .env plus dotted variants (.env.local, .env.production); a foo.env is
	// caught by the extension map below.
	if name == ".env" || strings.HasPrefix(name, ".env.") {
		return true
	}
	return secretScanExts[filepath.Ext(name)]
}

// walkSources visits root once, reporting each discovered source and its kind to
// onFound. A single pass (vs. one walk per source kind) halves the up-front scan
// time on large trees and lets callers stream discovery progress. A single-file
// root is reported directly without walking. When scanExtra is set, otherwise-
// skipped files are reported as sourceOther so a -secrets run can scan arbitrary
// loose files; with it off, discovery is byte-for-byte the credential-only walk.
func walkSources(root string, scanExtra, envExtra bool, onFound func(path string, kind sourceKind)) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if kind, ok := classifySource(root, scanExtra, envExtra); ok {
			onFound(root, kind)
		}
		return nil
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if kind, ok := classifySource(path, scanExtra, envExtra); ok {
			onFound(path, kind)
		}
		return nil
	})
}

func discoverPasswordFiles(root string) ([]SourceFile, error) {
	var files []SourceFile
	err := walkSources(root, false, false, func(path string, kind sourceKind) {
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
