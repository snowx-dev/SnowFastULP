package index

import "testing"

// validateChunks: gaps between compressed offsets are legal (skippable frames
// are not recorded as chunks); overlaps, unordered offsets, non-contiguous or
// non-zero-based uncompressed coverage, empty spans, and out-of-sequence IDs
// are all rejected.
func TestValidateChunksAcceptsGaps(t *testing.T) {
	sc := &Sidecar{Chunks: []Chunk{
		{ChunkID: 0, CompressedOffset: 0, CompressedSize: 16, UncompressedStart: 0, UncompressedEnd: 100},
		{ChunkID: 1, CompressedOffset: 40, CompressedSize: 16, UncompressedStart: 100, UncompressedEnd: 220},
	}}
	if err := validateChunks(sc); err != nil {
		t.Fatalf("gap between chunks must be accepted: %v", err)
	}
}

func TestValidateChunksRejectsBrokenMaps(t *testing.T) {
	valid := []Chunk{
		{ChunkID: 0, CompressedOffset: 0, CompressedSize: 16, UncompressedStart: 0, UncompressedEnd: 100},
		{ChunkID: 1, CompressedOffset: 16, CompressedSize: 16, UncompressedStart: 100, UncompressedEnd: 220},
	}
	for _, tc := range []struct {
		name  string
		chunk Chunk
		index int // position to replace
	}{
		{"compressed overlap", Chunk{ChunkID: 1, CompressedOffset: 8, CompressedSize: 16, UncompressedStart: 100, UncompressedEnd: 220}, 1},
		{"unordered offsets", Chunk{ChunkID: 1, CompressedOffset: -1, CompressedSize: 16, UncompressedStart: 100, UncompressedEnd: 220}, 1},
		{"non-contiguous coverage", Chunk{ChunkID: 1, CompressedOffset: 16, CompressedSize: 16, UncompressedStart: 120, UncompressedEnd: 220}, 1},
		{"empty uncompressed span", Chunk{ChunkID: 1, CompressedOffset: 16, CompressedSize: 16, UncompressedStart: 100, UncompressedEnd: 100}, 1},
		{"out-of-sequence id", Chunk{ChunkID: 3, CompressedOffset: 16, CompressedSize: 16, UncompressedStart: 100, UncompressedEnd: 220}, 1},
		{"zero compressed size", Chunk{ChunkID: 1, CompressedOffset: 16, CompressedSize: 0, UncompressedStart: 100, UncompressedEnd: 220}, 1},
		{"non-zero first start", Chunk{ChunkID: 0, CompressedOffset: 0, CompressedSize: 16, UncompressedStart: 4, UncompressedEnd: 100}, 0},
	} {
		chunks := append([]Chunk(nil), valid...)
		chunks[tc.index] = tc.chunk
		if err := validateChunks(&Sidecar{Chunks: chunks}); err == nil {
			t.Fatalf("%s: validateChunks accepted a broken map", tc.name)
		}
	}
}
