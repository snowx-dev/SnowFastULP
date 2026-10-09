package releasenotes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/muesli/termenv"
)

// forceAscii forces lipgloss's default renderer to plain ASCII so buffered
// output is byte-deterministic even when the test runner's stdout is a TTY
// (mirror of cmd/sfu's forceTrueColor helper, tui_color_test.go:17-20).
func forceAscii(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// forceTrueColor pins the full-truecolor SGR profile for golden render
// tests, restored on cleanup (tui_color_test.go:17-20 pattern).
func forceTrueColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

func hexHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// unavailableLine is the exact soft-failure line under the Ascii profile.
func unavailableLine() string {
	return unavailableStyle.Render(unavailableMessage) + "\n"
}

func bodyServer(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(body)
	}))
}

const sampleNotes = "### Changed\n- something new\n"

// The hard constraint: no notes_url → zero bytes, before any I/O.
func TestShowNotesAbsenceGateWritesZeroBytes(t *testing.T) {
	for _, url := range []string{"", "   ", "\t"} {
		var buf bytes.Buffer
		ShowNotes(&buf, url, hexHash([]byte("x")), "0.3.0", "sfu", "0.2.0")
		if buf.Len() != 0 {
			t.Errorf("notesURL %q: wrote %d bytes, want 0 (output %q)", url, buf.Len(), buf.String())
		}
	}
}

// Shrinking notesTimeout must break the fetch: the notes client really uses
// the package-level budget (selfupdate's httpTimeout test pattern).
func TestShowNotesTimeoutBudgetShrunk(t *testing.T) {
	forceAscii(t)
	prev := notesTimeout
	notesTimeout = 1 * time.Millisecond
	t.Cleanup(func() { notesTimeout = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(sampleNotes))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	ShowNotes(&buf, srv.URL, "", "0.3.0", "sfu", "0.2.0")
	if got := buf.String(); got != unavailableLine() {
		t.Fatalf("slow server: output %q, want %q", got, unavailableLine())
	}
}

func TestShowNotesSHA256MatchMismatchAbsentInvalid(t *testing.T) {
	forceAscii(t)
	body := []byte(sampleNotes)
	srv := bodyServer(t, body, http.StatusOK)
	defer srv.Close()

	t.Run("match renders", func(t *testing.T) {
		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, hexHash(body), "0.3.0", "sfu", "0.2.0")
		for _, want := range []string{"What's new in v0.3.0", "something new", "full changelog:"} {
			if !strings.Contains(buf.String(), want) {
				t.Fatalf("output missing %q:\n%s", want, buf.String())
			}
		}
	})
	t.Run("mismatch is muted", func(t *testing.T) {
		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, hexHash([]byte("tampered")), "0.3.0", "sfu", "0.2.0")
		if got := buf.String(); got != unavailableLine() {
			t.Fatalf("output %q, want %q", got, unavailableLine())
		}
	})
	t.Run("absent renders anyway", func(t *testing.T) {
		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, "", "0.3.0", "sfu", "0.2.0")
		if !strings.Contains(buf.String(), "something new") {
			t.Fatalf("absent sha should still render:\n%s", buf.String())
		}
	})
	t.Run("malformed hash is muted", func(t *testing.T) {
		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, "not-hex", "0.3.0", "sfu", "0.2.0")
		if got := buf.String(); got != unavailableLine() {
			t.Fatalf("output %q, want %q", got, unavailableLine())
		}
	})
}

func TestShowNotesSizeCap(t *testing.T) {
	forceAscii(t)
	exact := []byte(strings.Repeat("x", maxNotesSize))
	oversize := []byte(strings.Repeat("x", maxNotesSize+1))

	t.Run("exactly 256 KiB renders", func(t *testing.T) {
		srv := bodyServer(t, exact, http.StatusOK)
		defer srv.Close()
		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, "", "0.3.0", "sfu", "0.2.0")
		if !strings.Contains(buf.String(), "What's new in v0.3.0") {
			t.Fatalf("256 KiB body should render:\n%s", buf.String())
		}
	})
	t.Run("one byte over is muted", func(t *testing.T) {
		srv := bodyServer(t, oversize, http.StatusOK)
		defer srv.Close()
		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, "", "0.3.0", "sfu", "0.2.0")
		if got := buf.String(); got != unavailableLine() {
			t.Fatalf("output %q, want %q", got, unavailableLine())
		}
	})
}

func TestShowNotesStatusCodes(t *testing.T) {
	forceAscii(t)
	body := []byte(sampleNotes)

	t.Run("404 muted", func(t *testing.T) {
		srv := bodyServer(t, body, http.StatusNotFound)
		defer srv.Close()
		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, "", "0.3.0", "sfu", "0.2.0")
		if got := buf.String(); got != unavailableLine() {
			t.Fatalf("output %q, want %q", got, unavailableLine())
		}
	})
	t.Run("500 muted", func(t *testing.T) {
		srv := bodyServer(t, body, http.StatusInternalServerError)
		defer srv.Close()
		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, "", "0.3.0", "sfu", "0.2.0")
		if got := buf.String(); got != unavailableLine() {
			t.Fatalf("output %q, want %q", got, unavailableLine())
		}
	})
	t.Run("403 muted", func(t *testing.T) {
		srv := bodyServer(t, body, http.StatusForbidden)
		defer srv.Close()
		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, "", "0.3.0", "sfu", "0.2.0")
		if got := buf.String(); got != unavailableLine() {
			t.Fatalf("output %q, want %q", got, unavailableLine())
		}
	})
	t.Run("302 followed then renders", func(t *testing.T) {
		dst := bodyServer(t, body, http.StatusOK)
		defer dst.Close()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, dst.URL, http.StatusFound)
		}))
		defer srv.Close()
		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, "", "0.3.0", "sfu", "0.2.0")
		if !strings.Contains(buf.String(), "something new") {
			t.Fatalf("redirect should be followed:\n%s", buf.String())
		}
	})
}

// The notes request carries the same header discipline as every other
// selfupdate request.
func TestFetchNotesHeaders(t *testing.T) {
	var gotUA, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotAccept = r.UserAgent(), r.Header.Get("Accept")
		_, _ = w.Write([]byte(sampleNotes))
	}))
	defer srv.Close()

	if _, err := fetchNotes(srv.URL, "sfu", "0.2.0"); err != nil {
		t.Fatalf("fetchNotes: %v", err)
	}
	if want := "SnowFastULP-selfupdate/0.2.0 (sfu; gotest)"; gotUA != want {
		t.Errorf("User-Agent = %q, want %q", gotUA, want)
	}
	if want := notesAccept; gotAccept != want {
		t.Errorf("Accept = %q, want %q", gotAccept, want)
	}
}

func TestSanitizeStripsEscapeSequencesAndControls(t *testing.T) {
	raw := "### \x1b[31mTitle\x1b[0m\n- \x1b]8;;http://evil\x1b\\click\x1b]8;;\x1b\\\n" +
		"- nul\x00byte\x1b]0;title\x07here\r\n- tail\ttab"
	got := sanitize([]byte(raw))
	for _, bad := range []string{"\x1b", "\x00", "\x07", "\r", "\t"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitize output still contains %q: %q", bad, got)
		}
	}
	for _, want := range []string{"Title", "click", "nulbyte", "here", "tailtab"} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitize lost %q: %q", want, got)
		}
	}
}

// End-to-end: escape-laden notes can never leak control bytes into the
// rendered output.
func TestShowNotesSanitizedEndToEnd(t *testing.T) {
	forceAscii(t)
	evil := "### \x1b[31mEvil\x1b[0m\n- \x1b]8;;http://evil\x1b\\click\x1b]8;;\x1b\\\n- nul\x00byte\n"
	srv := bodyServer(t, []byte(evil), http.StatusOK)
	defer srv.Close()

	var buf bytes.Buffer
	ShowNotes(&buf, srv.URL, "", "0.3.0", "sfu", "0.2.0")
	out := buf.String()
	for _, bad := range []string{"\x1b", "\x00", "\x07"} {
		if strings.Contains(out, bad) {
			t.Errorf("rendered output contains control byte %q", bad)
		}
	}
	if !strings.Contains(out, "Evil") || !strings.Contains(out, "click") {
		t.Fatalf("content must survive sanitization:\n%s", out)
	}
}

// A renderer panic must collapse to the muted line, never crash the
// finished update.
func TestShowNotesRenderPanicIsMuted(t *testing.T) {
	forceAscii(t)
	orig := renderFn
	renderFn = func(out io.Writer, notes, version string, width int, start, end colorful.Color) error {
		panic("boom")
	}
	t.Cleanup(func() { renderFn = orig })

	var buf bytes.Buffer
	ShowNotes(&buf, "https://example.invalid/notes.md", "", "0.3.0", "sfu", "0.2.0")
	if got := buf.String(); got != unavailableLine() {
		t.Fatalf("output %q, want %q", got, unavailableLine())
	}
}

// R3: notes_url travels inside the (hand-editable) manifest, so a hostile
// manifest must not downgrade notes transport to plaintext http. Plain http
// is accepted only for loopback hosts — local test servers keep working,
// foreign hosts collapse to the muted unavailable line before any I/O.
func TestShowNotesPlainHTTPOnlyLoopback(t *testing.T) {
	forceAscii(t)
	body := []byte(sampleNotes)

	t.Run("foreign plain http is unavailable", func(t *testing.T) {
		var buf bytes.Buffer
		ShowNotes(&buf, "http://updates.example/notes.md", hexHash(body), "0.3.0", "sfu", "0.2.0")
		if got := buf.String(); got != unavailableLine() {
			t.Fatalf("output %q, want %q", got, unavailableLine())
		}
	})
	t.Run("loopback plain http renders", func(t *testing.T) {
		srv := bodyServer(t, body, http.StatusOK)
		defer srv.Close()

		var buf bytes.Buffer
		ShowNotes(&buf, srv.URL, hexHash(body), "0.3.0", "sfu", "0.2.0")
		for _, want := range []string{"What's new in v0.3.0", "something new"} {
			if !strings.Contains(buf.String(), want) {
				t.Fatalf("output missing %q:\n%s", want, buf.String())
			}
		}
	})
}

// The scheme check fires before the HTTP client is built: a rejected URL
// can never reach the network.
func TestFetchNotesRejectsPlainHTTPForeignHost(t *testing.T) {
	_, err := fetchNotes("http://updates.example/notes.md", "sfu", "0.2.0")
	if err == nil || !strings.Contains(err.Error(), "must be fetched over https") {
		t.Fatalf("expected https rejection, got: %v", err)
	}
}
