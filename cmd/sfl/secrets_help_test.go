//go:build secrets

package main

import (
	"strings"
	"testing"
)

// Counterpart to TestRenderHelpHidesSecretsWhenDisabled: in a -tags secrets
// build the secret-scanning flags must appear in -h.
func TestRenderHelpShowsSecretsWhenEnabled(t *testing.T) {
	if !secretsEnabled {
		t.Skip("secrets not compiled in")
	}
	help := renderHelp("sfl")
	for _, s := range []string{"-secrets", "-secrets-path", "-secrets-allow", "-secrets-deny"} {
		if !strings.Contains(help, s) {
			t.Fatalf("secrets-build help is missing %q\n\n%s", s, help)
		}
	}
}
