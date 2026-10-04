package sflog

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	zipenc "github.com/yeka/zip"
)

// buildDenseZip writes a zip with nMembers credential members, each holding
// nPerMember labeled credential blocks — the dense-credential shape that used
// to be retained whole per member (and up to a 256-member chunk in parallel).
func buildDenseZip(t *testing.T, nMembers, nPerMember int) string {
	t.Helper()
	members := make(map[string][]byte, nMembers)
	for i := range nMembers {
		var b strings.Builder
		for j := range nPerMember {
			fmt.Fprintf(&b, "URL: https://h%03d.example/login\nUSER: u%05d\nPASS: p%05d\n", i, j, j)
		}
		members[fmt.Sprintf("victim%03d/Passwords.txt", i)] = []byte(b.String())
	}
	p := filepath.Join(t.TempDir(), "dense.zip")
	writeZipMembers(t, p, members)
	return p
}

// Dense parallel members must still emerge in exact member order with every
// record present: the bounded sink replays each member's records in parse
// order at the chunk merge.
func TestDenseZipMembersEmergeInOrder(t *testing.T) {
	const (
		nMembers   = 8
		nPerMember = 5000
	)
	path := buildDenseZip(t, nMembers, nPerMember)
	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var got []string
	sem := make(chan struct{}, 4)
	sem <- struct{}{} // lend the owning worker's slot, as the engine does
	defer func() { <-sem }()
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   path,
		emit:      func(c Credential) { got = append(got, c.Password) },
		onIssue:   func(string, IssueKind, error) { t.Error("unexpected issue") },
		sem:       sem,
	}
	scan, err := readZipFiles(context.Background(), zr.File, ec, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if want := nMembers * nPerMember; len(got) != want {
		t.Fatalf("emitted %d credentials, want %d", len(got), want)
	}
	if scan.files != nMembers {
		t.Fatalf("scan.files = %d, want %d", scan.files, nMembers)
	}
	// Member i emits its j-ascending records, members in file order.
	idx := 0
	for i := range nMembers {
		for j := range nPerMember {
			if got[idx] != fmt.Sprintf("p%05d", j) {
				t.Fatalf("record %d = %q, want member %03d record %05d (order broken)", idx, got[idx], i, j)
			}
			idx++
		}
	}
}

// A dense credential archive must not scale retained memory with the record
// count: members spill to disk beyond a small bounded head, so peak heap
// stays far below what holding every Credential (plus the parallel chunk's
// outcomes) would cost.
func TestDenseZipMembersBoundedMemory(t *testing.T) {
	const (
		nMembers   = 8
		nPerMember = 60000
	)
	path := buildDenseZip(t, nMembers, nPerMember)
	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	var peak runtime.MemStats
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				runtime.GC() // sample live heap, not GC-lag garbage
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.HeapAlloc {
					peak = m
				}
			}
		}
	}()

	var count int64
	sem := make(chan struct{}, 4)
	sem <- struct{}{} // lend the owning worker's slot, as the engine does
	defer func() { <-sem }()
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   path,
		emit:      func(c Credential) { count++ },
		onIssue:   func(string, IssueKind, error) { t.Error("unexpected issue") },
		sem:       sem,
	}
	scan, err := readZipFiles(context.Background(), zr.File, ec, 1<<20)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(nMembers * nPerMember); count != want {
		t.Fatalf("emitted %d credentials, want %d", count, want)
	}
	if scan.files != nMembers {
		t.Fatalf("scan.files = %d, want %d", scan.files, nMembers)
	}
	// Holding every record (~480k Credentials) would retain ~60+ MiB; the
	// spilled path must peak well under that.
	const maxPeak = 40 << 20
	if peak.HeapAlloc > maxPeak {
		t.Fatalf("peak heap %d KiB exceeds %d KiB: records are being retained, not spilled",
			peak.HeapAlloc>>10, maxPeak>>10)
	}
	t.Logf("peak heap %d KiB for %d credentials", peak.HeapAlloc>>10, count)
}
