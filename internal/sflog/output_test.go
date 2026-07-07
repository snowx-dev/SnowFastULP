package sflog

import (
	"bytes"
	"testing"
)

func TestWriteULPLinesDedupsWithinRun(t *testing.T) {
	creds := []Credential{
		{URL: "https://a.example.com/login", Username: "u", Password: "p"},
		{URL: "https://a.example.com/login", Username: "u", Password: "p"},
		{URL: "https://b.example.com", Username: "u2", Password: "p2"},
	}
	var out bytes.Buffer
	stats, err := WriteULPLines(&out, creds, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Emitted != 2 || stats.Duplicates != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	want := "a.example.com/login:u:p\nb.example.com:u2:p2\n"
	if out.String() != want {
		t.Fatalf("out = %q want %q", out.String(), want)
	}
}
