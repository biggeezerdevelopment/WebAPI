package config

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// CollectorTypeGitHubAudit polls a GitHub organization audit log.
	CollectorTypeGitHubAudit = "github_audit"
	// CollectorTypeAtlassianOrgAudit polls an Atlassian organization's audit
	// log through the events-stream API.
	CollectorTypeAtlassianOrgAudit = "atlassian_org_audit"
	// CollectorTypeJiraAudit polls a Jira Cloud site's audit records.
	CollectorTypeJiraAudit = "jira_audit"

	// AuthTypeAPIKey sends api_key as a bearer token.
	AuthTypeAPIKey = "apikey"
	// AuthTypeBasic sends api_username and api_password as basic auth.
	AuthTypeBasic = "basic"

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
		APIKey         yaml.Node `yaml:"api_key"`
		APIKeyEnv      yaml.Node `yaml:"api_key_env"`
		APIPassword    yaml.Node `yaml:"api_password"`
		APIPasswordEnv yaml.Node `yaml:"api_password_env"`
		Collector      yaml.Node `yaml:"collector"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*w = WebAPI(decoded)
	w.apiKeySet = raw.APIKey.Kind != 0
	w.apiKeyEnvSet = raw.APIKeyEnv.Kind != 0
	w.apiPasswordSet = raw.APIPassword.Kind != 0
	w.apiPasswordEnvSet = raw.APIPasswordEnv.Kind != 0
	if raw.Collector.Kind != 0 && w.Collector == nil {
		w.Collector = &Collector{}
	}
	return nil
}

// ResolveAPIKey returns the inline key or the value of api_key_env. Errors name
// the variable but never the value.
func (w *WebAPI) ResolveAPIKey() (string, error) {
	return resolveSecret("api_key", w.APIKey, w.apiKeySet, w.APIKeyEnv, w.apiKeyEnvSet)
}

// ResolveAPIPassword returns the inline password or the value of
// api_password_env. Errors name the variable but never the value.
func (w *WebAPI) ResolveAPIPassword() (string, error) {
	return resolveSecret("api_password", w.APIPassword, w.apiPasswordSet, w.APIPasswordEnv, w.apiPasswordEnvSet)
}

// resolveSecret implements the inline-or-environment rule for a credential
// key. The *Set flags record that YAML named the key, so an empty value still
// counts as set.
func resolveSecret(key, inline string, inlineSet bool, envName string, envSet bool) (string, error) {
	envKey := key + "_env"
	if (inlineSet && envSet) || (inline != "" && envName != "") {
		return "", fmt.Errorf("%s and %s cannot both be set", key, envKey)
	}
	if envName == "" {
		if envSet {
			return "", fmt.Errorf("%s must name an environment variable", envKey)
		}
		return inline, nil
	}
	value := os.Getenv(envName)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("environment variable %q named by %s is unset or empty", envName, envKey)
	}
	if !isHeaderValue(value) {
		return "", fmt.Errorf("environment variable %q named by %s contains invalid characters", envName, envKey)
	}
	return value, nil
}

// collectorRules holds the rules specific to one collector.type. authType is
// checked with CheckAuth before the shared endpoint check, and params after
// it, which keeps the order in which errors are reported.
type collectorRules struct {
	authType string
	params   func(w *WebAPI) error
}

// collectorValidators selects the per-type rules by collector.type.
var collectorValidators = map[string]collectorRules{
	CollectorTypeGitHubAudit:       {authType: AuthTypeAPIKey, params: validateGitHubAuditParams},
	CollectorTypeAtlassianOrgAudit: {authType: AuthTypeAPIKey, params: validateAtlassianOrgAuditParams},
	CollectorTypeJiraAudit:         {authType: AuthTypeBasic, params: validateJiraAuditParams},
}

// CheckAuth reports whether the entry is configured for authType with a
// credential that resolves. Collector types use it to state their auth rule.
func (w *WebAPI) CheckAuth(authType string) error {
	switch authType {
	case AuthTypeAPIKey:
		key, err := w.ResolveAPIKey()
		if err != nil {
			return err
		}
		if w.AuthType != AuthTypeAPIKey || key == "" {
			return errors.New("collector requires auth_type apikey with api_key or api_key_env")
		}
	case AuthTypeBasic:
		password, err := w.ResolveAPIPassword()
		if err != nil {
			return err
		}
		if w.AuthType != AuthTypeBasic || w.APIUsername == "" || password == "" {
			return errors.New("collector requires auth_type basic with api_username and api_password or api_password_env")
		}
	default:
		return fmt.Errorf("unsupported auth_type %q", authType)
	}
	return nil
}

// validateCollector applies the rules shared by every collector type and those
// of the entry's type.
func (w *WebAPI) validateCollector(stateDir string) error {
	c := w.Collector
	rules, ok := collectorValidators[c.Type]
	if !ok {
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
	if err := w.CheckAuth(rules.authType); err != nil {
		return err
	}
	// The collector owns the cursor, so nothing may override or duplicate it.
	if u, err := url.Parse(w.Endpoint); err != nil || u.RawQuery != "" {
		return errors.New("collector endpoint must not contain a query string; use params")
	}
	return rules.params(w)
}

// validateGitHubAuditParams leaves the created: qualifier and ascending order
// to the collector.
func validateGitHubAuditParams(w *WebAPI) error {
	for _, name := range []string{"after", "before"} {
		if _, ok := w.Params[name]; ok {
			return fmt.Errorf("collector manages the %q parameter", name)
		}
	}
	if order, ok := w.Params["order"]; ok && order != "asc" {
		return errors.New(`collector requires params.order to be "asc" or omitted`)
	}
	if perPage, ok := w.Params["per_page"]; ok {
		if n, err := strconv.Atoi(perPage); err != nil || n < 1 || n > 100 {
			return errors.New("collector requires params.per_page to be a whole number from 1 to 100")
		}
	}
	if strings.Contains(strings.ToLower(w.Params["phrase"]), "created:") {
		return errors.New("collector manages the created: qualifier in params.phrase")
	}
	return nil
}

// validateAtlassianOrgAuditParams leaves the window, cursor and ordering to
// the collector and bounds the page size.
func validateAtlassianOrgAuditParams(w *WebAPI) error {
	for _, name := range []string{"from", "to", "cursor", "sortOrder"} {
		if _, ok := w.Params[name]; ok {
			return fmt.Errorf("collector manages the %q parameter", name)
		}
	}
	if limit, ok := w.Params["limit"]; ok {
		if n, err := strconv.Atoi(limit); err != nil || n < 1 || n > 500 {
			return errors.New("collector requires params.limit to be a whole number from 1 to 500")
		}
	}
	return nil
}

// validateJiraAuditParams leaves the time range and offset to the collector
// and bounds the page size.
func validateJiraAuditParams(w *WebAPI) error {
	for _, name := range []string{"from", "to", "offset"} {
		if _, ok := w.Params[name]; ok {
			return fmt.Errorf("collector manages the %q parameter", name)
		}
	}
	if limit, ok := w.Params["limit"]; ok {
		if n, err := strconv.Atoi(limit); err != nil || n < 1 || n > 1000 {
			return errors.New("collector requires params.limit to be a whole number from 1 to 1000")
		}
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

// collectorWarnings flags routes that cannot confirm delivery. Syslog has no
// application acknowledgement on any transport: a stream write succeeds once
// the frame is in the local socket buffer.
func (c *Config) collectorWarnings() []string {
	syslogNetworks := make(map[string]string)
	for i := range c.LogOutputs {
		o := &c.LogOutputs[i]
		if o.Enabled && o.Type == OutputTypeSyslog {
			syslogNetworks[o.Name] = o.Network
		}
	}
	var warnings []string
	for i := range c.WebAPI {
		entry := &c.WebAPI[i]
		if !entry.Enabled || entry.Collector == nil {
			continue
		}
		for _, name := range c.RoutesFor(entry) {
			network, isSyslog := syslogNetworks[name]
			if !isSyslog {
				continue
			}
			if network == SyslogNetworkUDP {
				warnings = append(warnings, fmt.Sprintf("collector %q routes to UDP output %q, which cannot confirm delivery", entry.Name, name))
			} else {
				warnings = append(warnings, fmt.Sprintf("collector %q routes to syslog output %q, which has no delivery acknowledgement: events written during a silent network outage can be lost", entry.Name, name))
			}
		}
	}
	return warnings
}
