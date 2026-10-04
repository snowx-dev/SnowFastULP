package sflog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	zipenc "github.com/yeka/zip"
)

type panicProcessor struct {
	panicOn string
}

func (p panicProcessor) Process(r io.Reader, provenance string) ([]Credential, bool, error) {
	if strings.Contains(provenance, p.panicOn) {
		panic("decoder boom")
	}
	return ParseCredentialsChecked(r, provenance)
}

func TestZipMemberDecoderPanicIsolatedSequential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "members.zip")
	writeZipMembers(t, path, map[string][]byte{
		"boom/Passwords.txt": []byte("URL: https://boom.example\nUSER: b\nPASS: p\n"),
		"good/Passwords.txt": []byte("URL: https://good.example\nUSER: g\nPASS: p\n"),
	})

	var creds []Credential
	var issues []Issue
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   path,
		emit:      func(c Credential) { creds = append(creds, c) },
		onIssue: func(path string, kind IssueKind, err error) {
			issues = append(issues, Issue{Path: path, Kind: kind, Err: err})
		},
		processor: panicProcessor{panicOn: "!boom/Passwords.txt"},
	}

	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	if _, err := readZipFiles(context.Background(), zr.File, ec, 1<<20); err != nil {
		t.Fatalf("readZipFiles: %v", err)
	}
	if len(creds) != 1 || creds[0].URL != "https://good.example" {
		t.Fatalf("credentials = %+v, want good sibling only", creds)
	}
	if len(issues) != 1 || issues[0].Kind != IssueParseError {
		t.Fatalf("issues = %+v, want one decoder parse issue", issues)
	}
	if !errors.Is(issues[0].Err, errDecoderPanic) ||
		!strings.Contains(issues[0].Err.Error(), "decoder panic") {
		t.Fatalf("issue = %v, want decoder panic detail", issues[0].Err)
	}
}

func TestZipMemberDecoderPanicIsolatedParallel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parallel.zip")
	members := map[string][]byte{
		"boom/Passwords.txt": []byte("URL: https://boom.example\nUSER: b\nPASS: p\n"),
	}
	for i := 0; i < minParallelZipMembers; i++ {
		members[filepath.Join("good", string(rune('a'+i)), "Passwords.txt")] =
			[]byte("URL: https://good.example\nUSER: g\nPASS: p\n")
	}
	writeZipMembers(t, path, members)

	var creds []Credential
	var issues []Issue
	sem := make(chan struct{}, 4)
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   path,
		emit:      func(c Credential) { creds = append(creds, c) },
		onIssue: func(path string, kind IssueKind, err error) {
			issues = append(issues, Issue{Path: path, Kind: kind, Err: err})
		},
		processor: panicProcessor{panicOn: "!boom/Passwords.txt"},
		sem:       sem,
	}

	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	sem <- struct{}{}
	defer func() { <-sem }()

	if _, err := readZipFiles(context.Background(), zr.File, ec, 1<<20); err != nil {
		t.Fatalf("readZipFiles: %v", err)
	}
	if len(creds) != minParallelZipMembers {
		t.Fatalf("credentials = %d, want %d good siblings", len(creds), minParallelZipMembers)
	}
	if len(issues) != 1 || issues[0].Kind != IssueParseError {
		t.Fatalf("issues = %+v, want one decoder parse issue", issues)
	}
	if !errors.Is(issues[0].Err, errDecoderPanic) {
		t.Fatalf("issue = %v, want errDecoderPanic cause", issues[0].Err)
	}
}

func TestNestedDecoderPanicIsolated(t *testing.T) {
	inner := zipBytes(t, map[string][]byte{
		"boom/Passwords.txt": []byte("URL: https://boom.example\nUSER: b\nPASS: p\n"),
	})
	outer := filepath.Join(t.TempDir(), "outer.zip")
	writeZipMembers(t, outer, map[string][]byte{
		"loose/Passwords.txt": []byte("URL: https://good.example\nUSER: g\nPASS: p\n"),
		"nested.zip":          inner,
	})

	var creds []Credential
	var issues []Issue
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   outer,
		emit:      func(c Credential) { creds = append(creds, c) },
		onIssue: func(path string, kind IssueKind, err error) {
			issues = append(issues, Issue{Path: path, Kind: kind, Err: err})
		},
		processor: panicProcessor{panicOn: "!nested.zip!boom/Passwords.txt"},
	}

	if _, err := readArchiveCredentials(context.Background(), outer, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if len(creds) != 1 || creds[0].URL != "https://good.example" {
		t.Fatalf("credentials = %+v, want outer sibling only", creds)
	}
	if len(issues) != 1 || issues[0].Kind != IssueParseError {
		t.Fatalf("issues = %+v, want one nested decoder parse issue", issues)
	}
	if !strings.Contains(issues[0].Path, "!nested.zip!") {
		t.Fatalf("issue path = %q, want nested provenance", issues[0].Path)
	}
	if errors.Is(issues[0].Err, context.Canceled) {
		t.Fatalf("decoder panic was misclassified as cancellation: %v", issues[0].Err)
	}
}

func TestPooledSpilledDecoderPanicReleasesBudget(t *testing.T) {
	inner := zipBytes(t, map[string][]byte{
		"boom/Passwords.txt": []byte("URL: https://boom.example\nUSER: b\nPASS: p\n"),
	})
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   "outer.rar",
		p:         NewProgress(),
		processor: panicProcessor{panicOn: "!nested.zip!boom/Passwords.txt"},
		sem:       make(chan struct{}, 1),
		spill:     newSpillBudget(1<<20, 1<<20),
	}
	ec.p.SetWorkers(1)
	var wg sync.WaitGroup
	var outcomes []*memberOutcome

	if err := spillAndDispatch(context.Background(), ec, &wg, &outcomes,
		bytes.NewReader(inner), "nested.zip", nil, false, false); err != nil {
		t.Fatalf("spillAndDispatch: %v", err)
	}
	wg.Wait()

	if len(outcomes) != 1 || len(outcomes[0].issues) != 1 {
		t.Fatalf("outcomes = %+v, want one isolated issue", outcomes)
	}
	if outcomes[0].issues[0].kind != IssueParseError {
		t.Fatalf("issue kind = %s, want parse-error", outcomes[0].issues[0].kind)
	}
	if got := len(ec.sem); got != 0 {
		t.Fatalf("extraction budget retains %d slot(s)", got)
	}
	slot := ec.p.acquireSlot()
	if slot < 0 {
		t.Fatal("pooled progress slot was not released")
	}
	ec.p.releaseSlot(slot)
}

func TestProcessArchiveConvertsVolumePanicToIssue(t *testing.T) {
	var acc accum
	eng := &Engine{Passwords: []string{""}}
	item := workItem{
		path:     "broken.part1.rar",
		kind:     kindArchive,
		weight:   1,
		assembly: assemblyRarVolumes,
	}

	eng.processArchive(context.Background(), -1, item, make(chan string), &acc)

	if got := acc.parseErrors.Load(); got != 1 {
		t.Fatalf("parse errors = %d, want 1", got)
	}
	if len(acc.issues) != 1 || acc.issues[0].Kind != IssueParseError {
		t.Fatalf("issues = %+v, want one parse issue", acc.issues)
	}
	if !errors.Is(acc.issues[0].Err, errDecoderPanic) {
		t.Fatalf("issue = %v, want errDecoderPanic cause", acc.issues[0].Err)
	}
}

func TestDecoderPanicErrorIsHumanized(t *testing.T) {
	err := decoderPanicError("decoder boom")
	if !errors.Is(err, errDecoderPanic) {
		t.Fatalf("decoder panic error = %v, want errDecoderPanic cause", err)
	}
	if got := IssueDetail(Issue{Kind: IssueParseError, Err: err}); got != "corrupt archive (decoder panic)" {
		t.Fatalf("IssueDetail() = %q, want decoder panic detail", got)
	}
}

func TestRecoverAsErrorConvertsPanic(t *testing.T) {
	var err error
	func() {
		defer recoverAsError(&err, extractCtx{display: "archive.rar"})
		panic("decoder boom")
	}()
	if !errors.Is(err, errDecoderPanic) {
		t.Fatalf("recovered error = %v, want errDecoderPanic", err)
	}
}

func TestRaceProbePropagatesDecoderPanic(t *testing.T) {
	_, ok, err := raceProbe(context.Background(), extractCtx{}, []string{"panic", "other"},
		func(context.Context, string) (bool, error) {
			panic("decoder boom")
		})
	if ok {
		t.Fatal("panic probe reported a password winner")
	}
	if !errors.Is(err, errDecoderPanic) {
		t.Fatalf("raceProbe error = %v, want errDecoderPanic", err)
	}
}
