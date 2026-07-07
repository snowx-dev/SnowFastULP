package main

import (
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// step mutates the engine metrics to mimic one observable moment of the -od
// pipeline, the way the monitor goroutine would sample it.
type step struct {
	name string
	mut  func(m *ulpengine.Metrics, od *ulpengine.ODMetrics)
}

// drive walks the steps, sampling ingestProgress through the same monotonic
// clamp the live BeginIngest closure applies, and returns the displayed
// fractions. It mirrors cmd/sfl/main.go's ingestToLibrary closure exactly.
func drive(ulpBytes int64, steps []step) []float64 {
	m := &ulpengine.Metrics{}
	od := &ulpengine.ODMetrics{}
	var last float64
	out := make([]float64, 0, len(steps))
	for _, s := range steps {
		s.mut(m, od)
		raw, _ := ingestProgress(m, od, ulpBytes)
		out = append(out, monotonic(raw, &last))
	}
	return out
}

func assertMonotonicInRange(t *testing.T, steps []step, fracs []float64) {
	t.Helper()
	prev := -1.0
	for i, f := range fracs {
		if f < 0 || f > 1 {
			t.Fatalf("step %d (%s): fraction %.4f out of [0,1]", i, steps[i].name, f)
		}
		if f < prev {
			t.Fatalf("step %d (%s): fraction went backward %.4f -> %.4f", i, steps[i].name, prev, f)
		}
		prev = f
	}
	if last := fracs[len(fracs)-1]; last != 1.0 {
		t.Fatalf("final fraction = %.4f, want 1.0", last)
	}
}

// TestIngestProgressColdLibraryMonotonic reproduces the cold-library regen path
// where shard runs concurrently with regen and m.Phase flips back to phasePhase0
// after shard. The displayed bar must never reverse and must end at 1.0.
func TestIngestProgressColdLibraryMonotonic(t *testing.T) {
	const ulpBytes = 1000
	steps := []step{
		{"init", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseInit)
		}},
		{"phase0 classify", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhasePhase0)
			od.RegenBytesTotal.Store(10000)
		}},
		{"shard reading (regen barely started)", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseShard)
			m.BytesRead.Store(900) // shard nearly done...
			od.RegenBytesRead.Store(500)
		}},
		{"shard done, regen still going", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseShard)
			m.BytesRead.Store(1000) // shard fully done - must NOT spike the bar
			od.RegenBytesRead.Store(3000)
		}},
		{"phase0 again, regen draining", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhasePhase0)
			od.RegenBytesRead.Store(7000)
		}},
		{"regen complete", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhasePhase0)
			od.RegenBytesRead.Store(10000)
		}},
		{"dedup start", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseDedup)
			m.BucketsBytesTotal.Store(2000)
			m.BucketsBytesRead.Store(0)
		}},
		{"dedup mid", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.BucketsBytesRead.Store(1000)
		}},
		{"dedup end", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.BucketsBytesRead.Store(2000)
		}},
		{"done", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseDone)
		}},
	}
	fracs := drive(ulpBytes, steps)
	assertMonotonicInRange(t, steps, fracs)
}

// TestIngestProgressAdverseReorderClamped covers the worst interleave: shard
// advances far while RegenBytesTotal is still 0 (od goroutine hasn't classified
// yet), then regen totals appear with little read. Raw ingestProgress would
// drop here; the clamp must hold the bar.
func TestIngestProgressAdverseReorderClamped(t *testing.T) {
	const ulpBytes = 1000
	steps := []step{
		{"shard ahead of classify", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseShard)
			m.BytesRead.Store(800) // ~0.53 via ULP branch (no regen total yet)
		}},
		{"regen total appears, read tiny", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhasePhase0)
			od.RegenBytesTotal.Store(100000)
			od.RegenBytesRead.Store(10) // raw would crater to ~0.03
		}},
		{"regen progressing", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			od.RegenBytesRead.Store(100000)
		}},
		{"dedup", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseDedup)
			m.BucketsBytesTotal.Store(10)
			m.BucketsBytesRead.Store(10)
		}},
		{"done", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseDone)
		}},
	}
	fracs := drive(ulpBytes, steps)
	assertMonotonicInRange(t, steps, fracs)

	// The second sample must not collapse below the shard-driven first sample.
	if fracs[1] < fracs[0] {
		t.Fatalf("clamp failed: %.4f -> %.4f", fracs[0], fracs[1])
	}
}

// TestIngestProgressWarmLibraryMonotonic covers the warm path: no regen, so the
// pre-dedup region is driven purely by the ULP read.
func TestIngestProgressWarmLibraryMonotonic(t *testing.T) {
	const ulpBytes = 1000
	steps := []step{
		{"init", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseInit)
		}},
		{"shard 0", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseShard)
			m.BytesRead.Store(0)
		}},
		{"shard half", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.BytesRead.Store(500)
		}},
		{"shard full", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.BytesRead.Store(1000)
		}},
		{"dedup", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseDedup)
			m.BucketsBytesTotal.Store(100)
			m.BucketsBytesRead.Store(50)
		}},
		{"dedup done", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.BucketsBytesRead.Store(100)
		}},
		{"done", func(m *ulpengine.Metrics, od *ulpengine.ODMetrics) {
			m.Phase.Store(ulpengine.PhaseDone)
		}},
	}
	fracs := drive(ulpBytes, steps)
	assertMonotonicInRange(t, steps, fracs)
}

// TestIngestProgressBounds spot-checks the static anchors of the mapping.
func TestIngestProgressBounds(t *testing.T) {
	m := &ulpengine.Metrics{}
	od := &ulpengine.ODMetrics{}

	m.Phase.Store(ulpengine.PhaseInit)
	if f, _ := ingestProgress(m, od, 0); f <= 0 || f >= 0.1 {
		t.Fatalf("init fraction = %.4f, want a small positive anchor", f)
	}

	m.Phase.Store(ulpengine.PhaseDone)
	if f, _ := ingestProgress(m, od, 0); f != 1.0 {
		t.Fatalf("done fraction = %.4f, want 1.0", f)
	}
}

// TestMonotonicHelper verifies the clamp primitive directly.
func TestMonotonicHelper(t *testing.T) {
	var last float64
	in := []float64{0.1, 0.4, 0.2, 0.2, 0.9, 0.5, 1.0}
	want := []float64{0.1, 0.4, 0.4, 0.4, 0.9, 0.9, 1.0}
	for i, v := range in {
		if got := monotonic(v, &last); got != want[i] {
			t.Fatalf("monotonic(%v) at %d = %v, want %v", v, i, got, want[i])
		}
	}
}
