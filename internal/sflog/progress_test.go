package sflog

import "testing"

func TestProgressWorkerRegistryTracksActiveSlots(t *testing.T) {
	p := NewProgress()
	if got := p.WorkerCount(); got != 0 {
		t.Fatalf("WorkerCount before SetWorkers = %d, want 0", got)
	}
	p.SetWorkers(3)
	if got := p.WorkerCount(); got != 3 {
		t.Fatalf("WorkerCount = %d, want 3", got)
	}

	// Two busy slots (0 and 2), one idle (1).
	p.setActive(0, "/data/a.zip", StageTestingPassword)
	p.setActive(2, "/data/loose/Passwords.txt", StageParsing)

	active := p.ActiveWorkers(8)
	if len(active) != 2 {
		t.Fatalf("ActiveWorkers = %d, want 2 (%+v)", len(active), active)
	}
	// Lowest index first.
	if active[0].Index != 0 || active[0].Path != "/data/a.zip" || active[0].Stage != StageTestingPassword {
		t.Fatalf("active[0] = %+v", active[0])
	}
	if active[1].Index != 2 || active[1].Stage != StageParsing {
		t.Fatalf("active[1] = %+v", active[1])
	}

	// Stage update without touching the path.
	p.setStage(0, StageExtracting)
	if got := p.ActiveWorkers(8)[0].Stage; got != StageExtracting {
		t.Fatalf("after setStage, stage = %v, want extracting", got)
	}

	// Clearing a slot drops it from the snapshot.
	p.clearActive(0)
	active = p.ActiveWorkers(8)
	if len(active) != 1 || active[0].Index != 2 {
		t.Fatalf("after clearActive(0), active = %+v", active)
	}
}

func TestProgressActiveWorkersHonorsMax(t *testing.T) {
	p := NewProgress()
	p.SetWorkers(5)
	for i := 0; i < 5; i++ {
		p.setActive(i, "/x", StageExtracting)
	}
	if got := len(p.ActiveWorkers(2)); got != 2 {
		t.Fatalf("ActiveWorkers(2) = %d, want 2", got)
	}
	if got := p.ActiveWorkers(0); got != nil {
		t.Fatalf("ActiveWorkers(0) = %v, want nil", got)
	}
}

func TestProgressWorkerRegistryNilAndBoundsSafe(t *testing.T) {
	var p *Progress
	// nil receiver paths must not panic.
	p.SetWorkers(4)
	p.setActive(0, "x", StageOpening)
	p.clearActive(0)
	if got := p.WorkerCount(); got != 0 {
		t.Fatalf("nil WorkerCount = %d, want 0", got)
	}
	if got := p.ActiveWorkers(4); got != nil {
		t.Fatalf("nil ActiveWorkers = %v, want nil", got)
	}

	// Out-of-range indices on a sized registry are no-ops, not panics.
	q := NewProgress()
	q.SetWorkers(2)
	q.setActive(7, "oob", StageParsing)
	q.setStage(-1, StageParsing)
	if got := len(q.ActiveWorkers(8)); got != 0 {
		t.Fatalf("out-of-range writes leaked into registry: %d active", got)
	}
}

func TestWorkerStageString(t *testing.T) {
	cases := map[WorkerStage]string{
		StageIdle:            "",
		StageOpening:         "opening",
		StageTestingPassword: "testing password",
		StageExtracting:      "extracting",
		StageParsing:         "parsing",
	}
	for stage, want := range cases {
		if got := stage.String(); got != want {
			t.Fatalf("WorkerStage(%d).String() = %q, want %q", stage, got, want)
		}
	}
}
