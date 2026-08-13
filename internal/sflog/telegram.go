package sflog

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var errTdataPathEscape = errors.New("tdata member path escaped staging root")

// Telegram Desktop stores session state in a folder named tdata.
// The identifying file is key_datas (encrypted localKey); multi-account
// variants are key_data#2s, key_data#3s, … (data name "data", "data#2", … plus
// the fixed "s" suffix Telegram appends). A 16-hex-uppercase sibling subdir
// (md5("data") byte-swapped -> D877F783D5D3EF8C) holds the per-account maps and
// auth-key files. We identify a tdata folder by: a directory named tdata
// containing a key_data* file. That pairing is effectively unique to Telegram
// Desktop; key_datas is not a name any other common app uses.

// isKeyDataFile reports whether name is Telegram Desktop's localKey file:
// key_datas, or the multi-account variants key_data#2s, key_data#3s, …
// Telegram always writes these lowercase; we match case-sensitively to avoid
// false positives on case-sensitive filesystems (Linux/macOS).
func isKeyDataFile(name string) bool {
	if name == "key_datas" {
		return true
	}
	const p = "key_data#"
	if !strings.HasPrefix(name, p) {
		return false
	}
	rest := name[len(p):]
	if len(rest) < 2 || rest[len(rest)-1] != 's' {
		return false
	}
	for _, r := range rest[:len(rest)-1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isRegularKeyDataName reports whether name is a key_data* file that exists
// as a regular file (not a symlink) directly under dir.
func isRegularKeyDataName(dir, name string) bool {
	if !isKeyDataFile(name) {
		return false
	}
	fi, err := os.Lstat(filepath.Join(dir, name))
	return err == nil && fi.Mode().IsRegular()
}

// isTelegramTdataDir reports whether path is a Telegram Desktop tdata folder:
// a directory whose base name is tdata containing a regular key_data* file.
func isTelegramTdataDir(path string) bool {
	if filepath.Base(path) != "tdata" {
		return false
	}
	return dirHasKeyDataFile(path)
}

// tdataMemberPrefix splits an archive member path on its first tdata segment.
// Returns the prefix up to and including tdata, the remainder under it, and
// ok=true when a tdata segment is present. Backslashes (rar on Windows) are
// normalized to slashes. Used to stage archive members into per-victim tdata
// trees; the prefix keeps distinct victims (VictimA/tdata vs VictimB/tdata)
// from collapsing into one staged tree.
func tdataMemberPrefix(memberName string) (prefix, rel string, ok bool) {
	name := strings.ReplaceAll(memberName, "\\", "/")
	parts := strings.Split(name, "/")
	for i, p := range parts {
		if p == "tdata" {
			return strings.Join(parts[:i+1], "/"), strings.Join(parts[i+1:], "/"), true
		}
	}
	return "", "", false
}

// dirHasKeyDataFile reports whether dir directly contains a regular key_data* file.
func dirHasKeyDataFile(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if isRegularKeyDataName(dir, e.Name()) {
			return true
		}
	}
	return false
}

// tdataConfirmedPrefixes returns the tdata prefixes that contain a direct
// key_data* child in names (zip/7z central-directory pre-filter).
func tdataConfirmedPrefixes(names []string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		if tdataMemberIsKeyData(n) {
			p, _, ok := tdataMemberPrefix(n)
			if ok {
				m[p] = true
			}
		}
	}
	return m
}

func tdataMemberIsKeyData(memberName string) bool {
	_, rel, ok := tdataMemberPrefix(memberName)
	if !ok {
		return false
	}
	rel = strings.ReplaceAll(rel, "\\", "/")
	if rel == "" || strings.Contains(rel, "/") {
		return false
	}
	return isKeyDataFile(rel)
}

// tdataStager buffers archive members that live under a tdata/ prefix into a
// private tree on the secrets volume, so a streaming reader can decide "is
// this tdata real?" only after seeing key_datas. At archive EOF, promote()
// moves every confirmed tdata dir into the -env secrets dest; decoys never
// touch the final tdata/ name. Staging lives under env.root so promote is
// same-filesystem (no EXDEV).
type tdataStager struct {
	tempDir     string
	root        string // created lazily on first stage; "" until then
	prefixBytes map[string]int64
	dropped     map[string]bool // prefixes that hit the size cap
}

func newTdataStager(env *EnvCopier) *tdataStager {
	if env == nil {
		return nil
	}
	return &tdataStager{tempDir: env.root}
}

func (s *tdataStager) cleanup() {
	if s != nil && s.root != "" {
		_ = os.RemoveAll(s.root)
	}
}

// stageIfTdata stages a tdata-prefixed member. consumed=true means r was fully
// read (success or drain-on-error). err is the copy/decode error and must
// propagate on streaming formats so a wrong password is not swallowed.
func stageIfTdata(tg *tdataStager, name string, r io.Reader) (consumed bool, err error) {
	if tg == nil {
		return false, nil
	}
	return tg.stage(name, r)
}

// stage writes r's bytes for one tdata-prefixed archive member into the temp
// tree. Returns consumed=false when memberName is not a tdata member (r is
// untouched). Copy errors are returned so RAR CRC/password failures retry.
func (s *tdataStager) stage(memberName string, r io.Reader) (bool, error) {
	prefix, rel, ok := tdataMemberPrefix(memberName)
	if !ok {
		return false, nil
	}
	if s.root == "" {
		if err := os.MkdirAll(s.tempDir, 0o700); err != nil {
			_, _ = io.Copy(io.Discard, r)
			return true, err
		}
		d, err := os.MkdirTemp(s.tempDir, "sfl-tdata-*")
		if err != nil {
			_, _ = io.Copy(io.Discard, r)
			return true, err
		}
		s.root = d
	}
	if s.dropped[prefix] {
		_, _ = io.Copy(io.Discard, r)
		return true, nil
	}
	joined := prefix
	if rel != "" {
		joined = prefix + "/" + rel
	}
	dest := filepath.Join(s.root, safeRelPath(joined))
	if !destUnderRoot(s.root, dest) {
		_, _ = io.Copy(io.Discard, r)
		return true, errTdataPathEscape
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		_, _ = io.Copy(io.Discard, r)
		return true, err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_, _ = io.Copy(io.Discard, r)
		return true, err
	}
	used := s.prefixBytes[prefix]
	remain := tdataCopyMaxBytes - used
	if remain < 0 {
		remain = 0
	}
	n, err := io.Copy(f, io.LimitReader(r, remain+1))
	cerr := f.Close()
	if s.prefixBytes == nil {
		s.prefixBytes = map[string]int64{}
	}
	s.prefixBytes[prefix] = used + n
	if n > remain {
		_, _ = io.Copy(io.Discard, r)
		s.dropPrefix(prefix)
		return true, errTdataOverCap
	}
	if err != nil {
		_ = os.Remove(dest)
		return true, err
	}
	if cerr != nil {
		_ = os.Remove(dest)
		return true, cerr
	}
	return true, nil
}

func (s *tdataStager) dropPrefix(prefix string) {
	if s.dropped == nil {
		s.dropped = map[string]bool{}
	}
	s.dropped[prefix] = true
	if s.root != "" {
		_ = os.RemoveAll(filepath.Join(s.root, safeRelPath(prefix)))
	}
}

// promote moves every confirmed tdata dir staged under root into the -env
// secrets dir via env.PromoteTdata. Returns the first PromoteTdata error so
// the archive can set HadIssue and -del keeps the source.
func (s *tdataStager) promote(env *EnvCopier) error {
	if s == nil || s.root == "" || env == nil {
		return nil
	}
	var first error
	_ = filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if filepath.Base(path) != "tdata" {
			return nil
		}
		if !dirHasKeyDataFile(path) {
			return nil
		}
		if err := env.PromoteTdata(path); err != nil {
			if first == nil {
				first = err
			}
			return nil
		}
		return filepath.SkipDir
	})
	return first
}
