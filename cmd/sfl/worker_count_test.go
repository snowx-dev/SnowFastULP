package main

import "testing"

func TestResolveWorkerCountScalesWithCores(t *testing.T) {
	cases := []struct {
		name      string
		flag, cpu int
		want      int
	}{
		{"auto uses all cores, no cap at 8", 0, 16, 16},
		{"auto on small box", 0, 4, 4},
		{"explicit flag wins over cores", 3, 16, 3},
		{"floor at 1 when cores unknown", 0, 0, 1},
		{"explicit 1 honored", 1, 32, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveWorkerCount(tc.flag, tc.cpu); got != tc.want {
				t.Fatalf("resolveWorkerCount(%d, %d) = %d, want %d", tc.flag, tc.cpu, got, tc.want)
			}
		})
	}
}
