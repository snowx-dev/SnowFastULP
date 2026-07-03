package searchidx

import (
	"os"
	"path/filepath"
)

const (
	SubdirName    = "sfu_search_idx"
	SidecarSuffix = ".sfsidx.json"
	FormatName    = "snowfastsearch.idx.v1"
	FormatVersion = 1
)

// LibrarySidecarPath returns the sfu library search sidecar path.
func LibrarySidecarPath(archivePath string) string {
	dir := filepath.Dir(archivePath)
	base := filepath.Base(archivePath)
	return filepath.Join(dir, SubdirName, base+SidecarSuffix)
}

// LegacyAdjacentSidecarPath returns the adjacent sidecar path (sfs v1).
func LegacyAdjacentSidecarPath(archivePath string) string {
	return archivePath + SidecarSuffix
}

// EnsureSubdir creates sfu_search_idx under archiveDir if missing.
func EnsureSubdir(archiveDir string) error {
	return os.MkdirAll(filepath.Join(archiveDir, SubdirName), 0o755)
}

// ResolveExistingSidecar returns the first existing sidecar path, library first.
func ResolveExistingSidecar(archivePath string) (string, bool) {
	for _, p := range []string{
		LibrarySidecarPath(archivePath),
		LegacyAdjacentSidecarPath(archivePath),
	} {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// WriteSidecarPath returns the preferred write path under sfu_search_idx/.
func WriteSidecarPath(archivePath string) string {
	return LibrarySidecarPath(archivePath)
}
