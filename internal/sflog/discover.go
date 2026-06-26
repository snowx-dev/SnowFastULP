package sflog

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// walkSources visits root once, reporting each discovered credential file or
// archive to onFound. A single pass (vs. one walk per source kind) halves the
// up-front scan time on large trees and lets callers stream discovery progress.
// A single-file root is reported directly without walking.
func walkSources(root string, onFound func(path string, isArchive bool)) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		switch {
		case isArchiveFile(root):
			onFound(root, true)
		case isPasswordFile(root):
			onFound(root, false)
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
		switch {
		case isArchiveFile(path):
			onFound(path, true)
		case isPasswordFile(path):
			onFound(path, false)
		}
		return nil
	})
}

func DiscoverPasswordFiles(root string) ([]SourceFile, error) {
	var files []SourceFile
	err := walkSources(root, func(path string, isArchive bool) {
		if !isArchive {
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
	case "passwords", "all passwords", "password list", "_allpasswords_list":
		return true
	}
	return strings.Contains(name, "password")
}
