package search

// Index subphase counters for the indexing TUI. The four methods are thin
// wrappers over the active-frame/active-decode atomics; the TUI's indexPhase
// label reads those atomics directly.
func (m *Metrics) BeginFrameScan() {
	if m == nil {
		return
	}
	m.IndexFrameScanActive.Add(1)
}

func (m *Metrics) EndFrameScan() {
	if m == nil {
		return
	}
	m.IndexFrameScanActive.Add(-1)
}

func (m *Metrics) BeginDecode() {
	if m == nil {
		return
	}
	m.IndexDecodeActive.Add(1)
}

func (m *Metrics) EndDecode() {
	if m == nil {
		return
	}
	m.IndexDecodeActive.Add(-1)
}
