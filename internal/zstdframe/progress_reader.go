package zstdframe

import "io"

type compressedProgressReader struct {
	r        io.Reader
	base     int64
	fileSize int64
	read     int64
	prog     Progress
}

func (c *compressedProgressReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && c.prog != nil {
		c.read += int64(n)
		done := c.base + c.read
		if done > c.fileSize {
			done = c.fileSize
		}
		c.prog(done, c.fileSize)
	}
	return n, err
}
