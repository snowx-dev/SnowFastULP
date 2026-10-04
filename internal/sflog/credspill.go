package sflog

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// credSinkMemCap bounds the in-memory head of a credSink. Records beyond the
// cap spill to a temp file, so a dense credential member (hundreds of
// thousands of one-line records) costs O(credSinkMemCap) memory instead of
// O(records), while small members — the common case — never touch disk.
const credSinkMemCap = 8 << 10

// credSink collects Credential values with bounded memory: a small in-memory
// head, then a length-prefixed temp-file spill. Records are replayed in
// insertion order via replay (so merge points keep emit order deterministic),
// and discard removes the spill file. A sticky write error surfaces from add
// and replay. Source is constant per sink and stamped on replay.
type credSink struct {
	tempDir string
	source  string

	mem      []Credential
	memBytes int64
	count    int

	f       *os.File
	bw      *bufio.Writer
	buf     []byte
	spilled bool
	err     error
}

func newCredSink(tempDir, source string) *credSink {
	return &credSink{tempDir: tempDir, source: source}
}

func (s *credSink) add(c Credential) error {
	if s.err != nil {
		return s.err
	}
	s.count++
	if s.f == nil && s.memBytes+int64(len(c.URL)+len(c.Username)+len(c.Password)) > credSinkMemCap {
		if err := s.spill(); err != nil {
			return err
		}
	}
	if s.f != nil {
		s.buf = binary.BigEndian.AppendUint32(s.buf, uint32(len(c.URL)))
		s.buf = binary.BigEndian.AppendUint32(s.buf, uint32(len(c.Username)))
		s.buf = binary.BigEndian.AppendUint32(s.buf, uint32(len(c.Password)))
		s.buf = append(s.buf, c.URL...)
		s.buf = append(s.buf, c.Username...)
		s.buf = append(s.buf, c.Password...)
		if _, err := s.bw.Write(s.buf); err != nil {
			s.err = err
			return err
		}
		s.buf = s.buf[:0]
		return nil
	}
	s.mem = append(s.mem, c)
	s.memBytes += int64(len(c.URL) + len(c.Username) + len(c.Password) + 64)
	return nil
}

// spill flushes the in-memory head to a temp file; later adds append there.
func (s *credSink) spill() error {
	dir := s.tempDir
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, "sfl-creds-*")
	if err != nil {
		s.err = err
		return err
	}
	s.f = f
	s.spilled = true
	s.bw = bufio.NewWriter(f)
	for _, c := range s.mem {
		s.buf = binary.BigEndian.AppendUint32(s.buf, uint32(len(c.URL)))
		s.buf = binary.BigEndian.AppendUint32(s.buf, uint32(len(c.Username)))
		s.buf = binary.BigEndian.AppendUint32(s.buf, uint32(len(c.Password)))
		s.buf = append(s.buf, c.URL...)
		s.buf = append(s.buf, c.Username...)
		s.buf = append(s.buf, c.Password...)
		if _, err := s.bw.Write(s.buf); err != nil {
			s.err = err
			return err
		}
		s.buf = s.buf[:0]
	}
	s.mem = nil
	s.memBytes = 0
	return nil
}

// replay emits every record in insertion order. The sink stays usable (the
// spill file is removed by discard, not replay) so a merge point can replay
// and the owner still cleans up on early exits.
func (s *credSink) replay(emit func(Credential) error) error {
	if s == nil {
		return nil
	}
	if s.err != nil {
		return s.err
	}
	if s.f != nil {
		if err := s.bw.Flush(); err != nil {
			s.err = err
			return err
		}
	}
	for i := range s.mem {
		s.mem[i].Source = s.source
		if err := emit(s.mem[i]); err != nil {
			return err
		}
	}
	if s.f == nil {
		return nil
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var (
		hdr [12]byte
		c   Credential
	)
	r := bufio.NewReader(s.f)
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		c.Source = s.source
		url := make([]byte, binary.BigEndian.Uint32(hdr[0:4]))
		user := make([]byte, binary.BigEndian.Uint32(hdr[4:8]))
		pass := make([]byte, binary.BigEndian.Uint32(hdr[8:12]))
		if _, err := io.ReadFull(r, url); err != nil {
			return err
		}
		if _, err := io.ReadFull(r, user); err != nil {
			return err
		}
		if _, err := io.ReadFull(r, pass); err != nil {
			return err
		}
		c.URL, c.Username, c.Password = string(url), string(user), string(pass)
		if err := emit(c); err != nil {
			return err
		}
	}
}

func (s *credSink) discard() {
	if s == nil {
		return
	}
	if s.f != nil {
		_ = s.bw.Flush()
		_ = s.f.Close()
		_ = os.Remove(s.f.Name())
		s.f = nil
	}
	s.mem = nil
}

// found reports whether any record was collected.
func (s *credSink) found() bool {
	return len(s.mem) > 0 || s.spilled
}

// len reports the number of collected records.
func (s *credSink) len() int {
	return s.count
}
