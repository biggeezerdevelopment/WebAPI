package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"webapi/config"
)

func TestPollLoopStopsWhenCanceledDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var polls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		pollLoop(ctx, time.Hour, func() {
			polls.Add(1)
			cancel()
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pollLoop did not return after cancel")
	}
	if n := polls.Load(); n != 1 {
		t.Fatalf("polls = %d, want 1", n)
	}
}

func TestPollLoopPollsEachInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var polls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		pollLoop(ctx, time.Millisecond, func() {
			if polls.Add(1) == 3 {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pollLoop did not return")
	}
	if n := polls.Load(); n != 3 {
		t.Fatalf("polls = %d, want 3", n)
	}
}

func TestPollOnceCanceledIsNotReportedOrDelivered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &captureDeliverer{}
	st := &stubStatus{}
	pollOnce(ctx, config.JarvisAgent{}, "src", stubFetcher{err: context.Canceled}, d, st)
	if len(d.records) != 0 {
		t.Fatalf("records = %+v, want none after shutdown", d.records)
	}
	if len(st.errs) != 0 || st.succeeded != 0 {
		t.Fatalf("status = %+v, want no outcome after shutdown", st)
	}
}

func TestShutdownWaitsThenCloses(t *testing.T) {
	var order []string
	err := shutdown(func() { order = append(order, "wait") }, func() error {
		order = append(order, "close")
		return nil
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "wait" || order[1] != "close" {
		t.Fatalf("order = %v", order)
	}
}

func TestShutdownReturnsCloseError(t *testing.T) {
	want := errors.New("boom")
	if err := shutdown(func() {}, func() error { return want }, time.Second); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestShutdownTimesOutOnHungWait(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	err := shutdown(func() { <-block }, func() error { return nil }, 50*time.Millisecond)
	if !errors.Is(err, errShutdownTimeout) {
		t.Fatalf("err = %v, want errShutdownTimeout", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("shutdown did not respect its timeout")
	}
}
