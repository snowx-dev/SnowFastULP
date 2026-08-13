package selfupdate

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// installStampName is the marker file dropped by the install scripts
// (scripts/install.sh and scripts/install.ps1) into the user's data dir
// after a successful fresh install. Its presence tells the self-updater
// that the install dir is trusted and new binaries may be auto-installed
// there alongside the existing ones.
const installStampName = "install.stamp"

const utf8BOM = "\ufeff"

// installStampDir returns the directory that should hold the install
// marker, mirroring the XDG_DATA_HOME / AppData convention used for the
// config dir (internal/config/path.go). On Windows the config and data
// dirs coincide (both %AppData%), so we reuse os.UserConfigDir for parity
// with the config path code; on Linux/macOS the data dir is distinct
// ($XDG_DATA_HOME or ~/.local/share).
func installStampDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "snowfast"), nil
	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "snowfast"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", "snowfast"), nil
	}
}

// installStampPath returns the full marker path, or "" if the data dir
// cannot be resolved (callers treat that as "no marker").
func installStampPath() string {
	dir, err := installStampDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, installStampName)
}

// stampDirField parses the install stamp's `dir=` line and returns the path.
// The stamp is one field per line so paths may contain spaces:
//
//	dir=<absolute path>
//	version=<ver>
//	installed_at=<ts>
//
// A UTF-8 BOM is stripped. Returns "" if the stamp is unreadable or
// carries no dir= field.
func stampDirField(stampPath string) string {
	data, err := os.ReadFile(stampPath)
	if err != nil {
		return ""
	}
	data = bytes.TrimPrefix(data, []byte(utf8BOM))
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if val, ok := strings.CutPrefix(line, "dir="); ok {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

func dirsSame(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// canInstallNewBinsFor reports whether missing binaries declared by the
// manifest may be auto-installed into the running executable's directory.
// True when --install-new is forced, or when the install-script marker is
// present AND its recorded `dir=` is the same directory as the running
// executable (os.SameFile: case, junctions, symlinks). False otherwise —
// missing binaries are then skipped so a fresh install never silently
// writes new files to an arbitrary directory.
func canInstallNewBinsFor(self string, forced bool) bool {
	if forced {
		return true
	}
	p := installStampPath()
	if p == "" {
		return false
	}
	if _, err := os.Stat(p); err != nil {
		return false
	}
	stampDir := stampDirField(p)
	if stampDir == "" {
		return false
	}
	return dirsSame(stampDir, filepath.Dir(self))
}
