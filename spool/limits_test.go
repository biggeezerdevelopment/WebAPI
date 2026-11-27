package spool

import (
	"slices"
	"strconv"
	"testing"
	"time"
)

// With 1-byte payloads a frame is 17 bytes; SegmentBytes 40 holds two, so a
// full segment is 34 bytes and MaxBytes 100 keeps at most two full segments.
var smallLimits = Options{MaxBytes: 100, SegmentBytes: 40}

func digits(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strconv.Itoa(i)
	}
	return out
}

func TestSizeLimitDropsOldestSegments(t *testing.T) {
	s := openTest(t, t.TempDir(), smallLimits)
	mustAppend(t, s, digits(10)...)
	st := s.TakeStats()
	if st.Dropped != 6 || st.Records != 4 || st.Bytes != 68 {
		t.Fatalf("stats = %+v", st)
	}
	if got := drain(t, s); !slices.Equal(got, []string{"6", "7", "8", "9"}) {
		t.Fatalf("got %q", got)
	}
	if st := s.TakeStats(); st.Dropped != 0 || st.Records != 0 {
		t.Fatalf("stats after drain = %+v", st)
	}
}

func TestSizeLimitMovesReaderPastDroppedRecords(t *testing.T) {
	s := openTest(t, t.TempDir(), smallLimits)
	mustAppend(t, s, "0")
	if p, err := peekWithin(t, s, time.Second); err != nil || p != "0" {
		t.Fatalf("Peek = %q, %v", p, err)
	}
	mustAppend(t, s, digits(10)[1:]...)
	if err := s.Ack(); err != nil {
		t.Fatalf("Ack of a dropped frame = %v", err)
	}
	if got := drain(t, s); !slices.Equal(got, []string{"6", "7", "8", "9"}) {
		t.Fatalf("got %q", got)
	}
}

func TestSizeLimitSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, smallLimits)
	mustAppend(t, s, digits(10)...)
	_ = s.Close()
	s2 := openTest(t, dir, smallLimits)
	if got := drain(t, s2); !slices.Equal(got, []string{"6", "7", "8", "9"}) {
		t.Fatalf("got %q", got)
	}
}

func TestAgeLimitSkipsOldRecords(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	opts := Options{MaxAge: time.Hour, Now: func() time.Time { return now }}
	s := openTest(t, t.TempDir(), opts)
	mustAppend(t, s, "old")
	now = now.Add(2 * time.Hour)
	mustAppend(t, s, "new")
	if got := drain(t, s); !slices.Equal(got, []string{"new"}) {
		t.Fatalf("got %q", got)
	}
	if st := s.TakeStats(); st.Dropped != 1 {
		t.Fatalf("Dropped = %d", st.Dropped)
	}
}

func TestNoAgeLimitKeepsOldRecords(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := openTest(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	mustAppend(t, s, "old")
	now = now.Add(24 * 365 * time.Hour)
	if got := drain(t, s); !slices.Equal(got, []string{"old"}) {
		t.Fatalf("got %q", got)
	}
}

func TestTakeStatsCountsRecordsAndBytes(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{})
	mustAppend(t, s, "a", "b")
	if st := s.TakeStats(); st.Records != 2 || st.Bytes != 34 || st.Dropped != 0 {
		t.Fatalf("stats = %+v", st)
	}
}
