// Package pathident reports whether two paths refer to the same on-disk file.
// uses os.SameFile, catches `./x` vs `x`, case folding, hardlink/symlink aliases
package pathident

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// SameFile reports whether a and b point at the same file.
// returns (false, nil) if either doesnt exist, for preflight checks
func SameFile(a, b string) (bool, error) {
	infoA, errA := os.Stat(a)
	if errA != nil {
		if os.IsNotExist(errA) {
			return false, nil
		}
		return false, errA
	}
	infoB, errB := os.Stat(b)
	if errB != nil {
		if os.IsNotExist(errB) {
			return false, nil
		}
		return false, errB
	}
	return os.SameFile(infoA, infoB), nil
}

// CanonicalProspective resolves symlinks in the deepest existing ancestor of
// path, preserving the requested suffix when the target does not exist. This
// lets a not-yet-created output path be compared against existing input paths
// without pretending the output already exists.
func CanonicalProspective(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	cur := abs
	var suffix []string
	for {
		_, statErr := os.Stat(cur)
		if statErr == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return filepath.Clean(real), nil
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", statErr
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

// WithinDir reports whether path is dir itself or lies inside it. Both paths
// are taken as given (callers canonicalize first).
func WithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// maxSymlinkHops bounds the Readlink loop; it matches the kernel's ELOOP
// depth, so a real chain never exhausts it.
const maxSymlinkHops = 40

// LinkResolution follows the symlink chain starting at path's final component
// and returns the canonical spelling of every referent, one per hop. A
// dangling referent is still included: CanonicalProspective resolves its
// deepest existing ancestor and preserves the not-yet-created suffix, which is
// exactly the identity a preflight check needs — the stream's create would
// follow the link onto that path. A path whose final component is not a
// symlink (including a nonexistent one) returns nil. A hop cycle or an
// unresolvable ancestor is an error.
func LinkResolution(path string) ([]string, error) {
	var refs []string
	cur := path
	for range maxSymlinkHops {
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) && len(refs) > 0 {
				return refs, nil
			}
			return nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return refs, nil
		}
		dest, err := os.Readlink(cur)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(cur), dest)
		}
		canonical, err := CanonicalProspective(dest)
		if err != nil {
			return nil, err
		}
		refs = append(refs, canonical)
		cur = canonical
	}
	return nil, errors.New("symlink chain too deep (cycle?)")
}

// LinkRefersTo reports whether path's final component is a symlink whose
// referent chain identifies target: per-hop canonical equality or, when both
// exist, filesystem identity. A non-symlink path never refers anywhere.
func LinkRefersTo(path, target string) bool {
	refs, err := LinkResolution(path)
	if err != nil {
		return false
	}
	if len(refs) == 0 {
		return false
	}
	tgt, err := CanonicalProspective(target)
	if err != nil {
		return false
	}
	for _, r := range refs {
		if r == tgt {
			return true
		}
		if same, serr := SameFile(r, target); serr == nil && same {
			return true
		}
	}
	return false
}

// LinkRefersWithinDir reports whether path's final component is a symlink
// whose referent chain lands on dir or inside it. The chain is canonicalized
// per hop with dangling referents preserved — exactly the identity a create
// through the link would materialize — and each referent is tested with
// WithinDir against dir's canonical spelling. A non-symlink path never refers
// anywhere. Callers pair it with the plain WithinDir(target, dir) compare,
// which covers non-symlink targets; existing referents are additionally
// caught by that compare, since CanonicalProspective follows them.
func LinkRefersWithinDir(path, dir string) bool {
	refs, err := LinkResolution(path)
	if err != nil {
		return false
	}
	dc, err := CanonicalProspective(dir)
	if err != nil {
		return false
	}
	for _, r := range refs {
		if WithinDir(r, dc) {
			return true
		}
	}
	return false
}
