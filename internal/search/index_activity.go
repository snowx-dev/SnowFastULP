package search

// Index subphase counters for the indexing TUI.
func (m *Metrics) BeginFrameScan(archiveName string) {
	if m == nil {
		return
	}
	m.IndexFrameScanActive.Add(1)
	m.indexFocusMu.Lock()
	m.indexFocusName = archiveName
	m.indexFocusMu.Unlock()
}

func (m *Metrics) EndFrameScan(archiveName string) {
	if m == nil {
		return
	}
	m.IndexFrameScanActive.Add(-1)
	m.indexFocusMu.Lock()
	if m.indexFocusName == archiveName {
		m.indexFocusName = ""
	}
	m.indexFocusMu.Unlock()
}

func (m *Metrics) BeginDecode(archiveName string) {
	if m == nil {
		return
	}
	m.IndexDecodeActive.Add(1)
	m.indexFocusMu.Lock()
	if m.indexFocusName == "" {
		m.indexFocusName = archiveName
	}
	m.indexFocusMu.Unlock()
}

func (m *Metrics) EndDecode(archiveName string) {
	if m == nil {
		return
	}
	m.IndexDecodeActive.Add(-1)
}

func (m *Metrics) IndexFocusName() string {
	if m == nil {
		return ""
	}
	m.indexFocusMu.Lock()
	defer m.indexFocusMu.Unlock()
	return m.indexFocusName
}
