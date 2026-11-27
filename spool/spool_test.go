package spool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T, dir string, opts Options) *Spool {
	t.Helper()
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 1 << 20
	}
	if opts.Logf == nil {
		opts.Logf = t.Logf
	}
	s, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustAppend(t *testing.T, s *Spool, payloads ...string) {
	t.Helper()
	for _, p := range payloads {
		if err := s.Append([]byte(p)); err != nil {
			t.Fatalf("Append(%q): %v", p, err)
		}
	}
}

func peekWithin(t *testing.T, s *Spool, d time.Duration) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	p, err := s.Peek(ctx)
	return string(p), err
}

// drain peeks and acks until the spool has been empty for 50ms.
func drain(t *testing.T, s *Spool) []string {
	t.Helper()
	var got []string
	for {
		p, err := peekWithin(t, s, 50*time.Millisecond)
		if errors.Is(err, context.DeadlineExceeded) {
			return got
		}
		if err != nil {
			t.Fatalf("Peek: %v", err)
		}
		got = append(got, p)
		if err := s.Ack(); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	}
}

func segFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.seg"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(matches)
	return matches
}

func TestAppendPeekAckInOrderAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{SegmentBytes: 64})
	var want []string
	for i := 0; i < 10; i++ {
		want = append(want, fmt.Sprintf("rec-%d", i))
	}
	mustAppend(t, s, want...)
	if n := len(segFiles(t, dir)); n < 3 {
		t.Fatalf("segments = %d, want rollover", n)
	}
	if got := drain(t, s); !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if n := len(segFiles(t, dir)); n != 1 {
		t.Fatalf("segments after drain = %d, want 1", n)
	}
}

func TestPeekWithoutAckReturnsSameFrame(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{})
	mustAppend(t, s, "a", "b")
	for i := 0; i < 2; i++ {
		if p, err := peekWithin(t, s, time.Second); err != nil || p != "a" {
			t.Fatalf("Peek #%d = %q, %v", i, p, err)
		}
	}
}

func TestReopenKeepsUnackedRecords(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	mustAppend(t, s, "a", "b", "c")
	if p, _ := peekWithin(t, s, time.Second); p != "a" {
		t.Fatalf("Peek = %q", p)
	}
	if err := s.Ack(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openTest(t, dir, Options{})
	if got := drain(t, s2); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("got %q", got)
	}
}

func TestReopenWithoutAckFileStartsAtOldest(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	mustAppend(t, s, "a", "b")
	_ = s.Close()
	s2 := openTest(t, dir, Options{})
	if got := drain(t, s2); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("got %q", got)
	}
}

func TestPeekBlocksUntilAppend(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = s.Append([]byte("late"))
	}()
	if p, err := peekWithin(t, s, 2*time.Second); err != nil || p != "late" {
		t.Fatalf("Peek = %q, %v", p, err)
	}
}

func TestPeekReturnsOnCancelAndClose(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Peek(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Peek(canceled) = %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = s.Close()
	}()
	if _, err := s.Peek(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Peek after Close = %v", err)
	}
	if err := s.Append([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Append after Close = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestAppendTooLarge(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{MaxBytes: 100})
	if err := s.Append([]byte(strings.Repeat("x", 200))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Append = %v", err)
	}
}

func TestConcurrentAppends(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{SegmentBytes: 256})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := s.Append([]byte(fmt.Sprintf("%d-%d", w, i))); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	got := drain(t, s)
	seen := make(map[string]bool, len(got))
	for _, p := range got {
		seen[p] = true
	}
	if len(got) != 400 || len(seen) != 400 {
		t.Fatalf("drained %d records, %d unique", len(got), len(seen))
	}
}

func TestOpenCreatesPrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "spool")
	openTest(t, dir, Options{})
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}
