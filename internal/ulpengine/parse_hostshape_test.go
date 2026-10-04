package ulpengine

import "testing"

// TestStrictAdmitsSpecialShapeHosts pins the A4 fix: recognized ULP records
// whose HOST carries unicode (IDN), '+', or '*' must parse strictly — real
// stealer logs contain such hostnames and dropping them is silent ingest-layer
// data loss. The >=2-letter ASCII TLD anchor is unchanged, so garbage dotted
// tokens without one stay out of strict.
func TestStrictAdmitsSpecialShapeHosts(t *testing.T) {
	ok := []struct {
		line      string
		wantHost  string
		wantLogin string
		wantPass  string
	}{
		{"示例.com:alice:hunter2", "示例.com", "alice", "hunter2"},
		{"foo+bar.com:bob:hunter2", "foo+bar.com", "bob", "hunter2"},
		{"a.b*c.com:carol:hunter2", "a.b*c.com", "carol", "hunter2"},
		{"https://示例.com/path:dave:hunter2", "示例.com", "dave", "hunter2"},
	}
	for _, c := range ok {
		h, _, l, p, ok := parse(c.line)
		if !ok {
			t.Fatalf("strict rejected special-shape host line %q", c.line)
		}
		if h != c.wantHost || l != c.wantLogin || p != c.wantPass {
			t.Fatalf("strict fields wrong for %q: got (%q,%q,%q), want (%q,%q,%q)",
				c.line, h, l, p, c.wantHost, c.wantLogin, c.wantPass)
		}
	}

	// No ASCII alpha TLD → still rejected, so dotted-token garbage without one
	// never enters strict.
	reject := []string{
		"示例.示例:alice:hunter2", // non-ASCII TLD
		"foo+bar:user:pass",   // no dot at all
		"a.b*c:carol:hunter2", // TLD "c" too short
		"x::y",
		"a:b:c:d",
	}
	for _, line := range reject {
		if _, _, _, _, ok := parse(line); ok {
			t.Fatalf("strict must keep rejecting %q", line)
		}
	}
}
