package ulpengine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/atomicfs"
	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
)

type searchFrameChunk struct {
	ChunkID           int   `json:"chunk_id"`
	CompressedOffset  int64 `json:"compressed_offset"`
	CompressedSize    int64 `json:"compressed_size"`
	UncompressedStart int64 `json:"uncompressed_start"`
	UncompressedEnd   int64 `json:"uncompressed_end"`
}

type searchSidecarDoc struct {
	Version        int                      `json:"version"`
	Format         string                   `json:"format"`
	Source         string                   `json:"source"`
	SourceIdentity searchidx.SourceIdentity `json:"source_identity"`
	Chunks         []searchFrameChunk       `json:"chunks"`
}

func searchSidecarPathForArchive(archivePath string) string {
	return searchidx.LibrarySidecarPath(archivePath)
}

func ensureSearchIdxSubdir(archiveDir string) error {
	return searchidx.EnsureSubdir(archiveDir)
}

// stageSearchSidecar writes the search-sidecar JSON for archivePath into a
// unique temp file inside the search-idx subdir and returns its path. The
// sidecar records the staged archive's content identity (size, mtime, SHA-256
// of stagedPath — the bytes that commit() renames to archivePath), binding the
// frame map to the exact archive content. The final sidecar is NOT touched:
// the caller publishes the staged file with publishSearchSidecar at commit
// time, so a failed run never exposes a final sidecar for an archive it did
// not publish.
func stageSearchSidecar(finalArchivePath, stagedArchivePath string, chunks []searchFrameChunk) (string, error) {
	if len(chunks) == 0 {
		return "", fmt.Errorf("search sidecar: no frames for %s", finalArchivePath)
	}
	ident, err := archiveSourceIdentity(stagedArchivePath)
	if err != nil {
		return "", fmt.Errorf("search sidecar: source identity for %s: %w", filepath.Base(stagedArchivePath), err)
	}
	if err := ensureSearchIdxSubdir(filepath.Dir(finalArchivePath)); err != nil {
		return "", err
	}
	doc := searchSidecarDoc{
		Version:        searchidx.FormatVersion,
		Format:         searchidx.FormatName,
		Source:         filepath.Base(finalArchivePath),
		SourceIdentity: ident,
		Chunks:         chunks,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')

	final := searchSidecarPathForArchive(finalArchivePath)
	dir := filepath.Dir(final)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(final)+".tmp-*")
	if err != nil {
		return "", err
	}
	staged := tmp.Name()
	RegisterCleanupPath(staged)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(staged)
		UnregisterCleanupPath(staged)
		return "", err
	}
	// durability: fsync the staged sidecar before close/rename
	if err := durableSyncFile(staged); err != nil {
		_ = tmp.Close()
		_ = os.Remove(staged)
		UnregisterCleanupPath(staged)
		return "", fmt.Errorf("search sidecar: sync %s: %w", filepath.Base(staged), err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(staged)
		UnregisterCleanupPath(staged)
		return "", err
	}
	return staged, nil
}

// archiveSourceIdentity hashes a finalized archive file with a bounded 1 MiB
// buffer and stats it, producing the identity a v2 search sidecar records.
// Callers pass the staged temp; rename preserves size and mtime, so the
// identity also describes the published archive.
func archiveSourceIdentity(path string) (searchidx.SourceIdentity, error) {
	var ident searchidx.SourceIdentity
	f, err := os.Open(path)
	if err != nil {
		return ident, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ident, err
	}
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return ident, err
	}
	ident.Size = fi.Size()
	ident.ModTimeUnixNano = fi.ModTime().UnixNano()
	ident.SHA256 = hex.EncodeToString(h.Sum(nil))
	return ident, nil
}

// publishSearchSidecar renames a staged search sidecar to its final name and
// copies the archive's mtime onto it (freshness signal for later library
// scans). archivePath must already be the published archive.
func publishSearchSidecar(staged, archivePath string) error {
	final := searchSidecarPathForArchive(archivePath)
	if err := atomicfs.Rename(staged, final); err != nil {
		return err
	}
	// the staged temp is now the final sidecar, not a deletable scratch file
	UnregisterCleanupPath(staged)
	if archStat, err := os.Stat(archivePath); err == nil {
		_ = os.Chtimes(final, archStat.ModTime(), time.Now())
	}
	return nil
}
