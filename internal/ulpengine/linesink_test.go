package ulpengine

// writeLine writes a single line through the batch path. Production sinks only
// expose writeBatch/writeBatchIndexed (multi-worker callers always batch); this
// is a test-only convenience for the few tests that write one line at a time.
func writeLine(s lineSink, line string, m *Metrics) error {
	return s.writeBatch([]byte(line+"\n"), 1, m)
}
