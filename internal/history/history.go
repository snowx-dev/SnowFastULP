package history

import (
	"context"
	"os"
)

// Identity content-addresses a processed source: the truncated xxhash of its
// bytes plus its size. Two sources with equal identities are interchangeable
// by design — a source whose content matches a completed source is a hit and
// is skipped, and recording an identity marks every same-content source
// complete, regardless of path or file name.
type Identity struct {
	Hash uint64
	Size int64
}

type Snapshot struct {
	Path string
	info os.FileInfo
}

type Candidate struct {
	ID        Identity
	Paths     []string
	Snapshots []Snapshot
	Assembly  string
}

type ProgressFunc func(path string, bytesDone, bytesTotal int64)

type Store interface {
	Lookup(context.Context, []Identity) (map[Identity]struct{}, error)
	Record(context.Context, []Identity) error
	Close() error
}
