package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/atomicfs"
	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"
)

// Progress reports indexing progress for lazy sidecar builds.
type Progress = zstdframe.Progress

// Chunk is one zstd frame entry in the sidecar.
type Chunk struct {
	ChunkID           int   `json:"chunk_id"`
	CompressedOffset  int64 `json:"compressed_offset"`
	CompressedSize    int64 `json:"compressed_size"`
	UncompressedStart int64 `json:"uncompressed_start"`
	UncompressedEnd   int64 `json:"uncompressed_end"`
}

// Sidecar is the JSON index for an archive.
type Sidecar struct {
	Version int     `json:"version"`
	Format  string  `json:"format"`
	Source  string  `json:"source"`
	Chunks  []Chunk `json:"chunks"`
}

// IsStale reports whether the sidecar should be rebuilt.
func IsStale(archivePath, sidecarPath string) (bool, error) {
	archInfo, err := os.Stat(archivePath)
	if err != nil {
		return false, err
	}
	idxInfo, err := os.Stat(sidecarPath)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return archInfo.ModTime().After(idxInfo.ModTime()), nil
}

// Load reads and validates a sidecar file.
func Load(path string) (*Sidecar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("parse sidecar: %w", err)
	}
	if sc.Version != searchidx.FormatVersion || sc.Format != searchidx.FormatName {
		return nil, errors.New("unsupported sidecar format")
	}
	if len(sc.Chunks) == 0 {
		return nil, errors.New("sidecar has no chunks")
	}
	return &sc, nil
}

// Build scans archivePath and writes a fresh sidecar atomically.
func Build(ctx context.Context, archivePath string, prog Progress, act *zstdframe.Activity) (*Sidecar, error) {
	if err := searchidx.EnsureSubdir(filepath.Dir(archivePath)); err != nil {
		return nil, err
	}

	frames, err := zstdframe.ScanFile(ctx, archivePath, prog, act)
	if err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, errors.New("no zstd frames found")
	}

	chunks := make([]Chunk, len(frames))
	for i, f := range frames {
		chunks[i] = Chunk{
			ChunkID:           f.ChunkID,
			CompressedOffset:  f.CompressedOffset,
			CompressedSize:    f.CompressedSize,
			UncompressedStart: f.UncompressedStart,
			UncompressedEnd:   f.UncompressedEnd,
		}
	}

	sc := &Sidecar{
		Version: searchidx.FormatVersion,
		Format:  searchidx.FormatName,
		Source:  filepath.Base(archivePath),
		Chunks:  chunks,
	}

	if err := writeAtomic(archivePath, sc); err != nil {
		return nil, err
	}
	return sc, nil
}

// Ensure loads existing sidecar or builds one if missing/stale.
func Ensure(ctx context.Context, archivePath string, prog Progress, act *zstdframe.Activity) (*Sidecar, EnsureMeta, error) {
	meta := EnsureMeta{}
	archInfo, err := os.Stat(archivePath)
	if err != nil {
		return nil, meta, err
	}
	meta.ArchiveMod = archInfo.ModTime()

	if path, ok := searchidx.ResolveExistingSidecar(archivePath); ok {
		meta.SidecarPath = path
		meta.Missing = false
		if _, idxMod, err := sidecarTimestamps(archivePath, path); err == nil {
			meta.SidecarMod = idxMod
		}
		stale, err := IsStale(archivePath, path)
		if err != nil {
			return nil, meta, err
		}
		meta.Stale = stale
		if !stale {
			sc, err := Load(path)
			meta.Action = EnsureActionLoad
			return sc, meta, err
		}
	} else {
		meta.Missing = true
		meta.Stale = true
	}

	sc, err := Build(ctx, archivePath, prog, act)
	if err != nil {
		return nil, meta, err
	}
	meta.Action = EnsureActionBuild
	meta.SidecarPath = searchidx.WriteSidecarPath(archivePath)
	if _, idxMod, err := sidecarTimestamps(archivePath, meta.SidecarPath); err == nil {
		meta.SidecarMod = idxMod
	}
	meta.Stale = false
	return sc, meta, nil
}

func writeAtomic(archivePath string, sc *Sidecar) error {
	if err := searchidx.EnsureSubdir(filepath.Dir(archivePath)); err != nil {
		return err
	}
	path := searchidx.WriteSidecarPath(archivePath)
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

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

// ArchiveSize returns archive size for progress totals.
func ArchiveSize(archivePath string) (int64, error) {
	fi, err := os.Stat(archivePath)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
