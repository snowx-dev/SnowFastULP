package discover

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ListTxt walks root recursively, returns sorted *.txt paths. no symlinks. .txt.zst excluded.
func ListTxt(root string) ([]string, error) {
	return listFiles(root, ".txt", "no .txt files under %s")
}

// ListZst walks root recursively, returns sorted *.zst paths. no symlinks.
func ListZst(root string) ([]string, error) {
	return listFiles(root, ".zst", "no .zst files under %s")
}

func listFiles(root, ext, emptyMsg string) ([]string, error) {
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
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf(emptyMsg, root)
	}
	return paths, nil
}
