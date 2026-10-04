package history

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
)

const hashBufferSize = 1 << 20

// fingerprintReadPause is a test-only latency injection (see
// SetFingerprintReadPause in export_test.go); always zero in production.
var fingerprintReadPause = fingerprintReadPauseFromEnv()

// fingerprintReadPauseFromEnv lets e2e tests (which drive the compiled
// binary, where the Go test seam is unreachable) slow the sampled reads the
// same way. The env var is only ever set by test code.
func fingerprintReadPauseFromEnv() time.Duration {
	ms, err := strconv.Atoi(os.Getenv("SNOWFAST_TEST_FP_READ_PAUSE_MS"))
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

var hashBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, hashBufferSize)
		return &buf
	},
}

func FingerprintFile(ctx context.Context, path string, progress ProgressFunc) (Candidate, error) {
	if err := ctx.Err(); err != nil {
		return Candidate{}, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Candidate{}, fmt.Errorf("history: resolve source %s: %w", path, err)
	}
	absolute = filepath.Clean(absolute)
	before, err := os.Stat(absolute)
	if err != nil {
		return Candidate{}, fmt.Errorf("history: stat source %s: %w", absolute, err)
	}
	if before.IsDir() {
		return Candidate{}, fmt.Errorf("history: source is a directory: %s", absolute)
	}

	f, err := os.Open(absolute)
	if err != nil {
		return Candidate{}, fmt.Errorf("history: open source %s: %w", absolute, err)
	}
	defer f.Close()
	// Track the read handle so a graceful Ctrl-C (fileabort.WatchInterrupt)
	// can close it and unstick a Read blocked on slow storage.
	if reg := fileabort.FromContext(ctx); reg != nil {
		defer reg.Register(f)()
	}

	// Sampled fingerprint: identity is stat size + xxhash64 of the first and
	// last fastSampleSize bytes — one or two short reads instead of reading
	// every byte twice (fingerprint + validate). Accepted error rate: two
	// same-size sources whose differing bytes fall in the untouched middle
	// produce the same identity.
	h := xxhash.New()
	h.Write([]byte("snowfast-history-fastsample-v2\x00"))
	writeUint64(h, uint64(before.Size()))

	bufp := hashBufferPool.Get().(*[]byte)
	defer hashBufferPool.Put(bufp)
	buf := (*bufp)[:fastSampleSize]

	report := func() {
		if progress != nil {
			// Report the stat size rather than the sampled bytes so
			// aggregate progress totals still add up to the real size.
			progress(absolute, before.Size(), before.Size())
		}
	}
	head, err := readSampledChunk(ctx, f, absolute, buf, report)
	if err != nil {
		return Candidate{}, err
	}
	writeUint32(h, uint32(len(head)))
	h.Write(head)
	if before.Size() > int64(len(head)) {
		if _, err := f.Seek(-int64(fastSampleSize), io.SeekEnd); err != nil {
			return Candidate{}, fmt.Errorf("history: seek source %s: %w", absolute, err)
		}
		tail, err := readSampledChunk(ctx, f, absolute, buf, report)
		if err != nil {
			return Candidate{}, err
		}
		writeUint32(h, uint32(len(tail)))
		h.Write(tail)
	}

	after, err := f.Stat()
	if err != nil {
		return Candidate{}, fmt.Errorf("history: stat source %s after reading: %w", absolute, err)
	}
	if !sameSnapshot(before, after) || before.Size() != after.Size() {
		return Candidate{}, sourceChangedError(absolute)
	}

	return Candidate{
		ID:        Identity{Hash: h.Sum64(), Size: before.Size()},
		Paths:     []string{absolute},
		Snapshots: []Snapshot{{Path: absolute, info: after}},
	}, nil
}

// fastSampleSize is the sampled fingerprint's per-window read size: identity
// hashes the first and last fastSampleSize bytes of each source (head only
// for sources this small or smaller, head+tail above it — larger sources are
// approximated). One or two short reads instead of reading every byte twice
// (fingerprint + validate); it exactly fills one pooled hash buffer.
const fastSampleSize = 1 << 20

// readSampledChunk reads up to len(buf) bytes from the file's current offset,
// honoring cancellation and the test-only read latency seam, and calls report
// (may be nil) once per successful read so callers can keep aggregate
// progress totals aligned with real source sizes.
func readSampledChunk(ctx context.Context, f *os.File, name string, buf []byte, report func()) ([]byte, error) {
	// Test seam: sampled reads complete in microseconds, so tests that need
	// a "long" hash/validate pass inject latency here (see
	// SetFingerprintReadPause in export_test.go; e2e binaries set
	// SNOWFAST_TEST_FP_READ_PAUSE_MS instead).
	pause := fingerprintReadPause
	if pause == 0 {
		pause = fingerprintReadPauseFromEnv()
	}
	if pause > 0 {
		time.Sleep(pause)
	}
	total := 0
	for total < len(buf) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, readErr := f.Read(buf[total:])
		total += n
		if n > 0 && report != nil {
			report()
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, fmt.Errorf("history: read source %s: %w", name, readErr)
		}
	}
	return buf[:total], nil
}

// FingerprintMultipart fingerprints a multi-volume assembly by sampling each
// volume the same way FingerprintFile samples a single file: per volume, the
// head fastSampleSize bytes plus the tail fastSampleSize bytes for volumes
// larger than the window (smaller volumes are hashed in full). The framing
// (assembly label, part count, per-part index and size) keeps volume order
// and membership part of the identity.
func FingerprintMultipart(ctx context.Context, assembly string, paths []string, progress ProgressFunc) (Candidate, error) {
	if err := ctx.Err(); err != nil {
		return Candidate{}, err
	}
	if len(paths) < 2 {
		return Candidate{}, fmt.Errorf("history: multipart source requires at least two parts")
	}
	if len(paths) > math.MaxUint32 {
		return Candidate{}, fmt.Errorf("history: multipart source has too many parts")
	}
	if len(assembly) > math.MaxUint32 {
		return Candidate{}, fmt.Errorf("history: multipart assembly label is too long")
	}

	absolute := make([]string, len(paths))
	before := make([]os.FileInfo, len(paths))
	var total int64
	for i, path := range paths {
		resolved, err := filepath.Abs(path)
		if err != nil {
			return Candidate{}, fmt.Errorf("history: resolve source %s: %w", path, err)
		}
		resolved = filepath.Clean(resolved)
		info, err := os.Stat(resolved)
		if err != nil {
			return Candidate{}, fmt.Errorf("history: stat source %s: %w", resolved, err)
		}
		if info.IsDir() {
			return Candidate{}, fmt.Errorf("history: source is a directory: %s", resolved)
		}
		if info.Size() > math.MaxInt64-total {
			return Candidate{}, fmt.Errorf("history: multipart source size overflows int64")
		}
		absolute[i] = resolved
		before[i] = info
		total += info.Size()
	}

	h := xxhash.New()
	h.Write([]byte("snowfast-history-multipart-fastsample-v2\x00"))
	writeUint32(h, uint32(len(assembly)))
	h.Write([]byte(assembly))
	writeUint32(h, uint32(len(paths)))

	bufp := hashBufferPool.Get().(*[]byte)
	defer hashBufferPool.Put(bufp)
	buf := (*bufp)[:fastSampleSize]

	snapshots := make([]Snapshot, 0, len(paths))
	var done int64
	for i, path := range absolute {
		if err := ctx.Err(); err != nil {
			return Candidate{}, err
		}
		writeUint32(h, uint32(i))
		writeUint64(h, uint64(before[i].Size()))

		f, err := os.Open(path)
		if err != nil {
			return Candidate{}, fmt.Errorf("history: open source %s: %w", path, err)
		}
		var unregister func()
		if reg := fileabort.FromContext(ctx); reg != nil {
			unregister = reg.Register(f)
		}
		report := func() {
			if progress != nil {
				// Report the volume's full stat size rather than the
				// sampled bytes so aggregate progress totals still add
				// up to the assembly's real size.
				progress(path, done+before[i].Size(), total)
			}
		}
		head, readErr := readSampledChunk(ctx, f, path, buf, report)
		if readErr == nil {
			writeUint32(h, uint32(len(head)))
			h.Write(head)
		}
		if readErr == nil && before[i].Size() > int64(len(head)) {
			if _, err := f.Seek(-int64(fastSampleSize), io.SeekEnd); err != nil {
				readErr = fmt.Errorf("history: seek source %s: %w", path, err)
			} else {
				var tail []byte
				tail, readErr = readSampledChunk(ctx, f, path, buf, report)
				if readErr == nil {
					writeUint32(h, uint32(len(tail)))
					h.Write(tail)
				}
			}
		}
		after, statErr := f.Stat()
		closeErr := f.Close()
		if unregister != nil {
			unregister()
		}
		if readErr != nil {
			return Candidate{}, readErr
		}
		if statErr != nil {
			return Candidate{}, fmt.Errorf("history: stat source %s after reading: %w", path, statErr)
		}
		if closeErr != nil {
			return Candidate{}, fmt.Errorf("history: close source %s: %w", path, closeErr)
		}
		if !sameSnapshot(before[i], after) || before[i].Size() != after.Size() {
			return Candidate{}, sourceChangedError(path)
		}
		done += before[i].Size()
		snapshots = append(snapshots, Snapshot{Path: path, info: after})
	}

	return Candidate{
		ID:        Identity{Hash: h.Sum64(), Size: total},
		Paths:     absolute,
		Snapshots: snapshots,
		Assembly:  assembly,
	}, nil
}

func writeUint32(h hash.Hash, value uint32) {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], value)
	h.Write(buf[:])
}

func writeUint64(h hash.Hash, value uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], value)
	h.Write(buf[:])
}

func Validate(c Candidate) error {
	return ValidateAt(context.Background(), c, c.Paths, nil)
}

// ValidateAt re-fingerprints a candidate from paths and compares its full
// content identity. It is used after sources have been atomically staged under
// quarantine names as well as for ordinary pre-record validation.
func ValidateAt(ctx context.Context, c Candidate, paths []string, progress ProgressFunc) error {
	if len(paths) != len(c.Paths) || len(paths) == 0 {
		return fmt.Errorf("history: validate source path count changed")
	}
	if len(c.Snapshots) != len(paths) {
		return fmt.Errorf("history: validate source snapshot count changed")
	}
	for i, path := range paths {
		currentInfo, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("history: validate source %s: %w", path, err)
		}
		if c.Snapshots[i].info == nil || !sameSnapshot(c.Snapshots[i].info, currentInfo) {
			return sourceChangedError(path)
		}
	}
	var current Candidate
	var err error
	if len(paths) == 1 {
		current, err = FingerprintFile(ctx, paths[0], progress)
	} else {
		current, err = FingerprintMultipart(ctx, c.Assembly, paths, progress)
	}
	if err != nil {
		return err
	}
	if current.ID != c.ID {
		return sourceChangedError(paths[0])
	}
	return nil
}

// ValidateAllContext revalidates every candidate from its original paths,
// reporting each re-read through progress (may be nil) so a long
// validating-before-record pass stays visible, and stops promptly on
// cancellation.
func ValidateAllContext(ctx context.Context, candidates []Candidate, progress ProgressFunc) error {
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ValidateAt(ctx, candidate, candidate.Paths, progress); err != nil {
			return err
		}
	}
	return nil
}

// sameSnapshot compares two FileInfo snapshots of the same path.
func sameSnapshot(before, after os.FileInfo) bool {
	return os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func sourceChangedError(path string) error {
	return fmt.Errorf("history: source changed while processing: %s", path)
}
