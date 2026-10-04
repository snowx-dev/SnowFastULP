package sflog

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestRarVolumesTruncatedSetSalvagesMemberSpanningVolumes pins the A2 fix: a
// truncated multi-volume set whose SINGLE credential member's decoded data
// crosses into the missing trailing volumes must still salvage every
// credential line decoded from the parts present. The member parser is
// all-or-nothing per member, so a missing continuation volume surfacing as a
// mid-member read error used to discard the whole prefix: zero lines
// salvaged while the debug log claimed "kept creds from parts read".
func TestRarVolumesTruncatedSetSalvagesMemberSpanningVolumes(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found; skipping member-spanning truncation test")
	}
	dir := t.TempDir()
	// One large Passwords.txt in store mode over tiny volumes so its data
	// spans the whole set; 300 planted credentials sit at the front (inside
	// the parts that remain on disk). Dropping the last two volumes cuts the
	// member mid-data, exactly like a truncated download.
	const planted = 300
	var b strings.Builder
	for i := 0; i < planted; i++ {
		fmt.Fprintf(&b, "URL: https://planted-%d.example/login\nUSER: user%d\nPASS: pass%d\n", i, i, i)
	}
	for b.Len() < 180_000 {
		b.WriteString("# padding line to push member data across volume boundaries\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "Passwords.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(rarBin, "a", "-m0", "-v5k", "-idq", "arc.rar", "Passwords.txt")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "arc.part*.rar"))
	sort.Strings(parts)
	if len(parts) < 4 {
		t.Skipf("rar produced %d part(s), need >= 4 for a truncation test", len(parts))
	}
	// The trailing volumes are absent from disk AND from the worklist.
	for _, p := range parts[len(parts)-2:] {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	present := parts[:len(parts)-2]

	var creds []Credential
	var dbg []string
	var issues []Issue
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   present[0],
		emit:      func(c Credential) { creds = append(creds, c) },
		onIssue:   func(p string, k IssueKind, e error) { issues = append(issues, Issue{Path: p, Kind: k, Err: e}) },
		debug:     func(f string, a ...any) { dbg = append(dbg, fmt.Sprintf(f, a...)) },
	}
	if _, err := readRarVolumes(context.Background(), present, ec, 200_000); err != nil {
		t.Fatalf("readRarVolumes returned error, want salvage success: %v\ndebug:\n%s",
			err, strings.Join(dbg, "\n"))
	}

	// All planted credentials from the parts present are salvaged.
	if len(creds) != planted {
		t.Fatalf("salvaged %d credentials, want all %d planted in the present parts\ndebug:\n%s",
			len(creds), planted, strings.Join(dbg, "\n"))
	}
	if creds[0].URL != "https://planted-0.example/login" ||
		creds[planted-1].URL != fmt.Sprintf("https://planted-%d.example/login", planted-1) {
		t.Fatalf("first/last salvaged credentials wrong: %q .. %q", creds[0].URL, creds[planted-1].URL)
	}

	// Exactly one honest missing-volume issue; no parse/password issue and no
	// false "no credential-named files found".
	missing := 0
	for _, is := range issues {
		if is.Kind == IssueMissingVolume {
			missing++
			continue
		}
		t.Fatalf("unexpected issue kind %v for a salvageable set: %+v", is.Kind, is)
	}
	if missing != 1 {
		t.Fatalf("missing-volume issues = %d, want 1\nissues=%+v\ndebug:\n%s", missing, issues, strings.Join(dbg, "\n"))
	}

	// The part counts in the debug/issue messages are truthful: the number of
	// parts actually decoded (never one MORE than the parts present, which the
	// old "36/35" garbling counted when rardecode appended the failed open).
	for _, d := range dbg {
		if strings.Contains(d, "next volume missing after") {
			if !strings.Contains(d, fmt.Sprintf("after %d/%d part(s)", len(present), len(present))) {
				t.Fatalf("part count in salvage message is not the decoded-part count: %q", d)
			}
		}
	}
	if !anyContains(dbg, "kept creds from parts read") {
		t.Fatalf("missing salvage debug line\ndebug:\n%s", strings.Join(dbg, "\n"))
	}
}
