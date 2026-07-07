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
