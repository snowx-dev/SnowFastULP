package zstdframe

import (
	"context"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/snowx-dev/SnowFastULP/internal/zstdlimits"
)

const discardReadSize = 32 * 1024

// decoderDicts holds raw dictionaries registered for every decoder this
// package (and the search workers, via RegisteredDecoderDicts) creates.
// Archives whose frames carry a dictionary ID can only be decoded when the
// matching dictionary is registered; dict-free frames are unaffected.
var (
	decoderDictsMu sync.RWMutex
	decoderDicts   []decoderDict
)

type decoderDict struct {
	id      uint32
	content []byte
}

// RegisterDecoderDict registers a raw zstd dictionary (id + content) used by
// every decoder created by this package and by the search workers. Content
// must be at least 8 bytes (zstd minimum). Registered dictionaries apply to
// frames whose dictionary ID matches; all other frames decode unchanged.
func RegisterDecoderDict(id uint32, content []byte) {
	decoderDictsMu.Lock()
	defer decoderDictsMu.Unlock()
	decoderDicts = append(decoderDicts, decoderDict{id: id, content: content})
}

// RegisteredDecoderDicts returns a snapshot of registered dictionaries for
// callers that build their own decoders (the search workers).
func RegisteredDecoderDicts() (ids []uint32, contents [][]byte) {
	decoderDictsMu.RLock()
	defer decoderDictsMu.RUnlock()
	for _, d := range decoderDicts {
		ids = append(ids, d.id)
		contents = append(contents, d.content)
	}
	return ids, contents
}

type discardDecoder struct {
	dec *zstd.Decoder
}

func newDecoder() (*discardDecoder, error) {
	opts := []zstd.DOption{zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(zstdlimits.MaxDecoderWindow)}
	decoderDictsMu.RLock()
	for _, d := range decoderDicts {
		opts = append(opts, zstd.WithDecoderDictRaw(d.id, d.content))
	}
	decoderDictsMu.RUnlock()
	dec, err := zstd.NewReader(nil, opts...)
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
