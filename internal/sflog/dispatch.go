package sflog

import (
	"context"
	"io"
	"sync"
)

// Processor turns one logical source (an archive member or a loose file) into
// findings. Today the only implementation parses ULP credentials; it exists as
// a seam so a future scanner (e.g. a secrets scanner) can be slotted in without
// touching the archive walkers or the dispatch machinery. Implementations must
// be safe to call concurrently: the parallel readers invoke it from pool tasks.
type Processor interface {
	Process(r io.Reader, provenance string) ([]Credential, error)
}

// credentialParser is the default Processor: the existing ULP credential parser.
type credentialParser struct{}

func (credentialParser) Process(r io.Reader, provenance string) ([]Credential, error) {
	return ParseCredentials(r, provenance)
}

// defaultProcessor is used when extractCtx.processor is unset (direct/test
// callers) so the readers can always call ec.parse without a nil check.
var defaultProcessor Processor = credentialParser{}

// parse runs this level's Processor (defaulting to the ULP parser) over r.
func (ec extractCtx) parse(r io.Reader, provenance string) ([]Credential, error) {
	if ec.processor != nil {
		return ec.processor.Process(r, provenance)
	}
	return defaultProcessor.Process(r, provenance)
}

// SecretSink receives raw member bytes for out-of-band secret scanning. It is
// the secrets analogue of the credential emit path: the readers call it for
// members they would otherwise skip. Implementations must be concurrency-safe.
type SecretSink interface {
	ScanSecrets(ctx context.Context, content []byte, provenance string)
}

// defaultSecretMaxLen caps how much of each scanned member is read into memory.
const defaultSecretMaxLen = 4 << 20 // 4 MiB

// scanSecrets reads up to secretMaxLen bytes of r and hands them to the sink.
// No-op when no sink is wired, so the credential path is unchanged with -secrets
// off. Read errors are swallowed: secret scanning is best-effort and must never
// fail an extraction.
func (ec extractCtx) scanSecrets(ctx context.Context, r io.Reader, provenance string) {
	if ec.secrets == nil {
		return
	}
	max := ec.secretMaxLen
	if max <= 0 {
		max = defaultSecretMaxLen
	}
	buf, err := io.ReadAll(io.LimitReader(r, max))
	if err != nil || len(buf) == 0 {
		return
	}
	ec.secrets.ScanSecrets(ctx, buf, provenance)
}

// slotSinks builds the stage/item publishers bound to a leased TUI slot so a
// dispatched task drives its own panel row instead of the producer's.
func slotSinks(p *Progress, idx int) (func(WorkerStage), func(string)) {
	return func(s WorkerStage) { p.setStage(idx, s) },
		func(label string) { p.setWorkerPath(idx, label) }
}

// dispatchOrInline runs fn either on a freshly leased pool slot (when the
// extraction budget has room *right now*) or inline on the caller (when it does
// not). It never blocks waiting for the budget, so it cannot form a
// hold-and-wait cycle at any recursion depth: a saturated pool simply degrades
// to today's sequential behaviour, and a drained worklist lets idle cores
// absorb a lone archive's members.
//
// fn receives the leased live-status slot index for a pooled run (>=0, or -1 if
// the registry was momentarily full — the task still runs, just without a row),
// or -1 for an inline run (the caller's own row stands). Pooled runs are tracked
// on wg; inline runs complete before returning.
//
// Only the top level (depth 0) offloads to the pool: deeper members run inline,
// matching the zip member model and keeping outstanding spilled temps bounded by
// the worker count (one per in-flight pooled child).
func dispatchOrInline(ctx context.Context, ec extractCtx, wg *sync.WaitGroup, fn func(slot int)) {
	if ec.depth == 0 && ec.sem != nil {
		select {
		case ec.sem <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-ec.sem }()
				slot := ec.p.acquireSlot()
				defer ec.p.releaseSlot(slot)
				fn(slot)
			}()
			return
		default:
		}
	}
	fn(-1)
}

// raceProbe finds the first candidate whose probe does not report a wrong
// password, running probes opportunistically on the pool (falling back inline
// when the budget is saturated) and cancelling the losers as soon as a winner is
// found. It resolves an archive password without re-streaming the whole archive:
// each probe reads only enough (the first member) to accept or reject a
// candidate.
//
// The accept condition is "not a wrong password" rather than "read cleanly" so a
// structural error (e.g. a truncated volume set, which is not a password
// problem) still yields a winner; the caller's subsequent single full pass then
// surfaces and salvages that structural condition. Returns the winning password
// and true, or ("", false) when every candidate is a wrong password.
func raceProbe(ctx context.Context, ec extractCtx, candidates []string, probe func(ctx context.Context, pw string) (wrongPassword bool)) (string, bool) {
	if len(candidates) == 0 {
		return "", false
	}
	if len(candidates) == 1 {
		// Nothing to race: treat the lone candidate as the winner and let the
		// caller's single full pass classify it (so we never read twice here).
		return candidates[0], true
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		winner string
		found  bool
	)
	record := func(pw string) {
		mu.Lock()
		if !found {
			found, winner = true, pw
			cancel() // signal the losers to bail
		}
		mu.Unlock()
	}
	for _, pw := range candidates {
		if rctx.Err() != nil {
			break
		}
		pw := pw
		task := func(slot int) {
			if rctx.Err() != nil {
				return
			}
			if slot >= 0 {
				ec.p.setActive(slot, ec.display, StageTestingPassword)
			}
			if !probe(rctx, pw) {
				record(pw)
			}
		}
		dispatched := false
		if ec.depth == 0 && ec.sem != nil {
			select {
			case ec.sem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-ec.sem }()
					s := ec.p.acquireSlot()
					defer ec.p.releaseSlot(s)
					task(s)
				}()
				dispatched = true
			default:
			}
		}
		if !dispatched {
			task(-1)
			mu.Lock()
			done := found
			mu.Unlock()
			if done {
				break // sequential fallback stops at the first winner
			}
		}
	}
	wg.Wait()
	return winner, found
}
