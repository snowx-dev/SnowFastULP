package selfupdate

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// releasePlatforms lists the asset suffixes every published release must
// ship, mirroring assetSuffix. The manifest generator refuses to emit a
// manifest whose assets map is missing any of these (per hardcoded-trio
// prefix), so a release can never advertise an update it cannot serve.
var releasePlatforms = []string{"linux-amd64", "macos-arm64", "windows-amd64.exe", "android-arm64"}

// ParseSHA256SUMS parses the SHA256SUMS artifact a release ships (the output
// of sha256sum: "<64 lowercase hex chars>  <name>", two spaces for text mode
// or space-asterisk for binary mode) into an asset-name → digest map.
func ParseSHA256SUMS(r io.Reader) (map[string]string, error) {
	sums := map[string]string{}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		var digest, name string
		switch {
		case len(line) > 66 && (line[64:66] == "  " || line[64:66] == " *"):
			digest, name = line[:64], line[66:]
		case len(line) == 66 && line[64:66] == "  ":
			return nil, fmt.Errorf("SHA256SUMS line has an empty file name: %q", line)
		default:
			return nil, fmt.Errorf("SHA256SUMS line is not \"<sha256>  <name>\": %q", line)
		}
		if _, err := hex.DecodeString(digest); err != nil || len(digest) != 64 {
			return nil, fmt.Errorf("SHA256SUMS entry for %q has an invalid sha256 digest %q", name, digest)
		}
		if _, dup := sums[name]; dup {
			return nil, fmt.Errorf("SHA256SUMS lists %q twice", name)
		}
		sums[name] = digest
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("SHA256SUMS is empty")
	}
	return sums, nil
}

// ReleaseNotesURL returns the notes_url for a release: the flat file
// (update-notes-<version>.md) the release owner mirrors next to update-manifest.json on
// the same host — latestURL, the very constant the manifest endpoint itself
// uses, so URL and host can never drift.
func ReleaseNotesURL(version string) string {
	return latestURL + "update-notes-" + version + ".md"
}

// ManifestOptions carries the optional manifest extensions. The zero value
// emits exactly the pre-notes manifest bytes: the fields are appended in
// struct order and omitempty drops them when unset, so the existing
// deterministic output is preserved byte for byte.
type ManifestOptions struct {
	NotesURL    string
	NotesSHA256 string
}

// BuildUpdateManifest renders the update-manifest JSON served from
// sfu-update.snowx.dev for a release without optional extensions.
func BuildUpdateManifest(version string, sums map[string]string) ([]byte, error) {
	return BuildUpdateManifestWithOptions(version, sums, ManifestOptions{})
}

// BuildUpdateManifestWithOptions renders the update-manifest JSON served
// from sfu-update.snowx.dev for a release. sums maps release-asset names to
// their lowercase hex sha256 — the exact digests from the release's
// SHA256SUMS artifact, so the manifest can never disagree with the shipped
// assets.
//
// The output decodes through fetchLatest's updateManifest schema: version,
// the hardcoded bin trio (consumers union manifest bins onto it, so the trio
// is always declared), and an assets map keyed by release-asset name. Every
// trio-prefix × releasePlatforms key must be present or this fails — a
// partial manifest would make existing installs on the missing platform
// refuse to update. Extra sums entries (the binaries zip) pass through as
// advisory entries consumers ignore. The map marshals with sorted keys and
// the bins keep products order, so output is deterministic. When opts sets
// the release-notes pair, both fields must be present and the hash a valid
// 32-byte hex digest; they are emitted last, after the assets map.
func BuildUpdateManifestWithOptions(version string, sums map[string]string, opts ManifestOptions) ([]byte, error) {
	if strings.TrimSpace(version) == "" {
		return nil, fmt.Errorf("update manifest needs a non-empty version")
	}
	for _, p := range products {
		for _, suffix := range releasePlatforms {
			assetName := fmt.Sprintf("%s-%s-%s", p.prefix, version, suffix)
			if _, ok := sums[assetName]; !ok {
				return nil, fmt.Errorf("update manifest: SHA256SUMS has no entry for required asset %q", assetName)
			}
		}
	}
	manifest := updateManifest{
		Version: version,
		Bins:    make([]manifestBin, len(products)),
		Assets:  make(map[string]manifestAsset, len(sums)),
	}
	for i, p := range products {
		manifest.Bins[i] = manifestBin{Name: p.bin, Prefix: p.prefix}
	}
	for name, digest := range sums {
		manifest.Assets[name] = manifestAsset{SHA256: digest}
	}
	if opts.NotesURL != "" || opts.NotesSHA256 != "" {
		if opts.NotesURL == "" || opts.NotesSHA256 == "" {
			return nil, fmt.Errorf("update manifest: notes_url and notes_sha256 must be set together")
		}
		if _, err := parseManifestHash(opts.NotesSHA256, "update notes"); err != nil {
			return nil, err
		}
		manifest.NotesURL = opts.NotesURL
		manifest.NotesSHA256 = opts.NotesSHA256
	}
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode update manifest: %w", err)
	}
	return append(out, '\n'), nil
}
