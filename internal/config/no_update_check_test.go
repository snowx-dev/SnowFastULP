package config_test

import (
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

// D3: the -no-update-check help points at the per-tool no_update_check config
// key as the persistent silencer — so the key must actually exist and pull
// for [sfu] and [sfs] the way it already does for [sfl].
func TestApplySFUAndSFSNoUpdateCheckPull(t *testing.T) {
	f := writeConfig(t, "[sfu]\nno_update_check = true\n\n[sfs]\nno_update_check = true\n")
	sfuOff := false
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{NoUpdateCheck: &sfuOff}); err != nil {
		t.Fatal(err)
	}
	if !sfuOff {
		t.Fatal("[sfu] no_update_check = true must disable the check")
	}
	sfsOff := false
	if err := f.ApplySFS(config.Visited{}, config.SFSFlags{NoUpdateCheck: &sfsOff}); err != nil {
		t.Fatal(err)
	}
	if !sfsOff {
		t.Fatal("[sfs] no_update_check = true must disable the check")
	}
}

// An explicit CLI flag wins over the config key, even -no-update-check=false.
func TestApplySFUAndSFSNoUpdateCheckCLIWins(t *testing.T) {
	f := writeConfig(t, "[sfu]\nno_update_check = true\n\n[sfs]\nno_update_check = true\n")
	sfuOff := false
	if err := f.ApplySFU(config.Visited{"no-update-check": true}, config.SFUFlags{NoUpdateCheck: &sfuOff}); err != nil {
		t.Fatal(err)
	}
	if sfuOff {
		t.Fatal("explicit -no-update-check must suppress the [sfu] pull")
	}
	sfsOff := false
	if err := f.ApplySFS(config.Visited{"no-update-check": true}, config.SFSFlags{NoUpdateCheck: &sfsOff}); err != nil {
		t.Fatal(err)
	}
	if sfsOff {
		t.Fatal("explicit -no-update-check must suppress the [sfs] pull")
	}
}
