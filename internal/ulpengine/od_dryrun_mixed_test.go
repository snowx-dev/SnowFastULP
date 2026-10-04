package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestODDryRunMixedStateSidecarFilter: a dry run regenerates dirty parts
// (missing, stale, malformed sidecar, legacy v2) into temp sidecars under
// the run temp dir (M-06), so the preview counts their real keys and the
// DestSidecarPaths list mirrors what a real -od would load. Only parts
// whose regen actually fails (corrupt archive) stay skipped. The library
// itself must remain byte-identical: the rebuilt .idx copies live in the
// run temp dir, never next to the archives.
func TestODDryRunMixedStateSidecarFilter(t *testing.T) {
	dir := t.TempDir()
	stage := t.TempDir()

	// Real archives: the dry-run regen path decompresses dirty parts, so
	// fake non-zstd payloads would fail regen and trip the majority-skipped
	// refusal (same refusal a real -od would raise).
	freshArch := writeZstdArchivePath(t, dir, "fresh", []string{
		"f1.example.com:u1:p", "f2.example.com:u2:p",
		"f3.example.com:u3:p", "f4.example.com:u4:p",
	})
	writeSidecarKeysForTest(t, freshArch, []uint64{7, 3, 9, 3, 1}) // 4 unique

	v2Arch := writeZstdArchivePath(t, dir, "legacyv2", []string{
		"v1.example.com:u1:p", "v2.example.com:u2:p",
	})
	writeV2Sidecar(t, v2Arch, []uint64{5, 2}) // legacy, no identity → regen

	writeZstdArchivePath(t, dir, "nosidecar", []string{
		"n1.example.com:u1:p", "n2.example.com:u2:p", "n3.example.com:u3:p",
	}) // no sidecar on disk → regen

	staleArch := writeZstdArchivePath(t, dir, "stale", []string{
		"s1.example.com:u1:p", "s2.example.com:u2:p",
	})
	writeSidecarKeysForTest(t, staleArch, []uint64{11, 12})
	// archive newer than its sidecar = stale → regen
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(staleArch, future, future); err != nil {
		t.Fatal(err)
	}

	malformedArch := writeZstdArchivePath(t, dir, "broken", []string{
		"b1.example.com:u1:p",
	})
	scPath := sidecarPathForArchive(malformedArch)
	if err := os.MkdirAll(filepath.Dir(scPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scPath, []byte("not a sidecar"), 0o644); err != nil {
		t.Fatal(err)
	} // malformed sidecar, valid archive → regen rebuilds it

	// corrupt archive: regen fails, part stays skipped (real -od parity)
	corruptArch := filepath.Join(dir, "sfu_corrupt.txt.zst")
	if err := os.WriteFile(corruptArch, []byte("garbage bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := libraryFingerprint(t, dir)

	logPath := filepath.Join(t.TempDir(), "sfu.log")
	dbg, err := NewDebugLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dbg.Close()

	res, err := runODScan(context.Background(), odConfig{
		Dest:         dir,
		CurrentRunID: "sfu_self",
		Buckets:      4,
		TempDir:      stage,
		DryRun:       true,
		Debug:        dbg,
	}, nil)
	if err != nil {
		t.Fatalf("dry-run scan: %v", err)
	}

	// 5 usable parts: the fresh one plus the four regen'd into temp. The
	// corrupt part stays skipped.
	if len(res.DestSidecarPaths) != 5 {
		t.Fatalf("DestSidecarPaths = %d entries (%v), want 5", len(res.DestSidecarPaths), res.DestSidecarPaths)
	}
	// keys: fresh 4 + v2 archive 2 + nosidecar 3 + stale 2 + broken 1
	if res.TotalKeysLoaded != 12 {
		t.Errorf("TotalKeysLoaded = %d, want 12 (all usable parts' real keys)", res.TotalKeysLoaded)
	}

	// every non-fresh entry is a temp sidecar under the run temp dir; the
	// fresh part keeps its library path.
	regenDir := filepath.Join(stage, dryRunRegenDirName)
	for _, sc := range res.DestSidecarPaths {
		if sc == sidecarPathForArchive(freshArch) {
			continue
		}
		if !strings.HasPrefix(sc, regenDir+string(os.PathSeparator)) {
			t.Errorf("dest sidecar %s is not the fresh library sidecar nor a temp regen sidecar under %s", sc, regenDir)
		}
	}
	for _, name := range []string{"sfu_legacyv2", "sfu_nosidecar", "sfu_stale", "sfu_broken"} {
		want := filepath.Join(regenDir, name+".txt.zst.idx")
		if _, err := os.Stat(want); err != nil {
			t.Errorf("missing temp regen sidecar %s: %v", want, err)
		}
	}

	// skipped corrupt part surfaces on the debug log
	logBody, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBody), "regen SKIP (corrupt): part=sfu_corrupt.txt.zst") {
		t.Errorf("debug log missing corrupt-part skip line:\n%s", logBody)
	}

	// library byte-identical: no .idx written for the missing/stale/broken
	// parts, corrupt archive untouched, fresh sidecar not rewritten.
	after := libraryFingerprint(t, dir)
	if len(after) != len(before) {
		t.Errorf("library file count changed: before=%d after=%d", len(before), len(after))
	}
	for rel, sum := range before {
		if after[rel] != sum {
			t.Errorf("library file %q changed (or appeared/disappeared)", rel)
		}
	}
}
