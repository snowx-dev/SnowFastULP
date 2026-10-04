package sflog

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	zipenc "github.com/yeka/zip"
)

// --- runWinner slot ---------------------------------------------------------

func TestRunWinnerNilSafe(t *testing.T) {
	var nilSlot *runWinner
	if got := nilSlot.get(); got != "" {
		t.Fatalf("nil get = %q, want empty", got)
	}
	nilSlot.promote("x") // must not panic
	var zero runWinner
	if got := zero.get(); got != "" {
		t.Fatalf("zero get = %q, want empty", got)
	}
	zero.promote("") // empty promote must be a no-op
	if got := zero.get(); got != "" {
		t.Fatalf("empty promote changed slot: %q", got)
	}
	zero.promote("pw")
	if got := zero.get(); got != "pw" {
		t.Fatalf("get = %q, want pw", got)
	}
}

func TestRunWinnerConcurrent(t *testing.T) {
	var w runWinner
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w.promote(fmt.Sprintf("pw%d", i))
			_ = w.get()
		}(i)
	}
	wg.Wait()
	if got := w.get(); got == "" {
		t.Fatal("slot empty after concurrent promotes")
	}
}

// --- orderedPasswords -------------------------------------------------------

func TestOrderedPasswords(t *testing.T) {
	for _, tt := range []struct {
		name      string
		winner    *runWinner
		passwords []string
		want      []string
	}{
		{"nil winner keeps order", nil, []string{"a", "b"}, []string{"a", "b"}},
		{"empty winner keeps empty-password candidate", &runWinner{}, []string{""}, []string{""}},
		{"empty winner keeps order", &runWinner{}, []string{"a", "b"}, []string{"a", "b"}},
		{"winner hoisted first", func() *runWinner { var w runWinner; w.promote("b"); return &w }(),
			[]string{"a", "b"}, []string{"b", "a"}},
		{"winner not in list is prepended", func() *runWinner { var w runWinner; w.promote("z"); return &w }(),
			[]string{"a", "b"}, []string{"z", "a", "b"}},
		{"winner hoisted once, no dup", func() *runWinner { var w runWinner; w.promote("a"); return &w }(),
			[]string{"a", "b", "a"}, []string{"a", "b"}},
		{"winner with empty list", func() *runWinner { var w runWinner; w.promote("a"); return &w }(),
			nil, []string{"a"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ec := extractCtx{passwords: tt.passwords, winner: tt.winner}
			got := ec.orderedPasswords()
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("orderedPasswords = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- integration: the real archive paths promote and skip -------------------

func credentialSink() (func(Credential), *[]Credential) {
	creds := &[]Credential{}
	return func(c Credential) { *creds = append(*creds, c) }, creds
}

func issueSink() (func(string, IssueKind, error), *[]Issue) {
	issues := &[]Issue{}
	return func(p string, k IssueKind, err error) {
		*issues = append(*issues, Issue{Path: p, Kind: k, Err: err})
	}, issues
}

// ZIP: the probe resolve must promote the archive winner (the skip itself is
// proven behaviorally by the rar/7z tests below, whose sweeps log attempts).
func TestZipStickyWinnerPromotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one.zip")
	writeEncryptedTestZipMethod(t, path, "secret", "victim/Passwords.txt",
		"URL: https://x.example/login\nUSER: u\nPASS: p\n", zipenc.AES256Encryption)

	var w runWinner
	emit, creds := credentialSink()
	onIssue, issues := issueSink()
	ec := extractCtx{
		passwords: []string{"wrong", "secret"},
		winner:    &w,
		display:   path,
		emit:      emit,
		onIssue:   onIssue,
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if len(*creds) != 1 {
		t.Fatalf("credentials = %d, want 1", len(*creds))
	}
	if len(*issues) != 0 {
		t.Fatalf("unexpected issues: %+v", *issues)
	}
	if got := w.get(); got != "secret" {
		t.Fatalf("winner after zip resolve = %q, want secret", got)
	}

	// A later archive sharing the winner still extracts (order change is
	// transparent).
	emit2, creds2 := credentialSink()
	ec2 := extractCtx{
		passwords: []string{"wrong", "secret"},
		winner:    &w,
		display:   path,
		emit:      emit2,
		onIssue:   onIssue,
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec2, 1<<20); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(*creds2) != 1 {
		t.Fatalf("second archive credentials = %d, want 1", len(*creds2))
	}
}

// RAR: the first archive pays the wrong-password probe ("first password
// wrong, racing"), the second archive — sharing the promoted winner — must
// not race at all.
func TestRarStickyWinnerSkipsProbeRace(t *testing.T) {
	path := filepath.Join("testdata", "ppmd-passwords.rar")
	var w runWinner

	debug1 := &[]string{}
	emit, creds1 := credentialSink()
	onIssue, _ := issueSink()
	ec1 := extractCtx{
		passwords: []string{"wrong", "123"},
		winner:    &w,
		display:   path,
		emit:      emit,
		onIssue:   onIssue,
		debug:     func(f string, a ...any) { *debug1 = append(*debug1, fmt.Sprintf(f, a...)) },
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec1, 1<<20); err != nil {
		t.Fatalf("first readArchiveCredentials: %v", err)
	}
	if got := w.get(); got != "123" {
		t.Fatalf("winner after rar resolve = %q, want 123", got)
	}
	if !anyDebugLineContains(*debug1, "first password wrong, racing") {
		t.Fatalf("first archive must pay the probe race; debug = %v", *debug1)
	}
	if len(*creds1) != 1 {
		t.Fatalf("first archive credentials = %d, want 1", len(*creds1))
	}

	debug2 := &[]string{}
	emit2, creds2 := credentialSink()
	onIssue2, issues2 := issueSink()
	ec2 := extractCtx{
		passwords: []string{"wrong", "123"},
		winner:    &w,
		display:   path,
		emit:      emit2,
		onIssue:   onIssue2,
		debug:     func(f string, a ...any) { *debug2 = append(*debug2, fmt.Sprintf(f, a...)) },
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec2, 1<<20); err != nil {
		t.Fatalf("second readArchiveCredentials: %v", err)
	}
	if anyDebugLineContains(*debug2, "first password wrong, racing") {
		t.Fatalf("second archive re-raced candidates; debug = %v", *debug2)
	}
	if len(*creds2) != 1 {
		t.Fatalf("second archive credentials = %d, want 1", len(*creds2))
	}
	if len(*issues2) != 0 {
		t.Fatalf("unexpected issues: %+v", *issues2)
	}
}

// 7z: the sweep logs every rejected candidate; with the promoted winner first,
// the second archive must log none. Builds its archive with the real 7z packer
// (skips when absent), like TestEncryptedSevenZipCopyModeRetriesPasswordCandidates.
func TestSevenZipStickyWinnerSkipsRejectedAttempts(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found")
	}
	dir := t.TempDir()
	mustWrite(t, dir, "Passwords.txt",
		"URL: https://retry.example.com/login\nUSER: analyst\nPASS: secret\n")
	cmd := exec.Command(bin, "a", "-t7z", "-mx=0", "-psecret", "-bd",
		"encrypted.7z", "Passwords.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("7z create failed: %v\n%s", err, out)
	}
	path := filepath.Join(dir, "encrypted.7z")
	var w runWinner

	debug1 := &[]string{}
	emit, creds1 := credentialSink()
	ec1 := extractCtx{
		passwords: []string{"wrong", "secret"},
		winner:    &w,
		display:   path,
		emit:      emit,
		onIssue:   func(string, IssueKind, error) {},
		debug:     func(f string, a ...any) { *debug1 = append(*debug1, fmt.Sprintf(f, a...)) },
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec1, 1<<20); err != nil {
		t.Fatalf("first readArchiveCredentials: %v", err)
	}
	if got := w.get(); got != "secret" {
		t.Fatalf("winner after 7z resolve = %q, want secret", got)
	}
	if !anyDebugLineContains(*debug1, "rejected", "incorrect") {
		t.Fatalf("first archive must log a rejected wrong candidate; debug = %v", *debug1)
	}
	if len(*creds1) != 1 {
		t.Fatalf("first archive credentials = %d, want 1", len(*creds1))
	}

	debug2 := &[]string{}
	emit2, creds2 := credentialSink()
	ec2 := extractCtx{
		passwords: []string{"wrong", "secret"},
		winner:    &w,
		display:   path,
		emit:      emit2,
		onIssue:   func(string, IssueKind, error) {},
		debug:     func(f string, a ...any) { *debug2 = append(*debug2, fmt.Sprintf(f, a...)) },
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec2, 1<<20); err != nil {
		t.Fatalf("second readArchiveCredentials: %v", err)
	}
	if anyDebugLineContains(*debug2, "rejected", "incorrect") {
		t.Fatalf("second archive still probed a rejected candidate; debug = %v", *debug2)
	}
	if len(*creds2) != 1 {
		t.Fatalf("second archive credentials = %d, want 1", len(*creds2))
	}
}

func anyDebugLineContains(lines []string, subs ...string) bool {
	for _, l := range lines {
		for _, s := range subs {
			if strings.Contains(l, s) {
				return true
			}
		}
	}
	return false
}

// rarRaceOrder must push the empty password last in the rar fallback race: it
// cannot prove an encrypted member, but it can win the probe on an archive
// whose first member is unencrypted while later members are encrypted.
func TestRarRaceOrderEmptyLast(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   []string
		want []string
	}{
		{"empty moved last", []string{"a", "", "b"}, []string{"a", "b", ""}},
		{"leading empty to tail", []string{"", "x"}, []string{"x", ""}},
		{"no empty unchanged", []string{"x", "y"}, []string{"x", "y"}},
		{"only empty", []string{""}, []string{""}},
		{"nil", nil, []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := rarRaceOrder(tt.in)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("rarRaceOrder(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// Multi-volume rar: the promoted winner must skip the "(multi-volume)" probe
// race on the second archive. Generated with the real rar packer (skips when
// absent), like the spanning tests.
func TestRarVolumesStickyWinnerSkipsProbeRace(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found; skipping multi-volume sticky-winner test")
	}
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "URL: https://vol-%d.example/login\nUSER: user%d\nPASS: pass%d\n", i, i, i)
	}
	for b.Len() < 60_000 {
		b.WriteString("# padding line to push member data across volume boundaries\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "Passwords.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(rarBin, "a", "-m0", "-v5k", "-hp123", "-idq", "arc.rar", "Passwords.txt")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "arc.part*.rar"))
	sort.Strings(parts)
	if len(parts) < 2 {
		t.Skipf("rar produced %d part(s), need a real multi-volume set", len(parts))
	}
	var weight int64
	for _, pp := range parts {
		fi, e := os.Stat(pp)
		if e != nil {
			t.Fatal(e)
		}
		weight += fi.Size()
	}

	var w runWinner
	debug1 := &[]string{}
	emit, creds1 := credentialSink()
	ec1 := extractCtx{
		passwords: []string{"wrong", "123"},
		winner:    &w,
		display:   parts[0],
		emit:      emit,
		onIssue:   func(string, IssueKind, error) {},
		debug:     func(f string, a ...any) { *debug1 = append(*debug1, fmt.Sprintf(f, a...)) },
	}
	if _, err := readRarVolumes(context.Background(), parts, ec1, weight); err != nil {
		t.Fatalf("first readRarVolumes: %v; debug=%v", err, *debug1)
	}
	// Note: new-style part sets are assembled by the engine worklist, so the
	// unit seam is readRarVolumes(parts, ...) — readArchiveCredentials on one
	// part alone legitimately takes the single-file path.
	if got := w.get(); got != "123" {
		t.Fatalf("winner after multi-volume rar = %q, want 123", got)
	}
	if !anyDebugLineContains(*debug1, "first password wrong, racing") {
		t.Fatalf("first archive must pay the multi-volume probe race; debug = %v", *debug1)
	}
	if len(*creds1) == 0 {
		t.Fatal("first archive extracted no credentials")
	}

	debug2 := &[]string{}
	emit2, creds2 := credentialSink()
	ec2 := extractCtx{
		passwords: []string{"wrong", "123"},
		winner:    &w,
		display:   parts[0],
		emit:      emit2,
		onIssue:   func(string, IssueKind, error) {},
		debug:     func(f string, a ...any) { *debug2 = append(*debug2, fmt.Sprintf(f, a...)) },
	}
	if _, err := readRarVolumes(context.Background(), parts, ec2, weight); err != nil {
		t.Fatalf("second readRarVolumes: %v; debug=%v", err, *debug2)
	}
	if anyDebugLineContains(*debug2, "first password wrong, racing") {
		t.Fatalf("second archive re-raced multi-volume candidates; debug = %v", *debug2)
	}
	if len(*creds2) != len(*creds1) {
		t.Fatalf("credential count changed between runs: %d then %d", len(*creds1), len(*creds2))
	}
}

// Nested inheritance: the winner slot is a shared pointer carried through the
// value-copied extractCtx, so an encrypted zip nested inside a plain outer
// archive promotes through recursion. Two inner archives prove the flow.
func TestNestedZipPromotesThroughRecursion(t *testing.T) {
	dir := t.TempDir()
	var inners []string
	for _, name := range []string{"in_a.zip", "in_b.zip"} {
		p := filepath.Join(dir, name)
		writeEncryptedTestZipMethod(t, p, "secret", "victim/Passwords.txt",
			fmt.Sprintf("URL: https://%s.example/login\nUSER: u\nPASS: p\n", strings.TrimSuffix(name, ".zip")),
			zipenc.AES256Encryption)
		inners = append(inners, p)
	}
	outerPath := filepath.Join(dir, "outer.zip")
	zf, err := os.Create(outerPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for _, p := range inners {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		w, err := zw.Create("nested/" + filepath.Base(p))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}

	var w runWinner
	emit, creds := credentialSink()
	ec := extractCtx{
		passwords: []string{"wrong", "secret"},
		winner:    &w,
		display:   outerPath,
		emit:      emit,
		onIssue:   func(string, IssueKind, error) {},
	}
	if _, err := readArchiveCredentials(context.Background(), outerPath, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if got := w.get(); got != "secret" {
		t.Fatalf("winner after nested run = %q, want secret (recursion must share the slot)", got)
	}
	if len(*creds) != 2 {
		t.Fatalf("credentials = %d, want 2 (one per inner zip)", len(*creds))
	}
}

// The Engine wires its shared winner slot into every worker's extractCtx.
func TestEngineWiresWinnerSlot(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.zip", "b.zip"} {
		writeEncryptedTestZipMethod(t, filepath.Join(dir, name), "secret", "victim/Passwords.txt",
			"URL: https://x.example/login\nUSER: u\nPASS: p\n", zipenc.AES256Encryption)
	}
	eng := &Engine{Workers: 1, Passwords: []string{"wrong", "secret"}}
	var out bytes.Buffer
	stats, _, err := eng.Run(context.Background(), dir, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := eng.winner.get(); got != "secret" {
		t.Fatalf("engine winner after run = %q, want secret", got)
	}
	if stats.Emitted == 0 {
		t.Fatalf("engine run emitted nothing (stats: %+v)", stats)
	}
}
