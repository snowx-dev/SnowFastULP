package config_test

import (
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func TestApplyBellPull(t *testing.T) {
	f := writeConfig(t, "[sfu]\nbell = true\n\n[sfs]\nbell = true\n\n[sfl]\nbell = true\n")
	sfuOn, sfsOn, sflOn := false, false, false
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{Bell: &sfuOn}); err != nil {
		t.Fatal(err)
	}
	if !sfuOn {
		t.Fatal("[sfu] bell = true must enable -bell")
	}
	if err := f.ApplySFS(config.Visited{}, config.SFSFlags{Bell: &sfsOn}); err != nil {
		t.Fatal(err)
	}
	if !sfsOn {
		t.Fatal("[sfs] bell = true must enable -bell")
	}
	if err := f.ApplySFL(config.Visited{}, config.SFLFlags{Bell: &sflOn}); err != nil {
		t.Fatal(err)
	}
	if !sflOn {
		t.Fatal("[sfl] bell = true must enable -bell")
	}
}

func TestApplyBellCLIWins(t *testing.T) {
	f := writeConfig(t, "[sfu]\nbell = true\n\n[sfs]\nbell = true\n\n[sfl]\nbell = true\n")
	sfuOn, sfsOn, sflOn := false, false, false
	if err := f.ApplySFU(config.Visited{"bell": true}, config.SFUFlags{Bell: &sfuOn}); err != nil {
		t.Fatal(err)
	}
	if sfuOn {
		t.Fatal("explicit -bell must suppress the [sfu] pull")
	}
	if err := f.ApplySFS(config.Visited{"bell": true}, config.SFSFlags{Bell: &sfsOn}); err != nil {
		t.Fatal(err)
	}
	if sfsOn {
		t.Fatal("explicit -bell must suppress the [sfs] pull")
	}
	if err := f.ApplySFL(config.Visited{"bell": true}, config.SFLFlags{Bell: &sflOn}); err != nil {
		t.Fatal(err)
	}
	if sflOn {
		t.Fatal("explicit -bell must suppress the [sfl] pull")
	}
}
