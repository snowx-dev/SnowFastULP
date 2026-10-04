package sflog

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// newStyleRarVolume matches RAR5 / new-style multi-volume names of the form
// "<prefix>.partN.rar" (case-insensitive). Group 1 is the shared prefix, group
// 2 is the volume number.
var newStyleRarVolume = regexp.MustCompile(`(?i)^(.*)\.part(\d+)\.rar$`)

// oldStyleRarPart matches old-style multi-volume RAR continuation names of the
// form "<name>.rNN" (case-insensitive, two or more digits: .r00, .r01, ...).
// Group 1 is the shared base name; the set's leading volume is the plain
// "<name>.rar" sibling. Unlike new-style parts, .rNN extensions never satisfy
// isArchiveFile, so discovery must admit them explicitly or they would be
// dropped and only volume 1 would ever be read.
var oldStyleRarPart = regexp.MustCompile(`(?i)^(.*)\.r(\d{2,})$`)

// oldStyleRarRole classifies a path within an old-style multi-volume RAR set:
// isPart when it matches the ".rNN" continuation scheme, and setKey = directory
// + shared base so the "<name>.rar" first volume and its continuations group.
func oldStyleRarRole(path string) (isPart bool, setKey string) {
	m := oldStyleRarPart.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return false, ""
	}
	return true, filepath.Join(filepath.Dir(path), m[1])
}

// isOldStyleRarPart reports whether path is an old-style RAR continuation file.
// Used by discovery so .rNN parts (whose extension is not a recognised archive
// ext) are still enqueued and grouped rather than dropped.
func isOldStyleRarPart(path string) bool {
	isPart, _ := oldStyleRarRole(path)
	return isPart
}

// oldStyleRarSet returns the ordered on-disk volumes of the old-style set that
// `first` belongs to ("<name>.rar" first, then "<name>.rNN" continuations by
// ascending number) by scanning its directory. It returns nil when `first` is
// not a plain .rar / continuation with at least one .rNN sibling, so callers
// stay on their fast path for single-volume archives. New-style part names are
// never treated as old-style first volumes.
func oldStyleRarSet(first string) []string {
	// A continuation named directly: its set head is the "<name>.rar" sibling.
	if isPart, key := oldStyleRarRole(first); isPart {
		if key == "" {
			return nil
		}
		return oldStyleRarSet(filepath.Join(filepath.Dir(first), filepath.Base(key)+".rar"))
	}
	if isVol, _, _ := rarVolumeRole(first); isVol {
		return nil // new-style part; old-style pairing does not apply
	}
	base := filepath.Base(first)
	if !strings.EqualFold(filepath.Ext(base), ".rar") {
		return nil
	}
	key := filepath.Join(filepath.Dir(first), base[:len(base)-4])
	dir := filepath.Dir(first)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type part struct {
		n int
		p string
	}
	var got []part
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if ip, k := oldStyleRarRole(p); ip && k == key {
			m := oldStyleRarPart.FindStringSubmatch(e.Name())
			n, _ := strconv.Atoi(m[2])
			got = append(got, part{n: n, p: p})
		}
	}
	if len(got) == 0 {
		return nil
	}
	sort.Slice(got, func(i, j int) bool { return got[i].n < got[j].n })
	out := make([]string, 0, len(got)+1)
	out = append(out, first)
	for _, v := range got {
		out = append(out, v.p)
	}
	return out
}

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

// splitPartName matches a raw byte-split archive part of the form
// "<logical>.<zip|7z>.NNN" (case-insensitive, 2-3 digit index). Group 1 is the
// logical archive name (e.g. "foo.zip"), group 2 the part number. These are
// produced by 7z's -v split and the `split` tool: concatenating the parts in
// order reproduces the original single archive.
var splitPartName = regexp.MustCompile(`(?i)^(.*\.(?:zip|7z))\.(\d{2,3})$`)

// isSplitArchivePart reports whether path is any part of a raw byte-split
// archive set. Used by discovery so split parts (whose extension is .NNN, not a
// recognised archive ext) are still enqueued and grouped rather than dropped.
func isSplitArchivePart(path string) bool {
	isPart, _, _ := splitPartRole(path)
	return isPart
}

// splitPartRole classifies a path within a raw byte-split archive set, mirroring
// rarVolumeRole: isPart when it matches the ".NNN" scheme, isFirst when the
// index is 1, and setKey = directory + logical name so sibling parts group.
func splitPartRole(path string) (isPart, isFirst bool, setKey string) {
	m := splitPartName.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return false, false, ""
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return false, false, ""
	}
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

// splitPartSet returns the ordered on-disk parts of the split set `first`
// belongs to and whether the set is complete (parts numbered 1..N with no gap).
// An incomplete set must not be opened (the concatenated reader would have a
// hole), so callers report it as a missing-volume skip. When `first` is not a
// recognised first part it returns ([]string{first}, false).
func splitPartSet(first string) (parts []string, complete bool) {
	isPart, isFirst, setKey := splitPartRole(first)
	if !isPart || !isFirst {
		return []string{first}, false
	}
	dir := filepath.Dir(first)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{first}, false
	}
	type part struct {
		n int
		p string
	}
	var got []part
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		ip, _, k := splitPartRole(p)
		if !ip || k != setKey {
			continue
		}
		m := splitPartName.FindStringSubmatch(e.Name())
		n, _ := strconv.Atoi(m[2])
		got = append(got, part{n: n, p: p})
	}
	if len(got) == 0 {
		return []string{first}, false
	}
	sort.Slice(got, func(i, j int) bool { return got[i].n < got[j].n })
	out := make([]string, len(got))
	complete = true
	for i, v := range got {
		if v.n != i+1 { // gap in the 1..N sequence
			complete = false
		}
		out[i] = v.p
	}
	return out, complete
}

// groupArchiveVolumes turns a flat list of discovered archive paths into work
// items, collapsing multi-part sets so each set is processed once via its first
// part. Three schemes are recognised, all mapped onto workItem.volumes:
//   - new-style multi-volume RAR (".partN.rar"), read via rardecode.OpenReader
//     (assemblyRarVolumes);
//   - old-style multi-volume RAR ("<name>.rar" + "<name>.rNN" continuations),
//     also read via rardecode.OpenReader (assemblyRarVolumes): an io.Reader has
//     no pathname, so the set must go through the pathname-based reader;
//   - raw byte-split archives (".zip.NNN" / ".7z.NNN"), read via a concatenated
//     reader (assemblySplitParts).
//
// For either scheme: the first part becomes one item carrying the ordered set
// with weight = sum of parts; continuation parts whose first part is present are
// dropped; an orphaned continuation (no first part) or an incomplete split set
// (gap in 1..N) becomes a single missing-volume skip so the gap is reported, not
// silently lost. Every other archive (plain .rar/.zip/.7z) becomes a normal
// single item. weightOf and keyOf are injected so volumes.go stays decoupled
// from fileWeight/logGroupKey.
func groupArchiveVolumes(archives []string, weightOf func(string) int64, keyOf func(string) string) []workItem {
	type numbered struct {
		n int
		p string
	}
	// Group every discovered multi-part file by its set key in a single pass
	// over the in-memory list, instead of re-listing the directory per set.
	// Discovery enqueues all parts of both schemes (".partN.rar" satisfies the
	// archive filter; split ".NNN" parts are added via isSplitArchivePart), so
	// this list is the complete set — and on an RDP/SMB share it avoids K extra
	// directory round-trips.
	rarSets := map[string][]numbered{}
	splitSets := map[string][]numbered{}
	oldSets := map[string][]numbered{}
	rarFirst := map[string]bool{}
	splitFirst := map[string]bool{}
	oldFirst := map[string]string{} // old-style set key -> "<name>.rar" first volume
	for _, a := range archives {
		if isVol, isFirst, key := rarVolumeRole(a); isVol {
			m := newStyleRarVolume.FindStringSubmatch(filepath.Base(a))
			n, _ := strconv.Atoi(m[2])
			rarSets[key] = append(rarSets[key], numbered{n, a})
			if isFirst {
				rarFirst[key] = true
			}
			continue
		}
		if isPart, isFirst, key := splitPartRole(a); isPart {
			m := splitPartName.FindStringSubmatch(filepath.Base(a))
			n, _ := strconv.Atoi(m[2])
			splitSets[key] = append(splitSets[key], numbered{n, a})
			if isFirst {
				splitFirst[key] = true
			}
			continue
		}
		if isPart, key := oldStyleRarRole(a); isPart {
			m := oldStyleRarPart.FindStringSubmatch(filepath.Base(a))
			n, _ := strconv.Atoi(m[2])
			oldSets[key] = append(oldSets[key], numbered{n, a})
			continue
		}
		// A plain .rar is the first volume of an old-style set when "<name>.rNN"
		// continuations share its base; pair them by the same key.
		if base := filepath.Base(a); strings.EqualFold(filepath.Ext(base), ".rar") {
			if _, isVol, _ := rarVolumeRole(a); !isVol {
				oldFirst[filepath.Join(filepath.Dir(a), base[:len(base)-4])] = a
			}
		}
	}
	// Order each set's parts by ascending number once, up front.
	orderParts := func(sets map[string][]numbered) {
		for k := range sets {
			v := sets[k]
			sort.Slice(v, func(i, j int) bool { return v[i].n < v[j].n })
			sets[k] = v
		}
	}
	orderParts(rarSets)
	orderParts(splitSets)
	orderParts(oldSets)

	pathsOf := func(v []numbered) []string {
		out := make([]string, len(v))
		for i := range v {
			out[i] = v[i].p
		}
		return out
	}
	// contiguous reports whether the ordered parts cover 1..N with no gap, so an
	// incomplete split set (a hole in the concatenated reader) can be skipped.
	contiguous := func(v []numbered) bool {
		for i := range v {
			if v[i].n != i+1 {
				return false
			}
		}
		return true
	}
	sumWeight := func(parts []string) int64 {
		var w int64
		for _, p := range parts {
			w += weightOf(p)
		}
		return w
	}

	var items []workItem
	rarOrphan := map[string]bool{}
	splitOrphan := map[string]bool{}
	oldOrphan := map[string]bool{}
	for _, a := range archives {
		if isVol, isFirst, key := rarVolumeRole(a); isVol {
			switch {
			case isFirst:
				set := pathsOf(rarSets[key])
				items = append(items, workItem{path: a, kind: kindArchive, weight: sumWeight(set), logKey: keyOf(a), volumes: set, assembly: assemblyRarVolumes})
			case rarFirst[key]:
				// covered by its first volume.
			case !rarOrphan[key]:
				rarOrphan[key] = true
				items = append(items, workItem{path: a, kind: kindArchive, weight: weightOf(a), logKey: keyOf(a), missingFirstVolume: true})
			}
			continue
		}
		if isPart, isFirst, key := splitPartRole(a); isPart {
			switch {
			case isFirst:
				parts := splitSets[key]
				set := pathsOf(parts)
				if !contiguous(parts) {
					items = append(items, workItem{path: a, kind: kindArchive, weight: sumWeight(set), logKey: keyOf(a), missingFirstVolume: true})
					continue
				}
				items = append(items, workItem{path: a, kind: kindArchive, weight: sumWeight(set), logKey: keyOf(a), volumes: set, assembly: assemblySplitParts})
			case splitFirst[key]:
				// covered by its first part.
			case !splitOrphan[key]:
				splitOrphan[key] = true
				items = append(items, workItem{path: a, kind: kindArchive, weight: weightOf(a), logKey: keyOf(a), missingFirstVolume: true})
			}
			continue
		}
		if isPart, key := oldStyleRarRole(a); isPart {
			if _, has := oldFirst[key]; has {
				continue // covered by its first volume
			}
			if !oldOrphan[key] {
				oldOrphan[key] = true
				items = append(items, workItem{path: a, kind: kindArchive, weight: weightOf(a), logKey: keyOf(a), missingFirstVolume: true})
			}
			continue
		}
		// A plain .rar with .rNN continuations is an old-style set head: one item
		// carrying the ordered set (leading .rar first) read via OpenReader.
		if strings.EqualFold(filepath.Ext(a), ".rar") {
			if _, isVol, _ := rarVolumeRole(a); !isVol {
				if key := filepath.Join(filepath.Dir(a), filepath.Base(a)[:len(filepath.Base(a))-4]); len(oldSets[key]) > 0 {
					set := append([]string{a}, pathsOf(oldSets[key])...)
					items = append(items, workItem{path: a, kind: kindArchive, weight: sumWeight(set), logKey: keyOf(a), volumes: set, assembly: assemblyRarVolumes})
					continue
				}
			}
		}
		items = append(items, workItem{path: a, kind: kindArchive, weight: weightOf(a), logKey: keyOf(a)})
	}
	return items
}

// VolumeSet exposes the on-disk parts of a multi-part set to callers outside
// the package (e.g. -del, which removes every part together when deleting a
// first part under the input root). It handles both rar volume sets and raw
// byte-split sets; for a non-multi-part path it returns just that path.
func VolumeSet(first string) []string {
	if isPart, _, _ := splitPartRole(first); isPart {
		parts, _ := splitPartSet(first)
		return parts
	}
	if set := oldStyleRarSet(first); set != nil {
		return set
	}
	return rarVolumeSet(first)
}
