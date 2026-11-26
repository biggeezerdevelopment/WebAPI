package collector

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"webapi/output"
)

// blockingOutput delivers the first record and blocks on the next until its
// context ends.
type blockingOutput struct {
	sent    int
	blocked chan struct{}
}

func (b *blockingOutput) Send(ctx context.Context, rec output.Record) error {
	b.sent++
	if b.sent == 1 {
		return nil
	}
	close(b.blocked)
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingOutput) Close() error { return nil }

func TestRunStopsOnCancelAndKeepsLastSavedCheckpoint(t *testing.T) {
	records := []cursorRecord{{"r1", ms(-3000)}, {"r2", ms(-2000)}, {"r3", ms(-1000)}}
	srv := cursorServer(t, records)
	cfg := cursorConfig(srv.URL + "/events")
	cfg.SleepTime = 3600
	dir := t.TempDir()
	out := &blockingOutput{blocked: make(chan struct{})}
	c := buildWithSource(t, cfg, &cursorSource{endpoint: cfg.Endpoint}, dir, out)
	c.sleep = sleepContext

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	select {
	case <-out.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("second delivery never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	state, err := loadState(statePath(dir, cfg.Name), cfg.Name, testNow)
	if err != nil {
		t.Fatalf("state after shutdown: %v", err)
	}
	if cp := state["o"]; cp.TimestampMS != ms(-3000) || len(cp.IDs) != 1 || cp.IDs[0] != "r1" {
		raw, _ := json.Marshal(cp)
		t.Fatalf("checkpoint = %s, want r1 (the canceled send must not advance it)", raw)
	}
}
