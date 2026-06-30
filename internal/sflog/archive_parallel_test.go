package sflog

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	zipenc "github.com/yeka/zip"
)

// buildBigZip writes a zip with nCred RedLine credential members plus any extra
// raw members (e.g. nested archives), enough to cross minParallelZipMembers.
func buildBigZip(t *testing.T, nCred int, extras map[string][]byte) string {
	t.Helper()
	members := map[string][]byte{}
	for i := 0; i < nCred; i++ {
		members[fmt.Sprintf("victim%03d/Passwords.txt", i)] =
			[]byte(fmt.Sprintf("URL: https://h%03d.example/login\nUSER: u%03d\nPASS: p%03d\n", i, i, i))
	}
	for k, v := range extras {
		members[k] = v
	}
	p := filepath.Join(t.TempDir(), "big.zip")
	writeZipMembers(t, p, members)
	return p
}

// runZipFiles drives readZipFiles over an on-disk zip and returns the creds in
// emit order plus the scan counts and issue count. sem==nil exercises the
// sequential path; a sized sem (with >=minParallelZipMembers members) exercises
// the parallel pool. emit/onIssue run on a single goroutine (inline or in the
// post-join merge), so no locking is needed here.
func runZipFiles(t *testing.T, path string, passwords []string, sem chan struct{}) (creds []string, files, nested, issues int) {
	t.Helper()
	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	ec := extractCtx{
		passwords: passwords,
		tempDir:   t.TempDir(),
		display:   path,
		emit:      func(c Credential) { creds = append(creds, FormatULPLine(c, false)) },
		onIssue:   func(string, IssueKind, error) { issues++ },
		sem:       sem,
	}
	// readZipFiles' parallel branch lends/reclaims one slot, mirroring the
	// engine worker loop. Hold a slot here so that release/reclaim is balanced
	// when calling readZipFiles directly (outside Run).
	if sem != nil {
		sem <- struct{}{}
		defer func() { <-sem }()
	}
	scan, err := readZipFiles(context.Background(), zr.File, ec, 1<<20)
	if err != nil {
		t.Fatalf("readZipFiles: %v", err)
	}
	return creds, scan.files, scan.nestedArchives, issues
}

// TestReadZipFilesParallelParity proves the parallel pool yields exactly the
// same credentials and counts as the sequential path for a big zip with both
// flat credential members and nested archives.
func TestReadZipFilesParallelParity(t *testing.T) {
	inner1 := zipBytes(t, map[string][]byte{"v/Passwords.txt": []byte("URL: https://n1.example\nUSER: a\nPASS: b\n")})
	inner2 := zipBytes(t, map[string][]byte{"v/Passwords.txt": []byte("URL: https://n2.example\nUSER: a\nPASS: b\n")})
	path := buildBigZip(t, 50, map[string][]byte{"nest/i1.zip": inner1, "nest/i2.zip": inner2})

	seqCreds, seqFiles, seqNested, seqIssues := runZipFiles(t, path, []string{""}, nil)
	parCreds, parFiles, parNested, parIssues := runZipFiles(t, path, []string{""}, make(chan struct{}, 4))

	if seqFiles != parFiles || seqNested != parNested || seqIssues != parIssues {
		t.Fatalf("counts differ: seq(files=%d,nested=%d,issues=%d) par(files=%d,nested=%d,issues=%d)",
			seqFiles, seqNested, seqIssues, parFiles, parNested, parIssues)
	}
	if parFiles != 52 || parNested != 2 {
		t.Fatalf("par files=%d nested=%d, want 52/2", parFiles, parNested)
	}
	sort.Strings(seqCreds)
	sort.Strings(parCreds)
	if len(seqCreds) != len(parCreds) {
		t.Fatalf("cred count: seq=%d par=%d", len(seqCreds), len(parCreds))
	}
	for i := range seqCreds {
		if seqCreds[i] != parCreds[i] {
			t.Fatalf("cred[%d]: seq=%q par=%q", i, seqCreds[i], parCreds[i])
		}
	}
}

// TestReadZipFilesParallelPublishesFanoutLabel proves a parallel run surfaces a
// "N members in parallel" worker label so the single panel row reads as
// concurrent work rather than one static archive.
func TestReadZipFilesParallelPublishesFanoutLabel(t *testing.T) {
	path := buildBigZip(t, 24, nil)
	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	var items []string
	sem := make(chan struct{}, 4)
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   path,
		emit:      func(Credential) {},
		onIssue:   func(string, IssueKind, error) {},
		setItem:   func(s string) { items = append(items, s) },
		sem:       sem,
	}
	sem <- struct{}{} // simulate the worker loop holding a slot
	if _, err := readZipFiles(context.Background(), zr.File, ec, 1<<20); err != nil {
		t.Fatal(err)
	}
	<-sem

	found := false
	for _, it := range items {
		if strings.Contains(it, "members in parallel") && strings.Contains(it, "24") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no fan-out label published; items=%v", items)
	}
}

// TestReadZipFilesParallelDeterministicOrder asserts the ordered merge keeps
// emit order stable across parallel runs (independent of completion order).
func TestReadZipFilesParallelDeterministicOrder(t *testing.T) {
	path := buildBigZip(t, 40, nil)
	a, _, _, _ := runZipFiles(t, path, []string{""}, make(chan struct{}, 8))
	b, _, _, _ := runZipFiles(t, path, []string{""}, make(chan struct{}, 8))
	if len(a) != len(b) {
		t.Fatalf("len differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("emit order differs at %d: %q vs %q", i, a[i], b[i])
		}
	}
}

// TestReadZipFilesParallelChunked exercises the multi-chunk path (members >
// memberFlushChunk): parity with the sequential read and a byte-identical,
// deterministic emit order across the chunk boundaries.
func TestReadZipFilesParallelChunked(t *testing.T) {
	nCred := memberFlushChunk*2 + 37 // spans three chunks, last one partial
	path := buildBigZip(t, nCred, nil)

	seq, seqFiles, _, _ := runZipFiles(t, path, []string{""}, nil)
	par, parFiles, _, _ := runZipFiles(t, path, []string{""}, make(chan struct{}, 6))
	par2, _, _, _ := runZipFiles(t, path, []string{""}, make(chan struct{}, 6))

	if parFiles != nCred || seqFiles != nCred {
		t.Fatalf("files: seq=%d par=%d, want %d", seqFiles, parFiles, nCred)
	}
	if len(par) != len(par2) {
		t.Fatalf("two parallel runs differ in length: %d vs %d", len(par), len(par2))
	}
	for i := range par {
		if par[i] != par2[i] {
			t.Fatalf("emit order not deterministic at %d: %q vs %q", i, par[i], par2[i])
		}
	}
	sort.Strings(seq)
	sort.Strings(par)
	if len(seq) != len(par) {
		t.Fatalf("cred count seq=%d par=%d", len(seq), len(par))
	}
	for i := range seq {
		if seq[i] != par[i] {
			t.Fatalf("cred[%d]: seq=%q par=%q", i, seq[i], par[i])
		}
	}
}

// TestReadZipFilesParallelMemberIsolation proves one bad member (a decoy nested
// archive) is recorded as an isolated issue while every good credential member
// still emits, under the parallel pool.
func TestReadZipFilesParallelMemberIsolation(t *testing.T) {
	path := buildBigZip(t, 8, map[string][]byte{"nest/junk.zip": []byte("this is not a real zip archive")})
	creds, files, nested, issues := runZipFiles(t, path, []string{""}, make(chan struct{}, 4))
	if len(creds) != 8 || files != 8 {
		t.Fatalf("good members: creds=%d files=%d, want 8/8", len(creds), files)
	}
	if nested != 1 || issues != 1 {
		t.Fatalf("decoy: nested=%d issues=%d, want 1/1 (isolated parse skip)", nested, issues)
	}
}

// TestExtractBudgetSharedAndBounded runs a many-member zip through the real
// engine and samples the shared extraction semaphore. Peak occupancy must never
// exceed the worker count (the global budget), and must climb above 1 (proving
// the parallel members draw from the SAME budget as the worker loop, i.e. a
// regression to a separate member pool would peg this at 1).
func TestExtractBudgetSharedAndBounded(t *testing.T) {
	const workers = 4
	// Bodies large enough that member parses overlap long enough to sample.
	block := strings.Repeat("URL: https://h.example/login\nUSER: u\nPASS: p\n\n", 400)
	members := map[string][]byte{}
	for i := 0; i < 64; i++ {
		members[fmt.Sprintf("victim%03d/Passwords.txt", i)] = []byte(block)
	}
	path := filepath.Join(t.TempDir(), "budget.zip")
	writeZipMembers(t, path, members)

	sem := make(chan struct{}, workers)
	eng := &Engine{Workers: workers, Passwords: []string{""}, extractSem: sem}

	var peak int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := int64(len(sem)); n > atomic.LoadInt64(&peak) {
				atomic.StoreInt64(&peak, n)
			}
			time.Sleep(20 * time.Microsecond)
		}
	}()

	var out bytes.Buffer
	if _, _, err := eng.Run(context.Background(), path, &out); err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-done

	p := atomic.LoadInt64(&peak)
	if p > workers {
		t.Fatalf("peak extraction concurrency %d > workers %d (budget breached)", p, workers)
	}
	if p < 2 {
		t.Fatalf("peak %d: parallel members did not share the worker budget (regression: separate member pool?)", p)
	}
}

func TestBoundedForEachRespectsCap(t *testing.T) {
	const capN, n = 3, 50
	sem := make(chan struct{}, capN)
	var inflight, peak, ran atomic.Int64
	boundedForEach(context.Background(), sem, n, func(int) {
		cur := inflight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		ran.Add(1)
		inflight.Add(-1)
	})
	if ran.Load() != n {
		t.Fatalf("ran = %d, want %d", ran.Load(), n)
	}
	if peak.Load() > capN {
		t.Fatalf("peak in-flight = %d, want <= cap %d", peak.Load(), capN)
	}
}

func TestBoundedForEachStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sem := make(chan struct{}, 4)
	var ran atomic.Int64
	boundedForEach(ctx, sem, 100, func(int) { ran.Add(1) })
	if ran.Load() != 0 {
		t.Fatalf("ran = %d on a pre-cancelled ctx, want 0", ran.Load())
	}
}
