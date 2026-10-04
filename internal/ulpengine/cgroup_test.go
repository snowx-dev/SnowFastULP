package ulpengine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCgroupSelfV2(t *testing.T) {
	in := []byte("0::/user.slice/session-3.scope\n")
	v2, v1 := parseCgroupSelf(in)
	if v2 != "/user.slice/session-3.scope" {
		t.Errorf("v2 path = %q", v2)
	}
	if v1 != "" {
		t.Errorf("v1 path should be empty, got %q", v1)
	}
}

func TestParseCgroupSelfV1Combined(t *testing.T) {
	in := []byte("12:cpuacct,memory,pids:/docker/abcd\n11:cpu:/docker/abcd\n")
	v2, v1 := parseCgroupSelf(in)
	if v1 != "/docker/abcd" {
		t.Errorf("v1 memory path = %q", v1)
	}
	if v2 != "" {
		t.Errorf("v2 should be empty for v1-only host")
	}
}

func TestParseCgroupSelfHybrid(t *testing.T) {
	in := []byte("0::/system.slice/foo.service\n12:memory:/system.slice/foo.service\n")
	v2, v1 := parseCgroupSelf(in)
	if v2 == "" || v1 == "" {
		t.Errorf("hybrid: expected both, got v2=%q v1=%q", v2, v1)
	}
}

func TestReadCgroupUintHandlesMaxAndSentinel(t *testing.T) {
	d := t.TempDir()
	maxFile := filepath.Join(d, "max")
	if err := writeText(maxFile, "max\n"); err != nil {
		t.Fatal(err)
	}
	if v, ok := readCgroupUint(maxFile); ok {
		t.Errorf("`max` should be unbounded, got v=%d ok=true", v)
	}

	sentinelFile := filepath.Join(d, "sentinel")
	if err := writeText(sentinelFile, "9223372036854771712\n"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCgroupUint(sentinelFile); ok {
		t.Errorf("v1 'no limit' sentinel should be treated as unbounded")
	}

	realFile := filepath.Join(d, "real")
	if err := writeText(realFile, "4294967296\n"); err != nil {
		t.Fatal(err)
	}
	v, ok := readCgroupUint(realFile)
	if !ok || v != 4_294_967_296 {
		t.Errorf("real value parse: ok=%v v=%d", ok, v)
	}

	if _, ok := readCgroupUint(filepath.Join(d, "missing")); ok {
		t.Errorf("missing file should return ok=false")
	}
}

func TestEffectiveAvailableTakesMin(t *testing.T) {
	cases := []struct {
		name string
		in   memInfo
		want uint64
	}{
		{"both unknown", memInfo{}, 0},
		{"only meminfo", memInfo{available: 8 << 30}, 8 << 30},
		{"only cgroup", memInfo{cgroupLimit: 4 << 30}, 4 << 30},
		{"cgroup smaller wins", memInfo{available: 32 << 30, cgroupLimit: 4 << 30}, 4 << 30},
		{"meminfo smaller wins", memInfo{available: 4 << 30, cgroupLimit: 32 << 30}, 4 << 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.effectiveAvailable(); got != c.want {
				t.Errorf("got %d want %d", got, c.want)
			}
		})
	}
}

func writeText(path, s string) error {
	return os.WriteFile(path, []byte(s), 0o644)
}
