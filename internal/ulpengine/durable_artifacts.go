package ulpengine

import (
	"os"

	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
)

// DurableArtifacts returns each output archive followed by every sidecar that
// currently exists for it. Missing optional sidecars are omitted.
func DurableArtifacts(archivePaths []string) []string {
	artifacts := make([]string, 0, len(archivePaths)*3)
	for _, archive := range archivePaths {
		if archive == "" {
			continue
		}
		artifacts = append(artifacts, archive)
		if idx := sidecarPathForArchive(archive); pathExists(idx) {
			artifacts = append(artifacts, idx)
		}
		if searchSidecar, ok := searchidx.ResolveExistingSidecar(archive); ok {
			artifacts = append(artifacts, searchSidecar)
		}
	}
	return artifacts
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
