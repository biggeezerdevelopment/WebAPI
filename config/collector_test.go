package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const collectorTemplate = `
state_dir: %STATE%
logoutputs:
  - name: "hook"
    enabled: true
    type: "webhook"
    url: "https://hooks.example.com/audit"
    method: "POST"
    timeout: 5
webapi:
  - enabled: true
    name: "github-audit"
    endpoint: "https://api.github.com/orgs/example-org/audit-log"
    method: "get"
    auth_type: "apikey"
    %AUTH%
    sleeptime: 60
    timeout: 30
    params:
      include: "all"
      per_page: "100"
      order: "asc"
    collector:
      type: "%TYPE%"
      %LOOKBACK%
    outputs: ["hook"]
`

type collectorCase struct {
	state, auth, ctype, lookback string
}

func loadCollector(t *testing.T, c collectorCase) (*Config, error) {
	t.Helper()
	if c.state == "" {
		c.state = filepath.ToSlash(t.TempDir())
	}
	if c.auth == "" {
		c.auth = `api_key_env: "TEST_AUDIT_TOKEN"`
	}
	if c.ctype == "" {
		c.ctype = CollectorTypeGitHubAudit
	}
	text := strings.NewReplacer("%STATE%", `"`+c.state+`"`, "%AUTH%", c.auth, "%TYPE%", c.ctype, "%LOOKBACK%", c.lookback).Replace(collectorTemplate)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestCollectorConfigLoads(t *testing.T) {
	t.Setenv("TEST_AUDIT_TOKEN", "secret-token-value")
	cfg, err := loadCollector(t, collectorCase{lookback: "initial_lookback_hours: 24"})
	if err != nil {
		t.Fatal(err)
	}
	entry := &cfg.WebAPI[0]
	if entry.Collector.LookbackHours() != 24 {
		t.Errorf("lookback = %d", entry.Collector.LookbackHours())
	}
	if key, err := entry.ResolveAPIKey(); err != nil || key != "secret-token-value" {
		t.Errorf("key resolution failed: %v", err)
	}
}

func TestCollectorLookbackBounds(t *testing.T) {
	t.Setenv("TEST_AUDIT_TOKEN", "secret-token-value")
	cases := map[string]bool{
		"":                             true,
		"initial_lookback_hours: 1":    true,
		"initial_lookback_hours: 4320": true,
		"initial_lookback_hours: 0":    false,
		"initial_lookback_hours: -1":   false,
		"initial_lookback_hours: 4321": false,
	}
	for lookback, valid := range cases {
		_, err := loadCollector(t, collectorCase{lookback: lookback})
		if valid != (err == nil) {
			t.Errorf("%q: err = %v", lookback, err)
		}
		if err != nil && !strings.Contains(err.Error(), "github-audit") {
			t.Errorf("%q: error does not name the entry: %v", lookback, err)
		}
	}
}

func TestCollectorValidationErrors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_AUDIT_TOKEN", "secret-token-value")
	t.Setenv("TEST_EMPTY_TOKEN", "")
	os.Unsetenv("TEST_UNSET_TOKEN")

	cases := map[string]struct {
		c    collectorCase
		want string
	}{
		"unknown type":  {collectorCase{ctype: "gitlab"}, "unknown collector.type"},
		"state is file": {collectorCase{state: filepath.ToSlash(blocker)}, "state_dir"},
		"blank state":   {collectorCase{state: "   "}, "state_dir"},
		"unset env":     {collectorCase{auth: `api_key_env: "TEST_UNSET_TOKEN"`}, "unset or empty"},
		"empty env":     {collectorCase{auth: `api_key_env: "TEST_EMPTY_TOKEN"`}, "unset or empty"},
		"both keys": {collectorCase{auth: `api_key: "inline-secret"
    api_key_env: "TEST_AUDIT_TOKEN"`}, "cannot both"},
		"both keys, one empty": {collectorCase{auth: `api_key: ""
    api_key_env: "TEST_AUDIT_TOKEN"`}, "cannot both"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadCollector(t, tc.c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			msg := err.Error()
			if !strings.Contains(msg, "github-audit") {
				t.Errorf("error does not name the entry: %s", msg)
			}
			for _, leak := range []string{"secret", "https://", "api.github.com"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error leaks %q: %s", leak, msg)
				}
			}
		})
	}
}

func TestCollectorRequiresStateDirKey(t *testing.T) {
	t.Setenv("TEST_AUDIT_TOKEN", "secret-token-value")
	cfg := Config{WebAPI: []WebAPI{{
		Enabled: true, Name: "a", Endpoint: "https://api.github.com/x", Method: "get",
		AuthType: "apikey", APIKeyEnv: "TEST_AUDIT_TOKEN", SleepTime: 1, Timeout: 1,
		Collector: &Collector{Type: CollectorTypeGitHubAudit},
	}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "state_dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestCollectorRejectsCursorOverridesAndEmptyRoutes(t *testing.T) {
	t.Setenv("TEST_AUDIT_TOKEN", "secret-token-value")
	base := func() Config {
		return Config{StateDir: t.TempDir(), LogOutputs: []LogOutput{{
			Name: "hook", Enabled: true, Type: OutputTypeWebhook,
			URL: "https://hooks.example.com/a", Method: "POST", Timeout: 5,
		}}, WebAPI: []WebAPI{{
			Enabled: true, Name: "a", Endpoint: "https://api.github.com/x", Method: "get",
			AuthType: "apikey", APIKeyEnv: "TEST_AUDIT_TOKEN", SleepTime: 1, Timeout: 1,
			Collector: &Collector{Type: CollectorTypeGitHubAudit}, Outputs: []string{"hook"},
		}}}
	}
	if cfg := base(); cfg.Validate() != nil {
		t.Fatalf("base config invalid: %v", cfg.Validate())
	}
	mutations := map[string]func(*Config){
		"after":        func(c *Config) { c.WebAPI[0].Params = map[string]string{"after": "x"} },
		"desc":         func(c *Config) { c.WebAPI[0].Params = map[string]string{"order": "desc"} },
		"created":      func(c *Config) { c.WebAPI[0].Params = map[string]string{"phrase": "created:>2020-01-01"} },
		"per_page 0":   func(c *Config) { c.WebAPI[0].Params = map[string]string{"per_page": "0"} },
		"per_page 101": func(c *Config) { c.WebAPI[0].Params = map[string]string{"per_page": "101"} },
		"per_page x":   func(c *Config) { c.WebAPI[0].Params = map[string]string{"per_page": "many"} },
		"query":        func(c *Config) { c.WebAPI[0].Endpoint += "?phrase=x" },
		"post":         func(c *Config) { c.WebAPI[0].Method = "post" },
		"no auth":      func(c *Config) { c.WebAPI[0].AuthType = "" },
		"no outputs":   func(c *Config) { c.WebAPI[0].Outputs = []string{} },
		"duplicate":    func(c *Config) { c.WebAPI = append(c.WebAPI, c.WebAPI[0]) },
	}
	for name, mutate := range mutations {
		cfg := base()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The per-type auth rule runs before the shared endpoint check, so an entry
// breaking both reports the auth error, as it did before the split.
func TestCollectorAuthErrorPrecedesQueryStringError(t *testing.T) {
	t.Setenv("TEST_AUDIT_TOKEN", "secret-token-value")
	w := &WebAPI{
		Enabled: true, Name: "a", Endpoint: "https://api.github.com/x?phrase=x", Method: "get",
		APIKeyEnv: "TEST_AUDIT_TOKEN", SleepTime: 1, Timeout: 1,
		Collector: &Collector{Type: CollectorTypeGitHubAudit},
	}
	err := w.validateCollector(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "auth_type apikey") {
		t.Fatalf("err = %v, want the auth error", err)
	}
}

func TestNullCollectorIsNotAPlainSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	text := "webapi:\n  - enabled: true\n    name: a\n    endpoint: https://example.com/x\n    method: get\n    sleeptime: 1\n    timeout: 1\n    collector:\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown collector.type") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlainSourceMayUseAPIKeyEnv(t *testing.T) {
	t.Setenv("TEST_AUDIT_TOKEN", "secret-token-value")
	w := WebAPI{APIKeyEnv: "TEST_AUDIT_TOKEN"}
	if key, err := w.ResolveAPIKey(); err != nil || key != "secret-token-value" {
		t.Fatalf("resolve failed: %v", err)
	}
}
