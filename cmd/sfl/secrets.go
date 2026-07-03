package main

import "path/filepath"

const secretsDBName = "sfl-secrets.sqlite"

// resolveSecretsPath picks where the secrets DB lives: explicit flag wins, else
// the output dir, else the library dir, else the current directory. This is
// build-tag agnostic (path logic only) so it and the -secrets-path flag behave
// identically whether or not scanning support was compiled in.
func resolveSecretsPath(flag, outDir, libDir string) string {
	switch {
	case flag != "":
		return flag
	case outDir != "":
		return filepath.Join(outDir, secretsDBName)
	case libDir != "":
		return filepath.Join(libDir, secretsDBName)
	default:
		return secretsDBName
	}
}
