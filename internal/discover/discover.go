package discover

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ListTxt walks root recursively, returns sorted *.txt paths. no symlinks. .txt.zst excluded.
func ListTxt(root string) ([]string, error) {
	return listFiles(root, ".txt", time.Time{})
}

// ListZst walks root recursively, returns sorted *.zst paths. no symlinks.
func ListZst(root string) ([]string, error) {
	return listFiles(root, ".zst", time.Time{})
}

// ListTxtSince is ListTxt limited to files whose mtime is on/after modifiedAfter.
func ListTxtSince(root string, modifiedAfter time.Time) ([]string, error) {
	return listFiles(root, ".txt", modifiedAfter)
}

// ListZstSince is ListZst limited to files whose mtime is on/after modifiedAfter.
func ListZstSince(root string, modifiedAfter time.Time) ([]string, error) {
	return listFiles(root, ".zst", modifiedAfter)
}

// listFiles walks root for files with ext. A non-zero modifiedAfter keeps only
// files whose mtime is on/after it (used by the -since age filter).
func listFiles(root, ext string, modifiedAfter time.Time) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", root)
	}

	var paths []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root && shouldSkipDir(d) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ext) {
			if !modifiedAfter.IsZero() {
				info, ierr := d.Info()
				if ierr != nil {
					return ierr
				}
				if info.ModTime().Before(modifiedAfter) {
					return nil
				}
			}
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		if !modifiedAfter.IsZero() {
			// An empty -since window is a normal filter outcome, not a
			// failure: the caller treats this sentinel as an empty result
			// (exit 0 with a stderr note), not a hard error.
			return nil, fmt.Errorf("no %s files under %s modified on/after %s — everything there is older than the window",
				ext, root, modifiedAfter.Format(time.RFC3339))
		}
		// Empty root: nothing was discovered. The caller maps this to the
		// shared nothing-usable exit code (internal/exitcode: 4, matching
		// sfu/sfl); the hint makes the likely fix obvious.
		if ext == ".zst" {
			return nil, fmt.Errorf("no .zst files under %s — the path may be wrong or the library is empty (has sfl -od ingested anything there yet?)", root)
		}
		return nil, fmt.Errorf("no %s files under %s — the path may be wrong or contain no matching files", ext, root)
	}
	return paths, nil
}

// IsEmptyResult reports whether err is a discovery "matched nothing" outcome
// (empty root or an empty -since window) rather than a hard failure. The
// caller decides the exit code: an empty root is nothing-usable, an empty
// window is a clean empty result.
func IsEmptyResult(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "no .") && strings.Contains(msg, " files under ")
}
