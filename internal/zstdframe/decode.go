package zstdframe

import (
	"context"
	"io"

	"github.com/klauspost/compress/zstd"
)

const discardReadSize = 32 * 1024

// maxDecoderWindow caps the zstd decompression window for untrusted archives.
// See internal/search for rationale; 128 MiB covers sfu output and `zstd --long`
// while bounding the per-decoder allocation a crafted frame can force.
const maxDecoderWindow = 128 << 20

type discardDecoder struct {
	dec *zstd.Decoder
}

func newDecoder() (*discardDecoder, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(maxDecoderWindow))
	if err != nil {
		return nil, err
	}
	return &discardDecoder{dec: dec}, nil
}

func (d *discardDecoder) Close() {
	if d.dec != nil {
		d.dec.Close()
	}
}

func (d *discardDecoder) CopyDiscard(ctx context.Context, r io.Reader) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d.dec.Reset(r)
	buf := make([]byte, discardReadSize)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		nr, err := d.dec.Read(buf)
		n += int64(nr)
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}
