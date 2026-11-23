package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"webapi/internal/testcerts"
	"webapi/tlsconfig"
)

func validOutput(name string) LogOutput {
	return LogOutput{
		Name: name, Enabled: true, Type: OutputTypeWebhook,
		URL: "https://user:hunter2@hooks.example.com/audit", Method: "POST", Timeout: 5,
	}
}

func loadYAML(t *testing.T, content string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestLoadIssueExample(t *testing.T) {
	b := testcerts.New(t)
	t.Setenv("SIEM_WEBHOOK_AUTH", "Bearer test-token")
	cfg, err := loadYAML(t, fmt.Sprintf(`
logoutputs:
  - name: "siem-webhook"
    enabled: true
    type: "webhook"
    url: "https://hooks.example.com/audit"
    method: "POST"
    timeout: 10
    headers:
      - key: "Content-Type"
        value: "application/json"
      - key: "Authorization"
        value_env: "SIEM_WEBHOOK_AUTH"
    tls:
      ca_file: %q
      cert_file: %q
      key_file: %q
      server_name: "hooks.internal"
webapi:
  - enabled: true
    name: "example"
    endpoint: "https://api.example.com/data"
    method: "get"
    sleeptime: 60
    timeout: 30
    outputs: ["siem-webhook"]
`, b.CAFile, b.ClientCertFile, b.ClientKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	out := cfg.LogOutputs[0]
	if out.Timeout != 10 || out.Headers[1].ValueEnv != "SIEM_WEBHOOK_AUTH" || out.TLS.ServerName != "hooks.internal" || out.TLS.CAFile != b.CAFile {
		t.Errorf("unexpected output: %+v", out)
	}
	if got := cfg.RoutesFor(&cfg.WebAPI[0]); !reflect.DeepEqual(got, []string{"siem-webhook"}) {
		t.Errorf("routes = %v", got)
	}
}

func TestRoutingDefaults(t *testing.T) {
	cfg, err := loadYAML(t, `
apilogremote:
  enabled: true
  api_url: "http://localhost:1/logs"
  api_method: "POST"
logoutputs:
  - {name: "a", enabled: true, type: "webhook", url: "https://a.example.com", method: "POST", timeout: 1}
  - {name: "off", enabled: false}
webapi:
  - {enabled: true, name: "omitted", endpoint: "http://localhost:1", method: "get", sleeptime: 1, timeout: 1}
  - {enabled: true, name: "empty", endpoint: "http://localhost:1", method: "get", sleeptime: 1, timeout: 1, outputs: []}
  - {enabled: true, name: "one", endpoint: "http://localhost:1", method: "get", sleeptime: 1, timeout: 1, outputs: ["apilogremote"]}
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RoutesFor(&cfg.WebAPI[0]); !reflect.DeepEqual(got, []string{"apilogremote", "a"}) {
		t.Errorf("omitted routes = %v", got)
	}
	if got := cfg.RoutesFor(&cfg.WebAPI[1]); len(got) != 0 {
		t.Errorf("empty routes = %v", got)
	}
	if got := cfg.RoutesFor(&cfg.WebAPI[2]); !reflect.DeepEqual(got, []string{"apilogremote"}) {
		t.Errorf("explicit routes = %v", got)
	}
}

func TestLegacyConfigCompatibility(t *testing.T) {
	cfg, err := loadYAML(t, `
apilogremote:
  enabled: true
  api_url: "http://localhost:1/logs"
  api_method: "POST"
webapi:
  - {enabled: true, name: "legacy", endpoint: "http://localhost:1", method: "get", sleeptime: 1, timeout: 1}
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RoutesFor(&cfg.WebAPI[0]); !reflect.DeepEqual(got, []string{"apilogremote"}) {
		t.Errorf("routes = %v", got)
	}
	if w := cfg.OutputWarnings(); len(w) != 0 {
		t.Errorf("legacy config produced warnings: %v", w)
	}

	cfg.APILogRemote.Enabled = false
	if got := cfg.RoutesFor(&cfg.WebAPI[0]); len(got) != 0 {
		t.Errorf("routes with logging disabled = %v", got)
	}
}

func TestValidateOutputs(t *testing.T) {
	b := testcerts.New(t)
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPUT_TEST_SET", "set-secret-value")
	t.Setenv("OUTPUT_TEST_EMPTY", "")
	t.Setenv("OUTPUT_TEST_NEWLINE", "set-secret-value\n")

	tests := []struct {
		name   string
		mutate func(*LogOutput)
		want   string
	}{
		{"missing name", func(o *LogOutput) { o.Name = "" }, "name is required"},
		{"blank name", func(o *LogOutput) { o.Name = "  " }, "name is required"},
		{"blank name when disabled", func(o *LogOutput) { o.Name = ""; o.Enabled = false }, "name is required"},
		{"reserved name", func(o *LogOutput) { o.Name = "apilogremote" }, "reserved"},
		{"reserved name when disabled", func(o *LogOutput) { o.Name = "APILogRemote"; o.Enabled = false }, "reserved"},
		{"unknown type", func(o *LogOutput) { o.Type = "syslog" }, "unknown type"},
		{"invalid url", func(o *LogOutput) { o.URL = "ftp://user:hunter2@example.com" }, "url must be"},
		{"invalid method", func(o *LogOutput) { o.Method = "FETCH" }, "invalid method"},
		{"bodyless method", func(o *LogOutput) { o.Method = "GET" }, "invalid method"},
		{"zero timeout", func(o *LogOutput) { o.Timeout = 0 }, "timeout"},
		{"negative timeout", func(o *LogOutput) { o.Timeout = -1 }, "timeout"},
		{"cert without key", func(o *LogOutput) { o.TLS.CertFile = b.ClientCertFile }, "key_file"},
		{"key without cert", func(o *LogOutput) { o.TLS.KeyFile = b.ClientKeyFile }, "cert_file"},
		{"unreadable CA", func(o *LogOutput) { o.TLS.CAFile = garbage + ".missing" }, "ca_file"},
		{"unparsable CA", func(o *LogOutput) { o.TLS.CAFile = garbage }, "ca_file"},
		{"unreadable cert", func(o *LogOutput) {
			o.TLS = tlsconfig.Options{CertFile: garbage + ".missing", KeyFile: b.ClientKeyFile}
		}, "client certificate"},
		{"unparsable key", func(o *LogOutput) {
			o.TLS = tlsconfig.Options{CertFile: b.ClientCertFile, KeyFile: garbage}
		}, "client certificate"},
		{"mismatched pair", func(o *LogOutput) {
			o.TLS = tlsconfig.Options{CertFile: b.ClientCertFile, KeyFile: b.OtherKeyFile}
		}, "client certificate"},
		{"skip verify with CA", func(o *LogOutput) {
			o.TLS = tlsconfig.Options{SkipVerify: true, CAFile: b.CAFile}
		}, "skip_verify"},
		{"unset env", func(o *LogOutput) {
			o.Headers = []OutputHeader{{Key: "Authorization", ValueEnv: "OUTPUT_TEST_UNSET"}}
		}, "OUTPUT_TEST_UNSET"},
		{"empty env", func(o *LogOutput) {
			o.Headers = []OutputHeader{{Key: "Authorization", ValueEnv: "OUTPUT_TEST_EMPTY"}}
		}, "OUTPUT_TEST_EMPTY"},
		{"value and env", func(o *LogOutput) {
			o.Headers = []OutputHeader{{Key: "Authorization", Value: "set-secret-value", ValueEnv: "OUTPUT_TEST_SET"}}
		}, "cannot both"},
		{"neither value nor env", func(o *LogOutput) {
			o.Headers = []OutputHeader{{Key: "Authorization"}}
		}, "required"},
		{"invalid header name", func(o *LogOutput) {
			o.Headers = []OutputHeader{{Key: "Bad Header", Value: "x"}}
		}, "invalid header name"},
		{"invalid env header value", func(o *LogOutput) {
			o.Headers = []OutputHeader{{Key: "Authorization", ValueEnv: "OUTPUT_TEST_NEWLINE"}}
		}, "invalid characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := validOutput("bad-output")
			tt.mutate(&entry)
			cfg := Config{LogOutputs: []LogOutput{validOutput("good"), entry}}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "logoutputs[1]") || !strings.Contains(msg, tt.want) {
				t.Errorf("error %q must name the entry and contain %q", msg, tt.want)
			}
			for _, secret := range []string{"hunter2", "hooks.example.com", "set-secret-value"} {
				if strings.Contains(msg, secret) {
					t.Errorf("error leaks %q: %s", secret, msg)
				}
			}
		})
	}
}

func TestValidateDuplicateOutputName(t *testing.T) {
	disabled := validOutput("dup")
	disabled.Enabled = false
	cfg := Config{LogOutputs: []LogOutput{validOutput("dup"), disabled}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "logoutputs[1]") || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateDisabledOutputIgnoresOtherFields(t *testing.T) {
	cfg := Config{LogOutputs: []LogOutput{{Name: "off", Type: "future"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRoutes(t *testing.T) {
	disabled := validOutput("off")
	disabled.Enabled = false

	tests := []struct {
		name   string
		routes []string
		want   string
	}{
		{"unknown", []string{"nope"}, "unknown output"},
		{"disabled", []string{"off"}, "disabled"},
		{"disabled apilogremote", []string{APILogRemoteOutputName}, "disabled"},
		{"repeated", []string{"on", "on"}, "more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := validWebAPI()
			source.Outputs = tt.routes
			cfg := Config{LogOutputs: []LogOutput{validOutput("on"), disabled}, WebAPI: []WebAPI{source}}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "webapi[0]") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v", err)
			}
		})
	}

	t.Run("disabled source is ignored", func(t *testing.T) {
		source := validWebAPI()
		source.Enabled = false
		source.Outputs = []string{"nope"}
		cfg := Config{WebAPI: []WebAPI{source}}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOutputWarnings(t *testing.T) {
	plain := validOutput("plain")
	plain.URL = "http://user:hunter2@hooks.example.com/"
	skip := validOutput("skip")
	skip.TLS.SkipVerify = true
	off := validOutput("off")
	off.Enabled = false
	off.URL = "http://hooks.example.com/"
	safe := validOutput("safe")

	cfg := Config{LogOutputs: []LogOutput{plain, skip, off, safe}}
	warnings := cfg.OutputWarnings()
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v", warnings)
	}
	if !strings.Contains(warnings[0], `"plain"`) || !strings.Contains(warnings[1], `"skip"`) {
		t.Errorf("warnings = %v", warnings)
	}
	if strings.Contains(strings.Join(warnings, " "), "hunter2") {
		t.Error("warning leaks URL")
	}
}
