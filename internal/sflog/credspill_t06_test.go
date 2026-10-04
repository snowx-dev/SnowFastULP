package sflog

import (
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
)

func TestCredSinkReplayEmptyMemoryAndSpill(t *testing.T) {
	for _, spill := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "empty-spill"}[spill], func(t *testing.T) {
			sink := newCredSink(t.TempDir(), "source-id")
			if spill {
				if err := sink.spill(); err != nil {
					t.Fatal(err)
				}
			}
			var got []Credential
			if err := sink.replay(func(c Credential) error {
				got = append(got, c)
				return nil
			}); err != nil {
				t.Fatalf("replay empty sink: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("empty sink replayed %d records: %+v", len(got), got)
			}
			sink.discard()
		})
	}
}

func TestCredSinkReplayTruncatedSpillRecord(t *testing.T) {
	sink := newCredSink(t.TempDir(), "source-id")
	defer sink.discard()
	want := Credential{URL: "https://example.com/" + string(make([]byte, credSinkMemCap)), Username: "alice", Password: "p:a:ss"}
	if err := sink.add(want); err != nil {
		t.Fatal(err)
	}
	if !sink.spilled || sink.f == nil {
		t.Fatal("large record did not enter spill path")
	}
	if err := sink.bw.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err := sink.f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 2 {
		t.Fatalf("spill record unexpectedly short: %d bytes", info.Size())
	}
	if err := os.Truncate(sink.f.Name(), info.Size()-1); err != nil {
		t.Fatal(err)
	}
	var got []Credential
	err = sink.replay(func(c Credential) error {
		got = append(got, c)
		return nil
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("replay truncated record error = %v, want io.ErrUnexpectedEOF", err)
	}
	if len(got) != 0 {
		t.Fatalf("truncated record was emitted as corrupt metadata: %+v", got)
	}
}

func TestCredSinkReplayMemoryToSpillPreservesOrderAndMetadata(t *testing.T) {
	sink := newCredSink(t.TempDir(), "source-id")
	defer sink.discard()
	want := []Credential{
		{URL: "https://first.example", Username: "first", Password: "one"},
		{URL: "https://large.example/" + string(make([]byte, credSinkMemCap)), Username: "second", Password: "two:three"},
		{URL: "https://last.example", Username: "last", Password: "four"},
	}
	for _, c := range want {
		if err := sink.add(c); err != nil {
			t.Fatal(err)
		}
	}
	var got []Credential
	if err := sink.replay(func(c Credential) error {
		got = append(got, c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := range want {
		want[i].Source = "source-id"
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replay = %+v, want ordered records %+v", got, want)
	}
}
