package config

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// CollectorTypeGitHubAudit polls a GitHub organization audit log.
	CollectorTypeGitHubAudit = "github_audit"

	// DefaultInitialLookbackHours applies when initial_lookback_hours is omitted.
	DefaultInitialLookbackHours = 24

	// MaxInitialLookbackHours is 180 days. The bound keeps the first checkpoint
	// representable, in the past, and after 1970.
	MaxInitialLookbackHours = 4320
)

// Collector selects source-specific polling for a webapi entry.
type Collector struct {
	Type string `yaml:"type"`
	// InitialLookbackHours is a pointer so that an explicit 0 is rejected
	// rather than treated as omitted.
	InitialLookbackHours *int `yaml:"initial_lookback_hours"`
}

// LookbackHours returns the configured lookback or the default.
func (c *Collector) LookbackHours() int {
	if c.InitialLookbackHours == nil {
		return DefaultInitialLookbackHours
	}
	return *c.InitialLookbackHours
}

// UnmarshalYAML records which credential keys are present, and keeps a null
// collector key from silently turning the entry into a plain source.
func (w *WebAPI) UnmarshalYAML(node *yaml.Node) error {
	type plain WebAPI
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	var raw struct {
		APIKey    yaml.Node `yaml:"api_key"`
		APIKeyEnv yaml.Node `yaml:"api_key_env"`
		Collector yaml.Node `yaml:"collector"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*w = WebAPI(decoded)
	w.apiKeySet = raw.APIKey.Kind != 0
	w.apiKeyEnvSet = raw.APIKeyEnv.Kind != 0
	if raw.Collector.Kind != 0 && w.Collector == nil {
		w.Collector = &Collector{}
	}
	return nil
}

// ResolveAPIKey returns the inline key or the value of api_key_env. Errors name
// the variable but never the value.
func (w *WebAPI) ResolveAPIKey() (string, error) {
	if (w.apiKeySet && w.apiKeyEnvSet) || (w.APIKey != "" && w.APIKeyEnv != "") {
		return "", errors.New("api_key and api_key_env cannot both be set")
	}
	if w.APIKeyEnv == "" {
		if w.apiKeyEnvSet {
			return "", errors.New("api_key_env must name an environment variable")
		}
		return w.APIKey, nil
	}
	value := os.Getenv(w.APIKeyEnv)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("environment variable %q named by api_key_env is unset or empty", w.APIKeyEnv)
	}
	if !isHeaderValue(value) {
		return "", fmt.Errorf("environment variable %q named by api_key_env contains invalid characters", w.APIKeyEnv)
	}
	return value, nil
}

func (w *WebAPI) validateCollector(stateDir string) error {
	c := w.Collector
	if c.Type != CollectorTypeGitHubAudit {
		return fmt.Errorf("unknown collector.type %q", c.Type)
	}
	if err := checkStateDir(stateDir); err != nil {
		return err
	}
	if hours := c.LookbackHours(); hours < 1 || hours > MaxInitialLookbackHours {
		return fmt.Errorf("collector.initial_lookback_hours must be between 1 and %d, got %d", MaxInitialLookbackHours, hours)
	}
	if !strings.EqualFold(w.Method, http.MethodGet) {
		return errors.New("collector requires method get")
	}
	if !w.Body.IsEmpty() {
		return errors.New("collector does not accept a body")
	}
	key, err := w.ResolveAPIKey()
	if err != nil {
		return err
	}
	if w.AuthType != "apikey" || key == "" {
		return errors.New("collector requires auth_type apikey with api_key or api_key_env")
	}
	// The collector owns the cursor, so nothing may override or duplicate it.
	if u, err := url.Parse(w.Endpoint); err != nil || u.RawQuery != "" {
		return errors.New("collector endpoint must not contain a query string; use params")
	}
	for _, name := range []string{"after", "before"} {
		if _, ok := w.Params[name]; ok {
			return fmt.Errorf("collector manages the %q parameter", name)
		}
	}
	if order, ok := w.Params["order"]; ok && order != "asc" {
		return errors.New(`collector requires params.order to be "asc" or omitted`)
	}
	if strings.Contains(strings.ToLower(w.Params["phrase"]), "created:") {
		return errors.New("collector manages the created: qualifier in params.phrase")
	}
	return nil
}

// checkStateDir creates the directory if needed and proves it is writable. The
// path is left out of errors, consistent with other config values.
func checkStateDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("collector requires state_dir")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("state_dir cannot be created")
	}
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return errors.New("state_dir is not writable")
	}
	name := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(name)
	if closeErr != nil || removeErr != nil {
		return errors.New("state_dir is not writable")
	}
	return nil
}

// validateCollectors checks rules that span entries. Source names key the
// state files, so two collectors cannot share one.
func (c *Config) validateCollectors() error {
	seen := make(map[string]bool)
	for i := range c.WebAPI {
		entry := &c.WebAPI[i]
		if !entry.Enabled || entry.Collector == nil {
			continue
		}
		var err error
		name := strings.TrimSpace(entry.Name)
		switch {
		case seen[name]:
			err = errors.New("duplicate collector name")
		case len(c.RoutesFor(entry)) == 0:
			err = errors.New("collector requires at least one output")
		}
		if err != nil {
			return fmt.Errorf("webapi[%d] (name %q): %w", i, entry.Name, err)
		}
		seen[name] = true
	}
	return nil
}

// collectorWarnings flags routes that cannot confirm delivery.
func (c *Config) collectorWarnings() []string {
	udp := make(map[string]bool)
	for i := range c.LogOutputs {
		o := &c.LogOutputs[i]
		if o.Enabled && o.Type == OutputTypeSyslog && o.Network == SyslogNetworkUDP {
			udp[o.Name] = true
		}
	}
	var warnings []string
	for i := range c.WebAPI {
		entry := &c.WebAPI[i]
		if !entry.Enabled || entry.Collector == nil {
			continue
		}
		for _, name := range c.RoutesFor(entry) {
			if udp[name] {
				warnings = append(warnings, fmt.Sprintf("collector %q routes to UDP output %q, which cannot confirm delivery", entry.Name, name))
			}
		}
	}
	return warnings
}
