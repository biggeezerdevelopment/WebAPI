package output

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"webapi/api"
	"webapi/config"
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

// spoolItem is what one spool frame holds. SourceKey identifies the plain
// source that appended it and selects the observer told about its delivery.
// Origin, the source's config index, is still written so an older version
// can read the spool, but it is no longer used: it changes when sources are
// reordered.
type spoolItem struct {
	Origin    int    `json:"origin"`
	SourceKey string `json:"source_key,omitempty"`
	Record    Record `json:"record"`
}

// sourceKey identifies a plain source across restarts and config edits by
// its name, subject, endpoint and method.
func sourceKey(src *config.WebAPI) string {
	h := sha256.New()
	for _, part := range []string{
		strings.TrimSpace(src.Name),
		strings.TrimSpace(src.Subject),
		strings.TrimSpace(src.Endpoint),
		strings.ToLower(strings.TrimSpace(src.Method)),
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// spooler owns one output's spool and the worker that empties it in order.
type spooler struct {
	name    string
	sp      *spool.Spool
	out     Output
	router  *Router
	minWait time.Duration
	maxWait time.Duration
	// routed holds the observers of the sources routed to this output. It is
	// guarded by router.obsMu.
	routed []DeliveryObserver
}

func (s *spooler) append(origin int, key string, rec Record) error {
	payload, err := json.Marshal(spoolItem{Origin: origin, SourceKey: key, Record: rec})
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
			log.Printf("Spool for output %q: skipping an unreadable record", s.name)
			s.ack()
			continue
		}
		err = s.out.Send(ctx, item.Record)
		if ctx.Err() != nil {
			return
		}
		obs := s.router.observer(item.SourceKey)
		if err == nil {
			s.ack()
			wait = 0
			if obs != nil {
				obs.Delivered(s.name)
			}
			continue
		}
		log.Printf("Error delivering spooled record from source %q to output %q: %v", item.Record.Source, s.name, err)
		s.reportFailure(obs, err)
		if isPermanent(err) {
			s.ack()
			wait = 0
			continue
		}
		wait = s.nextWait(wait)
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// reportFailure tells the item's source about a failed send. An item that
// matches no single source (written by an older version, or by a source that
// is gone or ambiguous) is reported to every source routed to this output, so
// that a failing output is never hidden from the heartbeat. Such an item's
// success credits no source, while its failure reaches all of them.
func (s *spooler) reportFailure(obs DeliveryObserver, err error) {
	if obs != nil {
		obs.DeliveryFailed(s.name, err)
		return
	}
	for _, o := range s.router.routedTo(s) {
		o.DeliveryFailed(s.name, err)
	}
}

// register records obs as the observer for key, and as routed to each of
// spools. Observers are compared with != and slices.Contains, so they must be
// comparable types, such as pointers. A key that a second source also has credits neither of them.
func (r *Router) register(key, name string, obs DeliveryObserver, spools []*spooler) {
	r.obsMu.Lock()
	defer r.obsMu.Unlock()
	for _, s := range spools {
		if !slices.Contains(s.routed, obs) {
			s.routed = append(s.routed, obs)
		}
	}
	if r.dupKeys[key] {
		return
	}
	if prev, ok := r.observers[key]; ok && prev != obs {
		delete(r.observers, key)
		r.dupKeys[key] = true
		log.Printf("WARNING: more than one source named %q has the same subject, endpoint and method; their spooled deliveries are not credited to either", name)
		return
	}
	r.observers[key] = obs
}

// observer returns the observer for a source key, if exactly one source has it.
func (r *Router) observer(key string) DeliveryObserver {
	if key == "" {
		return nil
	}
	r.obsMu.Lock()
	defer r.obsMu.Unlock()
	return r.observers[key]
}

// routedTo returns the observers of the sources routed to s.
func (r *Router) routedTo(s *spooler) []DeliveryObserver {
	r.obsMu.Lock()
	defer r.obsMu.Unlock()
	return slices.Clone(s.routed)
}

// ack acknowledges the current record. The spool logs the first failure of a
// run of failed saves, so a long disk-full outage does not fill the log; a
// closed spool is a shutdown and is not reported.
func (s *spooler) ack() {
	// Any error is already logged by the spool; ErrClosed needs no log.
	_ = s.sp.Ack()
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
	key    string
}

func (a *spoolAppender) Send(_ context.Context, rec Record) error {
	return a.s.append(a.origin, a.key, rec)
}

func (a *spoolAppender) Close() error { return nil }

func isSpooled(t target) bool {
	_, ok := t.out.(*spoolAppender)
	return ok
}
