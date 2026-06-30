package sflog

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDispatchOrInlineRunsInlineWithoutBudget(t *testing.T) {
	var wg sync.WaitGroup
	gotSlot := 99
	ran := false
	dispatchOrInline(context.Background(), extractCtx{}, &wg, func(slot int) {
		ran = true
		gotSlot = slot
	})
	wg.Wait()
	if !ran {
		t.Fatal("fn never ran")
	}
	if gotSlot != -1 {
		t.Fatalf("inline run slot = %d, want -1", gotSlot)
	}
}

func TestDispatchOrInlineRunsInlineAtDepth(t *testing.T) {
	p := NewProgress()
	p.SetWorkers(4)
	ec := extractCtx{sem: make(chan struct{}, 4), p: p, depth: 1}
	var wg sync.WaitGroup
	slot := 99
	dispatchOrInline(context.Background(), ec, &wg, func(s int) { slot = s })
	wg.Wait()
	if slot != -1 {
		t.Fatalf("depth>0 must run inline (slot -1), got %d", slot)
	}
}

func TestDispatchOrInlinePooledLeasesAndReturnsSlot(t *testing.T) {
	p := NewProgress()
	p.SetWorkers(4)
	ec := extractCtx{sem: make(chan struct{}, 4), p: p, depth: 0}
	var wg sync.WaitGroup
	got := make(chan int, 1)
	dispatchOrInline(context.Background(), ec, &wg, func(slot int) { got <- slot })
	wg.Wait()
	if slot := <-got; slot < 0 {
		t.Fatalf("pooled run should lease a slot >= 0, got %d", slot)
	}
	// The leased slot is returned to the free-list after the run, so all four
	// slots are acquirable again.
	seen := map[int]bool{}
	for i := 0; i < 4; i++ {
		s := p.acquireSlot()
		if s < 0 {
			t.Fatalf("slot exhausted at %d after pooled release", i)
		}
		seen[s] = true
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 distinct slots after release, got %v", seen)
	}
}

// TestDispatchOrInlineBoundedAndDeadlockFree fires far more tasks than the budget
// from a single caller and proves (a) every task completes (no deadlock / no
// dropped work) and (b) concurrency never exceeds the budget plus the one inline
// task the saturated caller runs itself.
func TestDispatchOrInlineBoundedAndDeadlockFree(t *testing.T) {
	const capN = 3
	const tasks = 50
	p := NewProgress()
	p.SetWorkers(capN)
	ec := extractCtx{sem: make(chan struct{}, capN), p: p, depth: 0}

	var wg sync.WaitGroup
	var inFlight, peak, done atomic.Int64
	bump := func() {
		n := inFlight.Add(1)
		for {
			pk := peak.Load()
			if n <= pk || peak.CompareAndSwap(pk, n) {
				break
			}
		}
	}
	for i := 0; i < tasks; i++ {
		dispatchOrInline(context.Background(), ec, &wg, func(int) {
			bump()
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
			done.Add(1)
		})
	}
	wg.Wait()
	if done.Load() != tasks {
		t.Fatalf("completed %d/%d tasks (deadlock or dropped work)", done.Load(), tasks)
	}
	if peak.Load() > capN+1 {
		t.Fatalf("peak concurrency %d exceeded budget+inline (%d)", peak.Load(), capN+1)
	}
}

func TestRaceProbeSingleCandidateSkipsProbe(t *testing.T) {
	calls := 0
	got, ok := raceProbe(context.Background(), extractCtx{}, []string{"only"},
		func(context.Context, string) bool { calls++; return false })
	if !ok || got != "only" {
		t.Fatalf("single candidate: got %q ok=%v, want only/true", got, ok)
	}
	if calls != 0 {
		t.Fatalf("single candidate should not probe; calls=%d", calls)
	}
}

func TestRaceProbeAllWrong(t *testing.T) {
	got, ok := raceProbe(context.Background(), extractCtx{}, []string{"a", "b", "c"},
		func(context.Context, string) bool { return true })
	if ok || got != "" {
		t.Fatalf("all-wrong: got %q ok=%v, want \"\"/false", got, ok)
	}
}

// TestRaceProbeSequentialStopsAtFirstWinner: with no budget the probes run inline
// in order and must stop at the first accepted candidate (no wasted probing).
func TestRaceProbeSequentialStopsAtFirstWinner(t *testing.T) {
	var tried []string
	probe := func(_ context.Context, pw string) bool {
		tried = append(tried, pw)
		return pw != "good" // wrong unless "good"
	}
	got, ok := raceProbe(context.Background(), extractCtx{depth: 0}, []string{"a", "good", "b"}, probe)
	if !ok || got != "good" {
		t.Fatalf("got %q ok=%v, want good/true", got, ok)
	}
	if len(tried) != 2 || tried[0] != "a" || tried[1] != "good" {
		t.Fatalf("sequential fallback should stop after good; tried=%v", tried)
	}
}

// TestRaceProbeParallelFirstWinsCancelsLosers proves the parallel race returns as
// soon as one candidate is accepted and cancels the rest: the loser probes block
// on the run context, so if cancellation did not propagate this test would hang
// (caught by the deadline), and the wrong candidate would never be returned.
func TestRaceProbeParallelFirstWinsCancelsLosers(t *testing.T) {
	p := NewProgress()
	p.SetWorkers(4)
	ec := extractCtx{sem: make(chan struct{}, 4), p: p, depth: 0}
	probe := func(ctx context.Context, pw string) bool {
		if pw == "good" {
			return false // immediate winner
		}
		<-ctx.Done() // loser: only unblocks when the winner cancels the race
		return true
	}
	type result struct {
		pw string
		ok bool
	}
	ch := make(chan result, 1)
	go func() {
		pw, ok := raceProbe(context.Background(), ec, []string{"l1", "good", "l2", "l3"}, probe)
		ch <- result{pw, ok}
	}()
	select {
	case r := <-ch:
		if !r.ok || r.pw != "good" {
			t.Fatalf("parallel race winner = %q ok=%v, want good/true", r.pw, r.ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("raceProbe did not return: losers were not cancelled by the winner")
	}
}

func TestProgressSlotAllocatorBounded(t *testing.T) {
	p := NewProgress()
	p.SetWorkers(3)
	seen := map[int]bool{}
	for i := 0; i < 3; i++ {
		s := p.acquireSlot()
		if s < 0 || s >= 3 || seen[s] {
			t.Fatalf("acquire %d returned bad/duplicate slot %d", i, s)
		}
		seen[s] = true
	}
	if s := p.acquireSlot(); s != -1 {
		t.Fatalf("over-acquire returned %d, want -1", s)
	}
	p.releaseSlot(1)
	if s := p.acquireSlot(); s != 1 {
		t.Fatalf("reacquire after release returned %d, want 1", s)
	}
}

func TestProgressSlotAllocatorNilSafe(t *testing.T) {
	var p *Progress
	if s := p.acquireSlot(); s != -1 {
		t.Fatalf("nil acquireSlot = %d, want -1", s)
	}
	p.releaseSlot(5) // must not panic
	if s := NewProgress().acquireSlot(); s != -1 {
		t.Fatalf("no-SetWorkers acquireSlot = %d, want -1", s)
	}
}

// TestProgressSlotAllocatorRace hammers acquire/release from many goroutines.
// Under -race it proves the free-list is race-clean; it also asserts slots stay
// within bounds and none leak (exactly n acquirable once the storm settles).
func TestProgressSlotAllocatorRace(t *testing.T) {
	const n = 8
	p := NewProgress()
	p.SetWorkers(n)
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				s := p.acquireSlot()
				if s >= n {
					t.Errorf("acquired slot %d >= n %d", s, n)
					return
				}
				if s >= 0 {
					p.setActive(s, "x", StageExtracting)
					p.releaseSlot(s)
				}
			}
		}()
	}
	wg.Wait()
	got := 0
	for p.acquireSlot() >= 0 {
		if got++; got > n {
			break
		}
	}
	if got != n {
		t.Fatalf("after the storm, acquirable slots = %d, want %d (leak or overflow)", got, n)
	}
}
