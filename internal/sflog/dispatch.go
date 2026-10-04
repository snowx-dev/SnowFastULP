package sflog

import (
	"context"
	"io"
	"sync"
)

// Processor turns one logical source (an archive member or a loose file) into
// findings. Today the only implementation parses ULP credentials. The bool
// result reports a mixed-format source (labeled blocks AND valid colon lines;
// review H-20): the credentials are unchanged, but the caller must flag
// HadIssue so -del keeps the source. Implementations
// must be safe to call concurrently: the parallel readers invoke it from pool tasks.
type Processor interface {
	Process(r io.Reader, provenance string) (creds []Credential, mixed bool, err error)
}

// credentialParser is the default Processor. loose selects the same built-in
// parser mode used by sfu; zero value remains strict for direct/test callers.
type credentialParser struct {
	loose       bool
	diagnostics bool
	onAmbiguity func(AmbiguityCounts)
}

func (p credentialParser) Process(r io.Reader, provenance string) ([]Credential, bool, error) {
	var creds []Credential
	mixed, err := ParseCredentialsStreamWithDiagnostics(r, provenance, "", p.loose, p.diagnostics, p.onAmbiguity, func(c Credential) error {
		creds = append(creds, c)
		return nil
	})
	return creds, mixed, err
}

func (p credentialParser) ProcessStream(r io.Reader, provenance, tempDir string, emit func(Credential) error) (mixed bool, err error) {
	return ParseCredentialsStreamWithDiagnostics(r, provenance, tempDir, p.loose, p.diagnostics, p.onAmbiguity, emit)
}

// defaultProcessor is used when extractCtx.processor is unset (direct/test
// callers) so the readers can always call ec.parse without a nil check.
var defaultProcessor Processor = credentialParser{}

// parse runs this level's Processor (defaulting to the ULP parser) over r.
// StreamProcessor is the bounded-memory parsing seam: credential members
// stream their records into emit as they parse, so a dense member costs a
// bounded memory head plus a temp-file spill instead of a full record slice.
// The returned mixed flag is the H-20 mixed-format signal (see
// ParseCredentialsStream).
type StreamProcessor interface {
	ProcessStream(r io.Reader, provenance, tempDir string, emit func(Credential) error) (mixed bool, err error)
}

// parse runs this level's Processor over r. Production parsers implement
// StreamProcessor and forward each record to emit; a slice-style processor
// (test seams, future scanners) is parsed whole and its records forwarded, so
// callers always get streaming emission regardless of the seam in place.
// Either way the caller receives the H-20 mixed-format flag alongside the
// error, so mixed sources are flagged no matter which seam parsed them.
func (ec extractCtx) parse(r io.Reader, provenance string, emit func(Credential) error) (mixed bool, err error) {
	p := ec.processor
	if p == nil {
		p = defaultProcessor
	}
	if sp, ok := p.(StreamProcessor); ok {
		return sp.ProcessStream(r, provenance, ec.tempDir, emit)
	}
	creds, mixed, err := p.Process(r, provenance)
	if err != nil {
		return false, err
	}
	for _, c := range creds {
		if err := emit(c); err != nil {
			return false, err
		}
	}
	return mixed, nil
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
// candidate. It returns wrongPassword=true for a rejected password, and a
// non-nil error for a structural/probe failure.
//
// The accept condition is "not a wrong password" rather than "read cleanly" so a
// structural error (e.g. a truncated volume set, which is not a password
// problem) still yields a winner; the caller's subsequent single full pass then
// surfaces and salvages that structural condition. Returns the winning password
// and true, or ("", false, nil) when every candidate is a wrong password.
// A non-nil error means probing failed structurally and no candidate won.
func raceProbe(ctx context.Context, ec extractCtx, candidates []string, probe func(ctx context.Context, pw string) (wrongPassword bool, err error)) (string, bool, error) {
	if len(candidates) == 0 {
		return "", false, nil
	}
	if len(candidates) == 1 {
		// Nothing to race: treat the lone candidate as the winner and let the
		// caller's single full pass classify it (so we never read twice here).
		return candidates[0], true, nil
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		winner   string
		found    bool
		probeErr error
	)
	recordWinner := func(pw string) {
		mu.Lock()
		if !found && probeErr == nil {
			found, winner = true, pw
			cancel() // signal the losers to bail
		}
		mu.Unlock()
	}
	recordError := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if !found && probeErr == nil {
			probeErr = err
			cancel()
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
			var (
				wrong bool
				err   error
			)
			func() {
				defer func() {
					if value := recover(); value != nil {
						err = decoderPanicError(value)
						ec.debugf("archive %s: decoder panic while probing password: %v", ec.display, value)
					}
				}()
				wrong, err = probe(rctx, pw)
			}()
			if err != nil {
				recordError(err)
			} else if !wrong {
				recordWinner(pw)
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
			done := found || probeErr != nil
			mu.Unlock()
			if done {
				break // sequential fallback stops at the first winner
			}
		}
	}
	wg.Wait()
	return winner, found, probeErr
}
