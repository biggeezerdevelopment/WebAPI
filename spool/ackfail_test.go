package spool

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ackSwitch makes a spool's ack writes fail while on is true.
type ackSwitch struct{ on atomic.Bool }

// failAcks makes s's ack writes fail until the returned switch is turned off.
// It works on every platform, unlike directory permissions.
func failAcks(s *Spool) *ackSwitch {
	sw := &ackSwitch{}
	sw.on.Store(true)
	s.mu.Lock()
	s.writeAckFn = func(dir string, p position) error {
		if sw.on.Load() {
			return errors.New("injected ack failure")
		}
		return writeAck(dir, p)
	}
	s.mu.Unlock()
	return sw
}

func TestAckFailureStillAdvancesInMemory(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	mustAppend(t, s, "a", "b", "c")
	sw := failAcks(s)

	var got []string
	for range 3 {
		p, err := peekWithin(t, s, time.Second)
		if err != nil {
			t.Fatalf("Peek: %v", err)
		}
		got = append(got, p)
		if err := s.Ack(); err == nil {
			t.Fatal("Ack succeeded while ack writes fail")
		}
	}
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %q, want each frame once", got)
	}
	if st := s.TakeStats(); st.Records != 0 {
		t.Fatalf("Records = %d", st.Records)
	}
	// Once writes work again, a later ack persists the newer position.
	sw.on.Store(false)
	mustAppend(t, s, "d")
	if got := drain(t, s); !slices.Equal(got, []string{"d"}) {
		t.Fatalf("after recovery got %q", got)
	}
	_ = s.Close()
	s2 := openTest(t, dir, Options{})
	if got := drain(t, s2); len(got) != 0 {
		t.Fatalf("replayed %q", got)
	}
}

func TestSegmentBoundaryCrossedWhileAckWritesFail(t *testing.T) {
	dir := t.TempDir()
	opts := Options{SegmentBytes: 40} // two 1-byte frames per segment
	s := openTest(t, dir, opts)
	mustAppend(t, s, "a", "b", "c")
	if p, err := peekWithin(t, s, time.Second); err != nil || p != "a" {
		t.Fatalf("Peek = %q, %v", p, err)
	}
	if err := s.Ack(); err != nil { // saved: the ack file points into segment 1
		t.Fatal(err)
	}
	failAcks(s)
	var got []string
	for range 2 {
		p, err := peekWithin(t, s, time.Second)
		if err != nil {
			t.Fatalf("Peek: %v", err)
		}
		got = append(got, p)
		_ = s.Ack()
	}
	if !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("got %q", got)
	}
	if segs := segFiles(t, dir); len(segs) != 1 {
		t.Fatalf("segments = %q, want the finished one removed", segs)
	}
	_ = s.Close()
	// The ack file still names the removed segment. Reopening replays c, which
	// was not saved as delivered, and skips nothing.
	s2 := openTest(t, dir, opts)
	if got := drain(t, s2); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("after reopen got %q", got)
	}
}

func TestExpiredRecordsSkippedWhileAckWritesFail(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := openTest(t, t.TempDir(), Options{MaxAge: time.Hour, Now: func() time.Time { return now }})
	mustAppend(t, s, "old")
	now = now.Add(2 * time.Hour)
	mustAppend(t, s, "new")
	failAcks(s)
	if p, err := peekWithin(t, s, time.Second); err != nil || p != "new" {
		t.Fatalf("Peek = %q, %v", p, err)
	}
	if st := s.TakeStats(); st.Dropped != 1 {
		t.Fatalf("Dropped = %d", st.Dropped)
	}
}

func TestSizeLimitEnforcedWhileAckWritesFail(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, smallLimits)
	failAcks(s)
	mustAppend(t, s, digits(10)...)
	if st := s.TakeStats(); st.Dropped != 6 || st.Bytes != 68 {
		t.Fatalf("stats = %+v", st)
	}
	_ = s.Close()
	s2 := openTest(t, dir, smallLimits)
	if got := drain(t, s2); !slices.Equal(got, []string{"6", "7", "8", "9"}) {
		t.Fatalf("after reopen got %q", got)
	}
}

func TestAckWriteFailuresLoggedOncePerRun(t *testing.T) {
	var logs []string
	opts := smallLimits
	opts.Logf = func(format string, _ ...any) { logs = append(logs, format) }
	s := openTest(t, t.TempDir(), opts)
	saveFailures := func() int {
		n := 0
		for _, l := range logs {
			if strings.Contains(l, "read position cannot be saved") {
				n++
			}
		}
		return n
	}
	sw := failAcks(s)
	// Each append past the limit drops a segment and tries to save.
	mustAppend(t, s, digits(10)...)
	if n := saveFailures(); n != 1 {
		t.Fatalf("logged %d save failures during one run, want 1: %q", n, logs)
	}
	// A successful save ends the run...
	sw.on.Store(false)
	mustAppend(t, s, "x", "y") // "y" drops a segment, and the save works
	// ...so the next failure is logged again.
	sw.on.Store(true)
	mustAppend(t, s, "z", "w") // "w" drops a segment, and the save fails
	if n := saveFailures(); n != 2 {
		t.Fatalf("logged %d save failures over two runs, want 2: %q", n, logs)
	}
}
