package spool

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[off] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTornTailIsTruncated(t *testing.T) {
	for name, tail := range map[string][]byte{
		"short header":  {1, 2, 3},
		"short payload": encodeFrame([]byte("lost"), 1)[:headerSize+2],
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openTest(t, dir, Options{})
			mustAppend(t, s, "a", "b")
			_ = s.Close()
			seg := segFiles(t, dir)[0]
			appendBytes(t, seg, tail)

			s2 := openTest(t, dir, Options{})
			mustAppend(t, s2, "c")
			if got := drain(t, s2); !slices.Equal(got, []string{"a", "b", "c"}) {
				t.Fatalf("got %q", got)
			}
			if st := s2.TakeStats(); st.Dropped != 0 {
				t.Fatalf("Dropped = %d", st.Dropped)
			}
		})
	}
}

func TestBadCRCAtTailIsTruncated(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	mustAppend(t, s, "a", "b")
	_ = s.Close()
	seg := segFiles(t, dir)[0]
	info, _ := os.Stat(seg)
	flipByte(t, seg, info.Size()-1) // last payload byte of "b"

	s2 := openTest(t, dir, Options{})
	if got := drain(t, s2); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("got %q", got)
	}
	if st := s2.TakeStats(); st.Dropped != 0 {
		t.Fatalf("Dropped = %d", st.Dropped)
	}
}

func TestCorruptOlderSegmentIsSkipped(t *testing.T) {
	dir := t.TempDir()
	// 1-byte payloads make 17-byte frames: two per 40-byte segment.
	s := openTest(t, dir, Options{SegmentBytes: 40})
	mustAppend(t, s, "a", "b", "c")
	_ = s.Close()
	segs := segFiles(t, dir)
	if len(segs) != 2 {
		t.Fatalf("segments = %d", len(segs))
	}
	flipByte(t, segs[0], 17+headerSize) // payload of "b"

	s2 := openTest(t, dir, Options{SegmentBytes: 40})
	if got := drain(t, s2); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("got %q", got)
	}
	if st := s2.TakeStats(); st.Dropped != 1 {
		t.Fatalf("Dropped = %d", st.Dropped)
	}
}

func TestCorruptActiveSegmentDuringRunIsSkipped(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	mustAppend(t, s, "a")
	flipByte(t, segFiles(t, dir)[0], headerSize) // corrupt "a" after it was written
	mustAppend(t, s, "b")
	got := drain(t, s)
	mustAppend(t, s, "c")
	got = append(got, drain(t, s)...)
	if !slices.Equal(got, []string{"c"}) {
		t.Fatalf("got %q", got)
	}
	if st := s.TakeStats(); st.Dropped != 2 {
		t.Fatalf("Dropped = %d", st.Dropped)
	}
}

func TestBadAckFileFailsOpen(t *testing.T) {
	cases := map[string]string{
		"not json":       "nope",
		"missing offset": `{"segment":1}`,
		"zero segment":   `{"segment":0,"offset":0}`,
		"trailing data":  `{"segment":1,"offset":0} x`,
		"past end":       `{"segment":1,"offset":9999}`,
		"future segment": `{"segment":99,"offset":0}`,
	}
	for name, ack := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openTest(t, dir, Options{})
			mustAppend(t, s, "a")
			_ = s.Close()
			if err := os.WriteFile(filepath.Join(dir, ackName), []byte(ack), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Open(dir, Options{MaxBytes: 1 << 20, Logf: t.Logf})
			if err == nil || !strings.Contains(err.Error(), "spool ack file") {
				t.Fatalf("Open = %v", err)
			}
		})
	}
}

func TestDeliveredSegmentLeftByCrashIsRemoved(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{SegmentBytes: 40})
	mustAppend(t, s, "a", "b", "c")
	_ = s.Close()
	// As if the agent crashed after moving the ack, before removing segment 1.
	if err := writeAck(dir, position{Seq: 2}); err != nil {
		t.Fatal(err)
	}
	s2 := openTest(t, dir, Options{SegmentBytes: 40})
	if got := drain(t, s2); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("got %q", got)
	}
	if segs := segFiles(t, dir); len(segs) != 1 {
		t.Fatalf("segments = %q", segs)
	}
}

func TestStaleAckTempFileIsRemoved(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".ack-123.tmp")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	openTest(t, dir, Options{})
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp file: %v", err)
	}
}

func TestZeroFilledTailIsTruncatedSilently(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	mustAppend(t, s, "a")
	_ = s.Close()
	appendBytes(t, segFiles(t, dir)[0], make([]byte, 64))

	s2 := openTest(t, dir, Options{})
	if got := drain(t, s2); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("got %q", got)
	}
	if st := s2.TakeStats(); st.Dropped != 0 {
		t.Fatalf("Dropped = %d", st.Dropped)
	}
}

func TestOpenAcceptsAckBeforeOldestSegment(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{SegmentBytes: 40})
	mustAppend(t, s, "a", "b", "c")
	_ = s.Close()
	// As if a full disk let segment 1 be removed but not the ack file updated.
	if err := os.Remove(segFiles(t, dir)[0]); err != nil {
		t.Fatal(err)
	}
	if err := writeAck(dir, position{Seq: 1, Off: 17}); err != nil {
		t.Fatal(err)
	}
	s2 := openTest(t, dir, Options{SegmentBytes: 40})
	if got := drain(t, s2); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("got %q", got)
	}
}

func TestOpenWithAckButNoSegmentsStartsAfterIt(t *testing.T) {
	dir := t.TempDir()
	if err := writeAck(dir, position{Seq: 5, Off: 40}); err != nil {
		t.Fatal(err)
	}
	s := openTest(t, dir, Options{})
	mustAppend(t, s, "x")
	if got := drain(t, s); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("got %q", got)
	}
	if segs := segFiles(t, dir); len(segs) != 1 || filepath.Base(segs[0]) != "0000000000000006.seg" {
		t.Fatalf("segments = %q, want only segment 6", segs)
	}
}

func TestOpenResumesAfterMissingMiddleSegment(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{SegmentBytes: 40})
	mustAppend(t, s, "a", "b", "c", "d", "e") // segments 1, 2 and 3
	_ = s.Close()
	segs := segFiles(t, dir)
	if len(segs) != 3 {
		t.Fatalf("segments = %q", segs)
	}
	if err := os.Remove(segs[1]); err != nil {
		t.Fatal(err)
	}
	if err := writeAck(dir, position{Seq: 2}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []string
	s2, err := Open(dir, Options{MaxBytes: 1 << 20, SegmentBytes: 40, Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}})
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer s2.Close()
	if got := drain(t, s2); !slices.Equal(got, []string{"e"}) {
		t.Fatalf("got %q, want [e]", got)
	}
	if _, err := os.Stat(segs[0]); !os.IsNotExist(err) {
		t.Fatalf("segment 1 still on disk: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "names missing segment 2; resuming at segment 3") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d matching log lines in %q", n, lines)
	}
}
