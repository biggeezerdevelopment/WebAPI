package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"webapi/config"
	"webapi/output"
	"webapi/status"
)

type captureDeliverer struct{ records chan output.Record }

func (c *captureDeliverer) Deliver(_ context.Context, rec output.Record) { c.records <- rec }

func receive(t *testing.T, ch <-chan output.Record) output.Record {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat delivered")
		return output.Record{}
	}
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

type fixedSnapshot struct{ hb status.Heartbeat }

func (f fixedSnapshot) Snapshot() status.Heartbeat { return f.hb }

func TestSendUsesSnapshotTimeAndAgent(t *testing.T) {
	at := time.Date(2026, 11, 25, 22, 0, 0, 0, time.UTC)
	d := &captureDeliverer{records: make(chan output.Record, 1)}
	Send(context.Background(), fixedSnapshot{status.Heartbeat{Type: status.HeartbeatType, At: at, Sources: []status.SourceSnapshot{}}},
		d, config.JarvisAgent{ID: "id", Name: "agent"}, "hb")
	rec := receive(t, d.records)
	if rec.Source != "hb" || rec.AgentID != "id" || rec.AgentName != "agent" || !rec.Timestamp.Equal(at) {
		t.Fatalf("record = %+v", rec)
	}
	if m := decode(t, rec.Data); m["type"] != "heartbeat" {
		t.Fatalf("data = %v", m)
	}
}

func TestRunSendsAtStartAndOnEveryTick(t *testing.T) {
	cfg := &config.Config{
		LogOutputs: []config.LogOutput{{Name: "hook", Enabled: true}},
		WebAPI:     []config.WebAPI{{Enabled: true, Name: "plain", SleepTime: 30}},
	}
	reg := status.NewRegistry(cfg)
	reg.Source("plain").PollFailed(errors.New("boom"))

	d := &captureDeliverer{records: make(chan output.Record, 4)}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, reg, d, config.JarvisAgent{ID: "id", Name: "agent"}, "hb", ticks)
		close(done)
	}()

	first := decode(t, receive(t, d.records).Data)
	src := first["sources"].([]any)[0].(map[string]any)
	if first["healthy"] != false || src["errors_since_last"] != float64(1) || src["last_error"] != "boom" {
		t.Fatalf("first = %v", first)
	}

	ticks <- time.Now()
	second := decode(t, receive(t, d.records).Data)
	src = second["sources"].([]any)[0].(map[string]any)
	if second["healthy"] != true || src["errors_since_last"] != float64(0) || src["last_error"] != "boom" {
		t.Fatalf("second = %v", second)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}
