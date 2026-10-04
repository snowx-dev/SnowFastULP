// Command genmanifest renders the SnowFastULP update manifest that the
// selfupdate consumers fetch from https://sfu-update.snowx.dev/.
//
// Usage:
//
//	genmanifest VERSION SHA256SUMS [NOTES.md]
//
// It parses the release's SHA256SUMS artifact (the same digests attached to
// the GitHub release) and writes the manifest JSON to stdout, so the
// manifest's checksums can never disagree with the shipped assets. The
// `make manifest` target wraps this: it reads dist/SHA256SUMS and the
// dist/update-notes-<version>.md extraction and writes
// dist/update-manifest.json.
//
// With NOTES.md given, the file's sha256 is recorded and the manifest points
// clients at update-notes-<version>.md on the update host
// (selfupdate.ReleaseNotesURL), so `update` can render the release notes as
// its exit message.
//
// Publishing contract: the release workflow attaches update-manifest.json to
// the draft release (after every other asset; the notes file it references
// attaches last). After the release is published, the release owner mirrors
// that exact file — plus the notes file — to https://sfu-update.snowx.dev/ —
// the manifest endpoint is hosted outside this repo and is never written by
// CI.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
)

func main() {
	if len(os.Args) != 3 && len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: genmanifest VERSION SHA256SUMS [NOTES.md]")
		os.Exit(2)
	}
	version, sumsPath := os.Args[1], os.Args[2]

	f, err := os.Open(sumsPath)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	sums, err := selfupdate.ParseSHA256SUMS(f)
	if err != nil {
		fatal(err)
	}
	opts := selfupdate.ManifestOptions{}
	if len(os.Args) == 4 {
		notes, err := os.ReadFile(os.Args[3])
		if err != nil {
			fatal(err)
		}
		sum := sha256.Sum256(notes)
		opts.NotesURL = selfupdate.ReleaseNotesURL(version)
		opts.NotesSHA256 = hex.EncodeToString(sum[:])
	}
	out, err := selfupdate.BuildUpdateManifestWithOptions(version, sums, opts)
	if err != nil {
		fatal(err)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "genmanifest: %v\n", err)
	os.Exit(1)
}
