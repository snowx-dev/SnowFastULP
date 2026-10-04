package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/snowx-dev/SnowFastULP/internal/history"
	"github.com/snowx-dev/SnowFastULP/internal/pathident"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

// deleteParsedSources removes inputs that finished extraction, mirroring sfu's
// post-success deletion. Scope (confirmed with the user):
//   - file input: delete the file itself (archive or loose log) if it finished
//     extraction.
//   - dir input: delete each top-level child under the input root as a unit,
//     but only when every source discovered under it finished extraction
//     (extraction failures — HadIssue without HistoryComplete — preserve the
//     group; parse-quality rejects no longer do, product decision
//     2026-09-30). The input root
//     the user passed is never deleted. Directory children are removed
//     recursively; regular file children are removed individually along with
//     their multi-part volume siblings.
//
// Anything matching a protected path (output file/dir, library, temp) or any
// path containing a protected path is skipped.
var afterHistoryDeleteStage func([]history.StagedPath) error

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
		if len(results) == 1 && !isProtected(absRoot, prot) {
			candidate := results[0].HistoryCandidate
			if len(candidate.Paths) > 0 {
				for _, path := range candidate.Paths {
					if isProtected(path, prot) {
						return nil, nil
					}
				}
				return history.DeleteStaged(context.Background(), candidate.Paths, []history.Candidate{candidate}, afterHistoryDeleteStage)
			}
			// Capture any multi-part siblings (rar volumes or split .NNN parts)
			// before removing the first part, then remove the rest of the set
			// too (best-effort).
			vols := sflog.VolumeSet(absRoot)
			if err := os.Remove(absRoot); err != nil {
				return nil, err
			}
			removed := []string{absRoot}
			removed = append(removed, removeVolumeSiblings(vols, absRoot)...)
			return removed, nil
		}
		return nil, nil
	}

	type group struct {
		child      string
		allOK      bool
		any        bool
		candidates []history.Candidate
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
		if len(r.HistoryCandidate.Paths) > 0 {
			g.candidates = append(g.candidates, r.HistoryCandidate)
		}
		// preservation disabled 2026-09-30 (user): -del deletes every discovered
		// source; the old gate kept failed groups:
		// if !r.OK || (r.HadIssue && !r.HistoryComplete) {
		// 	g.allOK = false
		// }
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
		if len(g.candidates) > 0 {
			roots := []string{g.child}
			if !fi.IsDir() {
				roots = nil
				protectedCandidate := false
				for _, candidate := range g.candidates {
					for _, path := range candidate.Paths {
						if isProtected(path, prot) {
							protectedCandidate = true
							break
						}
						roots = append(roots, path)
					}
					if protectedCandidate {
						break
					}
				}
				if protectedCandidate || len(roots) == 0 {
					continue
				}
			}
			stagedRemoved, err := history.DeleteStaged(context.Background(), roots, g.candidates, afterHistoryDeleteStage)
			removed = append(removed, stagedRemoved...)
			if err != nil {
				return removed, err
			}
			continue
		}
		if fi.IsDir() {
			// Any directory group — including a direct tdata-style child — is
			// removed as a unit; only regular files take the volume-sibling path.
			if err := os.RemoveAll(g.child); err != nil {
				return removed, err
			}
			removed = append(removed, g.child)
		} else {
			vols := sflog.VolumeSet(g.child)
			if err := os.Remove(g.child); err != nil {
				return removed, err
			}
			removed = append(removed, g.child)
			removed = append(removed, removeVolumeSiblings(vols, g.child)...)
		}
	}
	return removed, nil
}

// removeVolumeSiblings removes every multi-part sibling in `vols` (rar volumes
// or split .NNN parts) other than `keep` (which the caller has already deleted).
// Missing parts are ignored so a partially extracted set still cleans up what
// remains.
func removeVolumeSiblings(vols []string, keep string) []string {
	var removed []string
	for _, v := range vols {
		if v == keep {
			continue
		}
		if err := os.Remove(v); err == nil {
			removed = append(removed, v)
		}
	}
	return removed
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
// protected path nested beneath it. Containment is decided by a clean
// relative walk (no ".." components), not a raw string prefix: a sibling
// named like an extension of path (victim2 under victim) must not match.
func coversProtected(path string, protected []string) bool {
	for _, p := range protected {
		rel, err := filepath.Rel(path, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if rel == "." {
			// p == path: equality is isProtected's job.
			continue
		}
		return true
	}
	return false
}
