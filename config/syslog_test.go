package config

import (
	"fmt"
	"strings"
	"testing"

	"webapi/internal/testcerts"
	"webapi/tlsconfig"
)

func validSyslog() LogOutput {
	return LogOutput{
		Name: "soc", Enabled: true, Type: OutputTypeSyslog, Network: SyslogNetworkTLS,
		Address: "syslog.example.com:6514", Facility: "local0", Severity: "info", Timeout: 5,
	}
}

func TestLoadSyslogExample(t *testing.T) {
	b := testcerts.New(t)
	cfg, err := loadYAML(t, fmt.Sprintf(`
logoutputs:
  - name: "soc-syslog"
    enabled: true
    type: "syslog"
    network: "tls"
    address: "syslog.example.com:6514"
    facility: "local0"
    severity: "info"
    app_name: "webapi-agent"
    timeout: 10
    tls:
      ca_file: %q
      cert_file: %q
      key_file: %q
      server_name: "syslog.example.com"
      skip_verify: false
`, b.CAFile, b.ClientCertFile, b.ClientKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.LogOutputs[0]
	if got.Network != "tls" || got.Address != "syslog.example.com:6514" || got.Facility != "local0" ||
		got.Severity != "info" || got.AppName != "webapi-agent" || got.TLS.CAFile != b.CAFile {
		t.Errorf("decoded = %+v", got)
	}
	if warnings := cfg.OutputWarnings(); len(warnings) != 0 {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestValidateSyslog(t *testing.T) {
	b := testcerts.New(t)
	tests := []struct {
		name   string
		mutate func(*LogOutput)
		want   string
	}{
		{"unknown network", func(o *LogOutput) { o.Network = "sctp-hunter2" }, "unknown network"},
		{"unknown facility", func(o *LogOutput) { o.Facility = "local8-hunter2" }, "unknown facility"},
		{"unknown severity", func(o *LogOutput) { o.Severity = "loud-hunter2" }, "unknown severity"},
		{"missing port", func(o *LogOutput) { o.Address = "hunter2.example.com" }, "address"},
		{"port zero", func(o *LogOutput) { o.Address = "hunter2.example.com:0" }, "address"},
		{"port above range", func(o *LogOutput) { o.Address = "hunter2.example.com:65536" }, "address"},
		{"empty host", func(o *LogOutput) { o.Address = ":514" }, "address"},
		{"userinfo in host", func(o *LogOutput) { o.Address = "user-hunter2@example.com:514" }, "address"},
		{"unbracketed ipv6", func(o *LogOutput) { o.Address = "::1:514" }, "address"},
		{"zero timeout", func(o *LogOutput) { o.Timeout = 0 }, "timeout"},
		{"negative timeout", func(o *LogOutput) { o.Timeout = -1 }, "timeout"},
		{"timeout above maximum", func(o *LogOutput) { o.Timeout = MaxOutputTimeout + 1 }, "at most"},
		{"tls on tcp", func(o *LogOutput) { o.Network = "tcp"; o.TLS.CAFile = b.CAFile }, "require network tls"},
		{"tls on udp", func(o *LogOutput) { o.Network = "udp"; o.TLS.SkipVerify = true }, "require network tls"},
		{"cert without key", func(o *LogOutput) { o.TLS.CertFile = b.ClientCertFile }, "key_file"},
		{"unreadable CA", func(o *LogOutput) { o.TLS.CAFile = b.CAFile + ".missing" }, "ca_file"},
		{"mismatched pair", func(o *LogOutput) {
			o.TLS = tlsconfig.Options{CertFile: b.ClientCertFile, KeyFile: b.OtherKeyFile}
		}, "tls:"},
		{"skip verify with CA", func(o *LogOutput) {
			o.TLS = tlsconfig.Options{SkipVerify: true, CAFile: b.CAFile}
		}, "skip_verify"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := validSyslog()
			tt.mutate(&entry)
			err := (&Config{LogOutputs: []LogOutput{entry}}).Validate()
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			if !strings.Contains(msg, `logoutputs[0] (name "soc")`) || !strings.Contains(msg, tt.want) {
				t.Errorf("error = %q, want entry name and %q", msg, tt.want)
			}
			if strings.Contains(msg, "hunter2") {
				t.Errorf("error leaks a value: %q", msg)
			}
		})
	}
}

func TestValidateSyslogAcceptsAddresses(t *testing.T) {
	longestHost := strings.Repeat("a.", 126) + "b"
	for _, address := range []string{
		"localhost:514", "10.0.0.1:6514", "[::1]:6514", "[fe80::1%en0]:514", "[::ffff:10.0.0.1]:514",
		"log_host.example.com:1", "_syslog.example.com:514", "syslog.example.com.:514", "1a.example.com:514",
		"a-b.example.com:514", strings.Repeat("a", 63) + ".example.com:514",
		longestHost + ":514", longestHost + ".:514",
	} {
		entry := validSyslog()
		entry.Address = address
		if err := (&Config{LogOutputs: []LogOutput{entry}}).Validate(); err != nil {
			t.Errorf("%s: %v", address, err)
		}
	}
}

func TestValidateSyslogRejectsMalformedHosts(t *testing.T) {
	for _, address := range []string{
		"...:514", ".:514", "-:514", "_:514", "a..b:514", ".example.com:514", "example.com..:514",
		"-bad.example.com:514", "bad-.example.com:514", "bad_.example.com:514", "exa mple.com:514",
		"999.1.1.1:514", "1.2.3:514", "1.2.3.4.5:514", "010.0.0.1:514", "1.1.1.1.:514", "1234:514", "0x7f.0.0.1:514",
		"[example.com]:514", "[10.0.0.1]:514", "[]:514", "[::1:514",
		strings.Repeat("a", 64) + ".example.com:514",
		strings.Repeat("a.", 127) + "b:514",
	} {
		entry := validSyslog()
		entry.Address = address
		err := (&Config{LogOutputs: []LogOutput{entry}}).Validate()
		if err == nil {
			t.Errorf("%q: expected error", address)
			continue
		}
		if !strings.Contains(err.Error(), "address must be") || strings.Contains(err.Error(), address) {
			t.Errorf("%q: error = %q, want a value-free address error", address, err)
		}
	}
}

func TestSyslogEmptyTLSSectionRejectedOnPlaintext(t *testing.T) {
	_, err := loadYAML(t, `
logoutputs:
  - name: "plain"
    enabled: true
    type: "syslog"
    network: "tcp"
    address: "localhost:514"
    facility: "user"
    severity: "info"
    timeout: 5
    tls:
      skip_verify: false
`)
	if err == nil || !strings.Contains(err.Error(), "require network tls") {
		t.Fatalf("error = %v", err)
	}
}

func TestSyslogWarnings(t *testing.T) {
	tcp, udp, skip := validSyslog(), validSyslog(), validSyslog()
	tcp.Name, tcp.Network = "t", SyslogNetworkTCP
	udp.Name, udp.Network = "u", SyslogNetworkUDP
	skip.Name, skip.TLS.SkipVerify = "s", true
	warnings := (&Config{LogOutputs: []LogOutput{tcp, udp, skip}}).OutputWarnings()
	want := []string{"plaintext syslog over tcp", "plaintext syslog over udp", "skip_verify: true"}
	if len(warnings) != len(want) {
		t.Fatalf("warnings = %v", warnings)
	}
	for i := range want {
		if !strings.Contains(warnings[i], want[i]) {
			t.Errorf("warning %d = %q, want %q", i, warnings[i], want[i])
		}
	}
}

func TestSyslogPriority(t *testing.T) {
	for _, tt := range []struct {
		facility, severity string
		want               int
	}{{"kern", "emerg", 0}, {"local0", "info", 134}, {"local7", "debug", 191}} {
		if got, ok := SyslogPriority(tt.facility, tt.severity); !ok || got != tt.want {
			t.Errorf("%s.%s = %d, %v, want %d", tt.facility, tt.severity, got, ok, tt.want)
		}
	}
}
