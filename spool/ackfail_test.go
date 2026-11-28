package spool

import (
	"errors"
	"slices"
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
