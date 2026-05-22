package search_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"

	"github.com/klauspost/compress/zstd"
)

// 8 zstd frames x 8 MiB uncompressed = 64 MiB total, ULP-shape text.
// rare: 24-byte cookie ~once per frame, decode + BMH dominates.
// dense: 8-byte substr in every line, extractLine + chan send shows up.
// Workers=1 to isolate searchChunk loop
const (
	benchFrameBytes  = 8 << 20
	benchFrameCount  = 8
	benchRarePattern = "ZZZZ-NEEDLE-RARE-COOKIE"  // 24 bytes
	benchDensePat    = "user@xyz" // 8 bytes, in every line
)

func buildBenchCorpus(b *testing.B) string {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.zst")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()

	// synthetic ULP line, ~76 bytes, has the dense pattern
	const line = "https://shop.example.com/login:user@xyz.example.com:hunter2A_password\n"
	linesPerFrame := benchFrameBytes / len(line)
	frame := make([]byte, 0, benchFrameBytes+len(line))
	for i := 0; i < linesPerFrame; i++ {
		frame = append(frame, line...)
	}
	// rare pattern once at end of each frame
	rareTail := []byte(benchRarePattern + "\n")
	frame = append(frame, rareTail...)

	for i := 0; i < benchFrameCount; i++ {
		enc, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := enc.Write(frame); err != nil {
			b.Fatal(err)
		}
		if err := enc.Close(); err != nil {
			b.Fatal(err)
		}
	}
	return path
}

func runBenchSearch(b *testing.B, archive string, pattern []byte) {
	b.Helper()
	sc, err := index.Build(context.Background(), archive, nil, nil)
	if err != nil {
		b.Fatalf("index.Build: %v", err)
	}
	if len(sc.Chunks) != benchFrameCount {
		b.Fatalf("frames = %d, want %d", len(sc.Chunks), benchFrameCount)
	}

	var totalScan int64
	for _, c := range sc.Chunks {
		totalScan += c.UncompressedEnd - c.UncompressedStart
	}
	b.SetBytes(totalScan)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		hitCh := make(chan search.Hit, 1024)
		// drain concurrently, dont measure printer cost
		done := make(chan struct{})
		go func() {
			for range hitCh {
			}
			close(done)
		}()
		err := search.Run(search.Config{
			Pattern:    pattern,
			Workers:    1,
			Archives:   []string{archive},
			Sidecars:   map[string]*index.Sidecar{archive: sc},
			Hits:       hitCh,
			ArchiveOrd: map[string]int{archive: 0},
		})
		close(hitCh)
		<-done
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearch_Rare(b *testing.B) {
	archive := buildBenchCorpus(b)
	if fi, err := os.Stat(archive); err == nil {
		b.Logf("archive: %s (%.1f MiB compressed, %d MiB uncompressed)",
			archive, float64(fi.Size())/(1<<20),
			benchFrameBytes*benchFrameCount/(1<<20))
	}
	runBenchSearch(b, archive, []byte(benchRarePattern))
}

// sweep DecodeStep knob, confirms override path + per-cpu evidence
// for default 1-MiB. winner is host-dependent
func BenchmarkSearch_DecodeStep(b *testing.B) {
	archive := buildBenchCorpus(b)
	sc, err := index.Build(context.Background(), archive, nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	var totalScan int64
	for _, c := range sc.Chunks {
		totalScan += c.UncompressedEnd - c.UncompressedStart
	}
	for _, step := range []int{0 /* default */, 256 << 10, 512 << 10, 1 << 20} {
		label := "default"
		if step > 0 {
			label = fmt.Sprintf("%dKiB", step>>10)
		}
		b.Run("Dense_"+label, func(b *testing.B) {
			b.SetBytes(totalScan)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				hitCh := make(chan search.Hit, 1024)
				done := make(chan struct{})
				go func() {
					for range hitCh {
					}
					close(done)
				}()
				err := search.Run(search.Config{
					DecodeStep: step,
					Pattern:    []byte(benchDensePat),
					Workers:    1,
					Archives:   []string{archive},
					Sidecars:   map[string]*index.Sidecar{archive: sc},
					Hits:       hitCh,
					ArchiveOrd: map[string]int{archive: 0},
				})
				close(hitCh)
				<-done
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSearch_Dense(b *testing.B) {
	archive := buildBenchCorpus(b)
	runBenchSearch(b, archive, []byte(benchDensePat))
}

// dense + chunk-level parallelism, confirms worker pool scales when
// search loop is bottleneck vs hits channel contention
func BenchmarkSearch_DenseW4(b *testing.B) {
	archive := buildBenchCorpus(b)
	sc, err := index.Build(context.Background(), archive, nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	var totalScan int64
	for _, c := range sc.Chunks {
		totalScan += c.UncompressedEnd - c.UncompressedStart
	}
	b.SetBytes(totalScan)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hitCh := make(chan search.Hit, 1024)
		done := make(chan struct{})
		var n int
		go func() {
			for range hitCh {
				n++
			}
			close(done)
		}()
		err := search.Run(search.Config{
			Pattern:    []byte(benchDensePat),
			Workers:    4,
			Archives:   []string{archive},
			Sidecars:   map[string]*index.Sidecar{archive: sc},
			Hits:       hitCh,
			ArchiveOrd: map[string]int{archive: 0},
		})
		close(hitCh)
		<-done
		if err != nil {
			b.Fatal(err)
		}
		_ = n
	}
}

