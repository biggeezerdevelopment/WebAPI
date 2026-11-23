// Package output delivers records from sources to named destinations.
package output

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"webapi/api"
	"webapi/config"
)

const (
	// maxRetries is the number of retries after the first failed attempt.
	maxRetries = 3
	// defaultBackoff is the delay before the first retry; it doubles each time.
	defaultBackoff = 200 * time.Millisecond
	// laneQueueSize bounds the records held in memory per source and output.
	laneQueueSize = 32
)

// Record is one result collected from a source. Data is nil after a failed
// poll and must be treated as read-only because outputs share it.
type Record struct {
	AgentID   string
	AgentName string
	Source    string
	Timestamp time.Time
	Data      []byte
}

// Output is a delivery destination. Send must be safe for concurrent use.
type Output interface {
	Send(ctx context.Context, rec Record) error
	Close() error
}

// retrying retries an Output with exponential backoff. The whole sequence for
// a record, including backoff, is bounded by timeout and the caller's context.
type retrying struct {
	Output
	timeout time.Duration
	backoff time.Duration
}

func newRetrying(out Output, timeout time.Duration) *retrying {
	return &retrying{Output: out, timeout: timeout, backoff: defaultBackoff}
}

func (r *retrying) Send(ctx context.Context, rec Record) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	delay := r.backoff
	for attempt := 1; ; attempt++ {
		err := r.Output.Send(ctx, rec)
		if err == nil {
			return nil
		}
		if attempt > maxRetries || ctx.Err() != nil || errors.Is(err, ErrSyslogMessageTooLarge) {
			return fmt.Errorf("giving up after %d attempt(s): %w", attempt, err)
		}

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("giving up after %d attempt(s): %w", attempt, err)
		}
		delay *= 2
	}
}

// logRemoteOutput adapts the legacy apilogremote client. It is deliberately
// not retried so that existing configurations behave as they always have.
type logRemoteOutput struct {
	client *api.LogClient
}

func (o *logRemoteOutput) Send(_ context.Context, rec Record) error {
	return o.client.SendLog(rec.AgentID, rec.AgentName, rec.Source, rec.Data)
}

func (o *logRemoteOutput) Close() error { return nil }

type target struct {
	name string
	out  Output
}

type queuedRecord struct {
	ctx context.Context
	rec Record
}

// Dispatcher delivers one source's records to its outputs. A single output is
// called inline, which keeps the source paced by its destination. With several
// outputs each has its own bounded queue and worker, so a slow or failed
// output cannot delay another; when a queue is full the record is dropped for
// that output and the drop is logged.
type Dispatcher struct {
	source  string
	targets []target
	queues  []chan queuedRecord

	mu     sync.RWMutex
	closed bool
	wg     sync.WaitGroup
}

func newDispatcher(source string, targets []target, queueSize int) *Dispatcher {
	d := &Dispatcher{source: source, targets: targets}
	if len(targets) < 2 {
		return d
	}
	d.queues = make([]chan queuedRecord, len(targets))
	for i, t := range targets {
		queue := make(chan queuedRecord, queueSize)
		d.queues[i] = queue
		d.wg.Add(1)
		go func(t target) {
			defer d.wg.Done()
			for item := range queue {
				d.send(item.ctx, t, item.rec)
			}
		}(t)
	}
	return d
}

// Deliver hands rec to every output. Failures are logged, not returned, because
// delivery is best-effort for the polling source.
func (d *Dispatcher) Deliver(ctx context.Context, rec Record) {
	if len(d.targets) == 1 {
		d.send(ctx, d.targets[0], rec)
		return
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		if len(d.targets) > 0 {
			log.Printf("Dropping record from source %q: dispatcher is closed", d.source)
		}
		return
	}
	for i, queue := range d.queues {
		select {
		case queue <- queuedRecord{ctx: ctx, rec: rec}:
		default:
			log.Printf("Dropping record from source %q for output %q: queue is full", d.source, d.targets[i].name)
		}
	}
}

func (d *Dispatcher) send(ctx context.Context, t target, rec Record) {
	if err := t.out.Send(ctx, rec); err != nil {
		log.Printf("Error delivering record from source %q to output %q: %v", d.source, t.name, err)
	}
}

// Close stops accepting records and waits for queued ones to be attempted.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	for _, queue := range d.queues {
		close(queue)
	}
	d.mu.Unlock()
	d.wg.Wait()
}

// Router owns the configured outputs and the dispatchers that use them.
type Router struct {
	cfg         *config.Config
	outputs     map[string]Output
	dispatchers []*Dispatcher
}

// NewRouter builds every enabled output from a validated configuration.
func NewRouter(cfg *config.Config) (*Router, error) {
	r := &Router{cfg: cfg, outputs: make(map[string]Output)}
	if cfg.APILogRemote.Enabled {
		r.outputs[config.APILogRemoteOutputName] = &logRemoteOutput{
			client: api.NewLogClient(&cfg.APILogRemote),
		}
	}
	for i := range cfg.LogOutputs {
		entry := &cfg.LogOutputs[i]
		if !entry.Enabled {
			continue
		}
		out, err := newConfiguredOutput(entry)
		if err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("logoutputs[%d] (name %q): %w", i, entry.Name, err)
		}
		r.outputs[entry.Name] = newRetrying(out, time.Duration(entry.Timeout)*time.Second)
	}
	return r, nil
}

// newConfiguredOutput builds the output for a validated entry.
func newConfiguredOutput(entry *config.LogOutput) (Output, error) {
	if entry.Type == config.OutputTypeSyslog {
		return NewSyslog(entry)
	}
	return NewWebhook(entry)
}

// Dispatcher returns a new dispatcher for the source's configured routes.
func (r *Router) Dispatcher(source *config.WebAPI) (*Dispatcher, error) {
	routes, err := r.Routes(source)
	if err != nil {
		return nil, err
	}
	targets := make([]target, 0, len(routes))
	for _, route := range routes {
		targets = append(targets, target{name: route.Name, out: route.Output})
	}
	d := newDispatcher(source.Name, targets, laneQueueSize)
	r.dispatchers = append(r.dispatchers, d)
	return d, nil
}

// NamedOutput is an output with the name it is routed by.
type NamedOutput struct {
	Name   string
	Output Output
}

// Routes returns the source's outputs for callers that need each delivery's
// result, which a Dispatcher does not report.
func (r *Router) Routes(source *config.WebAPI) ([]NamedOutput, error) {
	names := r.cfg.RoutesFor(source)
	routes := make([]NamedOutput, 0, len(names))
	for _, name := range names {
		out, ok := r.outputs[name]
		if !ok {
			return nil, fmt.Errorf("webapi (name %q): output %q is not available", source.Name, name)
		}
		routes = append(routes, NamedOutput{Name: name, Output: out})
	}
	return routes, nil
}

// Close drains all dispatchers and then closes the outputs.
func (r *Router) Close() error {
	for _, d := range r.dispatchers {
		d.Close()
	}
	var errs []error
	for name, out := range r.outputs {
		if err := out.Close(); err != nil {
			errs = append(errs, fmt.Errorf("output %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
