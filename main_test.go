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
	pollOnce(context.Background(), agent, "src", stubFetcher{body: []byte("ok")}, d, &stubStatus{})

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
	pollOnce(context.Background(), config.JarvisAgent{}, "src", stubFetcher{err: errors.New("down")}, d, &stubStatus{})

	if len(d.records) != 1 || d.records[0].Data != nil {
		t.Fatalf("records = %+v", d.records)
	}
}

type stubStatus struct {
	started, succeeded int
	errs               []error
}

func (s *stubStatus) PollStarted()         { s.started++ }
func (s *stubStatus) PollSucceeded()       { s.succeeded++ }
func (s *stubStatus) PollFailed(err error) { s.errs = append(s.errs, err) }

func TestPollOnceReportsStatus(t *testing.T) {
	d := &captureDeliverer{}
	ok := &stubStatus{}
	pollOnce(context.Background(), config.JarvisAgent{}, "src", stubFetcher{body: []byte("ok")}, d, ok)
	if ok.started != 1 || ok.succeeded != 1 || len(ok.errs) != 0 {
		t.Fatalf("success status = %+v", ok)
	}
	bad := &stubStatus{}
	pollOnce(context.Background(), config.JarvisAgent{}, "src", stubFetcher{err: errors.New("down")}, d, bad)
	if bad.started != 1 || bad.succeeded != 0 || len(bad.errs) != 1 {
		t.Fatalf("failure status = %+v", bad)
	}
}
