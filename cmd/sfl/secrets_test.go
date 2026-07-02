package main

import "testing"

func TestResolveSecretsPathDefaults(t *testing.T) {
	if got := resolveSecretsPath("", "/out", ""); got != "/out/sfl-secrets.sqlite" {
		t.Fatalf("output-dir default = %q", got)
	}
	if got := resolveSecretsPath("", "", "/lib"); got != "/lib/sfl-secrets.sqlite" {
		t.Fatalf("library-dir default = %q", got)
	}
	if got := resolveSecretsPath("/x/custom.db", "/out", ""); got != "/x/custom.db" {
		t.Fatalf("explicit path = %q", got)
	}
	if got := resolveSecretsPath("", "", ""); got != secretsDBName {
		t.Fatalf("cwd fallback = %q", got)
	}
}
