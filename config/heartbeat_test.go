package config

import (
	"strings"
	"testing"
)

func heartbeatTestConfig(h Heartbeat) Config {
	off := validOutput("off")
	off.Enabled = false
	return Config{
		LogOutputs: []LogOutput{validOutput("on"), off},
		WebAPI:     []WebAPI{validWebAPI()},
		Heartbeat:  h,
	}
}

func TestHeartbeatDefaults(t *testing.T) {
	var h Heartbeat
	if h.IntervalSeconds() != DefaultHeartbeatInterval {
		t.Errorf("interval = %d", h.IntervalSeconds())
	}
	if h.SourceName() != DefaultHeartbeatName {
		t.Errorf("name = %q", h.SourceName())
	}
	h = Heartbeat{Interval: 45, Name: "  agent-hb "}
	if h.IntervalSeconds() != 45 || h.SourceName() != "agent-hb" {
		t.Errorf("got %d %q", h.IntervalSeconds(), h.SourceName())
	}
}

func TestValidateHeartbeat(t *testing.T) {
	tests := []struct {
		name    string
		hb      Heartbeat
		wantErr string
	}{
		{name: "valid", hb: Heartbeat{Enabled: true, Outputs: []string{"on"}}},
		{name: "valid bounds low", hb: Heartbeat{Enabled: true, Interval: 30, Outputs: []string{"on"}}},
		{name: "valid bounds high", hb: Heartbeat{Enabled: true, Interval: 3600, Outputs: []string{"on"}}},
		{name: "disabled ignores everything", hb: Heartbeat{Interval: -5, Outputs: []string{"nope"}}},
		{name: "interval too low", hb: Heartbeat{Enabled: true, Interval: 29, Outputs: []string{"on"}}, wantErr: "interval must be 30-3600"},
		{name: "interval too high", hb: Heartbeat{Enabled: true, Interval: 3601, Outputs: []string{"on"}}, wantErr: "interval must be 30-3600"},
		{name: "negative interval", hb: Heartbeat{Enabled: true, Interval: -1, Outputs: []string{"on"}}, wantErr: "interval must be 30-3600"},
		{name: "no outputs", hb: Heartbeat{Enabled: true}, wantErr: "outputs is required"},
		{name: "empty outputs", hb: Heartbeat{Enabled: true, Outputs: []string{}}, wantErr: "outputs is required"},
		{name: "unknown output", hb: Heartbeat{Enabled: true, Outputs: []string{"nope"}}, wantErr: `unknown output "nope"`},
		{name: "disabled output", hb: Heartbeat{Enabled: true, Outputs: []string{"off"}}, wantErr: `output "off" is disabled`},
		{name: "disabled apilogremote", hb: Heartbeat{Enabled: true, Outputs: []string{APILogRemoteOutputName}}, wantErr: "is disabled"},
		{name: "repeated output", hb: Heartbeat{Enabled: true, Outputs: []string{"on", "on"}}, wantErr: "more than once"},
		{name: "name collides with webapi", hb: Heartbeat{Enabled: true, Name: "metrics", Outputs: []string{"on"}}, wantErr: `name "metrics" is already used by webapi[0]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := heartbeatTestConfig(tt.hb)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "heartbeat") || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadHeartbeatYAML(t *testing.T) {
	cfg, err := loadYAML(t, `
logoutputs:
  - name: "on"
    enabled: true
    type: "webhook"
    url: "https://hooks.example.com/audit"
    method: "POST"
    timeout: 5
heartbeat:
  enabled: true
  interval: 120
  name: "agent-hb"
  outputs: ["on"]
`)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Heartbeat
	if !h.Enabled || h.IntervalSeconds() != 120 || h.SourceName() != "agent-hb" || len(h.Outputs) != 1 || h.Outputs[0] != "on" {
		t.Fatalf("heartbeat = %+v", h)
	}
}

func TestHeartbeatUDPWarning(t *testing.T) {
	udp := validSyslog()
	udp.Name = "udp"
	udp.Network = SyslogNetworkUDP
	cfg := Config{LogOutputs: []LogOutput{udp}, Heartbeat: Heartbeat{Enabled: true, Outputs: []string{"udp"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.OutputWarnings(), "\n")
	if !strings.Contains(joined, `heartbeat routes to UDP output "udp"`) {
		t.Fatalf("warnings = %s", joined)
	}

	cfg.Heartbeat.Enabled = false
	if strings.Contains(strings.Join(cfg.OutputWarnings(), "\n"), "heartbeat") {
		t.Fatal("disabled heartbeat must not warn")
	}
}
