package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/snowx-dev/SnowFastULP/internal/atomicfs"
)

// .idx sidecar layout at <archiveDir>/<idxSubdirName>/<basename>.idx.
// xxhash64 fingerprint index (NOT crypto). "key" means dedup hash key.
//
//	0..3    "SFIX"                magic
//	4..5    u16 LE                format version
//	6..7    u16 LE                hash algo id (0 = xxhash64)
//	8..15   u64 LE                keyCount
//	16..23  u64 LE                parserVersion
//	24..31  reserved (zeros)
//	32..    u64 LE × keyCount     packed dedup hashes
//
// LE everywhere matches the xxhash uint64 storage path. one .idx per
// ARCHIVE PART, not per logical run: touching one part of a 16-part run
// invalidates only that part.
const (
	sidecarMagic       = "SFIX"
	sidecarFormatVer   = 2
	sidecarHashAlgoXX  = 0
	sidecarHeaderBytes = 32
	sidecarKeyBytes    = 8
	sidecarSuffix      = ".idx"

	// holds all .idx for archives in the same dir. created on demand,
	// safe to delete manually (next -od run regens)
	idxSubdirName = "sfu_dedup_idx"

	// identifies parse.go + lineFormatter.HashKey rules. bump on any
	// change that affects xxhash64(host:login:password): host derivation,
	// localhost rejection, the colon-join format.
	// mismatch = errSidecarStale = silent regen on next run
	parserVersion uint64 = 1
)

var (
	errSidecarMissing   = errors.New("sidecar: not found")
	errSidecarMalformed = errors.New("sidecar: malformed header")
	errSidecarStale     = errors.New("sidecar: format/parser version mismatch")
)

type sidecarHeader struct {
	formatVersion uint16
	hashAlgo      uint16
	keyCount      uint64
	parserVersion uint64
}

// <archive>.idx under the sibling idxSubdirName subdir. callers needing
// the subdir created call ensureIdxSubdir(filepath.Dir(archivePath))
func sidecarPathForArchive(archivePath string) string {
	dir := filepath.Dir(archivePath)
	base := filepath.Base(archivePath)
	return filepath.Join(dir, idxSubdirName, base+sidecarSuffix)
}

// idempotent, safe under regen-pool concurrency
func ensureIdxSubdir(archiveDir string) error {
	return os.MkdirAll(filepath.Join(archiveDir, idxSubdirName), 0o755)
}

// reads + validates 32-byte header. errSidecarMissing/Malformed/Stale
// returned for the 3 fail cases. cheap stat-equivalent for discovery
func readSidecarHeader(path string) (*sidecarHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errSidecarMissing
		}
		return nil, err
	}
	defer f.Close()

	var raw [sidecarHeaderBytes]byte
	if _, err := io.ReadFull(f, raw[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", errSidecarMalformed, err)
	}
	if string(raw[0:4]) != sidecarMagic {
		return nil, errSidecarMalformed
	}

	h := &sidecarHeader{
		formatVersion: binary.LittleEndian.Uint16(raw[4:6]),
		hashAlgo:      binary.LittleEndian.Uint16(raw[6:8]),
		keyCount:      binary.LittleEndian.Uint64(raw[8:16]),
		parserVersion: binary.LittleEndian.Uint64(raw[16:24]),
	}

	if h.formatVersion != sidecarFormatVer ||
		h.hashAlgo != sidecarHashAlgoXX ||
		h.parserVersion != parserVersion {
		return h, errSidecarStale
	}

	// body size = header + keyCount*8. defence vs truncated writes
	fi, err := f.Stat()
	if err != nil {
		return h, err
	}
	if fi.Size() < sidecarHeaderBytes {
		return h, fmt.Errorf("%w: size %d < header %d", errSidecarMalformed, fi.Size(), sidecarHeaderBytes)
	}
	bodyBytes := fi.Size() - sidecarHeaderBytes
	if bodyBytes%sidecarKeyBytes != 0 {
		return h, fmt.Errorf("%w: body size %d not multiple of %d", errSidecarMalformed, bodyBytes, sidecarKeyBytes)
	}
	wantKeys := uint64(bodyBytes / sidecarKeyBytes)
	if h.keyCount != wantKeys {
		return h, fmt.Errorf("%w: keyCount %d != body keys %d", errSidecarMalformed, h.keyCount, wantKeys)
	}
	return h, nil
}

// validates header + calls fn per key. ~64 KiB chunks so RAM stays bounded
// regardless of key count. aborts on first fn err
func streamSidecarKeys(path string, fn func(uint64) error) error {
	return streamSidecarKeyBytes(path, 64*1024, func(raw []byte) error {
		for off := 0; off < len(raw); off += sidecarKeyBytes {
			if err := fn(binary.LittleEndian.Uint64(raw[off : off+sidecarKeyBytes])); err != nil {
				return err
			}
		}
		return nil
	})
}

// fast path for -od routing. calls fn w/ key-aligned raw byte chunks so
// reading many keys per io.ReadFull avoids billions of tiny callbacks
func streamSidecarKeyBytes(path string, chunkBytes int, fn func([]byte) error) error {
	hdr, err := readSidecarHeader(path)
	if err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(sidecarHeaderBytes, io.SeekStart); err != nil {
		return err
	}

	if chunkBytes < sidecarKeyBytes {
		chunkBytes = sidecarKeyBytes
	}
	chunkBytes -= chunkBytes % sidecarKeyBytes
	if chunkBytes == 0 {
		chunkBytes = sidecarKeyBytes
	}

	br := bufio.NewReaderSize(f, chunkBytes)
	buf := make([]byte, chunkBytes)
	keysRemaining := hdr.keyCount
	keysPerChunk := uint64(chunkBytes / sidecarKeyBytes)
	keysRead := uint64(0)
	for keysRemaining > 0 {
		nKeys := keysPerChunk
		if nKeys > keysRemaining {
			nKeys = keysRemaining
		}
		nBytes := int(nKeys) * sidecarKeyBytes
		if _, err := io.ReadFull(br, buf[:nBytes]); err != nil {
			return fmt.Errorf("sidecar: read keys %d-%d/%d: %w", keysRead, keysRead+nKeys, hdr.keyCount, err)
		}
		if err := fn(buf[:nBytes]); err != nil {
			return err
		}
		keysRead += nKeys
		keysRemaining -= nKeys
	}
	return nil
}

// writes .idx directly during archive stream. temp + fsync + atomic rename.
// avoids the old two-step archive->scratch->copy regen path
type sidecarWriter struct {
	finalPath string
	tmpPath   string
	f         *os.File
	bw        *bufio.Writer
	count     uint64
	buf       [sidecarKeyBytes]byte
}

func newSidecarWriter(archivePath string) (*sidecarWriter, error) {
	finalPath := sidecarPathForArchive(archivePath)
	if err := ensureIdxSubdir(filepath.Dir(archivePath)); err != nil {
		return nil, fmt.Errorf("sidecar: mkdir %s: %w", idxSubdirName, err)
	}
	// O_EXCL random name = no symlink clobbering in shared dirs
	tmp, err := os.CreateTemp(filepath.Dir(finalPath), filepath.Base(finalPath)+".write.*.tmp")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	registerCleanupPath(tmpPath)

	var headerPlaceholder [sidecarHeaderBytes]byte
	if _, err := tmp.Write(headerPlaceholder[:]); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return nil, err
	}

	return &sidecarWriter{
		finalPath: finalPath,
		tmpPath:   tmpPath,
		f:         tmp,
		bw:        bufio.NewWriterSize(tmp, 1<<20),
	}, nil
}

func (w *sidecarWriter) WriteHash(k uint64) error {
	if w == nil || w.f == nil {
		return fmt.Errorf("sidecar: writer closed")
	}
	binary.LittleEndian.PutUint64(w.buf[:], k)
	if _, err := w.bw.Write(w.buf[:]); err != nil {
		return err
	}
	w.count++
	return nil
}

func (w *sidecarWriter) Commit() (uint64, error) {
	if w == nil || w.f == nil {
		return 0, fmt.Errorf("sidecar: writer closed")
	}
	if err := w.bw.Flush(); err != nil {
		_ = w.Abort()
		return 0, err
	}
	header := makeSidecarHeader(w.count)
	if _, err := w.f.WriteAt(header[:], 0); err != nil {
		_ = w.Abort()
		return 0, err
	}
	if err := w.f.Sync(); err != nil {
		_ = w.Abort()
		return 0, err
	}
	if err := w.f.Close(); err != nil {
		w.f = nil
		_ = os.Remove(w.tmpPath)
		return 0, err
	}
	w.f = nil

	if err := atomicfs.Rename(w.tmpPath, w.finalPath); err != nil {
		_ = os.Remove(w.tmpPath)
		return 0, err
	}
	return w.count, nil
}

func (w *sidecarWriter) Abort() error {
	if w == nil {
		return nil
	}
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	err := os.Remove(w.tmpPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// 32-byte canonical header w/ given keyCount
func makeSidecarHeader(keyCount uint64) [sidecarHeaderBytes]byte {
	var h [sidecarHeaderBytes]byte
	copy(h[0:4], sidecarMagic)
	binary.LittleEndian.PutUint16(h[4:6], sidecarFormatVer)
	binary.LittleEndian.PutUint16(h[6:8], sidecarHashAlgoXX)
	binary.LittleEndian.PutUint64(h[8:16], keyCount)
	binary.LittleEndian.PutUint64(h[16:24], parserVersion)
	// bytes 24..31 reserved (zero)
	return h
}
