package main

import (
	"encoding/json"
	"fmt"
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
	Version int                `json:"version"`
	Format  string             `json:"format"`
	Source  string             `json:"source"`
	Chunks  []searchFrameChunk `json:"chunks"`
}

func searchSidecarPathForArchive(archivePath string) string {
	return searchidx.LibrarySidecarPath(archivePath)
}

func ensureSearchIdxSubdir(archiveDir string) error {
	return searchidx.EnsureSubdir(archiveDir)
}

func writeSearchSidecar(archivePath string, chunks []searchFrameChunk) error {
	if len(chunks) == 0 {
		return fmt.Errorf("search sidecar: no frames for %s", archivePath)
	}
	if err := ensureSearchIdxSubdir(filepath.Dir(archivePath)); err != nil {
		return err
	}
	doc := searchSidecarDoc{
		Version: searchidx.FormatVersion,
		Format:  searchidx.FormatName,
		Source:  filepath.Base(archivePath),
		Chunks:  chunks,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	path := searchSidecarPathForArchive(archivePath)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := atomicfs.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if archStat, err := os.Stat(archivePath); err == nil {
		_ = os.Chtimes(path, archStat.ModTime(), time.Now())
	}
	return nil
}
