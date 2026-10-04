package sflog

import "testing"

func TestCheckingHistoryProgress(t *testing.T) {
	p := NewProgress()
	p.BeginHistory(100)
	p.addHistoryBytes(40)
	if p.Phase() != phaseHistory || p.HistoryBytesDone() != 40 || p.HistoryBytesTotal() != 100 {
		t.Fatalf("phase=%d done=%d total=%d", p.Phase(), p.HistoryBytesDone(), p.HistoryBytesTotal())
	}
}
