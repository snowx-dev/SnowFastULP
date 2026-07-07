package main

import (
	"strings"
	"testing"
)

func TestRenderHelpMatchesSnowFastLayout(t *testing.T) {
	help := renderHelp("sfl")

	want := []string{
		"SnowFastLog",
		"Usage:",
		"Examples:",
		"Args:",
		"Args (for nerds):",
		"Args (for devs):",
		"-o DIR",
		"Write extracted ULP lines to this folder.",
		"-od DIR",
		"Ingest extracted ULP lines into an existing sfu library.",
		"-p PASSWORD_OR_FILE",
		"Archive password or password-list file.",
		"-no-uri",
		"Save only host:login:password.",
		"-workers N",
		"Set parser/archive worker count.",
		"-debug",
		"Write a debug log for this run.",
	}
	for _, s := range want {
		if !strings.Contains(help, s) {
			t.Fatalf("help is missing %q\n\n%s", s, help)
		}
	}
	if strings.Contains(help, "Flags:") {
		t.Fatalf("help should not use flag.PrintDefaults layout\n\n%s", help)
	}
}

// Default build has no secret-scanning support compiled in, so -h must not
// advertise any -secrets* flag. The secrets-build counterpart lives in
// secrets_help_test.go (//go:build secrets).
func TestRenderHelpHidesSecretsWhenDisabled(t *testing.T) {
	if secretsEnabled {
		t.Skip("secrets compiled in; covered by secrets_help_test.go")
	}
	help := renderHelp("sfl")
	for _, s := range []string{"-secrets", "-secrets-path", "-sec-path", "-secrets-allow", "-secrets-deny"} {
		if strings.Contains(help, s) {
			t.Fatalf("default-build help advertises %q\n\n%s", s, help)
		}
	}
}
