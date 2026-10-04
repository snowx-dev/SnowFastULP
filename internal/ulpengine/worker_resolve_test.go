package ulpengine

import "testing"

func TestResolveParserWorkersScalesWithCores(t *testing.T) {
	cases := []struct {
		name      string
		flag, cpu int
		want      int
	}{
		{"auto uses all cores, no cap at 8", 0, 16, 16},
		{"auto on small box", 0, 4, 4},
		{"explicit flag wins", 6, 16, 6},
		{"floor at 1", 0, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveParserWorkers(tc.flag, tc.cpu); got != tc.want {
				t.Fatalf("resolveParserWorkers(%d, %d) = %d, want %d", tc.flag, tc.cpu, got, tc.want)
			}
		})
	}
}

func TestResolveDedupWorkersHalfCoresNoFixedCeiling(t *testing.T) {
	cases := []struct {
		name      string
		flag, cpu int
		want      int
	}{
		{"half of 16 cores, beyond old cap of 4", 0, 16, 8},
		{"half of 4 cores", 0, 4, 2},
		{"floor at 1 on single core", 0, 1, 1},
		{"explicit flag wins", 3, 16, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveDedupWorkers(tc.flag, tc.cpu); got != tc.want {
				t.Fatalf("resolveDedupWorkers(%d, %d) = %d, want %d", tc.flag, tc.cpu, got, tc.want)
			}
		})
	}
}
