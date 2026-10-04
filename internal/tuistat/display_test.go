package tuistat

import "testing"

func TestShowLiveDisplay(t *testing.T) {
	cases := []struct {
		name       string
		noTUI      bool
		jsonTarget string
		stderrTTY  bool
		stdoutTTY  bool
		vtOK       bool
		want       bool
	}{
		{"plain terminal", false, "", true, true, true, true},
		{"no-tui", true, "", true, true, true, false},
		{"no-tui wins over a file stream", true, "run.jsonl", true, true, true, false},
		{"file stream keeps the frame", false, "run.jsonl", true, true, true, true},
		{"stdout stream on a terminal", false, "-", true, true, true, false},
		{"stdout stream piped, stderr still a tty", false, "-", true, false, true, true},
		{"stderr not a tty", false, "run.jsonl", false, true, true, false},
		{"vt unavailable", false, "", true, true, false, false},
		{"json off is empty, not dash", false, "", true, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShowLiveDisplay(tc.noTUI, tc.jsonTarget, tc.stderrTTY, tc.stdoutTTY, tc.vtOK)
			if got != tc.want {
				t.Fatalf("ShowLiveDisplay() = %v, want %v", got, tc.want)
			}
		})
	}
}
