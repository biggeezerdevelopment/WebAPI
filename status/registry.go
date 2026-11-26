package status

import (
	"encoding/json"
	"sync"
	"time"

	"webapi/config"
)

const (
	// KindCollector and KindPlain are the values of SourceSnapshot.Kind.
	KindCollector = "collector"
	KindPlain     = "plain"
	// HeartbeatType is the value of Heartbeat.Type.
	HeartbeatType = "heartbeat"
)

// Registry holds the status of every enabled source. It is safe for
// concurrent use.
type Registry struct {
	mu       sync.Mutex
	now      func() time.Time
	started  time.Time
	interval int
	sources  []*Source
	byName   map[string]*Source
}

// Source records one webapi entry's outcomes. Its methods are safe for
// concurrent use and do nothing on a nil *Source or an unknown output name.
type Source struct {
	reg           *Registry
	name          string
	kind          string
	collectorType string
	sleeptime     int

	lastPoll    time.Time
	lastSuccess time.Time
	lastError   string
	lastErrorAt time.Time
	errors      int

	outputs  []*outputState
	byOutput map[string]*outputState
}

type outputState struct {
	name          string
	lastDelivery  time.Time
	lastError     string
	lastErrorAt   time.Time
	failures      int
	checkpoint    time.Time
	hasCheckpoint bool
}

// NewRegistry builds a registry for cfg's enabled webapi entries, in config
// order, each with its routed outputs.
func NewRegistry(cfg *config.Config) *Registry {
	return newRegistry(cfg, time.Now)
}

func newRegistry(cfg *config.Config, now func() time.Time) *Registry {
	r := &Registry{
		now:      now,
		started:  now(),
		interval: cfg.Heartbeat.IntervalSeconds(),
		byName:   make(map[string]*Source),
	}
	for i := range cfg.WebAPI {
		entry := &cfg.WebAPI[i]
		if !entry.Enabled {
			continue
		}
		s := &Source{reg: r, name: entry.Name, kind: KindPlain, sleeptime: entry.SleepTime, byOutput: make(map[string]*outputState)}
		if entry.Collector != nil {
			s.kind = KindCollector
			s.collectorType = entry.Collector.Type
		}
		for _, name := range cfg.RoutesFor(entry) {
			o := &outputState{name: name}
			s.outputs = append(s.outputs, o)
			s.byOutput[name] = o
		}
		r.sources = append(r.sources, s)
		r.byName[entry.Name] = s
	}
	return r
}

// Source returns the named enabled source, or nil.
func (r *Registry) Source(name string) *Source {
	return r.byName[name]
}

func (s *Source) update(fn func(now time.Time)) {
	if s == nil {
		return
	}
	s.reg.mu.Lock()
	defer s.reg.mu.Unlock()
	fn(s.reg.now())
}

func (s *Source) updateOutput(output string, fn func(o *outputState, now time.Time)) {
	s.update(func(now time.Time) {
		if o := s.byOutput[output]; o != nil {
			fn(o, now)
		}
	})
}

func errorText(err error) string {
	if msg := Sanitize(err); msg != "" {
		return msg
	}
	return "unknown error"
}

// PollStarted records that a poll began.
func (s *Source) PollStarted() { s.update(func(now time.Time) { s.lastPoll = now }) }

// PollSucceeded records a successful pull.
func (s *Source) PollSucceeded() { s.update(func(now time.Time) { s.lastSuccess = now }) }

// PollFailed records a failed pull.
func (s *Source) PollFailed(err error) {
	msg := errorText(err)
	s.update(func(now time.Time) {
		s.errors++
		s.lastError = msg
		s.lastErrorAt = now
	})
}

// Delivered records a successful delivery to output.
func (s *Source) Delivered(output string) {
	s.updateOutput(output, func(o *outputState, now time.Time) { o.lastDelivery = now })
}

// DeliveryFailed records a failed delivery to output.
func (s *Source) DeliveryFailed(output string, err error) {
	msg := errorText(err)
	s.updateOutput(output, func(o *outputState, now time.Time) {
		o.failures++
		o.lastError = msg
		o.lastErrorAt = now
	})
}

// Checkpointed records a collector output's current checkpoint.
func (s *Source) Checkpointed(output string, at time.Time) {
	s.updateOutput(output, func(o *outputState, _ time.Time) {
		o.checkpoint = at
		o.hasCheckpoint = true
	})
}

// Heartbeat is one snapshot of the registry, serialized as the record data.
type Heartbeat struct {
	Type            string           `json:"type"`
	Healthy         bool             `json:"healthy"`
	StartedAt       string           `json:"started_at"`
	UptimeSeconds   int64            `json:"uptime_seconds"`
	IntervalSeconds int              `json:"interval_seconds"`
	Sources         []SourceSnapshot `json:"sources"`
	// At is when the snapshot was taken.
	At time.Time `json:"-"`
}

// SourceSnapshot is one source's status in a Heartbeat.
type SourceSnapshot struct {
	Name             string           `json:"name"`
	Kind             string           `json:"kind"`
	CollectorType    *string          `json:"collector_type"`
	SleeptimeSeconds int              `json:"sleeptime_seconds"`
	LastPollAt       *string          `json:"last_poll_at"`
	LastSuccessAt    *string          `json:"last_success_at"`
	ErrorsSinceLast  int              `json:"errors_since_last"`
	LastError        *string          `json:"last_error"`
	LastErrorAt      *string          `json:"last_error_at"`
	Outputs          []OutputSnapshot `json:"outputs"`
}

// OutputSnapshot is one output's delivery status in a SourceSnapshot.
type OutputSnapshot struct {
	Name                      string        `json:"name"`
	LastDeliveryAt            *string       `json:"last_delivery_at"`
	DeliveryFailuresSinceLast int           `json:"delivery_failures_since_last"`
	LastDeliveryError         *string       `json:"last_delivery_error"`
	LastDeliveryErrorAt       *string       `json:"last_delivery_error_at"`
	Checkpoint                *NullableTime `json:"checkpoint,omitempty"`
}

// NullableTime marshals as an RFC 3339 string, or null when Value is nil. A
// nil *NullableTime is omitted, which is how plain sources drop checkpoint.
type NullableTime struct {
	Value *string
}

// MarshalJSON implements json.Marshaler.
func (n NullableTime) MarshalJSON() ([]byte, error) {
	if n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Value)
}

// Snapshot copies every source's status and resets the counts since the last
// snapshot. Last-seen times and errors are kept. The heartbeat is unhealthy
// when any count since the last snapshot is above zero, or when a source or
// output has failed more recently than it last succeeded.
func (r *Registry) Snapshot() Heartbeat {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	hb := Heartbeat{
		Type:            HeartbeatType,
		Healthy:         true,
		StartedAt:       formatTime(r.started),
		UptimeSeconds:   int64(now.Sub(r.started) / time.Second),
		IntervalSeconds: r.interval,
		Sources:         make([]SourceSnapshot, 0, len(r.sources)),
		At:              now,
	}
	for _, s := range r.sources {
		snap := SourceSnapshot{
			Name:             s.name,
			Kind:             s.kind,
			SleeptimeSeconds: s.sleeptime,
			LastPollAt:       optTime(s.lastPoll),
			LastSuccessAt:    optTime(s.lastSuccess),
			ErrorsSinceLast:  s.errors,
			LastError:        optString(s.lastError),
			LastErrorAt:      optTime(s.lastErrorAt),
			Outputs:          make([]OutputSnapshot, 0, len(s.outputs)),
		}
		if s.kind == KindCollector {
			ct := s.collectorType
			snap.CollectorType = &ct
		}
		if s.errors > 0 || (!s.lastErrorAt.IsZero() && s.lastErrorAt.After(s.lastSuccess)) {
			hb.Healthy = false
		}
		s.errors = 0
		for _, o := range s.outputs {
			out := OutputSnapshot{
				Name:                      o.name,
				LastDeliveryAt:            optTime(o.lastDelivery),
				DeliveryFailuresSinceLast: o.failures,
				LastDeliveryError:         optString(o.lastError),
				LastDeliveryErrorAt:       optTime(o.lastErrorAt),
			}
			if s.kind == KindCollector {
				cp := &NullableTime{}
				if o.hasCheckpoint {
					cp.Value = optTime(o.checkpoint)
				}
				out.Checkpoint = cp
			}
			if o.failures > 0 || (!o.lastErrorAt.IsZero() && o.lastErrorAt.After(o.lastDelivery)) {
				hb.Healthy = false
			}
			o.failures = 0
			snap.Outputs = append(snap.Outputs, out)
		}
		hb.Sources = append(hb.Sources, snap)
	}
	return hb
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := formatTime(t)
	return &s
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
