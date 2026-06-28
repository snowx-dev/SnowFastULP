package sflog

import (
	"fmt"
	"io"
	"os"
)

// multiPartReaderAt presents the in-order concatenation of several files as one
// io.ReaderAt, without copying them into a single temp file. Raw byte-split
// archives (".zip.NNN" / ".7z.NNN") are exactly this: parts that, joined, form
// the original archive. The zip/7z readers only touch the central directory
// (tail of the last part) plus the members they actually read, so a large set
// is read by random access rather than reassembled on disk.
//
// All part handles are opened up front (the part count is tiny) and *os.File's
// ReadAt is safe for concurrent use at independent offsets, so the reader needs
// no locking of its own.
type multiPartReaderAt struct {
	files  []*os.File
	starts []int64 // global start offset of each part
	sizes  []int64 // size of each part
	size   int64   // total size
}

// openMultiPartReaderAt opens every part in order and computes the cumulative
// offset map. On any error it closes whatever it already opened.
func openMultiPartReaderAt(parts []string) (*multiPartReaderAt, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("multipart: no parts")
	}
	m := &multiPartReaderAt{
		files:  make([]*os.File, 0, len(parts)),
		starts: make([]int64, 0, len(parts)),
		sizes:  make([]int64, 0, len(parts)),
	}
	for _, p := range parts {
		f, err := os.Open(p)
		if err != nil {
			_ = m.Close()
			return nil, err
		}
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			_ = m.Close()
			return nil, err
		}
		m.files = append(m.files, f)
		m.starts = append(m.starts, m.size)
		m.sizes = append(m.sizes, fi.Size())
		m.size += fi.Size()
	}
	return m, nil
}

func (m *multiPartReaderAt) Size() int64 { return m.size }

// ReadAt satisfies io.ReaderAt across the part boundaries: a read that spans two
// parts is served from each in turn. It returns io.EOF only when the requested
// range reaches the true end of the last part, matching the ReaderAt contract
// (a short read is always accompanied by a non-nil error).
func (m *multiPartReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("multipart: negative offset")
	}
	if off >= m.size {
		return 0, io.EOF
	}
	var n int
	for n < len(p) {
		if off >= m.size {
			return n, io.EOF
		}
		i := m.partIndex(off)
		local := off - m.starts[i]
		want := len(p) - n
		if avail := m.sizes[i] - local; int64(want) > avail {
			want = int(avail)
		}
		got, err := m.files[i].ReadAt(p[n:n+want], local)
		n += got
		off += int64(got)
		if err != nil && !(err == io.EOF && got == want) {
			return n, err
		}
	}
	return n, nil
}

// partIndex returns the index of the part that contains global offset off
// (assumes 0 <= off < m.size).
func (m *multiPartReaderAt) partIndex(off int64) int {
	// Linear scan: the part count is tiny, so a binary search buys nothing.
	for i := len(m.starts) - 1; i >= 0; i-- {
		if off >= m.starts[i] {
			return i
		}
	}
	return 0
}

func (m *multiPartReaderAt) Close() error {
	var firstErr error
	for _, f := range m.files {
		if f == nil {
			continue
		}
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
