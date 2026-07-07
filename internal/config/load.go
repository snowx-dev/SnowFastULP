package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Load reads path. Missing file returns zero File, nil. Explicit + missing = err.
func Load(path string, explicit bool) (File, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return File{}, nil
	}
	path = filepath.Clean(path)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if explicit {
				return File{}, fmt.Errorf("config: file not found: %s", path)
			}
			return File{}, nil
		}
		return File{}, fmt.Errorf("config: %s: %w", path, err)
	}
	if info.IsDir() {
		return File{}, fmt.Errorf("config: %s is a directory", path)
	}

	var raw struct {
		SFU SFUSection `toml:"sfu"`
		SFS SFSSection `toml:"sfs"`
		SFL SFLSection `toml:"sfl"`
	}
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return File{}, fmt.Errorf("config: parse %s: %w", path, err)
	}

	// Both o and od may coexist in the config: when no CLI output flag is
	// given, ApplySFU/ApplySFL pick -od (library mode) in priority over -o.
	// Any CLI -o/-od/-odr suppresses the config pull so CLI wins.
	baseDir := filepath.Dir(path)
	return File{
		path:    path,
		baseDir: baseDir,
		SFU:     raw.SFU,
		SFS:     raw.SFS,
		SFL:     raw.SFL,
	}, nil
}

// LoadFromArgv resolves the config path from argv and loads it.
func LoadFromArgv(argv []string) (File, error) {
	path, explicit, err := ResolveConfigPath(argv)
	if err != nil {
		return File{}, err
	}
	return Load(path, explicit)
}
