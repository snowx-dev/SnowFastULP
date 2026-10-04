package history_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

type selectionStore struct {
	hits   map[history.Identity]struct{}
	err    error
	calls  int
	looked []history.Identity
}

func (s *selectionStore) Lookup(_ context.Context, identities []history.Identity) (map[history.Identity]struct{}, error) {
	s.calls++
	s.looked = append([]history.Identity(nil), identities...)
	return s.hits, s.err
}

func (*selectionStore) Record(context.Context, []history.Identity) error { return nil }
func (*selectionStore) Close() error                                     { return nil }

func candidate(hash uint64, size int64, path string) history.Candidate {
	return history.Candidate{ID: history.Identity{Hash: hash, Size: size}, Paths: []string{path}}
}

func candidatePaths(candidates []history.Candidate) []string {
	paths := make([]string, len(candidates))
	for i, c := range candidates {
		paths[i] = c.Paths[0]
	}
	return paths
}

func TestSelectEmptyInput(t *testing.T) {
	store := &selectionStore{hits: map[history.Identity]struct{}{}}
	selection, err := history.Select(context.Background(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Pending) != 0 || len(selection.Hits) != 0 {
		t.Fatalf("selection = %+v, want empty", selection)
	}
	if store.calls != 1 || len(store.looked) != 0 {
		t.Fatalf("Lookup calls/input = %d/%v, want one empty batch", store.calls, store.looked)
	}
}

func TestSelectAllMisses(t *testing.T) {
	candidates := []history.Candidate{candidate(1, 1, "a"), candidate(2, 2, "b")}
	store := &selectionStore{hits: map[history.Identity]struct{}{}}
	selection, err := history.Select(context.Background(), store, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got := candidatePaths(selection.Pending); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("pending = %v", got)
	}
	if len(selection.Hits) != 0 {
		t.Fatalf("hits = %v, want none", candidatePaths(selection.Hits))
	}
	if store.calls != 1 {
		t.Fatalf("Lookup calls = %d, want 1", store.calls)
	}
}

func TestSelectAllHits(t *testing.T) {
	candidates := []history.Candidate{candidate(1, 1, "a"), candidate(2, 2, "b")}
	store := &selectionStore{hits: map[history.Identity]struct{}{candidates[0].ID: {}, candidates[1].ID: {}}}
	selection, err := history.Select(context.Background(), store, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Pending) != 0 {
		t.Fatalf("pending = %v, want none", candidatePaths(selection.Pending))
	}
	if got := candidatePaths(selection.Hits); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("hits = %v", got)
	}
}

func TestSelectMixedPreservesOrder(t *testing.T) {
	candidates := []history.Candidate{
		candidate(1, 1, "miss-a"),
		candidate(2, 2, "hit-a"),
		candidate(3, 3, "miss-b"),
		candidate(4, 4, "hit-b"),
	}
	store := &selectionStore{hits: map[history.Identity]struct{}{candidates[1].ID: {}, candidates[3].ID: {}}}
	selection, err := history.Select(context.Background(), store, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got := candidatePaths(selection.Pending); !reflect.DeepEqual(got, []string{"miss-a", "miss-b"}) {
		t.Fatalf("pending = %v", got)
	}
	if got := candidatePaths(selection.Hits); !reflect.DeepEqual(got, []string{"hit-a", "hit-b"}) {
		t.Fatalf("hits = %v", got)
	}
}

func TestSelectKeepsDuplicateIdentityCandidates(t *testing.T) {
	first := candidate(7, 8, "first")
	second := candidate(7, 8, "second")
	store := &selectionStore{hits: map[history.Identity]struct{}{first.ID: {}}}
	selection, err := history.Select(context.Background(), store, []history.Candidate{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if got := candidatePaths(selection.Hits); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("duplicate identity candidates collapsed or reordered: %v", got)
	}
	if len(selection.Pending) != 0 {
		t.Fatalf("pending = %v, want none", candidatePaths(selection.Pending))
	}
}

func TestSelectPropagatesLookupError(t *testing.T) {
	want := errors.New("lookup failed")
	store := &selectionStore{err: want}
	_, err := history.Select(context.Background(), store, []history.Candidate{candidate(1, 1, "a")})
	if !errors.Is(err, want) {
		t.Fatalf("Select error = %v, want %v", err, want)
	}
}
