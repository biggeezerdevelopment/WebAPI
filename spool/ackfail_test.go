package spool

import (
	"os"
	"runtime"
	"slices"
	"testing"
	"time"
)

// denyWrites makes dir read-only until the test ends, so writeAck fails.
func denyWrites(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions are not enforced here")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func TestAckFailureStillAdvancesInMemory(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	mustAppend(t, s, "a", "b", "c")
	denyWrites(t, dir)

	var got []string
	for range 3 {
		p, err := peekWithin(t, s, time.Second)
		if err != nil {
			t.Fatalf("Peek: %v", err)
		}
		got = append(got, p)
		if err := s.Ack(); err == nil {
			t.Fatal("Ack succeeded in a read-only directory")
		}
	}
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %q, want each frame once", got)
	}
	if st := s.TakeStats(); st.Records != 0 {
		t.Fatalf("Records = %d", st.Records)
	}
	// Once writes work again, a later ack persists the newer position.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
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
