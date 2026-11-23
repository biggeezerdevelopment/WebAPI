package main

import (
	"context"
	"errors"
	"testing"

	"webapi/config"
	"webapi/output"
)

type stubFetcher struct {
	body []byte
	err  error
}

func (s stubFetcher) SendRequest() ([]byte, error) { return s.body, s.err }

type captureDeliverer struct {
	records []output.Record
}

func (c *captureDeliverer) Deliver(_ context.Context, rec output.Record) {
	c.records = append(c.records, rec)
}

func TestPollOnceForwardsResult(t *testing.T) {
	d := &captureDeliverer{}
	agent := config.JarvisAgent{ID: "id", Name: "agent"}
	pollOnce(context.Background(), agent, "src", stubFetcher{body: []byte("ok")}, d)

	if len(d.records) != 1 {
		t.Fatalf("records = %d", len(d.records))
	}
	rec := d.records[0]
	if rec.AgentID != "id" || rec.AgentName != "agent" || rec.Source != "src" || string(rec.Data) != "ok" {
		t.Errorf("record = %+v", rec)
	}
}

func TestPollOnceForwardsNullAfterFailedPoll(t *testing.T) {
	d := &captureDeliverer{}
	pollOnce(context.Background(), config.JarvisAgent{}, "src", stubFetcher{err: errors.New("down")}, d)

	if len(d.records) != 1 || d.records[0].Data != nil {
		t.Fatalf("records = %+v", d.records)
	}
}
