package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The nesting guard must judge by filesystem identity, not lexical shape:
// symlinked parents resolve, missing targets keep their prospective suffix,
// and any stat failure fails closed.
func TestOutputUnderInputTreeIdentities(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "in")
	if err := os.Mkdir(input, 0o700); err != nil {
		t.Fatal(err)
	}
	existingOut := filepath.Join(input, "out")
	if err := os.Mkdir(existingOut, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(input, alias); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		out     string
		input   string
		want    bool
		wantErr bool
	}{
		{name: "same directory", out: input, input: input, want: true},
		{name: "nested existing", out: existingOut, input: input, want: true},
		{name: "nested prospective (missing)", out: filepath.Join(input, "new", "deep"), input: input, want: true},
		{name: "symlink alias of input", out: alias, input: input, want: true},
		{name: "nested under alias", out: filepath.Join(alias, "out"), input: input, want: true},
		{name: "missing target under alias", out: filepath.Join(alias, "fresh"), input: input, want: true},
		{name: "disjoint sibling", out: filepath.Join(root, "other"), input: input, want: false},
		{name: "parent of input", out: root, input: input, want: false},
		{name: "inside out while input nested (reversed)", out: existingOut, input: filepath.Join(existingOut, "sub"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := outputUnderInputTree(tc.out, tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("outputUnderInputTree(%s, %s) = %v, want error", tc.out, tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("outputUnderInputTree(%s, %s): %v", tc.out, tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("outputUnderInputTree(%s, %s) = %v, want %v", tc.out, tc.input, got, tc.want)
			}
		})
	}
}

// An unresolvable path (a symlink loop in an ancestor) must fail closed
// rather than be judged outside by lexical fallback.
func TestOutputUnderInputTreeFailsClosedOnUnresolvable(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "in")
	if err := os.Mkdir(input, 0o700); err != nil {
		t.Fatal(err)
	}
	// a -> b -> a: every stat through the loop returns ELOOP, which is not a
	// "missing ancestor", so canonicalization must surface the error.
	a := filepath.Join(root, "loop-a")
	b := filepath.Join(root, "loop-b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := outputUnderInputTree(filepath.Join(a, "out"), input); err == nil {
		t.Fatal("symlink-loop ancestor did not fail closed")
	}
}
