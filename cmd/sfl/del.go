package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/snowx-dev/SnowFastULP/internal/pathident"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

// deleteParsedSources removes inputs that parsed cleanly, mirroring sfu's
// post-success deletion. Scope (confirmed with the user):
//   - file input: delete the file itself (archive or loose log) if it parsed OK.
//   - dir input: delete each top-level child under the input root as a unit,
//     but only when every source discovered under it parsed OK. The input root
//     the user passed is never deleted.
//
// Anything matching a protected path (output file/dir, library, temp) or any
// path containing a protected path is skipped.
func deleteParsedSources(inputRoot string, results []sflog.SourceResult, protected []string) ([]string, error) {
	info, err := os.Stat(inputRoot)
	if err != nil {
		return nil, err
	}
	absRoot, err := absClean(inputRoot)
	if err != nil {
		return nil, err
	}
	prot, err := cleanAll(protected)
	if err != nil {
		return nil, err
	}

	if !info.IsDir() {
		if len(results) == 1 && results[0].OK && !isProtected(absRoot, prot) {
			if err := os.Remove(absRoot); err != nil {
				return nil, err
			}
			return []string{absRoot}, nil
		}
		return nil, nil
	}

	type group struct {
		child   string
		allOK   bool
		any     bool
		isChild bool // a direct file child (delete file, not dir)
	}
	groups := map[string]*group{}
	for _, r := range results {
		abs, err := absClean(r.Path)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(absRoot, abs)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue // outside the input root; never touch
		}
		first := rel
		if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
			first = rel[:i]
		}
		childAbs := filepath.Join(absRoot, first)
		g := groups[childAbs]
		if g == nil {
			g = &group{child: childAbs, allOK: true}
			groups[childAbs] = g
		}
		g.any = true
		if !r.OK {
			g.allOK = false
		}
		if childAbs == abs {
			g.isChild = true
		}
	}

	var removed []string
	for _, g := range groups {
		if !g.any || !g.allOK {
			continue
		}
		if isProtected(g.child, prot) || coversProtected(g.child, prot) {
			continue
		}
		fi, err := os.Stat(g.child)
		if err != nil {
			continue // already gone
		}
		if fi.IsDir() && !g.isChild {
			if err := os.RemoveAll(g.child); err != nil {
				return removed, err
			}
		} else {
			if err := os.Remove(g.child); err != nil {
				return removed, err
			}
		}
		removed = append(removed, g.child)
	}
	return removed, nil
}

func absClean(p string) (string, error) {
	a, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(a), nil
}

func cleanAll(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		a, err := absClean(p)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func isProtected(path string, protected []string) bool {
	for _, p := range protected {
		if p == path {
			return true
		}
		if same, err := pathident.SameFile(path, p); err == nil && same {
			return true
		}
	}
	return false
}

// coversProtected reports whether deleting dir `path` would also remove a
// protected path nested beneath it.
func coversProtected(path string, protected []string) bool {
	prefix := path + string(filepath.Separator)
	for _, p := range protected {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
