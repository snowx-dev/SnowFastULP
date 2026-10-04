package sflog

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// hugeMember builds a labeled-form credential member of at least wantBytes with
// n planted credentials (first, last, and spread), padded with unrecognized
// filler lines so the member grows past any whole-body parse buffer.
func hugeMember(n int, wantBytes int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "URL: https://planted-%d.example/login\nUSER: user%d\nPASS: pass%d\n", i, i, i)
	}
	for b.Len() < wantBytes {
		b.WriteString("FILL: padding line that is not a credential field\n")
	}
	return b.String()
}

// TestParseCredentialsStreamsHugeMember pins the A1 fix: a credential member
// larger than the old 16 MiB whole-body buffer must be parsed in full, not
// dropped whole. The member is ~17 MiB with planted credentials at the start,
// middle, and end; every planted credential must survive.
func TestParseCredentialsStreamsHugeMember(t *testing.T) {
	const planted = 5_000
	body := hugeMember(planted, 17<<20)
	if bodyLen := len(body); bodyLen <= 16<<20 {
		t.Fatalf("fixture too small to exercise the old buffer cap: %d bytes", bodyLen)
	}
	creds, err := ParseCredentials(strings.NewReader(body), "Passwords.txt")
	if err != nil {
		t.Fatalf("ParseCredentials error = %v, want full parse of an over-buffer member", err)
	}
	if len(creds) != planted {
		t.Fatalf("parsed %d credentials, want all %d planted", len(creds), planted)
	}
	if creds[0].URL != "https://planted-0.example/login" ||
		creds[planted-1].URL != fmt.Sprintf("https://planted-%d.example/login", planted-1) {
		t.Fatalf("first/last credentials wrong: got %q .. %q", creds[0].URL, creds[planted-1].URL)
	}
}

// peakHeapReader samples HeapAlloc every 4 MiB read so the test can assert the
// parse's peak live heap stays far below the member size.
type peakHeapReader struct {
	r        io.Reader
	next     int64
	base     int64
	peakHeap atomic.Int64
}

func (p *peakHeapReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.next += int64(n)
	if p.next >= 4<<20 {
		p.next = 0
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		for {
			cur := p.peakHeap.Load()
			if int64(m.HeapAlloc) <= cur || p.peakHeap.CompareAndSwap(cur, int64(m.HeapAlloc)) {
				break
			}
		}
	}
	return n, err
}

// fillerStream synthesizes a labeled-form member on the fly: repeated filler
// lines with n planted credentials spread through the stream. Generating (not
// materializing) keeps the fixture out of the heap so the sampled peak measures
// the parser, not the test fixture.
type fillerStream struct {
	n          int   // total planted credentials
	left       int64 // bytes still to emit
	creds      int   // planted so far
	buf        []byte
	eofWritten bool
}

func newFillerStream(n int, wantBytes int64) *fillerStream {
	return &fillerStream{n: n, left: wantBytes}
}

func (f *fillerStream) Read(p []byte) (int, error) {
	for len(f.buf) == 0 {
		if f.left <= 0 {
			if f.eofWritten {
				return 0, io.EOF
			}
			f.eofWritten = true
			return 0, io.EOF
		}
		if f.creds < f.n {
			f.creds++
			i := f.creds - 1
			f.buf = []byte(fmt.Sprintf("URL: https://planted-%d.example/login\nUSER: user%d\nPASS: pass%d\n", i, i, i))
		} else {
			f.buf = []byte("FILL: padding line that is not a credential field and pads the member\n")
		}
		if int64(len(f.buf)) > f.left {
			f.buf = f.buf[:f.left]
		}
		f.left -= int64(len(f.buf))
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

// TestParseCredentialsHugeMemberBoundedMemory pins the A1 memory bound: parsing
// a ~96 MiB member must not hold the member in memory — peak heap during the
// parse stays well under half the member size. The member is generated on the
// fly so the sampled peak measures the parser, not a materialized fixture. The
// old whole-body buffering failed this by construction (the member was
// ReadAll'd verbatim before parsing).
func TestParseCredentialsHugeMemberBoundedMemory(t *testing.T) {
	const memberBytes = 96 << 20
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	ph := &peakHeapReader{r: newFillerStream(400, memberBytes), base: int64(base.HeapAlloc)}
	creds, err := ParseCredentials(ph, "Passwords.txt")
	if err != nil {
		t.Fatalf("ParseCredentials error = %v", err)
	}
	if len(creds) != 400 {
		t.Fatalf("parsed %d credentials, want 400", len(creds))
	}
	if peak := ph.peakHeap.Load() - ph.base; peak >= memberBytes/2 {
		t.Fatalf("parse grew live heap by %d bytes (>= half the %d-byte member); parse must stream",
			peak, memberBytes)
	}
}

// TestArchiveHugeCredentialMemberFullyParsed pins the A1 fix at the archive
// layer: a zip whose credential member is larger than the old parse buffer
// ingests ALL planted lines, alongside a normal member.
func TestArchiveHugeCredentialMemberFullyParsed(t *testing.T) {
	dir := t.TempDir()
	const planted = 2_000
	body := hugeMember(planted, 17<<20)
	path := filepath.Join(dir, "arc.zip")
	writeZipMembers(t, path, map[string][]byte{
		"Passwords.txt":       []byte(body),
		"Passwords-small.txt": []byte("URL: https://small.example/login\nUSER: s\nPASS: p\n"),
	})

	var creds []Credential
	var issues []Issue
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   path,
		emit:      func(c Credential) { creds = append(creds, c) },
		onIssue:   func(p string, k IssueKind, e error) { issues = append(issues, Issue{Path: p, Kind: k, Err: e}) },
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials error = %v, want success", err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %+v, want none for a parseable huge member", issues)
	}
	if len(creds) != planted+1 {
		t.Fatalf("ingested %d credentials, want %d (%d huge + 1 small)",
			len(creds), planted+1, planted)
	}
	found := 0
	for _, c := range creds {
		if strings.Contains(c.URL, "planted-") {
			found++
		}
	}
	if found != planted {
		t.Fatalf("planted credentials in library = %d, want %d", found, planted)
	}
}
