package ulpengine

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// host mem totals in bytes. 0 = unknown, callers fall back via effectiveAvailable
type memInfo struct {
	total       uint64 // host MemTotal
	available   uint64 // host MemAvailable
	cgroupLimit uint64 // 0 = unbounded
}

// linux only, zeros elsewhere
func readMemInfo() memInfo {
	if runtime.GOOS != "linux" {
		return memInfo{}
	}
	info := memInfo{}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		info.total, info.available, _ = parseMemInfoBytes(data)
	}
	info.cgroupLimit = readCgroupMemLimit()
	return info
}

// min(MemAvailable, cgroup limit). 0 if both unknown
func (m memInfo) effectiveAvailable() uint64 {
	a := m.available
	if m.cgroupLimit > 0 && (a == 0 || m.cgroupLimit < a) {
		a = m.cgroupLimit
	}
	return a
}

func parseMemInfoBytes(data []byte) (memTotal, memAvailable uint64, err error) {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			var kb uint64
			if _, e := fmt.Sscanf(line, "MemTotal: %d kB", &kb); e != nil {
				return 0, 0, fmt.Errorf("parse MemTotal: %w", e)
			}
			memTotal = kb * 1024
		}
		if strings.HasPrefix(line, "MemAvailable:") {
			var kb uint64
			if _, e := fmt.Sscanf(line, "MemAvailable: %d kB", &kb); e != nil {
				return 0, 0, fmt.Errorf("parse MemAvailable: %w", e)
			}
			memAvailable = kb * 1024
		}
	}
	return memTotal, memAvailable, nil
}

func nextPow2(n uint64) uint64 {
	if n <= 1 {
		return 1
	}
	p := uint64(1)
	for p < n {
		p <<= 1
	}
	return p
}

// clamps B under fd cap while keeping mask-and hot path
func largestPow2AtMost(n int) int {
	if n <= 0 {
		return 0
	}
	p := 1
	for p<<1 <= n {
		p <<= 1
	}
	return p
}

// picks B (pow2 in [minB, maxB]) targeting worst-case per-bucket ≤ half
// of MemAvailable across M workers.
// auxKeyBytes > 0 forces B large enough to keep -od per-worker dest-set
// state bounded regardless of host RAM (else 5B keys + 32GiB host → B=64
// → 625 MB per worker = 6+ GB resident).
func chooseBucketCount(inputBytes, auxKeyBytes int64, mem memInfo, dedupWorkers int, minB, maxB int) int {
	if inputBytes <= 0 {
		return minB
	}
	if dedupWorkers < 1 {
		dedupWorkers = 1
	}
	const targetPerBucket = int64(256 << 20) // 256 MiB worst case
	perBucket := targetPerBucket
	if avail := mem.effectiveAvailable(); avail > 0 {
		budget := int64(avail / 2)
		if cand := budget / int64(dedupWorkers); cand > targetPerBucket {
			perBucket = cand
		}
	}
	desired := uint64((inputBytes + perBucket - 1) / perBucket)

	// -od dest-set budget: 128 MiB/bucket for the library key-set alone, the
	// seen map and IO buffers share the rest. Bumped from 64 MiB to spend a
	// bit more RAM for fewer/bigger buckets (snappier dedup, fewer per-bucket
	// gather passes). 10B keys → 128 MiB/bucket → B ≥ 640 → 1024 after pow2.
	if auxKeyBytes > 0 {
		const destSetTargetPerBucket = int64(128 << 20)
		auxDesired := uint64((auxKeyBytes + destSetTargetPerBucket - 1) / destSetTargetPerBucket)
		if auxDesired > desired {
			desired = auxDesired
		}
	}

	if desired < uint64(minB) {
		desired = uint64(minB)
	}
	b := int(nextPow2(desired))
	if b < minB {
		b = minB
	}
	if b > maxB {
		b = maxB
	}
	return b
}
