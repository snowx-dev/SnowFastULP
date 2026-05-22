package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// headline -od integration. run2 must exclude run1 dups, include new
// creds. also checks sidecar key counts, linesSkippedByDest, and that
// the current run's stamp is filtered from discovery
func TestODTwoRunDedup(t *testing.T) {
	libDir := t.TempDir()

	// run1, 5 creds, emits archive + sidecar
	run1Input := filepath.Join(t.TempDir(), "in1.txt")
	writeFileContent(t, run1Input, strings.Join([]string{
		"https://a.example.com:user1:pw1",
		"https://b.example.com:user2:pw2",
		"https://c.example.com:user3:pw3",
		"https://d.example.com:user4:pw4",
		"https://e.example.com:user5:pw5",
	}, "\n")+"\n")

	run1Stamp := "stamp_one"
	r1, err := resolvePipelineConfig(pipelineConfig{
		Inputs:       []string{run1Input},
		Output:       filepath.Join(libDir, "sfu_"+run1Stamp+".txt.zst"),
		TempDir:      filepath.Join(libDir, ".stage1"),
		FastPathOff:  true,
		Buckets:      4,
		Compress:     true,
		DestDedup:    true,
		DestDedupDir: libDir,
		RunStamp:     run1Stamp,
	})
	if err != nil {
		t.Fatalf("resolve run1: %v", err)
	}
	if err := run(context.Background(), &resolved{
		cfg:          r1.cfg,
		totalInputs:  r1.totalInputs,
		mem:          r1.mem,
		bucketCount:  4,
		workers:      1,
		dedupWorkers: 1,
		chunkBytes:   1 << 20,
		tempDir:      filepath.Join(libDir, ".stage1"),
	}, &metrics{}); err != nil {
		t.Fatalf("run1: %v", err)
	}

	run1Archive := filepath.Join(libDir, "sfu_"+run1Stamp+".txt.zst")
	if _, err := os.Stat(run1Archive); err != nil {
		t.Fatalf("run1 archive missing: %v", err)
	}
	hdr, err := readSidecarHeader(sidecarPathForArchive(run1Archive))
	if err != nil {
		t.Fatalf("run1 sidecar: %v", err)
	}
	if hdr.keyCount != 5 {
		t.Errorf("run1 sidecar keyCount = %d, want 5", hdr.keyCount)
	}

	// run2, 3 dups w/ run1 + 2 new
	run2Input := filepath.Join(t.TempDir(), "in2.txt")
	writeFileContent(t, run2Input, strings.Join([]string{
		"https://a.example.com:user1:pw1", // dup
		"https://b.example.com:user2:pw2", // dup
		"https://c.example.com:user3:pw3", // dup
		"https://f.example.com:user6:pw6", // new
		"https://g.example.com:user7:pw7", // new
	}, "\n")+"\n")

	run2Stamp := "stamp_two"
	r2, err := resolvePipelineConfig(pipelineConfig{
		Inputs:       []string{run2Input},
		Output:       filepath.Join(libDir, "sfu_"+run2Stamp+".txt.zst"),
		TempDir:      filepath.Join(libDir, ".stage2"),
		FastPathOff:  true,
		Buckets:      4,
		Compress:     true,
		DestDedup:    true,
		DestDedupDir: libDir,
		RunStamp:     run2Stamp,
	})
	if err != nil {
		t.Fatalf("resolve run2: %v", err)
	}
	m2 := &metrics{}
	if err := run(context.Background(), &resolved{
		cfg:          r2.cfg,
		totalInputs:  r2.totalInputs,
		mem:          r2.mem,
		bucketCount:  4,
		workers:      1,
		dedupWorkers: 1,
		chunkBytes:   1 << 20,
		tempDir:      filepath.Join(libDir, ".stage2"),
	}, m2); err != nil {
		t.Fatalf("run2: %v", err)
	}

	if got := m2.linesSkippedByDest.Load(); got != 3 {
		t.Errorf("linesSkippedByDest = %d, want 3", got)
	}
	if got := m2.linesUnique.Load(); got != 2 {
		t.Errorf("linesUnique = %d, want 2", got)
	}

	run2Archive := filepath.Join(libDir, "sfu_"+run2Stamp+".txt.zst")
	out2 := readZstdLines(t, run2Archive)
	if len(out2) != 2 {
		t.Errorf("run2 output has %d lines, want 2; lines=%v", len(out2), out2)
	}
	for _, want := range []string{"f.example.com:user6:pw6", "g.example.com:user7:pw7"} {
		if !containsLine(out2, want) {
			t.Errorf("run2 output missing %q; got %v", want, out2)
		}
	}
	// no run1 creds in run2 output
	for _, oldCred := range []string{
		"a.example.com:user1:pw1",
		"b.example.com:user2:pw2",
		"c.example.com:user3:pw3",
	} {
		if containsLine(out2, oldCred) {
			t.Errorf("run2 should NOT contain %q (dup with run 1)", oldCred)
		}
	}

	hdr2, err := readSidecarHeader(sidecarPathForArchive(run2Archive))
	if err != nil {
		t.Fatalf("run2 sidecar: %v", err)
	}
	if hdr2.keyCount != 2 {
		t.Errorf("run2 sidecar keyCount = %d, want 2", hdr2.keyCount)
	}
}

// archive matching current run stamp must not be picked up. guards
// against deduping vs an in-progress sibling
func TestODSelfExcludesCurrentRunStamp(t *testing.T) {
	libDir := t.TempDir()
	currentStamp := "live_run"
	planted := filepath.Join(libDir, "sfu_"+currentStamp+".txt.zst")
	writeZstdArchive(t, planted, []string{"x.example.com:u:p"})

	runs, err := discoverArchiveRuns(libDir, "sfu_"+currentStamp)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("expected 0 runs (self filtered), got %d: %+v", len(runs), runs)
	}
}

// user deleted sidecar between runs, run2 must silently regen + dedup
func TestODSidecarRegenAcrossRuns(t *testing.T) {
	libDir := t.TempDir()
	// hand-plant run1 archive, skips full pipeline
	pastArchive := filepath.Join(libDir, "sfu_old.txt.zst")
	writeZstdArchive(t, pastArchive, []string{
		"https://a.example.com:user1:pw1",
		"https://b.example.com:user2:pw2",
	})

	run2Input := filepath.Join(t.TempDir(), "in.txt")
	writeFileContent(t, run2Input, strings.Join([]string{
		"https://a.example.com:user1:pw1", // dup
		"https://z.example.com:user9:pw9", // new
	}, "\n")+"\n")

	r, err := resolvePipelineConfig(pipelineConfig{
		Inputs:       []string{run2Input},
		Output:       filepath.Join(libDir, "sfu_new.txt.zst"),
		TempDir:      filepath.Join(libDir, ".stage"),
		FastPathOff:  true,
		Buckets:      4,
		Compress:     true,
		DestDedup:    true,
		DestDedupDir: libDir,
		RunStamp:     "new",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	m := &metrics{}
	if err := run(context.Background(), &resolved{
		cfg:          r.cfg,
		totalInputs:  r.totalInputs,
		mem:          r.mem,
		bucketCount:  4,
		workers:      1,
		dedupWorkers: 1,
		chunkBytes:   1 << 20,
		tempDir:      filepath.Join(libDir, ".stage"),
	}, m); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := m.linesSkippedByDest.Load(); got != 1 {
		t.Errorf("linesSkippedByDest = %d, want 1", got)
	}
	if got := m.linesUnique.Load(); got != 1 {
		t.Errorf("linesUnique = %d, want 1", got)
	}

	// regenerated sidecar for old archive must exist for next run
	if _, err := readSidecarHeader(sidecarPathForArchive(pastArchive)); err != nil {
		t.Errorf("regenerated sidecar for past archive missing: %v", err)
	}
}

// helpers, integration suite is self-contained

func writeFileContent(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// decompress .zst into lines, test-only
func readZstdLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	data, err := readAllLines(dec)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readAllLines(r interface {
	Read([]byte) (int, error)
}) ([]string, error) {
	var out []string
	var buf [4096]byte
	var partial []byte
	for {
		n, err := r.Read(buf[:])
		if n > 0 {
			chunk := append(partial, buf[:n]...)
			for {
				idx := bytesIndexByte(chunk, '\n')
				if idx < 0 {
					partial = append(partial[:0], chunk...)
					break
				}
				out = append(out, string(chunk[:idx]))
				chunk = chunk[idx+1:]
			}
		}
		if err != nil {
			if len(partial) > 0 {
				out = append(out, string(partial))
			}
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}

func bytesIndexByte(s []byte, b byte) int {
	for i, c := range s {
		if c == b {
			return i
		}
	}
	return -1
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
