package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ResolvePath joins rel onto baseDir, expands leading ~. no env var expansion.
func ResolvePath(baseDir, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", nil
	}
	if rel == "~" || strings.HasPrefix(rel, "~/") || strings.HasPrefix(rel, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("config: expand ~: %w", err)
		}
		if rel == "~" {
			return filepath.Clean(home), nil
		}
		rel = filepath.Join(home, rel[2:])
		return filepath.Clean(rel), nil
	}
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel), nil
	}
	if baseDir == "" {
		return filepath.Clean(rel), nil
	}
	return filepath.Clean(filepath.Join(baseDir, rel)), nil
}

// DefaultPath returns the platform default config file location.
// linux/mac: ~/.config/snowfast/config.toml. windows: %AppData%\snowfast\config.toml
func DefaultPath() (string, error) {
	switch runtime.GOOS {
	case "windows":
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "snowfast", "config.toml"), nil
	default:
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".config")
		}
		return filepath.Join(base, "snowfast", "config.toml"), nil
	}
}

// PathFromEnv returns SNOWFAST_CONFIG when set.
func PathFromEnv() (string, bool) {
	p := strings.TrimSpace(os.Getenv("SNOWFAST_CONFIG"))
	if p == "" {
		return "", false
	}
	return p, true
}

// bare "-" (stdin) treated as a value
func looksLikeFlag(s string) bool {
	return len(s) > 1 && s[0] == '-'
}

// StripConfigArgv removes -config / --config tokens so flag.Parse wont choke.
// keeps `-config` in argv if the next token looks like a flag, so the user sees
// a clear "flag provided but not defined" rather than silent swallowing
func StripConfigArgv(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-config", a == "--config":
			if i+1 < len(argv) && !looksLikeFlag(argv[i+1]) {
				i++
				continue
			}
			out = append(out, a)
		case strings.HasPrefix(a, "-config="), strings.HasPrefix(a, "--config="):
			continue
		default:
			out = append(out, a)
		}
	}
	return out
}

// PathFromArgv scans argv for -config / --config / -config=PATH.
// value form only matches if next token is not itself a flag
func PathFromArgv(argv []string) (path string, explicit bool) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-config", a == "--config":
			if i+1 < len(argv) && !looksLikeFlag(argv[i+1]) {
				return argv[i+1], true
			}
		case strings.HasPrefix(a, "-config="):
			return strings.TrimPrefix(a, "-config="), true
		case strings.HasPrefix(a, "--config="):
			return strings.TrimPrefix(a, "--config="), true
		}
	}
	return "", false
}

// ResolveConfigPath picks config path: argv -config, env, default.
func ResolveConfigPath(argv []string) (path string, explicit bool, err error) {
	if p, ok := PathFromArgv(argv); ok {
		return filepath.Clean(p), true, nil
	}
	if p, ok := PathFromEnv(); ok {
		return filepath.Clean(p), true, nil
	}
	p, err := DefaultPath()
	if err != nil {
		return "", false, fmt.Errorf("config default path: %w", err)
	}
	return p, false, nil
}

// DefaultPathHint returns user-facing description of default config location.
// per-OS literal fallback when DefaultPath fails (rare locked-down accounts)
func DefaultPathHint() string {
	if p, err := DefaultPath(); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		return `%AppData%\snowfast\config.toml`
	}
	return "~/.config/snowfast/config.toml"
}
