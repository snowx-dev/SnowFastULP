package zip

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// buildTwoMemberZip writes a normal in-memory zip with two stored members.
// Members are created in this fixed order; File[0] is a.txt.
func buildTwoMemberZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := NewWriter(&buf)
	for _, m := range []struct{ name, body string }{
		{"a.txt", "alpha-body"},
		{"b.log", "bravo-body"},
	} {
		w, err := zw.Create(m.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A true SFX/script prefix prepends bytes to an untouched zip: every internal
// offset stays relative to the zip payload start, so the reader must derive
// the prefix base from the EOCD position (base = EOCD offset - directory size
// - stored directory offset) and apply it to the central-directory seek and
// every local-header offset. Shell-text and PE-like prefixes must both open
// and read; garbage wrapped in a stub stays rejected.
func TestSFXPrefixedZipOpensWithoutRewrittenOffsets(t *testing.T) {
	raw := buildTwoMemberZip(t)

	prefixes := map[string][]byte{
		"shell": []byte("#!/bin/sh\necho self-extracting payload\nexit 0\n"),
		// PE-flavoured stub: DOS MZ magic, padding, PE signature.
		"pe": append(append([]byte{'M', 'Z', 0x90, 0x00},
			bytes.Repeat([]byte{0xCC}, 96)...),
			'P', 'E', 0x00, 0x00, 0x4C, 0x01),
	}

	for name, prefix := range prefixes {
		t.Run(name, func(t *testing.T) {
			prefixed := append(append([]byte{}, prefix...), raw...)
			zr, err := NewReader(bytes.NewReader(prefixed), int64(len(prefixed)))
			if err != nil {
				t.Fatalf("open prefixed zip: %v", err)
			}
			if len(zr.File) != 2 {
				t.Fatalf("members = %d, want 2", len(zr.File))
			}
			want := map[string]string{"a.txt": "alpha-body", "b.log": "bravo-body"}
			for _, f := range zr.File {
				rc, err := f.Open()
				if err != nil {
					t.Fatalf("open member %s: %v", f.Name, err)
				}
				got, err := io.ReadAll(rc)
				if err != nil {
					t.Fatalf("read member %s: %v", f.Name, err)
				}
				rc.Close()
				if string(got) != want[f.Name] {
					t.Fatalf("member %s body = %q, want %q", f.Name, got, want[f.Name])
				}
				// DataOffset is absolute: the local header (30 bytes + name)
				// must sit right before the data.
				off, err := f.DataOffset()
				if err != nil {
					t.Fatalf("DataOffset %s: %v", f.Name, err)
				}
				sig := make([]byte, 4)
				hdrOff := off - int64(fileHeaderLen+len(f.Name))
				if _, err := zr.r.ReadAt(sig, hdrOff); err != nil {
					t.Fatalf("read local header %s at %d: %v", f.Name, hdrOff, err)
				}
				if !bytes.Equal(sig, []byte{'P', 'K', 0x03, 0x04}) {
					t.Fatalf("member %s: no local header at DataOffset-%d", f.Name, fileHeaderLen+len(f.Name))
				}
			}
		})
	}
}

// A stub wrapped around non-zip bytes must stay a format error.
func TestSFXGarbageDecoyRejected(t *testing.T) {
	decoy := append([]byte("#!/bin/sh\necho decoy\n"), bytes.Repeat([]byte("junk "), 4000)...)
	if _, err := NewReader(bytes.NewReader(decoy), int64(len(decoy))); !errors.Is(err, ErrFormat) {
		t.Fatalf("decoy err = %v, want ErrFormat", err)
	}
}

// A prefix that pushes the stored directory offset past the raw file start
// must not be misread: base derivation keeps offsets inside the file.
func TestSFXPrefixBaseKeepsOffsetsInFile(t *testing.T) {
	raw := buildTwoMemberZip(t)
	prefix := bytes.Repeat([]byte{0x90}, 4096) // large stub
	prefixed := append(append([]byte{}, prefix...), raw...)
	zr, err := NewReader(bytes.NewReader(prefixed), int64(len(prefixed)))
	if err != nil {
		t.Fatalf("open 4KiB-prefixed zip: %v", err)
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatalf("open member: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != "alpha-body" {
		t.Fatalf("member body = %q err = %v, want alpha-body", got, err)
	}
}
