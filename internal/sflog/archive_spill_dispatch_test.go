package sflog

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/nwaples/rardecode/v2"
)

// boundedBody serves limit bytes of a deterministic pattern and records how
// many bytes were served, so a test can prove whether a member body was fully
// drained.
type boundedBody struct {
	limit  int64
	served int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	n := len(p)
	if rem := b.limit - b.served; int64(n) > rem {
		n = int(rem)
	}
	if n <= 0 {
		return 0, io.EOF
	}
	for i := 0; i < n; i++ {
		p[i] = byte(i * 7)
	}
	b.served += int64(n)
	return n, nil
}

// boomBody serves limit bytes, then fails every further read with boomErr, so
// a drain that reads past the spill stop point hits the boom.
type boomBody struct {
	limit   int64
	boomErr error
	hit     bool
}

func (b *boomBody) Read(p []byte) (int, error) {
	if b.hit {
		return 0, b.boomErr
	}
	n := len(p)
	if int64(n) > b.limit {
		n = int(b.limit)
	}
	for i := 0; i < n; i++ {
		p[i] = 0x5A
	}
	if int64(n) >= b.limit {
		b.hit = true
	}
	return n, nil
}

func spillTestEC(t *testing.T, memberLimit int64) extractCtx {
	t.Helper()
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   "outer.rar",
		p:         NewProgress(),
		emit:      func(Credential) {},
		onIssue:   func(string, IssueKind, error) {},
	}
	ec.p.SetWorkers(1)
	ec.spill = newSpillBudget(memberLimit, memberLimit)
	return ec
}

// A non-solid over-cap nested member must not be drained: the next
// rardecode.Next() skips its remaining packed bytes, so the body reader never
// sees the bytes past the spill stop point. A solid stream must be drained to
// keep the decoder window aligned.
func TestSpillAndDispatchOverCapDrainsOnlySolid(t *testing.T) {
	const bodyBytes = 4 << 20 // full member body, far past the member cap
	const memberCap = 64 << 10

	// Non-solid: spill stops at the cap and the drain never runs.
	ec := spillTestEC(t, memberCap)
	body := &boundedBody{limit: bodyBytes}
	var wg sync.WaitGroup
	var outcomes []*memberOutcome
	if err := spillAndDispatch(context.Background(), ec, &wg, &outcomes, body, "inner.zip", nil, false, false); err != nil {
		t.Fatalf("non-solid spillAndDispatch error = %v, want nil", err)
	}
	wg.Wait()
	if body.served >= bodyBytes/2 {
		t.Fatalf("non-solid over-cap member read %d of %d bytes: the drain ran", body.served, bodyBytes)
	}
	if len(outcomes) != 1 || len(outcomes[0].issues) != 1 ||
		!errors.Is(outcomes[0].issues[0].err, errNestedArchiveOverCap) {
		t.Fatalf("non-solid outcomes = %+v, want one over-cap issue", outcomes)
	}

	// Solid: the remaining bytes are drained so the decoder stays aligned.
	ec = spillTestEC(t, memberCap)
	body = &boundedBody{limit: bodyBytes}
	outcomes = nil
	if err := spillAndDispatch(context.Background(), ec, &wg, &outcomes, body, "inner.zip", nil, true, false); err != nil {
		t.Fatalf("solid spillAndDispatch error = %v, want nil", err)
	}
	wg.Wait()
	if body.served != bodyBytes {
		t.Fatalf("solid over-cap member served %d of %d bytes: the drain did not finish", body.served, bodyBytes)
	}
	if len(outcomes) != 1 || len(outcomes[0].issues) != 1 ||
		!errors.Is(outcomes[0].issues[0].err, errNestedArchiveOverCap) {
		t.Fatalf("solid outcomes = %+v, want one over-cap issue", outcomes)
	}
}

// Over-depth members likewise skip the drain for non-solid streams.
func TestSpillAndDispatchOverDepthDrainsOnlySolid(t *testing.T) {
	ec := spillTestEC(t, 1<<20)
	ec.depth = maxArchiveDepth // child would exceed the limit
	body := &boundedBody{limit: 1 << 20}
	var wg sync.WaitGroup
	var outcomes []*memberOutcome
	if err := spillAndDispatch(context.Background(), ec, &wg, &outcomes, body, "inner.zip", nil, false, false); err != nil {
		t.Fatalf("non-solid over-depth error = %v, want nil", err)
	}
	wg.Wait()
	if body.served != 0 {
		t.Fatalf("non-solid over-depth member read %d bytes: the drain ran", body.served)
	}
	if len(outcomes) != 1 || len(outcomes[0].issues) != 1 ||
		!errors.Is(outcomes[0].issues[0].err, errNestTooDeep) {
		t.Fatalf("non-solid outcomes = %+v, want one nest-too-deep issue", outcomes)
	}

	ec = spillTestEC(t, 1<<20)
	ec.depth = maxArchiveDepth
	body = &boundedBody{limit: 1 << 20}
	outcomes = nil
	if err := spillAndDispatch(context.Background(), ec, &wg, &outcomes, body, "inner.zip", nil, true, false); err != nil {
		t.Fatalf("solid over-depth error = %v, want nil", err)
	}
	wg.Wait()
	if body.served != 1<<20 {
		t.Fatalf("solid over-depth member served %d of %d bytes", body.served, int64(1<<20))
	}
}

// A body read failure inside an encrypted member before the password is
// confirmed is a wrong-password symptom: it must propagate as a classified
// wrong-password error (so readRarCredentials races the remaining candidates),
// not be swallowed as a parse issue on a truncated spill.
func TestSpillAndDispatchEncryptedBodyErrorPropagates(t *testing.T) {
	ec := spillTestEC(t, 1<<20)
	body := &boomBody{limit: 1 << 12, boomErr: rardecode.ErrBadPassword}
	var wg sync.WaitGroup
	var outcomes []*memberOutcome
	err := spillAndDispatch(context.Background(), ec, &wg, &outcomes, body, "inner.zip", nil, false, true)
	wg.Wait()
	if !isWrongPassword(err) {
		t.Fatalf("encrypted body error = %v, want a wrong-password-classified error", err)
	}
	if !strings.Contains(err.Error(), "incorrect password") {
		t.Fatalf("error = %v, want the classified wrong-password wrapping", err)
	}
	if len(outcomes[0].issues) != 0 {
		t.Fatalf("issues = %+v, want none: the wrong password must fail the attempt", outcomes[0].issues)
	}

	// The same failure inside an unencrypted member is an isolated parse issue.
	ec = spillTestEC(t, 1<<20)
	body = &boomBody{limit: 1 << 12, boomErr: rardecode.ErrBadPassword}
	outcomes = nil
	err = spillAndDispatch(context.Background(), ec, &wg, &outcomes, body, "inner.zip", nil, false, false)
	wg.Wait()
	if err != nil {
		t.Fatalf("unencrypted body error = %v, want nil", err)
	}
	if len(outcomes[0].issues) != 1 || outcomes[0].issues[0].kind != IssueParseError {
		t.Fatalf("unencrypted outcomes = %+v, want one parse issue", outcomes[0].issues)
	}
}

// An I/O failure inside a non-encrypted member's body stays an isolated issue.
func TestSpillAndDispatchPlainIOErrorIsIsolated(t *testing.T) {
	ec := spillTestEC(t, 1<<20)
	boom := errors.New("disk hiccup")
	body := &boomBody{limit: 1 << 12, boomErr: boom}
	var wg sync.WaitGroup
	var outcomes []*memberOutcome
	if err := spillAndDispatch(context.Background(), ec, &wg, &outcomes, body, "inner.zip", nil, false, false); err != nil {
		t.Fatalf("spillAndDispatch error = %v, want nil", err)
	}
	wg.Wait()
	if len(outcomes[0].issues) != 1 || outcomes[0].issues[0].kind != IssueParseError {
		t.Fatalf("outcomes = %+v, want one isolated parse issue", outcomes[0].issues)
	}
	if !errors.Is(outcomes[0].issues[0].err, boom) {
		t.Fatalf("issue err = %v, want the body boom cause", outcomes[0].issues[0].err)
	}
}
