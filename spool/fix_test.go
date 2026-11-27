package spool

import (
	"os"
	"slices"
	"testing"
)

func TestCorruptionInMiddleOfLastSegmentIsCountedAndDropped(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	mustAppend(t, s, "a", "b", "c")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	files := segFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("segments = %v, want 1", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	data[17+16] ^= 0xff // b's payload
	if err := os.WriteFile(files[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir, Options{})
	if got := drain(t, s); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("drain = %q, want [a]", got)
	}
	if st := s.TakeStats(); st.Dropped != 2 {
		t.Fatalf("Dropped = %d, want 2", st.Dropped)
	}
	mustAppend(t, s, "d")
	if got := drain(t, s); !slices.Equal(got, []string{"d"}) {
		t.Fatalf("drain after append = %q, want [d]", got)
	}
}

func TestSegmentBytesClampedToMaxBytes(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{MaxBytes: 100, SegmentBytes: 1000})
	for range 10 {
		mustAppend(t, s, "x")
	}
	if st := s.TakeStats(); st.Bytes > 100 {
		t.Fatalf("Bytes = %d, want <= 100", st.Bytes)
	}
}

// A failed write must not leave the next append landing after stray bytes:
// the next append starts a new segment.
func TestAppendAfterWriteFailureRotates(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	mustAppend(t, s, "a")
	_ = s.active.Close() // makes the next Write fail
	if err := s.Append([]byte("lost")); err == nil {
		t.Fatal("Append on a broken file succeeded")
	}
	mustAppend(t, s, "b")
	if n := len(segFiles(t, dir)); n != 2 {
		t.Fatalf("segments = %d, want 2", n)
	}
	if got := drain(t, s); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("drain = %q, want [a b]", got)
	}
}
