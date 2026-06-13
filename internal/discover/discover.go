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
// files whose mtime is on/after it (used by the --since age filter).
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
			if path != root && shouldSkipDir(path, d) {
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
			return nil, fmt.Errorf("no %s files under %s modified on/after %s",
				ext, root, modifiedAfter.Format(time.RFC3339))
		}
		return nil, fmt.Errorf("no %s files under %s", ext, root)
	}
	return paths, nil
}
