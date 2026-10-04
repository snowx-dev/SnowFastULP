package ulpengine

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// cgroup mem limit in bytes, 0 if none. handles v1+v2.
// needed b/c /proc/meminfo reports host RAM not the container cap
func readCgroupMemLimit() uint64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	v2Path, v1Path := parseCgroupSelf(data)

	if v2Path != "" {
		if v, ok := readCgroupUint(filepath.Join("/sys/fs/cgroup", v2Path, "memory.max")); ok {
			return v
		}
	}
	if v1Path != "" {
		if v, ok := readCgroupUint(filepath.Join("/sys/fs/cgroup/memory", v1Path, "memory.limit_in_bytes")); ok {
			return v
		}
	}
	return 0
}

// returns (v2 unified path, v1 memory controller path), either may be empty.
// v2 line: 0::/path. v1 line: id:controllers:/path
func parseCgroupSelf(data []byte) (v2, v1 string) {
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		hierarchyID, controllers, path := parts[0], parts[1], parts[2]
		if hierarchyID == "0" && controllers == "" {
			v2 = path
			continue
		}
		for _, c := range strings.Split(controllers, ",") {
			if c == "memory" {
				v1 = path
			}
		}
	}
	return v2, v1
}

// reads uint from cgroup file. "max", empty, 0, or >1 PiB means "no limit"
func readCgroupUint(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	if s == "" || s == "max" {
		return 0, false
	}
	var v uint64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0, false
	}
	const sentinelUnlimited = uint64(1) << 50 // 1 PiB, cgroup v1 reports ~9.2 EiB for unlimited
	if v == 0 || v > sentinelUnlimited {
		return 0, false
	}
	return v, true
}
