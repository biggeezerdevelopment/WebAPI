// Package spool is a durable first-in, first-out queue of opaque payloads,
// kept in append-only segment files with an atomically replaced ack file. It
// has one reader and any number of writers. Delivery is at least once: a
// crash between a send and Ack sends that frame again.
package spool

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	segmentSuffix = ".seg"
	ackName       = "ack"
	// maxSegmentSize is the largest default segment size.
	maxSegmentSize = 8 << 20
)

var (
	// ErrClosed reports use of a closed spool.
	ErrClosed = errors.New("spool is closed")
	// ErrTooLarge reports a payload that cannot fit within MaxBytes.
	ErrTooLarge = errors.New("record is larger than the spool size limit")
)

// Options configures a spool.
type Options struct {
	// Name labels log messages, normally the output name.
	Name string
	// MaxBytes caps the segments' total size; the oldest are dropped first.
	MaxBytes int64
	// MaxAge drops frames older than this when they are read. 0 means none.
	MaxAge time.Duration
	// SegmentBytes is the size at which a new segment starts. 0 means
	// min(8 MiB, MaxBytes/4).
	SegmentBytes int64
	// Now and Logf default to time.Now and log.Printf.
	Now  func() time.Time
	Logf func(format string, args ...any)
}

// Stats describes a spool.
type Stats struct {
	// Records is the number of frames not yet acked.
	Records int
	// Bytes is the segments' total size on disk.
	Bytes int64
	// Dropped counts frames dropped by a limit or corruption since the last
	// TakeStats.
	Dropped int
}

type segment struct {
	seq  uint64
	size int64
	// frames counts the segment's frames not yet acked or dropped.
	frames int
}

type pendingFrame struct {
	at   position
	next int64
}

// Spool is an open spool directory. Run only one Spool per directory.
type Spool struct {
	dir     string
	opts    Options
	segSize int64

	mu sync.Mutex
	// segs is oldest first. segs[0] holds the read position and the last
	// segment is the one appended to.
	segs    []*segment
	active  *os.File
	reader  *os.File
	readSeq uint64
	pos     position
	pending *pendingFrame
	dropped int
	closed  bool
	// broken is set when a write to the active segment failed, which may
	// have left stray bytes in it; the next Append starts a new segment.
	broken bool
	// writeAckFn saves the read position. It is writeAck except in tests,
	// which replace it to make saves fail on every platform.
	writeAckFn func(dir string, p position) error
	// ackFailing is set while saves of the read position fail.
	ackFailing bool

	wake chan struct{}
	done chan struct{}
}

// Open opens or creates the spool in dir. A torn frame at the end of the
// newest segment, left by a crash, is cut off. An ack file that cannot be
// read, or that does not match the segments, is an error.
func Open(dir string, opts Options) (*Spool, error) {
	if opts.MaxBytes <= 0 {
		return nil, errors.New("spool size limit must be greater than 0")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	segSize := opts.SegmentBytes
	if segSize <= 0 {
		segSize = max(min(int64(maxSegmentSize), opts.MaxBytes/4), 1)
	}
	segSize = min(segSize, opts.MaxBytes)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("spool directory cannot be created")
	}
	s := &Spool{dir: dir, opts: opts, segSize: segSize, writeAckFn: writeAck, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if err := s.load(); err != nil {
		_ = s.closeFiles()
		return nil, err
	}
	return s, nil
}

func (s *Spool) load() error {
	seqs, err := s.listSegments()
	if err != nil {
		return err
	}
	pos, hasAck, err := readAck(filepath.Join(s.dir, ackName))
	if err != nil {
		return err
	}
	if hasAck && len(seqs) > 0 {
		if pos.Seq > seqs[len(seqs)-1] {
			return errors.New("spool ack file points past the end of the spool")
		}
		if pos.Seq < seqs[0] {
			// The segment holding the position was removed before the ack file
			// could be updated, as on a full disk. Everything before the oldest
			// segment was delivered or dropped, so reading starts there.
			pos = position{Seq: seqs[0]}
		} else if !slices.Contains(seqs, pos.Seq) {
			return errors.New("spool ack file points to a missing segment")
		}
	}
	if !hasAck {
		pos = position{Seq: 1}
		if len(seqs) > 0 {
			pos.Seq = seqs[0]
		}
	}
	// A crash after the ack moved to a later segment can leave delivered
	// segments behind.
	for len(seqs) > 0 && seqs[0] < pos.Seq {
		if err := removeFile(s.segPath(seqs[0])); err != nil {
			return errors.New("spool segment cannot be removed")
		}
		seqs = seqs[1:]
	}
	if len(seqs) == 0 {
		if hasAck {
			// Start after the saved position so sequence numbers never go back.
			pos = position{Seq: pos.Seq + 1}
		}
		s.pos = pos
		return s.createSegment(pos.Seq)
	}
	s.pos = pos
	for i, seq := range seqs {
		seg, err := s.loadSegment(seq, i == len(seqs)-1)
		if err != nil {
			return err
		}
		s.segs = append(s.segs, seg)
	}
	f, err := os.OpenFile(s.segPath(seqs[len(seqs)-1]), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return errors.New("spool segment cannot be opened")
	}
	s.active = f
	return nil
}

// loadSegment counts the segment's unread frames. The newest segment is
// verified frame by frame and cut at the first bad one, which is where a
// crash interrupted a write.
func (s *Spool) loadSegment(seq uint64, last bool) (*segment, error) {
	path := s.segPath(seq)
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("spool segment cannot be read")
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errors.New("spool segment cannot be read")
	}
	seg := &segment{seq: seq, size: info.Size()}
	start := int64(0)
	if seq == s.pos.Seq {
		if s.pos.Off > seg.size {
			_ = f.Close()
			return nil, errors.New("spool ack file points past the end of the spool")
		}
		start = s.pos.Off
	}
	frames, end, err := scan(f, start, seg.size, last)
	_ = f.Close() // Windows cannot truncate a file that is open.
	if err != nil {
		return nil, errors.New("spool segment cannot be read")
	}
	seg.frames = frames
	if last && end < seg.size {
		if lost, corrupt := s.middleCorruption(path, end, seg.size); corrupt {
			s.opts.Logf("Spool %s: segment %d is corrupt; dropping its remaining %d record(s)", s.opts.Name, seq, lost)
			s.dropped += lost
		} else {
			s.opts.Logf("Spool %s: removing a partial record at the end of segment %d", s.opts.Name, seq)
		}
		if err := os.Truncate(path, end); err != nil {
			return nil, errors.New("spool segment cannot be repaired")
		}
		seg.size = end
	}
	return seg, nil
}

// middleCorruption reports whether the bad frame at off is damage in the
// middle of the segment rather than a torn tail: its header is complete and
// its declared extent ends before the end of the file. lost counts that frame
// and the readable frame headers after it.
func (s *Spool) middleCorruption(path string, off, size int64) (lost int, corrupt bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	if size-off < headerSize {
		return 0, false
	}
	var b [4]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		return 0, false
	}
	length := int64(binary.LittleEndian.Uint32(b[:]))
	if allZero(f, off, size) {
		return 0, false // preallocated or zero-filled tail after power loss
	}
	next := off + headerSize + length
	if next >= size {
		return 0, false
	}
	after, _, err := scan(f, next, size, false)
	if err != nil {
		after = 0
	}
	return 1 + after, true
}

// allZero reports whether every byte of f from off to size is zero.
func allZero(f *os.File, off, size int64) bool {
	buf := make([]byte, 32<<10)
	for off < size {
		n := int(min(int64(len(buf)), size-off))
		if _, err := f.ReadAt(buf[:n], off); err != nil {
			return false
		}
		for _, c := range buf[:n] {
			if c != 0 {
				return false
			}
		}
		off += int64(n)
	}
	return true
}

func (s *Spool) listSegments() ([]uint64, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, errors.New("spool directory cannot be read")
	}
	var seqs []uint64
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".ack-") {
			_ = os.Remove(filepath.Join(s.dir, name)) // left by a crash mid-write
			continue
		}
		if e.IsDir() || !strings.HasSuffix(name, segmentSuffix) {
			continue
		}
		seq, err := strconv.ParseUint(strings.TrimSuffix(name, segmentSuffix), 10, 64)
		if err != nil || seq == 0 {
			continue
		}
		seqs = append(seqs, seq)
	}
	slices.Sort(seqs)
	return seqs, nil
}

func (s *Spool) segPath(seq uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%016d%s", seq, segmentSuffix))
}

// persist saves p as the read position and notes whether saves are failing.
func (s *Spool) persist(p position) error {
	err := s.writeAckFn(s.dir, p)
	s.ackFailing = err != nil
	return err
}

// persistQuiet is persist for paths that carry on when the save fails: the
// position has already moved in memory, and the ack file on disk stays
// behind it, which Open accepts. Only the first failure of a run is logged.
func (s *Spool) persistQuiet(p position) {
	wasFailing := s.ackFailing
	if err := s.persist(p); err != nil && !wasFailing {
		s.opts.Logf("Spool %s: the read position cannot be saved: %v (further failures are not logged until one succeeds)", s.opts.Name, err)
	}
}

// createSegment starts segment seq and makes it the one appended to.
func (s *Spool) createSegment(seq uint64) error {
	f, err := os.OpenFile(s.segPath(seq), os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("spool segment cannot be created")
	}
	if s.active != nil {
		_ = s.active.Close()
	}
	s.active = f
	s.broken = false
	s.segs = append(s.segs, &segment{seq: seq})
	syncDir(s.dir)
	return nil
}

func (s *Spool) rotate() error {
	return s.createSegment(s.segs[len(s.segs)-1].seq + 1)
}

// Append writes payload durably, then drops the oldest segments while the
// spool is over MaxBytes.
func (s *Spool) Append(payload []byte) error {
	if uint64(len(payload)) > math.MaxUint32 {
		return ErrTooLarge
	}
	frame := encodeFrame(payload, s.opts.Now().UnixNano())
	n := int64(len(frame))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if n > s.opts.MaxBytes {
		return ErrTooLarge
	}
	last := s.segs[len(s.segs)-1]
	if s.broken || (last.size > 0 && last.size+n > s.segSize) {
		if err := s.rotate(); err != nil {
			return err
		}
		last = s.segs[len(s.segs)-1]
	}
	if _, err := s.active.Write(frame); err != nil {
		s.writeFailed(last.seq, last.size)
		return errors.New("spool write failed")
	}
	if err := s.active.Sync(); err != nil {
		s.writeFailed(last.seq, last.size)
		return errors.New("spool write failed")
	}
	last.size += n
	last.frames++
	s.enforceSize()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// writeFailed handles a failed write or sync: the segment may now hold bytes
// beyond size, so no more frames go into it. Cutting it back through the path
// is best effort (it works on Windows, where the append handle cannot); reads
// are capped at the in-memory size either way.
func (s *Spool) writeFailed(seq uint64, size int64) {
	s.broken = true
	_ = os.Truncate(s.segPath(seq), size)
}

func (s *Spool) totalBytes() int64 {
	var total int64
	for _, seg := range s.segs {
		total += seg.size
	}
	return total
}

// enforceSize drops whole segments, oldest first, until the spool is within
// MaxBytes. The read position moves past each one in memory; saving it is
// best effort, because Open accepts an ack file that points at a removed
// segment and starts at the oldest one left.
func (s *Spool) enforceSize() {
	dropped, removed := 0, false
	for s.totalBytes() > s.opts.MaxBytes && len(s.segs) > 1 {
		dropped += s.segs[0].frames
		s.pos = position{Seq: s.segs[1].seq}
		s.pending = nil
		s.removeOldest()
		removed = true
	}
	if removed {
		s.persistQuiet(s.pos)
	}
	if dropped > 0 {
		s.dropped += dropped
		s.opts.Logf("Spool %s: dropped %d record(s) (size limit)", s.opts.Name, dropped)
	}
}

// removeOldest deletes segs[0]. The read position must already be past it.
func (s *Spool) removeOldest() {
	seq := s.segs[0].seq
	if s.reader != nil && s.readSeq == seq {
		_ = s.reader.Close()
		s.reader = nil
	}
	if err := removeFile(s.segPath(seq)); err != nil {
		// The next Open removes it, because it is before the ack.
		s.opts.Logf("Spool %s: segment %d cannot be removed: %v", s.opts.Name, seq, err)
	}
	s.segs = s.segs[1:]
}

// finishSegment moves the read position to the next segment and removes the
// current one, which also frees space on a full disk. Saving the position is
// best effort; see enforceSize.
func (s *Spool) finishSegment() {
	next := position{Seq: s.segs[1].seq}
	s.pos = next
	s.removeOldest()
	s.persistQuiet(next)
}

// Peek returns the oldest frame not yet acked, waiting for one if the spool
// is empty. It returns the same frame until Ack.
func (s *Spool) Peek(ctx context.Context) ([]byte, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, ErrClosed
		}
		payload, ok, err := s.next()
		s.mu.Unlock()
		if err != nil || ok {
			return payload, err
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.done:
			return nil, ErrClosed
		}
	}
}

// next finds the frame at the read position, skipping finished segments,
// corrupt segments and expired frames. ok is false when there is none.
func (s *Spool) next() (payload []byte, ok bool, err error) {
	expired := 0
	defer func() {
		if expired > 0 {
			s.dropped += expired
			s.opts.Logf("Spool %s: dropped %d record(s) (age limit)", s.opts.Name, expired)
		}
	}()
	for {
		seg := s.segs[0]
		if s.pos.Off >= seg.size {
			if len(s.segs) == 1 {
				return nil, false, nil
			}
			s.finishSegment()
			continue
		}
		f, err := s.readerFor(seg.seq)
		if err != nil {
			return nil, false, err
		}
		payload, at, err := readFrame(f, s.pos.Off, seg.size)
		if errors.Is(err, errBadFrame) {
			s.opts.Logf("Spool %s: segment %d is corrupt; dropping its remaining %d record(s)", s.opts.Name, seg.seq, seg.frames)
			s.dropped += seg.frames
			seg.frames = 0
			if len(s.segs) == 1 {
				if err := s.rotate(); err != nil {
					return nil, false, err
				}
			}
			s.finishSegment()
			continue
		}
		if err != nil {
			return nil, false, errors.New("spool segment cannot be read")
		}
		next := s.pos.Off + headerSize + int64(len(payload))
		if s.opts.MaxAge > 0 && s.opts.Now().Sub(time.Unix(0, at)) > s.opts.MaxAge {
			s.pos.Off = next
			seg.frames--
			expired++
			s.persistQuiet(s.pos)
			continue
		}
		s.pending = &pendingFrame{at: s.pos, next: next}
		return payload, true, nil
	}
}

func (s *Spool) readerFor(seq uint64) (*os.File, error) {
	if s.reader != nil && s.readSeq == seq {
		return s.reader, nil
	}
	if s.reader != nil {
		_ = s.reader.Close()
		s.reader = nil
	}
	f, err := os.Open(s.segPath(seq))
	if err != nil {
		return nil, errors.New("spool segment cannot be read")
	}
	s.reader, s.readSeq = f, seq
	return f, nil
}

// Ack marks the frame last returned by Peek as delivered. It does nothing if
// a limit dropped that frame after Peek returned it.
func (s *Spool) Ack() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	p := s.pending
	s.pending = nil
	if p == nil || p.at != s.pos {
		return nil
	}
	// The position advances in memory even if the ack file cannot be written,
	// so a full disk does not make Peek return the same frame forever. The
	// next successful ack write persists the newer position; a crash before
	// then only replays frames already sent, which at-least-once allows.
	// The file on disk never points past memory. It may point at a segment
	// already removed, which Open treats as the start of the oldest segment.
	s.pos = position{Seq: p.at.Seq, Off: p.next}
	s.segs[0].frames--
	return s.persist(s.pos)
}

// TakeStats returns the spool's state and resets Dropped.
func (s *Spool) TakeStats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Dropped: s.dropped}
	s.dropped = 0
	for _, seg := range s.segs {
		st.Records += seg.frames
		st.Bytes += seg.size
	}
	return st
}

// Close closes the spool's files. A blocked Peek returns ErrClosed.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.done)
	return s.closeFiles()
}

func (s *Spool) closeFiles() error {
	var errs []error
	if s.active != nil {
		errs = append(errs, s.active.Close())
		s.active = nil
	}
	if s.reader != nil {
		errs = append(errs, s.reader.Close())
		s.reader = nil
	}
	return errors.Join(errs...)
}
