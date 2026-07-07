package config_test

import (
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func TestResolveIntAliasExplicitCanonicalWins(t *testing.T) {
	canonical, alias := 8, 4
	v := config.Visited{"workers": true, "j": true}
	v.ResolveIntAlias(&canonical, &alias, "workers", "j")
	if canonical != 8 {
		t.Fatalf("explicit canonical should win: got %d, want 8", canonical)
	}
}

func TestResolveIntAliasCopiesAliasWhenOnlyAliasSet(t *testing.T) {
	canonical, alias := 0, 4
	v := config.Visited{"j": true}
	v.ResolveIntAlias(&canonical, &alias, "workers", "j")
	if canonical != 4 {
		t.Fatalf("alias value should fill canonical: got %d, want 4", canonical)
	}
	if !v["workers"] {
		t.Fatalf("canonical must be marked visited so it beats config-file values")
	}
}

func TestResolveIntAliasNoAliasNoChange(t *testing.T) {
	canonical, alias := 0, 0
	v := config.Visited{}
	v.ResolveIntAlias(&canonical, &alias, "workers", "j")
	if canonical != 0 || v["workers"] {
		t.Fatalf("no flag set: canonical=%d visited=%v, want 0/false", canonical, v["workers"])
	}
}
