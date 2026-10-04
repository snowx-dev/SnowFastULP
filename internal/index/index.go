package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
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

// Sidecar is the JSON index for an archive. The recorded SourceIdentity binds
// the frame map to the exact archive content it was built from.
type Sidecar struct {
	Version        int                      `json:"version"`
	Format         string                   `json:"format"`
	Source         string                   `json:"source"`
	SourceIdentity searchidx.SourceIdentity `json:"source_identity"`
	Chunks         []Chunk                  `json:"chunks"`

	// ChunksSHA256 pins the complete chunk map: hex sha256 over the canonical
	// JSON of the Chunks array as written. Load verifies it, so a dropped or
	// altered frame — whose remainder can look structurally self-consistent —
	// is rejected and rebuilt instead of silently searched (M-08). Empty in
	// sidecars written before this field existed; those skip the pin check.
	ChunksSHA256 string `json:"chunks_sha256,omitempty"`

	// Verify records the archive identity that was last CONFIRMED by a full
	// content digest pass (not merely asserted by the build). Optional and
	// additive: sidecars without it (e.g. written by sfu's staged builds) are
	// valid and simply require one digest pass on next sight, after which the
	// block is backfilled. Nil means "no digest has been confirmed for this
	// archive instance yet".
	Verify *VerifyInfo `json:"verify,omitempty"`
}

// VerifyInfo records the archive stat identity whose digest was confirmed by
// a full hashArchive pass. Size and MtimeUnixNano mirror SourceIdentity's
// fields; InodeOrHandle is the platform instance token from
// archiveInstanceToken (empty on Windows).
type VerifyInfo struct {
	Size          int64  `json:"size"`
	MtimeUnixNano int64  `json:"mtime_ns"`
	InodeOrHandle string `json:"inode_or_handle"`
	SHA256        string `json:"sha256"`
}

// ErrArchiveChanged is returned when the archive at archivePath is no longer
// the file that was scanned by the time the sidecar would be published.
var ErrArchiveChanged = errors.New("archive changed during index build")

// afterScanForTest, when non-nil, runs after the scan completes and before
// publish-time identity verification (deterministic test seam only).
var afterScanForTest func(archivePath string)

// hashCountForTest counts full-content digest passes performed by hashArchive
// (deterministic test seam, same spirit as afterScanForTest).
var hashCountForTest atomic.Int64

// IsStale reports whether the sidecar should be rebuilt. Freshness is
// content-bound, but the full SHA-256 digest pass is not repeated on every
// run: the sidecar's Verify block records which archive identity (size,
// mtime, instance token) a previous digest pass CONFIRMED, and while the
// current stat matches that record the sidecar is fresh without hashing.
// Any size or mtime change invalidates both the fast path and the record and
// forces a digest pass; so does the first sighting of a new archive instance
// (a different inode, e.g. after an atomic replacement, even with identical
// content and a restored mtime).
//
// Residual tradeoff (accepted; strictly better than the v1 mtime-only
// scheme): a same-size rewrite whose mtime is forged back to the recorded
// value on the SAME file instance is trusted until the mtime changes, since
// the recorded confirmation matches every observable. A rewritten file
// normally gets a new mtime or a new inode (rename), and the digest is still
// checked on first sight of every new (size, mtime) pair. On Windows the
// instance token is unavailable, so the record degrades to size+mtime
// matching (see archiveInstanceToken).
//
// Missing, corrupt, and unsupported-format (e.g. v1) sidecars are stale by
// definition: they are derived data and are rebuilt automatically, never
// migrated in place. When a digest confirms the recorded identity and no
// valid Verify block exists yet, the confirmation is backfilled with an
// atomic rewrite preserving all other fields. The re-stat guard narrows the
// race with archive changes, but a backfill rename can still overwrite a
// concurrent build published after our hash; the next Ensure's stat check
// reconciles that case. Backfill is best-effort; failure costs one extra
// digest pass next run.
func IsStale(ctx context.Context, archivePath, sidecarPath string) (bool, error) {
	archInfo, err := os.Stat(archivePath)
	if err != nil {
		return false, err
	}
	sc, err := Load(sidecarPath)
	if err != nil {
		return true, nil
	}
	ident := sc.SourceIdentity
	if archInfo.Size() != ident.Size || archInfo.ModTime().UnixNano() != ident.ModTimeUnixNano {
		return true, nil
	}
	// Same size and mtime. Cheap path first: if a previous digest pass
	// already confirmed exactly this archive instance, trust it.
	if v := sc.Verify; v != nil &&
		v.Size == archInfo.Size() &&
		v.MtimeUnixNano == archInfo.ModTime().UnixNano() &&
		v.InodeOrHandle == archiveInstanceToken(archInfo) &&
		v.SHA256 == ident.SHA256 {
		return false, nil
	}
	// First sight of this archive instance, or Verify block absent/older:
	// only the digest can distinguish an equal-mtime rewrite from the
	// content the sidecar was built from.
	digest, err := hashArchive(ctx, archivePath)
	if err != nil {
		return false, err
	}
	if digest != ident.SHA256 {
		return true, nil
	}
	// The digest describes the file seen when hashArchive opened it. Avoid
	// backfilling stale stat data if the pathname changed while hashing.
	postStat, err := os.Stat(archivePath)
	if err != nil {
		return false, err
	}
	if postStat.Size() != archInfo.Size() ||
		postStat.ModTime().UnixNano() != archInfo.ModTime().UnixNano() ||
		archiveInstanceToken(postStat) != archiveInstanceToken(archInfo) {
		return true, nil
	}
	// Digest confirmed: persist the confirmation so later runs skip the
	// hash. All other fields are preserved. The stat recheck narrows, but
	// cannot eliminate, the window in which a concurrent build may publish
	// between our hash and this backfill rename.
	updated := *sc
	updated.Verify = &VerifyInfo{
		Size:          archInfo.Size(),
		MtimeUnixNano: archInfo.ModTime().UnixNano(),
		InodeOrHandle: archiveInstanceToken(archInfo),
		SHA256:        ident.SHA256,
	}
	// Best-effort: the sidecar is fresh and usable whether or not the
	// backfill lands.
	_, _ = writeAtomic(ctx, archivePath, &updated)
	return false, nil
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
		return nil, fmt.Errorf("unsupported sidecar format %q v%d", sc.Format, sc.Version)
	}
	if sc.SourceIdentity.SHA256 == "" {
		return nil, errors.New("sidecar missing source identity")
	}
	if len(sc.Chunks) == 0 {
		return nil, errors.New("sidecar has no chunks")
	}
	if err := validateChunks(&sc); err != nil {
		return nil, err
	}
	if sc.ChunksSHA256 != "" && sc.ChunksSHA256 != chunkMapDigest(sc.Chunks) {
		return nil, errors.New("sidecar chunk map does not match its digest (dropped or altered frame)")
	}
	return &sc, nil
}

// chunkMapDigest pins the complete chunk map: hex sha256 over the canonical
// JSON encoding of the chunks array. A dropped or altered frame changes the
// digest even when the remainder stays structurally self-consistent.
func chunkMapDigest(chunks []Chunk) string {
	data, err := json.Marshal(chunks)
	if err != nil {
		// []Chunk cannot fail to marshal; unreachable, but fail closed.
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// validateChunks checks the chunk map's structural consistency: IDs sequential
// from 0, positive compressed/uncompressed spans, compressed offsets ordered
// and non-overlapping (gaps are legal — skippable frames are not recorded as
// chunks), and uncompressed coverage contiguous from zero. A map that fails
// any of these cannot be the scan the source identity was built from
// (corruption or tampering dropped/altered frames), so the caller must rebuild
// instead of silently searching a subset — the M-08 false negative.
func validateChunks(sc *Sidecar) error {
	for i, c := range sc.Chunks {
		if c.ChunkID != i {
			return fmt.Errorf("sidecar chunk %d: chunk_id %d out of sequence", i, c.ChunkID)
		}
		if c.CompressedOffset < 0 || c.CompressedSize <= 0 {
			return fmt.Errorf("sidecar chunk %d: invalid compressed span", i)
		}
		if c.UncompressedEnd <= c.UncompressedStart {
			return fmt.Errorf("sidecar chunk %d: empty uncompressed span", i)
		}
		if i == 0 {
			if c.UncompressedStart != 0 {
				return fmt.Errorf("sidecar chunk 0: uncompressed coverage starts at %d, want 0", c.UncompressedStart)
			}
			continue
		}
		prev := sc.Chunks[i-1]
		if c.CompressedOffset < prev.CompressedOffset+prev.CompressedSize {
			return fmt.Errorf("sidecar chunk %d: compressed overlap with chunk %d", i, i-1)
		}
		if c.UncompressedStart != prev.UncompressedEnd {
			return fmt.Errorf("sidecar chunk %d: uncompressed coverage gap or overlap at %d", i, c.UncompressedStart)
		}
	}
	return nil
}

// Build scans archivePath and writes a fresh sidecar atomically. The scan is
// bound to the descriptor it used; if the archive at archivePath is replaced
// or rewritten before publish, nothing is written and ErrArchiveChanged is
// returned.
func Build(ctx context.Context, archivePath string, prog Progress, act *zstdframe.Activity) (*Sidecar, error) {
	if err := searchidx.EnsureSubdir(filepath.Dir(archivePath)); err != nil {
		return nil, err
	}

	frames, ident, err := zstdframe.ScanFileWithIdentity(ctx, archivePath, prog, act)
	if err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, errors.New("no zstd frames found")
	}

	if afterScanForTest != nil {
		afterScanForTest(archivePath)
	}
	if err := verifyArchiveIdentity(archivePath, ident); err != nil {
		return nil, err
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
		SourceIdentity: searchidx.SourceIdentity{
			Size:            ident.Stat.Size(),
			ModTimeUnixNano: ident.Stat.ModTime().UnixNano(),
			SHA256:          ident.SHA256,
		},
		Chunks: chunks,
	}
	sc.ChunksSHA256 = chunkMapDigest(chunks)

	published, err := writeAtomic(ctx, archivePath, sc)
	if err != nil {
		return nil, err
	}
	return published, nil
}

// verifyArchiveIdentity proves the archive pathname still names the exact
// file that was scanned: same file (inode) plus unchanged size and mtime.
// The digest was computed from the scanned descriptor itself, so a positive
// check confirms the recorded digest describes the current content.
func verifyArchiveIdentity(archivePath string, ident *zstdframe.Identity) error {
	cur, err := os.Stat(archivePath)
	if err != nil {
		return fmt.Errorf("stat archive after scan: %w", err)
	}
	if !os.SameFile(cur, ident.Stat) ||
		cur.Size() != ident.Stat.Size() ||
		!cur.ModTime().Equal(ident.Stat.ModTime()) {
		return fmt.Errorf("%w: %s was replaced or rewritten during scan", ErrArchiveChanged, archivePath)
	}
	return nil
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
		if _, idxMod, err := sidecarTimestamps(archivePath, path); err == nil {
			meta.SidecarMod = idxMod
		}
		stale, err := IsStale(ctx, archivePath, path)
		if err != nil {
			return nil, meta, err
		}
		meta.Stale = stale
		if !stale {
			if sc, lerr := Load(path); lerr == nil {
				// Legacy sidecars written before the chunk-map pin existed
				// load fine but stay pinless forever, so a frame they later
				// lose is never noticed and hits drop silently on every run
				// (M-08's permanent exposure window). Upgrade in place:
				// re-serialize with the digest computed over the loaded
				// chunks — no rescan; Load already validated the map. Best
				// effort, like the Verify backfill: the loaded sidecar is
				// usable whether or not the write lands; failure costs one
				// retry next run.
				if sc.ChunksSHA256 == "" {
					updated := *sc
					updated.ChunksSHA256 = chunkMapDigest(sc.Chunks)
					if up, werr := writeAtomic(ctx, archivePath, &updated); werr == nil {
						sc = up
					}
				}
				meta.Action = EnsureActionLoad
				if _, idxMod, err := sidecarTimestamps(archivePath, path); err == nil {
					meta.SidecarMod = idxMod
				}
				return sc, meta, nil
			}
			// Non-stale but unusable (corrupt, wrong format/version, or
			// otherwise failing validation): self-heal by rebuilding. Stale
			// and missing metadata above stay as observed on disk.
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

// writeAtomic publishes sc to the archive's sidecar path atomically. The
// temporary file has a unique name in the sidecar directory, so concurrent
// builders never share a .tmp path. If a concurrent builder wins the rename,
// the published sidecar is accepted only when it describes exactly the
// archive content this build verified; otherwise the write is retried once
// with a fresh temp and, failing that, the real error is returned — a
// vanished temp is never treated as success or omission.
func writeAtomic(ctx context.Context, archivePath string, sc *Sidecar) (*Sidecar, error) {
	if err := searchidx.EnsureSubdir(filepath.Dir(archivePath)); err != nil {
		return nil, err
	}
	path := searchidx.WriteSidecarPath(archivePath)
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
		if err != nil {
			return nil, err
		}
		tmpPath := tmp.Name()
		_, werr := tmp.Write(data)
		if werr == nil {
			werr = tmp.Close()
		} else {
			_ = tmp.Close()
		}
		if werr != nil {
			_ = os.Remove(tmpPath)
			return nil, werr
		}

		rerr := atomicfs.Rename(tmpPath, path)
		if rerr == nil {
			if archStat, serr := os.Stat(archivePath); serr == nil {
				_ = os.Chtimes(path, archStat.ModTime(), time.Now())
			}
			return sc, nil
		}
		_ = os.Remove(tmpPath)
		lastErr = rerr

		if !os.IsNotExist(rerr) {
			return nil, rerr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A concurrent builder may have won the rename with its own sidecar.
		if published, lerr := Load(path); lerr == nil && published.SourceIdentity == sc.SourceIdentity {
			return published, nil
		}
		// Otherwise our own temp vanished or the winner describes different
		// content: retry once with a fresh temp.
	}
	return nil, fmt.Errorf("publish sidecar %s: %w", path, lastErr)
}

func hashArchive(ctx context.Context, path string) (string, error) {
	hashCountForTest.Add(1)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return zstdframe.HashFile(ctx, f)
}

// ArchiveSize returns archive size for progress totals.
func ArchiveSize(archivePath string) (int64, error) {
	fi, err := os.Stat(archivePath)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
