package selfupdate

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// forceNotesAscii forces lipgloss's default renderer to plain ASCII so the
// notes box bytes are deterministic even when the test runner's stdout is a
// TTY (same pattern as cmd/sfu's forceTrueColor, tui_color_test.go:17-20).
func forceNotesAscii(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// startNotesReleaseServer serves a two-binary release whose manifest carries
// notes_url/notes_sha256 (when withNotes) pointing at /notes, plus a notes
// endpoint controlled by notesStatus/notesBody. Every request path is
// recorded in order so tests can pin the manifest → assets → notes ordering.
func startNotesReleaseServer(t *testing.T, version, suffix string, sfuPayload, sfsPayload, notesBody []byte, notesStatus int, withNotes bool, notesSHA string) (*httptest.Server, *[]string) {
	t.Helper()
	reqs := []string{}
	names := mockAssetNames{
		sfu:     "SnowFastULP-" + version + "-" + suffix,
		sfs:     "SnowFastSearch-" + version + "-" + suffix,
		sfuHash: hexHash(sfuPayload),
		sfsHash: hexHash(sfsPayload),
	}
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.URL.Path)
		switch {
		case r.URL.Path == "/releases/latest":
			manifest := updateManifest{
				Version: version,
				Assets:  map[string]manifestAsset{},
				Bins: []manifestBin{
					{Name: "sfu", Prefix: "SnowFastULP"},
					{Name: "sfs", Prefix: "SnowFastSearch"},
				},
			}
			manifest.Assets[names.sfu] = manifestAsset{SHA256: names.sfuHash, URL: baseURL + "/asset/sfu"}
			manifest.Assets[names.sfs] = manifestAsset{SHA256: names.sfsHash, URL: baseURL + "/asset/sfs"}
			if withNotes {
				manifest.NotesURL = baseURL + "/notes"
				manifest.NotesSHA256 = notesSHA
			}
			_ = json.NewEncoder(w).Encode(manifest)
		case r.URL.Path == "/asset/sfu":
			_, _ = w.Write(sfuPayload)
		case r.URL.Path == "/asset/sfs":
			_, _ = w.Write(sfsPayload)
		case r.URL.Path == "/notes":
			if notesStatus != http.StatusOK {
				w.WriteHeader(notesStatus)
				return
			}
			_, _ = w.Write(notesBody)
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = srv.URL
	return srv, &reqs
}

const sampleRunNotes = "### Changed\n- notes entry one\n- notes entry two\n"

// The manifest schema grows in both directions: notes fields decode when
// present and stay empty when absent (the old-binary leniency twin).
func TestManifestDecodesNotesFieldsBothWays(t *testing.T) {
	withNotes := `{"version":"0.3.0","assets":{},
		"notes_url":"https://sfu-update.snowx.dev/update-notes-0.3.0.md",
		"notes_sha256":"` + strings.Repeat("ab", 32) + `"}`
	withoutNotes := `{"version":"0.3.0","assets":{},"future_field":42}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/with":
			_, _ = w.Write([]byte(withNotes))
		default:
			_, _ = w.Write([]byte(withoutNotes))
		}
	}))
	defer srv.Close()

	m, err := fetchLatest(&testHooks{releaseURL: srv.URL + "/with"}, "sfu", "0.2.0")
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if m.NotesURL != "https://sfu-update.snowx.dev/update-notes-0.3.0.md" {
		t.Errorf("NotesURL = %q", m.NotesURL)
	}
	if m.NotesSHA256 != strings.Repeat("ab", 32) {
		t.Errorf("NotesSHA256 = %q", m.NotesSHA256)
	}

	m, err = fetchLatest(&testHooks{releaseURL: srv.URL + "/without"}, "sfu", "0.2.0")
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if m.NotesURL != "" || m.NotesSHA256 != "" {
		t.Errorf("notes fields = %q/%q, want empty for a pre-notes manifest", m.NotesURL, m.NotesSHA256)
	}
}

// The notes GET happens only after every payload is downloaded and applied,
// and the box follows the success lines in the output.
func TestRunNotesFetchedAfterApplies(t *testing.T) {
	forceNotesAscii(t)
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	newSFU := []byte("#!/bin/sh\necho sfu-0.1.2\n")
	newSFS := []byte("#!/bin/sh\necho sfs-0.1.2\n")
	srv, reqs := startNotesReleaseServer(t, "0.1.2", suffix, newSFU, newSFS,
		[]byte(sampleRunNotes), http.StatusOK, true, hexHash([]byte(sampleRunNotes)))
	defer srv.Close()

	_, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	var applied []string
	applyPayloadHook = func(_ []byte, target string, _ []byte) error {
		applied = append(applied, filepath.Base(target))
		return nil
	}
	t.Cleanup(func() { applyPayloadHook = nil })

	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	// ordering: manifest, then asset downloads, then — only after both
	// applies — the notes GET.
	if len(*reqs) != 4 {
		t.Fatalf("requests = %v, want 4 (manifest, 2 assets, notes)", *reqs)
	}
	if (*reqs)[0] != "/releases/latest" || (*reqs)[3] != "/notes" {
		t.Fatalf("request order = %v, want manifest first and notes last", *reqs)
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %v, want 2 targets before the notes fetch", applied)
	}

	out := buf.String()
	if !strings.Contains(out, "updated sfu, sfs to 0.1.2") {
		t.Fatalf("missing success line:\n%s", out)
	}
	if i, j := strings.Index(out, "updated sfu, sfs to 0.1.2"), strings.Index(out, "What's new in v0.1.2"); i < 0 || j < 0 || j < i {
		t.Fatalf("success lines must precede the notes box:\n%s", out)
	}
	if !strings.Contains(out, "notes entry one") || !strings.Contains(out, "notes entry two") {
		t.Fatalf("notes content missing:\n%s", out)
	}
	if !strings.Contains(out, "full changelog:") || !strings.Contains(out, "CHANGELOG.md") {
		t.Fatalf("footer changelog link missing:\n%s", out)
	}
}

// The hard constraint: dry-run and up-to-date never fetch notes and their
// output stays byte-identical — even when the manifest carries notes.
func TestRunGatePathsNoNotesGETByteGolden(t *testing.T) {
	forceNotesAscii(t)
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	newSFU := []byte("#!/bin/sh\necho sfu-0.1.2\n")
	newSFS := []byte("#!/bin/sh\necho sfs-0.1.2\n")

	t.Run("up-to-date", func(t *testing.T) {
		srv, reqs := startNotesReleaseServer(t, "0.1.2", suffix, newSFU, newSFS,
			[]byte(sampleRunNotes), http.StatusOK, true, hexHash([]byte(sampleRunNotes)))
		defer srv.Close()

		self := filepath.Join(t.TempDir(), "sfu"+exeExt())
		if err := os.WriteFile(self, []byte("placeholder"), 0o755); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := run(nil, "0.1.2", "sfu", &buf, &testHooks{
			releaseURL:     srv.URL + "/releases/latest",
			executablePath: self,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
		if want := "checking for updates…\nalready up to date (0.1.2)\n"; buf.String() != want {
			t.Fatalf("output = %q, want exactly %q", buf.String(), want)
		}
		for _, r := range *reqs {
			if r == "/notes" {
				t.Fatalf("up-to-date fetched notes: %v", *reqs)
			}
		}
	})

	t.Run("dry-run", func(t *testing.T) {
		srv, reqs := startNotesReleaseServer(t, "0.1.2", suffix, newSFU, newSFS,
			[]byte(sampleRunNotes), http.StatusOK, true, hexHash([]byte(sampleRunNotes)))
		defer srv.Close()

		_, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
		var buf bytes.Buffer
		if err := run([]string{"--dry-run"}, "0.1.1", "sfu", &buf, &testHooks{
			releaseURL:     srv.URL + "/releases/latest",
			executablePath: hooks.executablePath,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
		want := "checking for updates…\nwould update: sfu, sfs (to 0.1.2)\nno changes made (dry run)\n"
		if buf.String() != want {
			t.Fatalf("output = %q, want exactly %q", buf.String(), want)
		}
		for _, r := range *reqs {
			if r == "/notes" {
				t.Fatalf("dry-run fetched notes: %v", *reqs)
			}
		}
	})
}

// Any notes failure collapses to one muted line: run still returns nil, the
// success lines stay intact, and stdout ends with the soft-failure line.
func TestRunNotesFailureMuted(t *testing.T) {
	forceNotesAscii(t)
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	newSFU := []byte("#!/bin/sh\necho sfu-0.1.2\n")
	newSFS := []byte("#!/bin/sh\necho sfs-0.1.2\n")

	cases := []struct {
		name     string
		status   int
		notesSHA string
	}{
		{"404", http.StatusNotFound, ""},
		{"500", http.StatusInternalServerError, ""},
		{"sha mismatch", http.StatusOK, hexHash([]byte("other body"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := startNotesReleaseServer(t, "0.1.2", suffix, newSFU, newSFS,
				[]byte(sampleRunNotes), tc.status, true, tc.notesSHA)
			defer srv.Close()

			_, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
			var buf bytes.Buffer
			if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
				releaseURL:     srv.URL + "/releases/latest",
				executablePath: hooks.executablePath,
			}); err != nil {
				t.Fatalf("run: %v", err)
			}
			want := "updated sfu, sfs to 0.1.2\nrelease notes unavailable\n"
			if !strings.HasSuffix(buf.String(), want) {
				t.Fatalf("stdout must end with %q, got:\n%q", want, buf.String())
			}
		})
	}
}
