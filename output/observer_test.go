package output

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"webapi/config"
)

type recordingObserver struct {
	mu        sync.Mutex
	delivered map[string]int
	failed    map[string][]error
}

func newRecordingObserver() *recordingObserver {
	return &recordingObserver{delivered: map[string]int{}, failed: map[string][]error{}}
}

func (o *recordingObserver) Delivered(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.delivered[name]++
}

func (o *recordingObserver) DeliveryFailed(name string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failed[name] = append(o.failed[name], err)
}

var (
	okOutput   = funcOutput{send: func(context.Context, Record) error { return nil }}
	downOutput = funcOutput{send: func(context.Context, Record) error { return errors.New("down") }}
)

func TestObserverSingleTarget(t *testing.T) {
	obs := newRecordingObserver()
	good := newObservedDispatcher("src", []target{{"good", okOutput}}, 8, obs)
	good.Deliver(context.Background(), testRecord())
	bad := newObservedDispatcher("src", []target{{"bad", downOutput}}, 8, obs)
	bad.Deliver(context.Background(), testRecord())
	if obs.delivered["good"] != 1 || len(obs.failed["bad"]) != 1 || len(obs.failed["good"]) != 0 {
		t.Fatalf("delivered=%v failed=%v", obs.delivered, obs.failed)
	}
}

func TestObserverFanOut(t *testing.T) {
	obs := newRecordingObserver()
	d := newObservedDispatcher("src", []target{{"good", okOutput}, {"bad", downOutput}}, 8, obs)
	for i := 0; i < 3; i++ {
		d.Deliver(context.Background(), testRecord())
	}
	d.Close()
	if obs.delivered["good"] != 3 || len(obs.failed["bad"]) != 3 {
		t.Fatalf("delivered=%v failed=%v", obs.delivered, obs.failed)
	}
}

func TestObserverCountsQueueFull(t *testing.T) {
	obs := newRecordingObserver()
	release := make(chan struct{})
	hung := funcOutput{send: func(context.Context, Record) error { <-release; return nil }}
	d := newObservedDispatcher("src", []target{{"hung", hung}, {"good", okOutput}}, 1, obs)
	// The hung lane holds at most one in flight and one queued, so a third is dropped.
	for i := 0; i < 3; i++ {
		d.Deliver(context.Background(), testRecord())
	}
	close(release)
	d.Close()
	obs.mu.Lock()
	defer obs.mu.Unlock()
	full := 0
	for _, err := range obs.failed["hung"] {
		if errors.Is(err, ErrQueueFull) {
			full++
		}
	}
	if full == 0 {
		t.Fatalf("no queue-full failure recorded: %v", obs.failed)
	}
}

func TestObserverIgnoresClosedAndCanceled(t *testing.T) {
	obs := newRecordingObserver()
	d := newObservedDispatcher("src", []target{{"a", okOutput}, {"b", okOutput}}, 8, obs)
	d.Close()
	d.Deliver(context.Background(), testRecord())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	single := newObservedDispatcher("src", []target{{"bad", downOutput}}, 8, obs)
	single.Deliver(ctx, testRecord())

	if len(obs.delivered) != 0 || len(obs.failed) != 0 {
		t.Fatalf("delivered=%v failed=%v", obs.delivered, obs.failed)
	}
}

func TestHeartbeatDispatcher(t *testing.T) {
	got := make(chan Record, 1)
	capture := funcOutput{send: func(_ context.Context, rec Record) error { got <- rec; return nil }}
	r := &Router{cfg: &config.Config{}, outputs: map[string]Output{"hook": capture}}

	d, err := r.HeartbeatDispatcher("hb", []string{"hook"})
	if err != nil {
		t.Fatal(err)
	}
	rec := testRecord()
	rec.Source = "hb"
	d.Deliver(context.Background(), rec)
	if (<-got).Source != "hb" {
		t.Fatal("heartbeat record not delivered")
	}

	_, err = r.HeartbeatDispatcher("hb", []string{"nope"})
	if err == nil || !strings.Contains(err.Error(), `heartbeat: output "nope" is not available`) {
		t.Fatalf("err = %v", err)
	}
}

func (o *recordingObserver) counts(name string) (delivered, failed int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.delivered[name], len(o.failed[name])
}
