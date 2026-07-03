package sflog

import (
	"sync"
	"testing"
	"time"
)

// TestScanFractionAndSecretCounters covers the -secrets progress plumbing: the
// scan bar tracks a source's whole weight credited once it finishes, against
// the same fixed total as the byte bar, so it only moves forward; the live
// found / scanned / total counters accumulate. Nil receivers stay safe.
func TestScanFractionAndSecretCounters(t *testing.T) {
	var np *Progress
	if np.ScanFraction() != 0 || np.SecretsFound() != 0 || np.SecretFilesScanned() != 0 || np.SecretFilesTotal() != 0 {
		t.Fatal("nil Progress must report zero secret metrics")
	}
	np.EnableSecrets() // must not panic
	if np.SecretsEnabled() {
		t.Fatal("nil Progress is never secrets-enabled")
	}

	p := NewProgress()
	if p.SecretsEnabled() {
		t.Fatal("secrets not enabled by default")
	}
	p.EnableSecrets()
	if !p.SecretsEnabled() {
		t.Fatal("EnableSecrets did not stick")
	}
	if p.ScanFraction() != 0 {
		t.Fatalf("ScanFraction with no total = %v, want 0", p.ScanFraction())
	}
	// The scan bar is a file count: scanned / total. Y (candidates) is seeded
	// ahead of X (scanned), so two of four scanned reads 50%.
	p.addSecretFilesTotal(4)
	p.addSecretFilesTotal(0) // no-op
	p.addSecretFileScanned()
	p.addSecretFileScanned()
	if p.SecretFilesScanned() != 2 {
		t.Fatalf("SecretFilesScanned = %d, want 2", p.SecretFilesScanned())
	}
	if got := p.ScanFraction(); got != 0.5 {
		t.Fatalf("ScanFraction = %v, want 0.5", got)
	}
	// scanned can never exceed total in practice, but a stray overshoot clamps.
	for i := 0; i < 5; i++ {
		p.addSecretFileScanned()
	}
	if got := p.ScanFraction(); got != 1.0 {
		t.Fatalf("ScanFraction (overshoot) = %v, want 1.0", got)
	}

	p.addSecretsFound(3)
	p.addSecretsFound(0) // no-op
	if p.SecretFilesTotal() != 4 {
		t.Fatalf("SecretFilesTotal = %d, want 4", p.SecretFilesTotal())
	}
	if p.SecretsFound() != 3 {
		t.Fatalf("SecretsFound = %d, want 3", p.SecretsFound())
	}
}

// TestCreditorAddConcurrent hammers creditor.add from many goroutines (as the
// zip member pool does) and asserts the clamp is atomic: exactly weight is
// credited, never more (overshoot) and never less (lost CAS retries). Run under
// -race to catch unsynchronized access.
func TestCreditorAddConcurrent(t *testing.T) {
	p := NewProgress()
	const weight = 1 << 20
	c := newCreditor(p, weight, 1)

	var wg sync.WaitGroup
	const goroutines, each, chunk = 64, 1000, 64 // 64*1000*64 = 4 MiB attempted >> 1 MiB weight
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				c.add(chunk)
			}
		}()
	}
	wg.Wait()
	c.finish()

	if got := c.credited.Load(); got != weight {
		t.Fatalf("credited = %d, want exactly weight %d (atomic clamp failed)", got, weight)
	}
	if got := p.DoneBytes(); got != weight {
		t.Fatalf("DoneBytes = %d, want %d (no overshoot, no loss)", got, weight)
	}
}

func TestProgressWorkerRegistryTracksActiveSlots(t *testing.T) {
	p := NewProgress()
	if got := p.WorkerCount(); got != 0 {
		t.Fatalf("WorkerCount before SetWorkers = %d, want 0", got)
	}
	p.SetWorkers(3)
	if got := p.WorkerCount(); got != 3 {
		t.Fatalf("WorkerCount = %d, want 3", got)
	}

	// Two busy slots (0 and 2), one idle (1).
	p.setActive(0, "/data/a.zip", StageTestingPassword)
	p.setActive(2, "/data/loose/Passwords.txt", StageParsing)

	active := p.ActiveWorkers(8)
	if len(active) != 2 {
		t.Fatalf("ActiveWorkers = %d, want 2 (%+v)", len(active), active)
	}
	// Lowest index first.
	if active[0].Index != 0 || active[0].Path != "/data/a.zip" || active[0].Stage != StageTestingPassword {
		t.Fatalf("active[0] = %+v", active[0])
	}
	if active[1].Index != 2 || active[1].Stage != StageParsing {
		t.Fatalf("active[1] = %+v", active[1])
	}

	// Stage update without touching the path.
	p.setStage(0, StageExtracting)
	if got := p.ActiveWorkers(8)[0].Stage; got != StageExtracting {
		t.Fatalf("after setStage, stage = %v, want extracting", got)
	}

	// Clearing a slot drops it from the snapshot.
	p.clearActive(0)
	active = p.ActiveWorkers(8)
	if len(active) != 1 || active[0].Index != 2 {
		t.Fatalf("after clearActive(0), active = %+v", active)
	}
}

func TestSetWorkerPathUpdatesPathKeepsStage(t *testing.T) {
	p := NewProgress()
	p.SetWorkers(2)
	p.setActive(0, "/data/outer.rar", StageExtracting)

	// Descending into a nested archive re-points the path but must not reset
	// the stage the nested reader publishes independently.
	p.setWorkerPath(0, "/data/outer.rar!inner.7z")
	got := p.ActiveWorkers(8)[0]
	if got.Path != "/data/outer.rar!inner.7z" {
		t.Fatalf("path = %q, want nested provenance", got.Path)
	}
	if got.Stage != StageExtracting {
		t.Fatalf("stage = %v, want unchanged (extracting)", got.Stage)
	}

	// Restoring the parent label leaves the (separately set) stage alone.
	p.setStage(0, StageTestingPassword)
	p.setWorkerPath(0, "/data/outer.rar")
	got = p.ActiveWorkers(8)[0]
	if got.Path != "/data/outer.rar" || got.Stage != StageTestingPassword {
		t.Fatalf("after restore = %+v, want parent path + testing-password stage", got)
	}

	// nil-safe and bounds-safe like the other slot writers.
	var np *Progress
	np.setWorkerPath(0, "x")
	q := NewProgress()
	q.SetWorkers(1)
	q.setWorkerPath(9, "oob") // out of range: no-op, no panic
}

func TestProgressActiveWorkersHonorsMax(t *testing.T) {
	p := NewProgress()
	p.SetWorkers(5)
	for i := 0; i < 5; i++ {
		p.setActive(i, "/x", StageExtracting)
	}
	if got := len(p.ActiveWorkers(2)); got != 2 {
		t.Fatalf("ActiveWorkers(2) = %d, want 2", got)
	}
	if got := p.ActiveWorkers(0); got != nil {
		t.Fatalf("ActiveWorkers(0) = %v, want nil", got)
	}
}

func TestProgressWorkerRegistryNilAndBoundsSafe(t *testing.T) {
	var p *Progress
	// nil receiver paths must not panic.
	p.SetWorkers(4)
	p.setActive(0, "x", StageOpening)
	p.clearActive(0)
	if got := p.WorkerCount(); got != 0 {
		t.Fatalf("nil WorkerCount = %d, want 0", got)
	}
	if got := p.ActiveWorkers(4); got != nil {
		t.Fatalf("nil ActiveWorkers = %v, want nil", got)
	}

	// Out-of-range indices on a sized registry are no-ops, not panics.
	q := NewProgress()
	q.SetWorkers(2)
	q.setActive(7, "oob", StageParsing)
	q.setStage(-1, StageParsing)
	if got := len(q.ActiveWorkers(8)); got != 0 {
		t.Fatalf("out-of-range writes leaked into registry: %d active", got)
	}
}

func TestWorkerStageString(t *testing.T) {
	cases := map[WorkerStage]string{
		StageIdle:            "",
		StageOpening:         "opening",
		StageTestingPassword: "testing password",
		StageExtracting:      "extracting",
		StageParsing:         "parsing",
		StageScanning:        "scanning",
	}
	for stage, want := range cases {
		if got := stage.String(); got != want {
			t.Fatalf("WorkerStage(%d).String() = %q, want %q", stage, got, want)
		}
	}
}

// TestSecretStreamsOpenCounter covers the "Y+" denominator signal plumbing:
// begin/end mark non-pre-counted sources open, SecretStreamsOpen reports the
// in-flight count, nesting balances, and nil receivers stay safe.
func TestSecretStreamsOpenCounter(t *testing.T) {
	var np *Progress
	np.beginSecretStream()
	np.endSecretStream()
	if got := np.SecretStreamsOpen(); got != 0 {
		t.Fatalf("nil SecretStreamsOpen = %d, want 0", got)
	}

	p := NewProgress()
	if got := p.SecretStreamsOpen(); got != 0 {
		t.Fatalf("initial SecretStreamsOpen = %d, want 0", got)
	}
	p.beginSecretStream()
	if got := p.SecretStreamsOpen(); got != 1 {
		t.Fatalf("after begin = %d, want 1", got)
	}
	p.beginSecretStream()
	if got := p.SecretStreamsOpen(); got != 2 {
		t.Fatalf("after nested begin = %d, want 2", got)
	}
	p.endSecretStream()
	if got := p.SecretStreamsOpen(); got != 1 {
		t.Fatalf("after one end = %d, want 1", got)
	}
	p.endSecretStream()
	if got := p.SecretStreamsOpen(); got != 0 {
		t.Fatalf("after all end = %d, want 0", got)
	}
}

// TestSetStageRecordsULPAndSecretActivity covers the "ulp + secrets" combined
// label signal: setStage timestamps the slot's last ULP (extracting/parsing) and
// last secret (scanning) activity, setActive starts a fresh window, and
// clearActive zeroes both so a reused slot can't inherit the prior item's
// window.
func TestSetStageRecordsULPAndSecretActivity(t *testing.T) {
	p := NewProgress()
	p.SetWorkers(2)

	p.setActive(0, "/data/a.rar", StageExtracting)
	w := p.ActiveWorkers(8)[0]
	if w.LastULP.IsZero() {
		t.Fatalf("setActive(Extracting) did not record LastULP")
	}
	if !w.LastSec.IsZero() {
		t.Fatalf("LastSec should be zero after setActive(Extracting), got %v", w.LastSec)
	}
	if time.Since(w.LastULP) > time.Second {
		t.Fatalf("LastULP not recent: %v", w.LastULP)
	}

	p.setStage(0, StageScanning)
	w = p.ActiveWorkers(8)[0]
	if w.LastSec.IsZero() {
		t.Fatalf("setStage(Scanning) did not record LastSec")
	}
	if time.Since(w.LastSec) > time.Second {
		t.Fatalf("LastSec not recent: %v", w.LastSec)
	}

	// StageTestingPassword must not erase the activity window (a brief opening
	// shouldn't reset recent ULP/secret signals).
	p.setStage(0, StageTestingPassword)
	w = p.ActiveWorkers(8)[0]
	if w.LastULP.IsZero() || w.LastSec.IsZero() {
		t.Fatalf("TestingPassword erased activity window: ULP=%v Sec=%v", w.LastULP, w.LastSec)
	}

	// clearActive zeroes both so the next lease starts clean.
	p.clearActive(0)
	p.setActive(0, "/data/b.zip", StageExtracting)
	w = p.ActiveWorkers(8)[0]
	if !w.LastSec.IsZero() {
		t.Fatalf("LastSec leaked across clearActive/re-lease: %v", w.LastSec)
	}
}
