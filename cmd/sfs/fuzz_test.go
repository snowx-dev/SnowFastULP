package main

import (
	"strings"
	"testing"
)

// FuzzSlugify feeds arbitrary search terms through the slugifier (via
// patternFileName, the caller that turns slugs into output filenames). No
// oracle: panic/hang detection is the value, plus the invariant that the
// produced name stays filename-safe (no path separators, no NUL).
func FuzzSlugify(f *testing.F) {
	for _, seed := range []string{
		"",
		"user@example.com",
		"foo bar",
		"a/b\\c",
		"a..b",
		"!!!",
		"hello",
		"a:b:c",
		`a/b\c:d*e?f"g<h>i|j`,
		"示例.com",
		"foo+bar.com",
		" \t lead/trail \x00 ",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, term string) {
		name := patternFileName(term, "20260924-1200")
		for _, bad := range []string{"/", "\\", "\x00"} {
			if strings.Contains(name, bad) {
				t.Fatalf("patternFileName(%q) = %q contains %q", term, name, bad)
			}
		}
	})
}

// FuzzParseSince feeds arbitrary age-window strings through parseSince. No
// oracle: panic/hang detection is the value (strconv/time parsing on hostile
// input).
func FuzzParseSince(f *testing.F) {
	for _, seed := range []string{
		"",
		"7d", "12h", "90m", "1d6h", " 2d ",
		"7", "abc", "d", "0d", "-3h", "7x",
		"999999999999999999999d",
		"1d9999999999999999999h",
		"9999999h",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := parseSince(s)
		if err == nil && d < 0 {
			t.Fatalf("parseSince(%q) accepted a negative duration %v", s, d)
		}
	})
}
