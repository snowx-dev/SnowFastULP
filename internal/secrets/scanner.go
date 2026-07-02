package secrets

import (
	"context"

	"github.com/praetorian-inc/titus"
)

// Pool holds N Titus scanners. A Titus scanner needs exclusive access to its
// Hyperscan scratch during a scan, so concurrent callers each borrow one.
type Pool struct {
	scanners chan *titus.Scanner
}

// NewPool builds size scanners (>=1). Each loads the full rule set once.
func NewPool(size int) (*Pool, error) {
	if size < 1 {
		size = 1
	}
	ch := make(chan *titus.Scanner, size)
	for i := 0; i < size; i++ {
		s, err := titus.NewScanner()
		if err != nil {
			for len(ch) > 0 {
				(<-ch).Close()
			}
			return nil, err
		}
		ch <- s
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
		Secret:     string(m.Snippet.Matching),
		Score:      -1, // v1: rule-metadata scoring is a follow-up
		SourcePath: provenance,
	}
	if m.ValidationResult != nil {
		f.Validation = string(m.ValidationResult.Status)
	}
	return f
}
