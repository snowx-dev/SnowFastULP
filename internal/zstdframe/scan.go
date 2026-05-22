package zstdframe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
)

const (
	ZstdMagic    uint32 = 0xFD2FB528
	SkippableMin uint32 = 0x184D2A50
	SkippableMax uint32 = 0x184D2A5F
)

// Frame describes one searchable zstd frame in an archive.
type Frame struct {
	ChunkID           int
	CompressedOffset  int64
	CompressedSize    int64
	UncompressedStart int64
	UncompressedEnd   int64
}

// Progress is called during scan with bytes processed and total file size.
type Progress func(bytesDone, bytesTotal int64)

// ScanFile finds all zstd frames in path, skipping skippable frames.
func ScanFile(ctx context.Context, path string, prog Progress, act *Activity) ([]Frame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if reg := fileabort.FromContext(ctx); reg != nil {
		unreg := reg.Register(f)
		defer unreg()
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size == 0 {
		return nil, nil
	}
	if prog != nil {
		prog(0, size)
	}

	var frames []Frame
	var uncomp int64
	offset := int64(0)
	chunkID := 0

	for offset < size {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		magic, err := readU32LE(f, offset)
		if err != nil {
			return nil, fmt.Errorf("read magic at %d: %w", offset, err)
		}

		if isSkippable(magic) {
			userSize, err := readU32LE(f, offset+4)
			if err != nil {
				return nil, fmt.Errorf("read skippable size at %d: %w", offset, err)
			}
			skip := int64(8 + userSize)
			if skip <= 0 || offset+skip > size {
				return nil, fmt.Errorf("invalid skippable frame at %d", offset)
			}
			offset += skip
			continue
		}

		if magic != ZstdMagic {
			return nil, fmt.Errorf("unknown magic 0x%08X at offset %d", magic, offset)
		}

		act.beginFrameScan()
		frameEnd, err := findZstdFrameEnd(ctx, f, size, offset, prog)
		act.endFrameScan()
		if err != nil {
			return nil, fmt.Errorf("frame at %d: %w", offset, err)
		}
		compSize := frameEnd - offset
		if compSize <= 0 {
			return nil, fmt.Errorf("empty frame at %d", offset)
		}
		if prog != nil {
			prog(offset, size)
		}

		act.beginDecode()
		uncompSize, err := measureUncompressedSize(ctx, f, offset, compSize, size, prog)
		act.endDecode()
		if err != nil {
			return nil, fmt.Errorf("decode frame at %d: %w", offset, err)
		}

		frames = append(frames, Frame{
			ChunkID:           chunkID,
			CompressedOffset:  offset,
			CompressedSize:    compSize,
			UncompressedStart: uncomp,
			UncompressedEnd:   uncomp + uncompSize,
		})
		uncomp += uncompSize
		offset = frameEnd
		chunkID++
		if prog != nil {
			prog(offset, size)
		}
	}

	return frames, nil
}

func isSkippable(magic uint32) bool {
	return magic >= SkippableMin && magic <= SkippableMax
}

func readU32LE(r io.ReaderAt, off int64) (uint32, error) {
	var buf [4]byte
	if _, err := r.ReadAt(buf[:], off); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf[:]), nil
}

const frameScanCancelInterval = 256

func findZstdFrameEnd(ctx context.Context, r io.ReaderAt, fileSize, frameStart int64, prog Progress) (int64, error) {
	if frameStart+4 > fileSize {
		return 0, errors.New("truncated frame magic")
	}

	headerLen, hasChecksum, err := zstdFrameHeaderLen(r, frameStart+4, fileSize)
	if err != nil {
		return 0, err
	}

	pos := frameStart + 4 + int64(headerLen)
	iter := 0
	reportProgress := func() {
		if prog != nil {
			prog(pos, fileSize)
		}
	}
	for {
		if ctx != nil && iter%frameScanCancelInterval == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			reportProgress()
		}
		iter++

		if pos+3 > fileSize {
			return 0, errors.New("truncated block header")
		}
		var hdr [3]byte
		if _, err := r.ReadAt(hdr[:], pos); err != nil {
			return 0, err
		}
		blockHeader := uint32(hdr[0]) | uint32(hdr[1])<<8 | uint32(hdr[2])<<16
		lastBlock := blockHeader & 1
		blockType := (blockHeader >> 1) & 0x03
		blockSize := int64((blockHeader >> 3) & 0x1FFFFF)

		pos += 3
		switch blockType {
		case 0, 2: // Raw or Compressed
			pos += blockSize
		case 1: // RLE
			pos++
		default:
			return 0, fmt.Errorf("reserved block type %d at %d", blockType, pos-3)
		}
		if pos > fileSize {
			return 0, errors.New("block extends past EOF")
		}
		if lastBlock != 0 {
			if hasChecksum {
				pos += 4
			}
			if pos > fileSize {
				return 0, errors.New("checksum extends past EOF")
			}
			reportProgress()
			return pos, nil
		}
	}
}

func zstdFrameHeaderLen(r io.ReaderAt, off, fileSize int64) (headerLen int, hasChecksum bool, err error) {
	if off >= fileSize {
		return 0, false, errors.New("truncated frame header")
	}
	var fhd [1]byte
	if _, err := r.ReadAt(fhd[:], off); err != nil {
		return 0, false, err
	}
	pos := 1
	hasChecksum = (fhd[0]>>2)&1 != 0
	dictFlag := fhd[0] & 0x03
	switch dictFlag {
	case 1:
		pos++
	case 2, 3:
		pos += 4
	}
	singleSegment := (fhd[0] >> 5) & 1
	fcsFlag := fhd[0] >> 6

	if singleSegment == 0 {
		pos++ // Window Descriptor
	}
	if singleSegment == 1 || fcsFlag != 0 {
		switch fcsFlag {
		case 0:
			if singleSegment == 1 {
				pos++ // 1-byte FCS
			}
		case 1:
			pos += 2
		case 2:
			pos += 4
		case 3:
			pos += 8
		}
	}
	if off+int64(pos) > fileSize {
		return 0, false, errors.New("truncated frame header fields")
	}
	return pos, hasChecksum, nil
}

func measureUncompressedSize(ctx context.Context, r io.ReaderAt, offset, compSize, fileSize int64, prog Progress) (int64, error) {
	section := io.NewSectionReader(r, offset, compSize)
	var src io.Reader = section
	if prog != nil {
		src = &compressedProgressReader{
			r:        section,
			base:     offset,
			fileSize: fileSize,
			prog:     prog,
		}
	}
	src = readerWithContext(ctx, src)
	dec, err := newDecoder()
	if err != nil {
		return 0, err
	}
	defer dec.Close()

	n, err := dec.CopyDiscard(ctx, src)
	if err != nil {
		return 0, err
	}
	if prog != nil {
		end := offset + compSize
		if end > fileSize {
			end = fileSize
		}
		prog(end, fileSize)
	}
	return n, nil
}
