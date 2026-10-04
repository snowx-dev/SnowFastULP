package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
		SFU     SFUSection     `toml:"sfu"`
		SFS     SFSSection     `toml:"sfs"`
		SFL     SFLSection     `toml:"sfl"`
		History HistorySection `toml:"history"`
	}
	md, err := toml.DecodeFile(path, &raw)
	if err != nil {
		return File{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	// Typos must fail loudly instead of silently falling back to defaults.
	// Migration classification already drops deprecated keys before the
	// generated temp is validated through Load, so production configs are
	// expected to be clean; the example is clean by construction.
	if undec := md.Undecoded(); len(undec) > 0 {
		names := make([]string, 0, len(undec))
		for _, k := range undec {
			names = append(names, k.String())
		}
		sort.Strings(names)
		return File{}, fmt.Errorf("config: unknown key(s): %s", strings.Join(names, ", "))
	}

	// Both o and od may coexist in the config: when no CLI output flag is
	// given, ApplySFU/ApplySFL pick -od (library mode) in priority over -o.
	// Any CLI -o/-od/-odr suppresses the config pull so CLI wins.
	return File{
		path:    path,
		SFU:     raw.SFU,
		SFS:     raw.SFS,
		SFL:     raw.SFL,
		History: raw.History,
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
