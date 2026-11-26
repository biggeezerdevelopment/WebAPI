package status

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"webapi/config"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func testConfig() *config.Config {
	return &config.Config{
		LogOutputs: []config.LogOutput{{Name: "hook", Enabled: true}, {Name: "off", Enabled: false}},
		Heartbeat:  config.Heartbeat{Interval: 60},
		WebAPI: []config.WebAPI{
			{Enabled: true, Name: "gh", SleepTime: 60, Collector: &config.Collector{Type: "github_audit"}},
			{Enabled: false, Name: "skipped"},
			{Enabled: true, Name: "plain", SleepTime: 30},
		},
	}
}

var start = time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSnapshotInitialState(t *testing.T) {
	clock := &fakeClock{t: start}
	reg := newRegistry(testConfig(), clock.now)
	clock.t = start.Add(90 * time.Second)

	hb := reg.Snapshot()
	if !hb.At.Equal(clock.t) {
		t.Errorf("At = %v", hb.At)
	}
	want := `{"type":"heartbeat","healthy":true,"started_at":"2026-11-25T20:00:00Z","uptime_seconds":90,"interval_seconds":60,"sources":[` +
		`{"name":"gh","kind":"collector","collector_type":"github_audit","sleeptime_seconds":60,"last_poll_at":null,"last_success_at":null,"errors_since_last":0,"last_error":null,"last_error_at":null,` +
		`"outputs":[{"name":"hook","last_delivery_at":null,"delivery_failures_since_last":0,"last_delivery_error":null,"last_delivery_error_at":null,"checkpoint":null}]},` +
		`{"name":"plain","kind":"plain","collector_type":null,"sleeptime_seconds":30,"last_poll_at":null,"last_success_at":null,"errors_since_last":0,"last_error":null,"last_error_at":null,` +
		`"outputs":[{"name":"hook","last_delivery_at":null,"delivery_failures_since_last":0,"last_delivery_error":null,"last_delivery_error_at":null}]}]}`
	if got := marshal(t, hb); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestSnapshotResetsCountsButKeepsLastValues(t *testing.T) {
	clock := &fakeClock{t: start}
	reg := newRegistry(testConfig(), clock.now)
	gh := reg.Source("gh")

	clock.t = start.Add(time.Minute)
	gh.PollStarted()
	gh.PollFailed(errors.New(`Get "https://api.example.com/x?token=secret": 502`))
	gh.PollFailed(errors.New("second"))
	clock.t = start.Add(2 * time.Minute)
	gh.PollStarted()
	gh.PollSucceeded()
	gh.DeliveryFailed("hook", errors.New("down"))
	gh.Delivered("hook")
	gh.Checkpointed("hook", start.Add(90*time.Second))

	first := reg.Snapshot()
	s := first.Sources[0]
	if first.Healthy || s.ErrorsSinceLast != 2 || *s.LastError != "second" || *s.LastErrorAt != "2026-11-25T20:01:00Z" {
		t.Fatalf("first = %+v", s)
	}
	if *s.LastPollAt != "2026-11-25T20:02:00Z" || *s.LastSuccessAt != "2026-11-25T20:02:00Z" {
		t.Fatalf("poll times = %s %s", *s.LastPollAt, *s.LastSuccessAt)
	}
	o := s.Outputs[0]
	if o.DeliveryFailuresSinceLast != 1 || *o.LastDeliveryError != "down" || *o.LastDeliveryAt != "2026-11-25T20:02:00Z" || *o.Checkpoint.Value != "2026-11-25T20:01:30Z" {
		t.Fatalf("first output = %+v", o)
	}

	second := reg.Snapshot()
	s = second.Sources[0]
	o = s.Outputs[0]
	if !second.Healthy || s.ErrorsSinceLast != 0 || o.DeliveryFailuresSinceLast != 0 {
		t.Fatalf("counts not reset: %+v", second)
	}
	if s.LastError == nil || s.LastSuccessAt == nil || o.LastDeliveryError == nil || o.Checkpoint.Value == nil {
		t.Fatal("last values must survive a snapshot")
	}
}

func TestSanitizedErrorIsStored(t *testing.T) {
	reg := newRegistry(testConfig(), (&fakeClock{t: start}).now)
	reg.Source("plain").PollFailed(errors.New("GET https://h/p?api_key=hunter2 failed"))
	reg.Source("plain").DeliveryFailed("hook", errors.New(""))
	s := reg.Snapshot().Sources[1]
	if *s.LastError != "GET https://h/p failed" {
		t.Fatalf("last_error = %q", *s.LastError)
	}
	if *s.Outputs[0].LastDeliveryError != "unknown error" {
		t.Fatalf("empty error = %q", *s.Outputs[0].LastDeliveryError)
	}
}

func TestUnknownNamesAreIgnored(t *testing.T) {
	reg := newRegistry(testConfig(), (&fakeClock{t: start}).now)
	if reg.Source("skipped") != nil || reg.Source("nope") != nil {
		t.Fatal("disabled or unknown source must be nil")
	}
	var nilSource *Source
	nilSource.PollStarted()
	nilSource.PollFailed(errors.New("x"))
	nilSource.Delivered("hook")
	reg.Source("gh").Delivered("nope")
	reg.Source("gh").DeliveryFailed("nope", errors.New("x"))
	if !reg.Snapshot().Healthy {
		t.Fatal("ignored calls must not affect health")
	}
}

func TestConcurrentUpdatesAreCounted(t *testing.T) {
	reg := NewRegistry(testConfig())
	gh := reg.Source("gh")
	var wg sync.WaitGroup
	total := 0
	var mu sync.Mutex
	stop := make(chan struct{})
	snapDone := make(chan struct{})
	go func() {
		defer close(snapDone)
		for {
			select {
			case <-stop:
				return
			default:
				n := reg.Snapshot().Sources[0].ErrorsSinceLast
				mu.Lock()
				total += n
				mu.Unlock()
			}
		}
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				gh.PollFailed(errors.New("x"))
				gh.Delivered("hook")
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-snapDone
	total += reg.Snapshot().Sources[0].ErrorsSinceLast
	if total != 800 {
		t.Fatalf("total errors = %d, want 800", total)
	}
}

func TestDeliveryFailureKeepsUnhealthyUntilDelivered(t *testing.T) {
	clock := &fakeClock{t: start}
	reg := newRegistry(testConfig(), clock.now)
	plain := reg.Source("plain")

	clock.t = start.Add(time.Minute)
	plain.DeliveryFailed("hook", errors.New("down"))
	if reg.Snapshot().Healthy {
		t.Fatal("first snapshot must be unhealthy")
	}
	if hb := reg.Snapshot(); hb.Healthy || hb.Sources[1].Outputs[0].DeliveryFailuresSinceLast != 0 {
		t.Fatalf("second snapshot must stay unhealthy with counts reset: %+v", hb)
	}
	clock.t = start.Add(2 * time.Minute)
	plain.Delivered("hook")
	if !reg.Snapshot().Healthy {
		t.Fatal("must recover after a later delivery")
	}
}

func TestPollFailureKeepsUnhealthyUntilSucceeded(t *testing.T) {
	clock := &fakeClock{t: start}
	reg := newRegistry(testConfig(), clock.now)
	gh := reg.Source("gh")

	clock.t = start.Add(time.Minute)
	gh.PollFailed(errors.New("boom"))
	if reg.Snapshot().Healthy {
		t.Fatal("first snapshot must be unhealthy")
	}
	if hb := reg.Snapshot(); hb.Healthy || hb.Sources[0].ErrorsSinceLast != 0 {
		t.Fatalf("second snapshot must stay unhealthy with counts reset: %+v", hb)
	}
	clock.t = start.Add(2 * time.Minute)
	gh.PollSucceeded()
	if !reg.Snapshot().Healthy {
		t.Fatal("must recover after a later successful poll")
	}
}

func TestDuplicateNamesGetDistinctSources(t *testing.T) {
	cfg := &config.Config{
		LogOutputs: []config.LogOutput{{Name: "hook", Enabled: true}},
		WebAPI: []config.WebAPI{
			{Enabled: true, Name: "x"},
			{Enabled: false, Name: "off"},
			{Enabled: true, Name: "x"},
		},
	}
	reg := newRegistry(cfg, (&fakeClock{t: start}).now)
	a, b := reg.SourceAt(0), reg.SourceAt(2)
	if a == nil || b == nil || a == b {
		t.Fatalf("SourceAt(0)=%p SourceAt(2)=%p", a, b)
	}
	if reg.SourceAt(1) != nil || reg.SourceAt(3) != nil || reg.SourceAt(-1) != nil {
		t.Fatal("disabled or out-of-range index must be nil")
	}
	b.PollFailed(errors.New("only b"))
	hb := reg.Snapshot()
	if len(hb.Sources) != 2 {
		t.Fatalf("sources = %d", len(hb.Sources))
	}
	if hb.Sources[0].LastError != nil || hb.Sources[0].ErrorsSinceLast != 0 {
		t.Fatalf("first entry polluted: %+v", hb.Sources[0])
	}
	if hb.Sources[1].LastError == nil || *hb.Sources[1].LastError != "only b" {
		t.Fatalf("second entry = %+v", hb.Sources[1])
	}
}
