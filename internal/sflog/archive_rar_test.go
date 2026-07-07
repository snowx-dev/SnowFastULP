package sflog

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nwaples/rardecode"
)

func TestIsMissingVolume(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"path-error-enoent", &os.PathError{Op: "open", Path: "x.part7.rar", Err: fs.ErrNotExist}, true},
		{"wrapped-enoent", fmt.Errorf("opening next volume: %w", os.ErrNotExist), true},
		{"wrong-password", errors.New("rardecode: incorrect password"), false},
		{"generic", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := isMissingVolume(c.err); got != c.want {
			t.Errorf("isMissingVolume(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestIsWrongPassword(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rardecode", errors.New("rardecode: incorrect password"), true},
		{"sevenzip-checksum", fmt.Errorf("sevenzip: error reading: %w", errors.New("sevenzip: checksum error")), true},
		{"missing-volume", &os.PathError{Op: "open", Path: "x.part7.rar", Err: fs.ErrNotExist}, false},
		{"generic", errors.New("not a valid 7-zip file"), false},
	}
	for _, c := range cases {
		if got := isWrongPassword(c.err); got != c.want {
			t.Errorf("isWrongPassword(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// credBlocks returns n ULP blocks with distinct, greppable URLs plus padding so
// the file is at least minBytes (to help span RAR volume boundaries).
func credBlocks(prefix string, n, minBytes int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "URL: https://%s-%d.example/login\nUSER: user%d\nPASS: pass%d\n", prefix, i, i, i)
	}
	for b.Len() < minBytes {
		b.WriteString("# padding line to grow the member across volume boundaries\n")
	}
	return b.String()
}

// TestReadRarVolumesTruncatedSetSalvages builds a real multi-volume RAR set,
// deletes the trailing volume to simulate a truncated download, and proves the
// loop: (a) commits the creds from the parts present, (b) records exactly one
// IssueMissingVolume, and (c) runs a single extraction pass (no per-password
// re-stream) even with extra wrong passwords in the list.
func TestReadRarVolumesTruncatedSetSalvages(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found; skipping truncated multi-volume test")
	}
	dir := t.TempDir()
	// Many small password members (each ~8k) stored across 32k volumes: the
	// first is fully inside volume 1 (its creds are guaranteed committed before
	// truncation), and rardecode crosses volume boundaries between members so the
	// worker line advances part 1->2->... This mirrors a real stealer log far
	// better than one giant member, which would pin Next() to volume 1.
	const members = 30
	names := make([]string, members)
	for i := range names {
		names[i] = fmt.Sprintf("Passwords-%02d.txt", i)
		body := credBlocks(fmt.Sprintf("m%02d", i), 3, 8_000)
		if err := os.WriteFile(filepath.Join(dir, names[i]), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Store mode (-m0) + 32k volumes so the ~240k of stored data spans several
	// parts. Files are listed in order so member 00 lands in volume 1.
	cmd := exec.Command(rarBin, append([]string{"a", "-m0", "-v32k", "-idq", "arc.rar"}, names...)...)
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "arc.part*.rar"))
	sort.Strings(parts)
	if len(parts) < 3 {
		t.Skipf("rar produced %d part(s), need >= 3 for a truncation test", len(parts))
	}
	// Truncate: drop the last volume on disk and from the worklist, mirroring a
	// real partial download where grouping only sees the parts present.
	last := parts[len(parts)-1]
	if err := os.Remove(last); err != nil {
		t.Fatal(err)
	}
	present := parts[:len(parts)-1]

	var creds []Credential
	var dbg []string
	var labels []string
	type issue struct {
		path string
		kind IssueKind
	}
	var issues []issue
	ec := extractCtx{
		passwords: []string{"", "wrong-1", "wrong-2"},
		tempDir:   t.TempDir(),
		display:   present[0],
		emit:      func(c Credential) { creds = append(creds, c) },
		onIssue:   func(p string, k IssueKind, _ error) { issues = append(issues, issue{p, k}) },
		setItem:   func(l string) { labels = append(labels, l) },
		debug:     func(f string, a ...any) { dbg = append(dbg, fmt.Sprintf(f, a...)) },
	}

	scan, err := readRarVolumes(context.Background(), present, ec, 200_000)
	if err != nil {
		t.Fatalf("readRarVolumes returned error, want salvage success: %v\ndebug:\n%s",
			err, strings.Join(dbg, "\n"))
	}

	// (a) Creds from the present parts are committed.
	if len(creds) == 0 {
		t.Fatalf("no creds committed from a salvageable truncated set\ndebug:\n%s", strings.Join(dbg, "\n"))
	}
	if !anyContains(credURLs(creds), "m00-0.example") {
		t.Fatalf("expected first-volume creds to survive; got URLs:\n%s", strings.Join(credURLs(creds), "\n"))
	}
	_ = scan

	// (b) Exactly one missing-volume issue, no scary parse/password issue.
	missing := 0
	for _, is := range issues {
		if is.kind == IssueMissingVolume {
			missing++
		}
		if is.kind == IssuePasswordNotFound || is.kind == IssueParseError {
			t.Fatalf("unexpected issue kind %v for a salvageable set", is.kind)
		}
	}
	if missing != 1 {
		t.Fatalf("missing-volume issues = %d, want 1\nissues=%+v", missing, issues)
	}

	// (c) Single extraction pass: the loop never advanced to password 2/3.
	for _, l := range dbg {
		if strings.Contains(l, "testing password") {
			t.Fatalf("re-streamed on a structural error (found %q)\ndebug:\n%s", l, strings.Join(dbg, "\n"))
		}
	}
	if c := countContains(dbg, "extracting (multi-volume"); c != 1 {
		t.Fatalf("extraction passes = %d, want exactly 1\ndebug:\n%s", c, strings.Join(dbg, "\n"))
	}
	if !anyContains(dbg, "incomplete set, next volume missing") {
		t.Fatalf("missing honest truncation debug line\ndebug:\n%s", strings.Join(dbg, "\n"))
	}

	// (d) The worker line advances across volumes (set base name + "part N/M"),
	// instead of staying pinned to part01 for the whole set.
	maxPart, total := 0, 0
	for _, l := range labels {
		var base string
		var n, m int
		if _, e := fmt.Sscanf(l, "%s  ·  part %d/%d", &base, &n, &m); e == nil {
			if base != "arc" {
				t.Fatalf("worker label set name = %q, want %q (label=%q)", base, "arc", l)
			}
			if n > maxPart {
				maxPart = n
			}
			total = m
		}
	}
	if total != len(present) {
		t.Fatalf("worker label total = %d, want %d; labels=%v", total, len(present), labels)
	}
	if maxPart < 2 {
		t.Fatalf("worker line never advanced past part 1 (max=%d); labels=%v", maxPart, labels)
	}
}

// TestReadRarVolumeStreamCreditsBytesPerMember proves a complete multi-volume
// set credits on-disk bytes per member (so the TUI bar moves) rather than only
// per finished volume. It drives the stream directly so finish() can't mask the
// per-member crediting.
func TestReadRarVolumeStreamCreditsBytesPerMember(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found; skipping multi-volume crediting test")
	}
	dir := t.TempDir()
	const members = 24
	names := make([]string, members)
	for i := range names {
		names[i] = fmt.Sprintf("Passwords-%02d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, names[i]),
			[]byte(credBlocks(fmt.Sprintf("c%02d", i), 3, 8_000)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(rarBin, append([]string{"a", "-m0", "-v32k", "-idq", "arc.rar"}, names...)...)
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
	for _, p := range parts {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		weight += fi.Size()
	}

	rc, err := rardecode.OpenReader(parts[0], "")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	prog := &Progress{}
	prog.total.Store(weight)
	cr := newCreditor(prog, weight, 1)
	var creds int
	ec := extractCtx{
		display: parts[0],
		p:       prog,
		emit:    func(Credential) { creds++ },
		onIssue: func(string, IssueKind, error) {},
	}

	// No cr.finish() here: whatever is credited is purely per-member.
	if _, err := readRarVolumeStream(context.Background(), ec, rc, cr, len(parts)); err != nil {
		t.Fatalf("readRarVolumeStream: %v", err)
	}
	if creds == 0 {
		t.Fatal("no creds parsed from the multi-volume set")
	}
	done := prog.DoneBytes()
	if done <= 0 {
		t.Fatalf("DoneBytes = %d; per-member crediting never advanced the bar", done)
	}
	if done > weight {
		t.Fatalf("DoneBytes = %d overshot weight %d", done, weight)
	}
	// PackedSize sums to ~on-disk minus headers/volume-spanning remainders; for a
	// 24-member store-mode set it should cover most of the bytes well before any
	// finish() top-up, proving the bar tracks progress instead of sitting at 0.
	if done < weight/2 {
		t.Fatalf("DoneBytes = %d of %d (<50%%); crediting too coarse to move the bar", done, weight)
	}
}

func credURLs(creds []Credential) []string {
	out := make([]string, len(creds))
	for i, c := range creds {
		out[i] = c.URL
	}
	return out
}

func countContains(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}
