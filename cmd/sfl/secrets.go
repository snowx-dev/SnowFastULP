package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
)

const secretsDBName = "sfl-secrets.sqlite"

// stringAccum is a flag.Value that appends into a backing []string, so the
// same slice can be shared with config.SFLFlags.SecretsAllow (*[]string). Each
// occurrence appends; comma-separated values within one occurrence are split so
// config arrays and CLI stay symmetrical.
type stringAccum struct{ v *[]string }

func (a stringAccum) String() string {
	if a.v == nil || len(*a.v) == 0 {
		return ""
	}
	return strings.Join(*a.v, ",")
}

func (a stringAccum) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*a.v = append(*a.v, p)
		}
	}
	return nil
}

var _ flag.Value = stringAccum{}

// resolveSecretsPath picks where the secrets DB lives: an explicit
// -secrets-path wins, else the output dir, else the library dir, else the
// current directory. -secrets-path may be either a full file path (legacy) or a
// directory (mirroring -o/-od): a trailing slash or an existing directory entry
// gets sfl-secrets.sqlite appended, so `-secrets-path ./out/` and `-o ./out/
// -secrets` land at the same file. This is build-tag agnostic (path logic only)
// so it and the -secrets-path flag behave identically whether or not scanning
// support was compiled in.
func resolveSecretsPath(flag, outDir, libDir string) string {
	switch {
	case flag != "":
		if isDirPath(flag) {
			return filepath.Join(flag, secretsDBName)
		}
		return flag
	case outDir != "":
		return filepath.Join(outDir, secretsDBName)
	case libDir != "":
		return filepath.Join(libDir, secretsDBName)
	default:
		return secretsDBName
	}
}

// isDirPath reports whether p should be treated as a directory: it ends in a
// path separator, or an entry exists at p and is a directory.
func isDirPath(p string) bool {
	if strings.HasSuffix(p, string(filepath.Separator)) {
		return true
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return true
	}
	return false
}
