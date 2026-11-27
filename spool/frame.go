package spool

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
)

// headerSize is the frame header: uint32 payload length, uint32 CRC-32C of
// the timestamp and payload, int64 append time in unix nanoseconds.
const headerSize = 16

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// errBadFrame reports a frame that is incomplete or fails its CRC.
var errBadFrame = errors.New("bad frame")

type header struct {
	length  int64
	crc     uint32
	atNanos int64
}

// encodeFrame returns the whole frame in one buffer so one write appends it.
func encodeFrame(payload []byte, atNanos int64) []byte {
	buf := make([]byte, headerSize+len(payload))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(atNanos))
	copy(buf[headerSize:], payload)
	binary.LittleEndian.PutUint32(buf[4:8], crc32.Checksum(buf[8:], castagnoli))
	return buf
}

// readHeader reads the header at off. size is the segment's known size; a
// header or payload that would run past it is a bad frame.
func readHeader(f *os.File, off, size int64) (header, error) {
	if size-off < headerSize {
		return header{}, errBadFrame
	}
	var b [headerSize]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		return header{}, err
	}
	h := header{
		length:  int64(binary.LittleEndian.Uint32(b[0:4])),
		crc:     binary.LittleEndian.Uint32(b[4:8]),
		atNanos: int64(binary.LittleEndian.Uint64(b[8:16])),
	}
	if h.length > size-off-headerSize {
		return header{}, errBadFrame
	}
	return h, nil
}

// readFrame reads and verifies the frame at off, returning its payload and
// append time.
func readFrame(f *os.File, off, size int64) ([]byte, int64, error) {
	h, err := readHeader(f, off, size)
	if err != nil {
		return nil, 0, err
	}
	buf := make([]byte, headerSize+h.length)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, 0, err
	}
	if crc32.Checksum(buf[8:], castagnoli) != h.crc {
		return nil, 0, errBadFrame
	}
	return buf[headerSize:], h.atNanos, nil
}

// scan counts whole frames from off. With verify it reads every frame and
// checks its CRC; otherwise it follows headers only, which is cheap for large
// older segments. end is where the first bad frame starts, or size.
func scan(f *os.File, off, size int64, verify bool) (frames int, end int64, err error) {
	for off < size {
		var length int64
		if verify {
			payload, _, rerr := readFrame(f, off, size)
			if errors.Is(rerr, errBadFrame) {
				return frames, off, nil
			}
			if rerr != nil {
				return 0, 0, rerr
			}
			length = int64(len(payload))
		} else {
			h, herr := readHeader(f, off, size)
			if errors.Is(herr, errBadFrame) {
				return frames, off, nil
			}
			if herr != nil {
				return 0, 0, herr
			}
			length = h.length
		}
		frames++
		off += headerSize + length
	}
	return frames, off, nil
}
