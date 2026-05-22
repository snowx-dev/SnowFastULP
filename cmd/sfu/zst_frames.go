package main

import (
	"fmt"
	"os"
)

// max uncompressed payload per zstd frame. tests may shrink this
var zstFrameUncompressedBytes int64 = 128 << 20

type zstFrameTracker struct {
	chunks           []searchFrameChunk
	frameCompStart   int64
	frameUncompStart int64
	frameUncompBytes int64
}

func newZstFrameTracker() *zstFrameTracker {
	return &zstFrameTracker{}
}

func (t *zstFrameTracker) noteUncompressed(n int64) {
	t.frameUncompBytes += n
}

func (t *zstFrameTracker) needsRotate() bool {
	return t.frameUncompBytes >= zstFrameUncompressedBytes
}

func (t *zstFrameTracker) recordFrame(f *os.File) error {
	if t.frameUncompBytes == 0 {
		return nil
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	compEnd := fi.Size()
	if compEnd < t.frameCompStart {
		return fmt.Errorf("zst frame: negative compressed size at %d", t.frameCompStart)
	}
	t.chunks = append(t.chunks, searchFrameChunk{
		ChunkID:           len(t.chunks),
		CompressedOffset:  t.frameCompStart,
		CompressedSize:    compEnd - t.frameCompStart,
		UncompressedStart: t.frameUncompStart,
		UncompressedEnd:   t.frameUncompStart + t.frameUncompBytes,
	})
	t.frameCompStart = compEnd
	t.frameUncompStart += t.frameUncompBytes
	t.frameUncompBytes = 0
	return nil
}

func (t *zstFrameTracker) chunksCopy() []searchFrameChunk {
	if len(t.chunks) == 0 {
		return nil
	}
	out := make([]searchFrameChunk, len(t.chunks))
	copy(out, t.chunks)
	return out
}
