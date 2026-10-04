package durablefs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// SyncPaths makes file contents and the directory entries naming them durable.
// Directory paths are walked recursively. Empty input performs no filesystem I/O.
//
// Directory syncing is deliberately scoped: only directories whose entries
// changed are synced — the output tree itself plus the immediate parent of
// each supplied root — never the whole ancestor chain up to the volume root.
func SyncPaths(paths []string) error {
	files, dirs, err := collectTargets(paths)
	if err != nil {
		return err
	}

	for _, path := range files {
		// FlushFileBuffers requires a write-capable handle on Windows. These are
		// outputs created by the current run, so opening read/write is both valid
		// and necessary; Unix fsync accepts the same descriptor.
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open output for sync %s: %w", path, err)
		}
		err = file.Sync()
		closeErr := file.Close()
		if err != nil {
			return fmt.Errorf("sync output file %s: %w", path, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close synced output file %s: %w", path, closeErr)
		}
	}

	for _, path := range dirs {
		if err := syncDirectory(path); err != nil {
			return fmt.Errorf("sync output directory %s: %w", path, err)
		}
	}
	return nil
}

// collectTargets computes the regular files to sync and the directories to
// dir-sync for the supplied output paths: the files themselves, every
// directory in a supplied directory tree, and the immediate parent of each
// supplied root — the only directories whose entries this run can have
// changed.
func collectTargets(paths []string) (files, dirs []string, err error) {
	fileSet := make(map[string]struct{})
	dirSet := make(map[string]struct{})
	for _, path := range paths {
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, nil, fmt.Errorf("sync output %s: %w", path, err)
		}
		switch {
		case info.Mode().IsRegular():
			fileSet[path] = struct{}{}
			dirSet[filepath.Dir(path)] = struct{}{}
		case info.IsDir():
			if err := filepath.WalkDir(path, func(child string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Type().IsRegular() {
					fileSet[child] = struct{}{}
				} else if entry.IsDir() {
					dirSet[child] = struct{}{}
				}
				return nil
			}); err != nil {
				return nil, nil, fmt.Errorf("walk output %s: %w", path, err)
			}
			dirSet[filepath.Dir(path)] = struct{}{}
		default:
			return nil, nil, fmt.Errorf("sync output %s: not a regular file or directory", path)
		}
	}

	files = make([]string, 0, len(fileSet))
	for path := range fileSet {
		files = append(files, path)
	}
	sort.Strings(files)
	// Deepest first so child directories are flushed before their parents.
	dirs = make([]string, 0, len(dirSet))
	for path := range dirSet {
		dirs = append(dirs, path)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})
	return files, dirs, nil
}
