package output

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"webapi/config"
)

// sink is a fake output that can be taken down and records what it accepted.
type sink struct {
	mu       sync.Mutex
	down     bool
	attempts int
	got      []string
}

func (s *sink) send(_ context.Context, rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.down {
		return errors.New("down")
	}
	s.got = append(s.got, string(rec.Data))
	return nil
}

func (s *sink) setDown(down bool) { s.mu.Lock(); s.down = down; s.mu.Unlock() }

func (s *sink) state() (int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts, slices.Clone(s.got)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func numbered(n int) Record {
	rec := testRecord()
	rec.Data = []byte(fmt.Sprintf(`{"n":%d}`, n))
	return rec
}

func numberedData(from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprintf(`{"n":%d}`, i))
	}
	return out
}

func spooledOutputConfig(name string) config.LogOutput {
	return config.LogOutput{Name: name, Enabled: true, Type: config.OutputTypeWebhook, Timeout: 1, Spool: &config.Spool{Enabled: true}}
}

// testRouter builds a router whose outputs are the given fakes, with fast
// retries and spool backoff.
func testRouter(t *testing.T, cfg *config.Config, readOnly bool, outs map[string]Output) *Router {
	t.Helper()
	r, err := NewRouterWithOptions(cfg, RouterOptions{
		ReadOnly:     readOnly,
		newOutput:    func(e *config.LogOutput) (Output, error) { return outs[e.Name], nil },
		retryBackoff: time.Millisecond,
		spoolMinWait: 5 * time.Millisecond,
		spoolMaxWait: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func oneSpoolRouter(t *testing.T, stateDir string, s *sink) *Router {
	cfg := &config.Config{StateDir: stateDir, LogOutputs: []config.LogOutput{spooledOutputConfig("hook")}}
	return testRouter(t, cfg, false, map[string]Output{"hook": funcOutput{send: s.send}})
}

func TestSpoolDeliversBacklogInOrderAfterOutage(t *testing.T) {
	s := &sink{down: true}
	r := oneSpoolRouter(t, t.TempDir(), s)
	defer r.Close()
	d, err := r.Dispatcher(&config.WebAPI{Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		d.Deliver(context.Background(), numbered(i))
	}
	waitFor(t, "a failed attempt", func() bool { n, _ := s.state(); return n >= 2 })
	if _, got := s.state(); len(got) != 0 {
		t.Fatalf("delivered while down: %q", got)
	}
	s.setDown(false)
	waitFor(t, "backlog", func() bool { _, got := s.state(); return len(got) == 5 })
	if _, got := s.state(); !slices.Equal(got, numberedData(0, 5)) {
		t.Fatalf("got %q", got)
	}
}

func TestSpoolReplaysBacklogAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s := &sink{down: true}
	r1 := oneSpoolRouter(t, dir, s)
	d, _ := r1.Dispatcher(&config.WebAPI{Name: "src"})
	for i := 0; i < 3; i++ {
		d.Deliver(context.Background(), numbered(i))
	}
	waitFor(t, "a failed attempt", func() bool { n, _ := s.state(); return n >= 1 })
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	s.setDown(false)
	r2 := oneSpoolRouter(t, dir, s)
	defer r2.Close()
	waitFor(t, "replay", func() bool { _, got := s.state(); return len(got) == 3 })
	if _, got := s.state(); !slices.Equal(got, numberedData(0, 3)) {
		t.Fatalf("got %q", got)
	}
}

func TestSpoolSkipsPermanentErrors(t *testing.T) {
	s := &sink{}
	send := func(ctx context.Context, rec Record) error {
		if string(rec.Data) == `{"n":0}` {
			return fmt.Errorf("send: %w", ErrSyslogMessageTooLarge)
		}
		return s.send(ctx, rec)
	}
	cfg := &config.Config{StateDir: t.TempDir(), LogOutputs: []config.LogOutput{spooledOutputConfig("hook")}}
	r := testRouter(t, cfg, false, map[string]Output{"hook": funcOutput{send: send}})
	defer r.Close()
	d, _ := r.Dispatcher(&config.WebAPI{Name: "src"})
	d.Deliver(context.Background(), numbered(0))
	d.Deliver(context.Background(), numbered(1))
	waitFor(t, "second record", func() bool { _, got := s.state(); return len(got) == 1 })
	if _, got := s.state(); got[0] != `{"n":1}` {
		t.Fatalf("got %q", got)
	}
}

func TestSpoolFanOutDoesNotDropForSpooledOutput(t *testing.T) {
	s := &sink{down: true}
	cfg := &config.Config{StateDir: t.TempDir(), LogOutputs: []config.LogOutput{
		spooledOutputConfig("hook"),
		{Name: "other", Enabled: true, Type: config.OutputTypeWebhook, Timeout: 1},
	}}
	r := testRouter(t, cfg, false, map[string]Output{"hook": funcOutput{send: s.send}, "other": okOutput})
	defer r.Close()
	d, _ := r.Dispatcher(&config.WebAPI{Name: "src"})
	n := laneQueueSize * 3
	for i := 0; i < n; i++ {
		d.Deliver(context.Background(), numbered(i))
	}
	s.setDown(false)
	waitFor(t, "all records", func() bool { _, got := s.state(); return len(got) == n })
	if _, got := s.state(); !slices.Equal(got, numberedData(0, n)) {
		t.Fatalf("records out of order or missing")
	}
}

func TestSpoolBypassedByRoutesAndHeartbeat(t *testing.T) {
	s := &sink{down: true}
	r := oneSpoolRouter(t, t.TempDir(), s)
	defer r.Close()
	routes, err := r.Routes(&config.WebAPI{Name: "collector"})
	if err != nil {
		t.Fatal(err)
	}
	if err := routes[0].Output.Send(context.Background(), numbered(1)); err == nil {
		t.Fatal("collector route send succeeded while the output is down; it went to the spool")
	}
	hb, err := r.HeartbeatDispatcher("hb", []string{"hook"})
	if err != nil {
		t.Fatal(err)
	}
	hb.Deliver(context.Background(), numbered(2))
	if st := r.TakeSpoolStats(); len(st) != 1 || st[0].Output != "hook" || st[0].Records != 0 {
		t.Fatalf("spool stats = %+v", st)
	}
}

func TestSpoolReportsToOriginObserver(t *testing.T) {
	s := &sink{down: true}
	cfg := &config.Config{
		StateDir:   t.TempDir(),
		LogOutputs: []config.LogOutput{spooledOutputConfig("hook")},
		WebAPI:     []config.WebAPI{{Enabled: true, Name: "a"}, {Enabled: true, Name: "b"}},
	}
	r := testRouter(t, cfg, false, map[string]Output{"hook": funcOutput{send: s.send}})
	defer r.Close()
	obsA, obsB := newRecordingObserver(), newRecordingObserver()
	dA, _ := r.ObservedDispatcher(&cfg.WebAPI[0], obsA)
	dB, _ := r.ObservedDispatcher(&cfg.WebAPI[1], obsB)
	dA.Deliver(context.Background(), numbered(0))
	if delivered, _ := obsA.counts("hook"); delivered != 0 {
		t.Fatal("dispatcher reported Delivered for a spool append")
	}
	waitFor(t, "a failure for a", func() bool { _, failed := obsA.counts("hook"); return failed >= 1 })
	s.setDown(false)
	dB.Deliver(context.Background(), numbered(1))
	waitFor(t, "deliveries", func() bool {
		a, _ := obsA.counts("hook")
		b, _ := obsB.counts("hook")
		return a == 1 && b == 1
	})
	if _, failed := obsB.counts("hook"); failed != 0 {
		t.Fatalf("b saw %d failures that belong to a", failed)
	}
}

func TestSpoolStatsReportBacklog(t *testing.T) {
	s := &sink{down: true}
	r := oneSpoolRouter(t, t.TempDir(), s)
	defer r.Close()
	d, _ := r.Dispatcher(&config.WebAPI{Name: "src"})
	d.Deliver(context.Background(), numbered(0))
	d.Deliver(context.Background(), numbered(1))
	st := r.TakeSpoolStats()
	if len(st) != 1 || st[0].Records != 2 || st[0].Bytes == 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestReadOnlyRouterOpensNoSpool(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir, LogOutputs: []config.LogOutput{spooledOutputConfig("hook")}}
	r := testRouter(t, cfg, true, map[string]Output{"hook": okOutput})
	if st := r.TakeSpoolStats(); len(st) != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "spool")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only router created the spool dir: %v", err)
	}
}
