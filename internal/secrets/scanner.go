//go:build secrets

package secrets

import (
	"context"
	"os"
	"sync"

	"github.com/praetorian-inc/titus"
)

// Pool holds N Titus scanners. A Titus scanner needs exclusive access to its
// Hyperscan scratch during a scan, so concurrent callers each borrow one.
type Pool struct {
	scanners chan *titus.Scanner
}

// NewPool builds size scanners (>=1). Each loads the full rule set once
// (~150-200ms), so they are built concurrently: a per-core pool then costs one
// scanner's build latency instead of size× it. On any failure every scanner
// built so far is closed and the first error returned.
func NewPool(size int) (*Pool, error) {
	if size < 1 {
		size = 1
	}
	ch := make(chan *titus.Scanner, size)
	// Titus's vectorscan matcher prints a startup diagnostic to os.Stderr
	// during scanner init ("[vectorscan] N/N rules compiled for Hyperscan, …"
	// plus an incompatible-patterns list). Silence it for the build window so
	// it doesn't scroll above / interleave with sfl's live TUI. sfl's TUI
	// renders to a captured stderr (cmd/sfl.stderrFile), not the os.Stderr
	// package var, so redirecting the var here spares the live draws while the
	// diagnostic goes to /dev/null. Best-effort: if /dev/null can't be opened,
	// the diagnostic is left as-is rather than failing the pool.
	if devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		prev := os.Stderr
		os.Stderr = devnull
		defer func() { os.Stderr = prev; devnull.Close() }()
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for i := 0; i < size; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := titus.NewScanner()
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			ch <- s // ch is buffered to size, so this never blocks
		}()
	}
	wg.Wait()
	if firstErr != nil {
		close(ch)
		for s := range ch {
			s.Close()
		}
		return nil, firstErr
	}
	return &Pool{scanners: ch}, nil
}

func (p *Pool) Close() {
	for len(p.scanners) > 0 {
		(<-p.scanners).Close()
	}
}

// Scan runs Titus over content and tags every match with provenance.
func (p *Pool) Scan(ctx context.Context, content []byte, provenance string) ([]Finding, error) {
	s := <-p.scanners
	defer func() { p.scanners <- s }()
	matches, err := s.ScanBytesWithContext(ctx, content)
	if err != nil {
		return nil, err
	}
	out := make([]Finding, 0, len(matches))
	for _, m := range matches {
		out = append(out, matchToFinding(m, provenance))
	}
	return out, nil
}

// matchToFinding maps a titus.Match to our Finding. Snippet.Matching is the
// canonical matched text and is used verbatim: named groups vary per rule
// (AWS uses key_id/secret_key, GitHub uses token, etc.) with no universal
// "secret" group, so the matched span is the only rule-agnostic value — and for
// multi-part rules like AWS it captures every part, which is what we want.
func matchToFinding(m *titus.Match, provenance string) Finding {
	f := Finding{
		RuleID:     m.RuleID,
		RuleName:   m.RuleName,
		Secret:     sanitizeSecret(string(m.Snippet.Matching)),
		Score:      -1, // v1: rule-metadata scoring is a follow-up
		SourcePath: provenance,
	}
	if m.ValidationResult != nil {
		f.Validation = string(m.ValidationResult.Status)
	}
	return f
}
