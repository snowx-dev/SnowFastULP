package history

import "context"

type Selection struct {
	Pending []Candidate
	Hits    []Candidate
}

// Select partitions candidates into Hits and Pending by identity lookup.
// Selection is content-addressed: every candidate whose identity matches a
// recorded entry is a hit, so identical copies of one completed source all
// report as hits even though only one identity was ever recorded.
func Select(ctx context.Context, store Store, candidates []Candidate) (Selection, error) {
	identities := make([]Identity, len(candidates))
	for i, candidate := range candidates {
		identities[i] = candidate.ID
	}
	hits, err := store.Lookup(ctx, identities)
	if err != nil {
		return Selection{}, err
	}

	selection := Selection{
		Pending: make([]Candidate, 0, len(candidates)),
		Hits:    make([]Candidate, 0, len(candidates)),
	}
	for _, candidate := range candidates {
		if _, ok := hits[candidate.ID]; ok {
			selection.Hits = append(selection.Hits, candidate)
		} else {
			selection.Pending = append(selection.Pending, candidate)
		}
	}
	return selection, nil
}
