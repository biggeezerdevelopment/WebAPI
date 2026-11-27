package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"webapi/api"
	"webapi/spool"
)

const (
	// defaultSpoolMinWait and defaultSpoolMaxWait bound the spool worker's
	// backoff while its output is failing.
	defaultSpoolMinWait = time.Second
	defaultSpoolMaxWait = 5 * time.Minute
)

// SpoolStat is one output spool's state for the heartbeat.
type SpoolStat struct {
	Output  string
	Records int
	Bytes   int64
	// Dropped counts records dropped since the last TakeSpoolStats.
	Dropped int
}

// isPermanent reports errors that would repeat on every attempt.
func isPermanent(err error) bool {
	return errors.Is(err, ErrSyslogMessageTooLarge) || errors.Is(err, api.ErrRedirect)
}

// spoolItem is what one spool frame holds. Origin is the config index of the
// plain source that appended it, or -1, and selects the observer told about
// its delivery.
type spoolItem struct {
	Origin int    `json:"origin"`
	Record Record `json:"record"`
}

// spooler owns one output's spool and the worker that empties it in order.
type spooler struct {
	name    string
	sp      *spool.Spool
	out     Output
	router  *Router
	minWait time.Duration
	maxWait time.Duration
}

func (s *spooler) append(origin int, rec Record) error {
	payload, err := json.Marshal(spoolItem{Origin: origin, Record: rec})
	if err != nil {
		return fmt.Errorf("spool: %w", err)
	}
	if err := s.sp.Append(payload); err != nil {
		return fmt.Errorf("spool: %w", err)
	}
	return nil
}

// run sends spooled records until ctx is canceled. A record is acked after a
// successful send or a permanent error; any other failure is retried with
// backoff, so outages delay records but never drop them. A send canceled by
// shutdown stays on disk for the next start.
func (s *spooler) run(ctx context.Context) {
	var wait time.Duration
	for {
		payload, err := s.sp.Peek(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, spool.ErrClosed) {
				return
			}
			log.Printf("Spool for output %q: %v", s.name, err)
			wait = s.nextWait(wait)
			if !sleepCtx(ctx, wait) {
				return
			}
			continue
		}
		var item spoolItem
		if err := json.Unmarshal(payload, &item); err != nil {
			log.Printf("Spool for output %q: skipping an unreadable record: %v", s.name, err)
			s.ack()
			continue
		}
		err = s.out.Send(ctx, item.Record)
		if ctx.Err() != nil {
			return
		}
		obs := s.router.observer(item.Origin)
		if err == nil {
			s.ack()
			wait = 0
			if obs != nil {
				obs.Delivered(s.name)
			}
			continue
		}
		log.Printf("Error delivering spooled record from source %q to output %q: %v", item.Record.Source, s.name, err)
		if obs != nil {
			obs.DeliveryFailed(s.name, err)
		}
		if isPermanent(err) {
			s.ack()
			continue
		}
		wait = s.nextWait(wait)
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

func (s *spooler) ack() {
	if err := s.sp.Ack(); err != nil && !errors.Is(err, spool.ErrClosed) {
		log.Printf("Spool for output %q: %v", s.name, err)
	}
}

func (s *spooler) nextWait(wait time.Duration) time.Duration {
	if wait == 0 {
		return s.minWait
	}
	return min(wait*2, s.maxWait)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// spoolAppender is the Dispatcher target for a spooled output: Send only
// appends, and the spooler reports the delivery later.
type spoolAppender struct {
	s      *spooler
	origin int
}

func (a *spoolAppender) Send(_ context.Context, rec Record) error { return a.s.append(a.origin, rec) }

func (a *spoolAppender) Close() error { return nil }

func isSpooled(t target) bool {
	_, ok := t.out.(*spoolAppender)
	return ok
}
