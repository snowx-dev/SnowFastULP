package main

import (
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// comboOf extracts the login:password half of one U:L:P hit line using the
// shared engine grammar (strict, the library default — the same accept class
// ingest uses). ok=false when the line is not a credential record; such hits
// are never emitted in -combo mode (they would pollute a combo list) and are
// counted for the end-of-run note instead.
func comboOf(line string) (string, bool) {
	_, _, login, password, ok := ulpengine.ParseLine(line, false)
	if !ok {
		return "", false
	}
	return login + ":" + password, true
}

// comboOfStored extracts login:password from a trusted stored-record hit.
// Archive/library views use the stored decoder; raw -txt input stays strict.
func comboOfStored(line string) (string, bool) {
	_, _, login, password, ok := ulpengine.ParseStoredLine(line)
	if !ok {
		return "", false
	}
	return login + ":" + password, true
}
