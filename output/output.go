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
	"webapi/spool"
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

// ErrQueueFull reports a record dropped because an output's queue was full.
var ErrQueueFull = errors.New("queue is full")

// DeliveryObserver learns the outcome of each delivery by output name. It must
// be safe for concurrent use. Shutdown drops and canceled sends are not
// reported.
type DeliveryObserver interface {
	Delivered(output string)
	DeliveryFailed(output string, err error)
}

// retrying retries an Output with exponential backoff. The whole sequence for
// a record, including backoff, is bounded by timeout and the caller's context.
// Errors that repeat on every attempt, an oversized syslog message or a
// refused redirect, are not retried.
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
		if attempt > maxRetries || ctx.Err() != nil || isPermanent(err) {
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

// logRemoteOutput adapts the legacy apilogremote client. The router wraps it
// in retrying, like the named outputs, bounded by api_timeout.
type logRemoteOutput struct {
	client *api.LogClient
}

func (o *logRemoteOutput) Send(ctx context.Context, rec Record) error {
	return o.client.SendLog(ctx, rec.AgentID, rec.AgentName, rec.Source, rec.Data)
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

// Dispatcher delivers one source's records to its outputs. A spooled output
// is appended to inline, which is quick and never drops a record for a full
// queue. Of the others, a single output is called inline, which keeps the
// source paced by its destination. With several outputs each non-spooled one
// has its own bounded queue and worker, so a slow or failed output cannot
// delay another; when a queue is full the record is dropped for that output
// and the drop is logged.
type Dispatcher struct {
	source string
	inline []target
	lanes  []target
	queues []chan queuedRecord
	obs    DeliveryObserver

	mu     sync.RWMutex
	closed bool
	wg     sync.WaitGroup
}

func newDispatcher(source string, targets []target, queueSize int) *Dispatcher {
	return newObservedDispatcher(source, targets, queueSize, nil)
}

// newObservedDispatcher is newDispatcher with an optional observer.
func newObservedDispatcher(source string, targets []target, queueSize int, obs DeliveryObserver) *Dispatcher {
	d := &Dispatcher{source: source, obs: obs}
	for _, t := range targets {
		if isSpooled(t) || len(targets) < 2 {
			d.inline = append(d.inline, t)
		} else {
			d.lanes = append(d.lanes, t)
		}
	}
	d.queues = make([]chan queuedRecord, len(d.lanes))
	for i, t := range d.lanes {
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
	for _, t := range d.inline {
		d.send(ctx, t, rec)
	}
	if len(d.lanes) == 0 {
		return
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		log.Printf("Dropping record from source %q: dispatcher is closed", d.source)
		return
	}
	for i, queue := range d.queues {
		select {
		case queue <- queuedRecord{ctx: ctx, rec: rec}:
		default:
			log.Printf("Dropping record from source %q for output %q: queue is full", d.source, d.lanes[i].name)
			if d.obs != nil {
				d.obs.DeliveryFailed(d.lanes[i].name, ErrQueueFull)
			}
		}
	}
}

// send delivers to one target. A successful spool append is not a delivery;
// the spooler reports that when the record is sent.
func (d *Dispatcher) send(ctx context.Context, t target, rec Record) {
	err := t.out.Send(ctx, rec)
	if err != nil {
		log.Printf("Error delivering record from source %q to output %q: %v", d.source, t.name, err)
	}
	if d.obs == nil || ctx.Err() != nil {
		return
	}
	if err != nil {
		d.obs.DeliveryFailed(t.name, err)
	} else if !isSpooled(t) {
		d.obs.Delivered(t.name)
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
	// confirmedLogRemote replaces apilogremote in Routes. Its success means the
	// body reached the configured URL, which checkpointing callers rely on.
	confirmedLogRemote Output

	// spools holds the spooled outputs by name; spoolList is the same in
	// config order.
	spools     map[string]*spooler
	spoolList  []*spooler
	stopSpools context.CancelFunc
	spoolWG    sync.WaitGroup

	obsMu sync.Mutex
	// observers holds each plain source's observer by source key; dupKeys
	// are keys that more than one source has, which credit no source.
	observers map[string]DeliveryObserver
	dupKeys   map[string]bool
}

// RouterOptions changes how NewRouterWithOptions builds a Router.
type RouterOptions struct {
	// ReadOnly opens no spool, so nothing is written to state_dir; spooled
	// outputs are used directly. -check-config uses it.
	ReadOnly bool

	// Test hooks; zero values mean the defaults.
	newOutput    func(*config.LogOutput) (Output, error)
	retryBackoff time.Duration
	spoolMinWait time.Duration
	spoolMaxWait time.Duration
}

// NewRouter builds every enabled output from a validated configuration.
func NewRouter(cfg *config.Config) (*Router, error) {
	return NewRouterWithOptions(cfg, RouterOptions{})
}

// NewRouterWithOptions is NewRouter with options. It opens the spool of every
// spooled output and starts its worker; Close stops them.
func NewRouterWithOptions(cfg *config.Config, opts RouterOptions) (*Router, error) {
	if opts.newOutput == nil {
		opts.newOutput = newConfiguredOutput
	}
	if opts.spoolMinWait <= 0 {
		opts.spoolMinWait = defaultSpoolMinWait
	}
	if opts.spoolMaxWait <= 0 {
		opts.spoolMaxWait = defaultSpoolMaxWait
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Router{
		cfg:        cfg,
		outputs:    make(map[string]Output),
		spools:     make(map[string]*spooler),
		stopSpools: cancel,
		observers:  make(map[string]DeliveryObserver),
		dupKeys:    make(map[string]bool),
	}
	wrap := func(out Output, timeout time.Duration) *retrying {
		rt := newRetrying(out, timeout)
		if opts.retryBackoff > 0 {
			rt.backoff = opts.retryBackoff
		}
		return rt
	}
	if cfg.APILogRemote.Enabled {
		client := api.NewLogClient(&cfg.APILogRemote)
		timeout := time.Duration(cfg.APILogRemote.TimeoutSeconds()) * time.Second
		r.outputs[config.APILogRemoteOutputName] = wrap(&logRemoteOutput{client: client}, timeout)
		r.confirmedLogRemote = wrap(&logRemoteOutput{client: client.WithoutRedirects()}, timeout)
		if err := r.addSpool(ctx, opts, config.APILogRemoteOutputName, cfg.APILogRemote.Spool); err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("apilogremote: %w", err)
		}
	}
	for i := range cfg.LogOutputs {
		entry := &cfg.LogOutputs[i]
		if !entry.Enabled {
			continue
		}
		out, err := opts.newOutput(entry)
		if err == nil {
			r.outputs[entry.Name] = wrap(out, time.Duration(entry.Timeout)*time.Second)
			err = r.addSpool(ctx, opts, entry.Name, entry.Spool)
		}
		if err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("logoutputs[%d] (name %q): %w", i, entry.Name, err)
		}
	}
	return r, nil
}

// addSpool opens the named output's spool and starts its worker, unless the
// spool is off or the router is read-only.
func (r *Router) addSpool(ctx context.Context, opts RouterOptions, name string, cfg *config.Spool) error {
	if opts.ReadOnly || !cfg.IsEnabled() {
		return nil
	}
	sp, err := spool.Open(config.SpoolDir(r.cfg.StateDir, name), spool.Options{
		Name:     name,
		MaxBytes: cfg.MaxBytes(),
		MaxAge:   cfg.MaxAge(),
	})
	if err != nil {
		return fmt.Errorf("spool: %w", err)
	}
	s := &spooler{name: name, sp: sp, out: r.outputs[name], router: r, minWait: opts.spoolMinWait, maxWait: opts.spoolMaxWait}
	r.spools[name] = s
	r.spoolList = append(r.spoolList, s)
	r.spoolWG.Add(1)
	go func() {
		defer r.spoolWG.Done()
		s.run(ctx)
	}()
	return nil
}

// originOf returns source's index in the config, or -1 when source is not one
// of its entries.
func (r *Router) originOf(source *config.WebAPI) int {
	for i := range r.cfg.WebAPI {
		if &r.cfg.WebAPI[i] == source {
			return i
		}
	}
	return -1
}

// TakeSpoolStats returns each spool's state, apilogremote first and then in
// config order, and resets their drop counts.
func (r *Router) TakeSpoolStats() []SpoolStat {
	stats := make([]SpoolStat, 0, len(r.spoolList))
	for _, s := range r.spoolList {
		st := s.sp.TakeStats()
		stats = append(stats, SpoolStat{Output: s.name, Records: st.Records, Bytes: st.Bytes, Dropped: st.Dropped})
	}
	return stats
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
	return r.ObservedDispatcher(source, nil)
}

// ObservedDispatcher is Dispatcher with an observer told each delivery's
// outcome. obs may be nil. Spooled outputs are appended to, and their
// deliveries are reported to obs when the spool worker sends them. The
// source is identified by sourceKey, so records left on disk are credited
// to the right source after the config is reordered.
func (r *Router) ObservedDispatcher(source *config.WebAPI, obs DeliveryObserver) (*Dispatcher, error) {
	routes, err := r.namedOutputs(source)
	if err != nil {
		return nil, err
	}
	origin := r.originOf(source)
	key := sourceKey(source)
	var spooled []*spooler
	for i := range routes {
		if s := r.spools[routes[i].Name]; s != nil {
			spooled = append(spooled, s)
			routes[i].Output = &spoolAppender{s: s, origin: origin, key: key}
		}
	}
	if obs != nil {
		r.register(key, source.Name, obs, spooled)
	}
	return r.dispatcherFor(source.Name, routes, obs), nil
}

// HeartbeatDispatcher returns a dispatcher that sends heartbeat records named
// name to the given outputs.
func (r *Router) HeartbeatDispatcher(name string, outputs []string) (*Dispatcher, error) {
	routes, err := r.outputsNamed("heartbeat", outputs)
	if err != nil {
		return nil, err
	}
	return r.dispatcherFor(name, routes, nil), nil
}

func (r *Router) dispatcherFor(source string, routes []NamedOutput, obs DeliveryObserver) *Dispatcher {
	targets := make([]target, 0, len(routes))
	for _, route := range routes {
		targets = append(targets, target{name: route.Name, out: route.Output})
	}
	d := newObservedDispatcher(source, targets, laneQueueSize, obs)
	r.dispatchers = append(r.dispatchers, d)
	return d
}

// NamedOutput is an output with the name it is routed by.
type NamedOutput struct {
	Name   string
	Output Output
}

// Routes returns the source's outputs for callers that act on each delivery's
// result, which a Dispatcher does not report. A nil error from these outputs
// must not hide a redirect, so apilogremote refuses redirects here while plain
// sources keep its legacy behavior through their Dispatcher.
func (r *Router) Routes(source *config.WebAPI) ([]NamedOutput, error) {
	routes, err := r.namedOutputs(source)
	if err != nil {
		return nil, err
	}
	for i := range routes {
		if routes[i].Name == config.APILogRemoteOutputName {
			routes[i].Output = r.confirmedLogRemote
		}
	}
	return routes, nil
}

func (r *Router) namedOutputs(source *config.WebAPI) ([]NamedOutput, error) {
	return r.outputsNamed(fmt.Sprintf("webapi (name %q)", source.Name), r.cfg.RoutesFor(source))
}

// outputsNamed resolves output names. label prefixes errors.
func (r *Router) outputsNamed(label string, names []string) ([]NamedOutput, error) {
	routes := make([]NamedOutput, 0, len(names))
	for _, name := range names {
		out, ok := r.outputs[name]
		if !ok {
			return nil, fmt.Errorf("%s: output %q is not available", label, name)
		}
		routes = append(routes, NamedOutput{Name: name, Output: out})
	}
	return routes, nil
}

// Close drains all dispatchers, stops the spool workers (a send in progress
// is canceled and its record stays on disk), closes the spools and then the
// outputs.
func (r *Router) Close() error {
	for _, d := range r.dispatchers {
		d.Close()
	}
	r.stopSpools()
	r.spoolWG.Wait()
	var errs []error
	for _, s := range r.spoolList {
		if err := s.sp.Close(); err != nil {
			errs = append(errs, fmt.Errorf("output %q spool: %w", s.name, err))
		}
	}
	for name, out := range r.outputs {
		if err := out.Close(); err != nil {
			errs = append(errs, fmt.Errorf("output %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
