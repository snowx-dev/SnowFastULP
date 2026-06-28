package sflog

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// newStyleRarVolume matches RAR5 / new-style multi-volume names of the form
// "<prefix>.partN.rar" (case-insensitive). Group 1 is the shared prefix, group
// 2 is the volume number. Old-style continuation files ("<name>.r00", ".r01",
// ...) never satisfy isArchiveFile, so only the leading "<name>.rar" is ever
// enqueued and they need no grouping here.
var newStyleRarVolume = regexp.MustCompile(`(?i)^(.*)\.part(\d+)\.rar$`)

// rarVolumeRole classifies a path within a new-style multi-volume RAR set:
//   - isVolume: the name follows the ".partN.rar" scheme.
//   - isFirst:  it is volume 1 (".part1.rar" / ".part01.rar").
//   - setKey:   directory + shared prefix identifying the set, so sibling
//     volumes group together. Empty when the name is not a volume.
func rarVolumeRole(path string) (isVolume, isFirst bool, setKey string) {
	m := newStyleRarVolume.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return false, false, ""
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return false, false, ""
	}
	// Keep the prefix's original case (Linux paths are case-sensitive) but join
	// with the directory so two identically named sets in different folders stay
	// distinct.
	return true, n == 1, filepath.Join(filepath.Dir(path), m[1])
}

// rarVolumeSet returns the ordered on-disk volumes of the set that `first`
// belongs to (part1, part2, ... by ascending number) by scanning its
// directory. When `first` is not a recognised first volume, it returns
// []string{first} unchanged so single-archive callers stay on their fast path.
func rarVolumeSet(first string) []string {
	isVol, isFirst, setKey := rarVolumeRole(first)
	if !isVol || !isFirst {
		return []string{first}
	}
	dir := filepath.Dir(first)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{first}
	}
	type vol struct {
		n int
		p string
	}
	var vols []vol
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		iv, _, k := rarVolumeRole(p)
		if !iv || k != setKey {
			continue
		}
		m := newStyleRarVolume.FindStringSubmatch(e.Name())
		n, _ := strconv.Atoi(m[2])
		vols = append(vols, vol{n: n, p: p})
	}
	if len(vols) == 0 {
		return []string{first}
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].n < vols[j].n })
	out := make([]string, len(vols))
	for i, v := range vols {
		out[i] = v.p
	}
	return out
}

// groupRarVolumes turns a flat list of discovered archive paths into work
// items, collapsing new-style multi-volume RAR sets so each set is processed
// once via its first volume:
//   - a first volume (".part1.rar") becomes one archive item carrying the whole
//     ordered set in Volumes, with weight equal to the sum of the parts;
//   - continuation volumes whose first part is present are dropped (covered by
//     the first volume);
//   - continuation volumes with no first part present become a single
//     missing-volume skip item per orphaned set, so the gap is reported instead
//     of silently lost;
//   - every other archive (plain .rar/.zip/.7z) becomes a normal item.
//
// weightOf and keyOf are injected so the engine can reuse fileWeight/logGroupKey
// without volumes.go depending on their call sites.
func groupRarVolumes(archives []string, weightOf func(string) int64, keyOf func(string) string) []workItem {
	// Which sets have their first volume present.
	firstPresent := map[string]bool{}
	for _, a := range archives {
		if isVol, isFirst, key := rarVolumeRole(a); isVol && isFirst {
			firstPresent[key] = true
		}
	}

	var items []workItem
	orphanSeen := map[string]bool{} // one missing-volume item per orphaned set
	for _, a := range archives {
		isVol, isFirst, key := rarVolumeRole(a)
		switch {
		case !isVol:
			items = append(items, workItem{path: a, kind: kindArchive, weight: weightOf(a), logKey: keyOf(a)})
		case isFirst:
			set := rarVolumeSet(a)
			var weight int64
			for _, v := range set {
				weight += weightOf(v)
			}
			items = append(items, workItem{path: a, kind: kindArchive, weight: weight, logKey: keyOf(a), volumes: set})
		case firstPresent[key]:
			// covered by its first volume; skip enqueuing.
		default:
			if orphanSeen[key] {
				continue
			}
			orphanSeen[key] = true
			items = append(items, workItem{path: a, kind: kindArchive, weight: weightOf(a), logKey: keyOf(a), missingFirstVolume: true})
		}
	}
	return items
}

// RarVolumeSet exposes rarVolumeSet to callers outside the package (e.g. -del,
// which must remove every part of a multi-volume set together when deleting a
// first volume that sits directly under the input root).
func RarVolumeSet(first string) []string { return rarVolumeSet(first) }
