package config

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// DefaultHeartbeatInterval is used when heartbeat.interval is omitted.
	DefaultHeartbeatInterval = 300
	// MinHeartbeatInterval and MaxHeartbeatInterval bound heartbeat.interval.
	MinHeartbeatInterval = 30
	MaxHeartbeatInterval = 3600
	// DefaultHeartbeatName is the record Source when heartbeat.name is omitted.
	DefaultHeartbeatName = "heartbeat"
)

// Heartbeat configures the periodic liveness record sent to a SIEM.
type Heartbeat struct {
	Enabled  bool     `yaml:"enabled"`
	Interval int      `yaml:"interval"`
	Name     string   `yaml:"name"`
	Outputs  []string `yaml:"outputs"`
}

// IntervalSeconds returns the configured interval, or the default when unset.
func (h Heartbeat) IntervalSeconds() int {
	if h.Interval == 0 {
		return DefaultHeartbeatInterval
	}
	return h.Interval
}

// SourceName returns the record Source used for heartbeats.
func (h Heartbeat) SourceName() string {
	if name := strings.TrimSpace(h.Name); name != "" {
		return name
	}
	return DefaultHeartbeatName
}

func (c *Config) validateHeartbeat() error {
	h := &c.Heartbeat
	if !h.Enabled {
		return nil
	}
	if iv := h.IntervalSeconds(); iv < MinHeartbeatInterval || iv > MaxHeartbeatInterval {
		return fmt.Errorf("heartbeat: interval must be %d-%d seconds, got %d", MinHeartbeatInterval, MaxHeartbeatInterval, h.Interval)
	}
	if len(h.Outputs) == 0 {
		return errors.New("heartbeat: outputs is required")
	}
	enabled := c.outputStates()
	seen := make(map[string]bool, len(h.Outputs))
	for _, name := range h.Outputs {
		isEnabled, known := enabled[name]
		switch {
		case !known:
			return fmt.Errorf("heartbeat: unknown output %q", name)
		case !isEnabled:
			return fmt.Errorf("heartbeat: output %q is disabled", name)
		case seen[name]:
			return fmt.Errorf("heartbeat: output %q is listed more than once", name)
		}
		seen[name] = true
	}
	name := h.SourceName()
	for i := range c.WebAPI {
		if c.WebAPI[i].Enabled && strings.TrimSpace(c.WebAPI[i].Name) == name {
			return fmt.Errorf("heartbeat: name %q is already used by webapi[%d]", name, i)
		}
	}
	return nil
}

// heartbeatWarnings flags heartbeat routes that cannot confirm delivery.
func (c *Config) heartbeatWarnings() []string {
	if !c.Heartbeat.Enabled {
		return nil
	}
	var warnings []string
	for _, name := range c.Heartbeat.Outputs {
		for i := range c.LogOutputs {
			o := &c.LogOutputs[i]
			if o.Enabled && o.Name == name && o.Type == OutputTypeSyslog && o.Network == SyslogNetworkUDP {
				warnings = append(warnings, fmt.Sprintf("heartbeat routes to UDP output %q, which cannot confirm delivery", name))
			}
		}
	}
	return warnings
}
