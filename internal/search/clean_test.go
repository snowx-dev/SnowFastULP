package search

import "testing"

func TestCleanLine(t *testing.T) {
	in := "https://example.com:user:pass"
	want := "example.com:user:pass"
	if got := cleanLine(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCleanLineMultipleSchemes(t *testing.T) {
	in := "user:pass:https://host/path"
	got := cleanLine(in)
	if got != "user:pass:host/path" {
		t.Fatalf("got %q", got)
	}
}

// TestCleanLineUnicodeDoesNotShiftOffsets pins ASCII-only case folding: a
// strings.ToLower index source changes byte offsets for runes whose lowercase
// form differs in length (dotted I grows 2→3 bytes), slicing the ORIGINAL
// string at positions valid only in the lowered one. Scheme matching must
// never treat non-ASCII lookalikes (Kelvin sign, dotted I, sharp S) as ASCII
// scheme letters, and valid UTF-8 must pass through intact.

func TestCleanLineDottedIBeforeScheme(t *testing.T) {
	// U+0130 lowercases to a 3-byte sequence; the pre-fix ToLower index
	// source shifted every later offset by +1 and corrupted the strip.
	in := "\u0130nfo https://user:pass@example.com"
	want := "\u0130nfo user:pass@example.com"
	if got := cleanLine(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCleanLineKelvinSignNotASchemeLetter(t *testing.T) {
	// U+212A KELVIN SIGN lowercases to ASCII 'k' under strings.ToLower, so
	// the pre-fix matcher treated "http<K>" as "httpk"... and more
	// importantly let ToLower-based matching cross the ASCII boundary. It is
	// not 'k': no scheme match, line unchanged.
	in := "http\u212A://host/path"
	if got := cleanLine(in); got != in {
		t.Fatalf("kelvin sign must not fold to 'k': got %q", got)
	}
	// A Kelvin sign before a real scheme must not disturb stripping.
	in = "K\u212A https://user:pass@host"
	want := "K\u212A user:pass@host"
	if got := cleanLine(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCleanLineSharpSNotASchemeLetter(t *testing.T) {
	// U+00DF sharp S is unaffected by scheme matching; surrounding valid
	// UTF-8 must survive byte-for-byte.
	in := "https://stra\u00DFe.de:u:p"
	want := "stra\u00DFe.de:u:p"
	if got := cleanLine(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	in = "Stra\u00DFe https://a:b"
	want = "Stra\u00DFe a:b"
	if got := cleanLine(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCleanLineASCIIFoldStillMatches(t *testing.T) {
	// ASCII case folding is preserved: uppercase scheme literals strip.
	in := "HTTPS://EXAMPLE.COM:u:p"
	want := "EXAMPLE.COM:u:p"
	if got := cleanLine(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCleanLineNestedSchemes(t *testing.T) {
	// Schemes appearing after an earlier strip are removed on the next pass,
	// case-insensitively, including around multi-byte content.
	in := "http://aHTTP://B\u4e2d\u6587ftp://c"
	want := "aB\u4e2d\u6587c"
	if got := cleanLine(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCleanLineValidUTF8Intact(t *testing.T) {
	in := "https://\u4f8b\u3048.jp:\u30e6\u30fc\u30b6\u30fc:\u30d1\u30b9\U0001F600"
	want := "\u4f8b\u3048.jp:\u30e6\u30fc\u30b6\u30fc:\u30d1\u30b9\U0001F600"
	if got := cleanLine(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
