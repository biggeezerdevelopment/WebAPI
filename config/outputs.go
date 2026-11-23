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

	"webapi/tlsconfig"
)

const (
	// OutputTypeWebhook delivers records as HTTP requests.
	OutputTypeWebhook = "webhook"

	// OutputTypeSyslog delivers records to a remote syslog server.
	OutputTypeSyslog = "syslog"

	// APILogRemoteOutputName is the reserved name of the output backed by the
	// legacy apilogremote section.
	APILogRemoteOutputName = "apilogremote"

	// MaxOutputTimeout is the largest accepted output timeout in seconds. It
	// keeps the conversion to time.Duration from overflowing.
	MaxOutputTimeout = 3600
)

// webhookMethods lists the methods that can carry the JSON record body.
var webhookMethods = map[string]bool{
	http.MethodPost:  true,
	http.MethodPut:   true,
	http.MethodPatch: true,
}

// OutputHeader is a request header whose value is given inline or read from an
// environment variable.
type OutputHeader struct {
	Key      string `yaml:"key"`
	Value    string `yaml:"value"`
	ValueEnv string `yaml:"value_env"`

	// bothSet records that YAML named both keys, even with an empty value,
	// which the decoded strings alone cannot show.
	bothSet bool
}

// UnmarshalYAML keeps track of which value keys are present.
func (h *OutputHeader) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		Key      string    `yaml:"key"`
		Value    yaml.Node `yaml:"value"`
		ValueEnv yaml.Node `yaml:"value_env"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*h = OutputHeader{Key: raw.Key, bothSet: raw.Value.Kind != 0 && raw.ValueEnv.Kind != 0}
	if raw.Value.Kind != 0 {
		if err := raw.Value.Decode(&h.Value); err != nil {
			return fmt.Errorf("header %q: value must be a string", h.Key)
		}
	}
	if raw.ValueEnv.Kind != 0 {
		if err := raw.ValueEnv.Decode(&h.ValueEnv); err != nil {
			return fmt.Errorf("header %q: value_env must be a string", h.Key)
		}
	}
	return nil
}

// Resolve returns the header value. Errors name the header and variable but
// never the value.
func (h OutputHeader) Resolve() (string, error) {
	if h.bothSet || (h.Value != "" && h.ValueEnv != "") {
		return "", fmt.Errorf("header %q: value and value_env cannot both be set", h.Key)
	}
	if h.ValueEnv == "" {
		if h.Value == "" {
			return "", fmt.Errorf("header %q: value or value_env is required", h.Key)
		}
		return h.Value, nil
	}
	value := os.Getenv(h.ValueEnv)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("header %q: environment variable %q is unset or empty", h.Key, h.ValueEnv)
	}
	return value, nil
}

// LogOutput is a named destination for records.
type LogOutput struct {
	Name    string            `yaml:"name"`
	Enabled bool              `yaml:"enabled"`
	Type    string            `yaml:"type"`
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Timeout int               `yaml:"timeout"`
	Headers []OutputHeader    `yaml:"headers"`
	TLS     tlsconfig.Options `yaml:"tls"`

	// Syslog settings.
	Network  string `yaml:"network"`
	Address  string `yaml:"address"`
	Facility string `yaml:"facility"`
	Severity string `yaml:"severity"`
	AppName  string `yaml:"app_name"`

	// tlsSet records that YAML had a tls key, even one holding only zero
	// values, which the decoded options alone cannot show.
	tlsSet bool
}

// UnmarshalYAML keeps track of whether a tls section is present.
func (o *LogOutput) UnmarshalYAML(node *yaml.Node) error {
	type plain LogOutput
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	var raw struct {
		TLS yaml.Node `yaml:"tls"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*o = LogOutput(decoded)
	o.tlsSet = raw.TLS.Kind != 0
	return nil
}

func (o *LogOutput) validate() error {
	switch o.Type {
	case OutputTypeWebhook:
		return o.validateWebhook()
	case OutputTypeSyslog:
		return o.validateSyslog()
	default:
		return fmt.Errorf("unknown type %q", o.Type)
	}
}

func (o *LogOutput) validateTimeout() error {
	if o.Timeout <= 0 {
		return fmt.Errorf("timeout must be greater than 0, got %d", o.Timeout)
	}
	if o.Timeout > MaxOutputTimeout {
		return fmt.Errorf("timeout must be at most %d seconds, got %d", MaxOutputTimeout, o.Timeout)
	}
	return nil
}

func (o *LogOutput) validateWebhook() error {
	if !isHTTPURL(o.URL) {
		return errors.New("url must be an absolute http or https URL")
	}
	if !hasValidPort(o.URL) {
		return errors.New("url port must be between 1 and 65535")
	}
	if !webhookMethods[strings.ToUpper(o.Method)] {
		return fmt.Errorf("invalid method %q: must be POST, PUT, or PATCH", o.Method)
	}
	if err := o.validateTimeout(); err != nil {
		return err
	}
	for _, header := range o.Headers {
		if !isHeaderName(header.Key) {
			return fmt.Errorf("invalid header name %q", header.Key)
		}
		value, err := header.Resolve()
		if err != nil {
			return err
		}
		if !isHeaderValue(value) {
			return fmt.Errorf("header %q: value contains invalid characters", header.Key)
		}
	}
	if _, err := tlsconfig.Build(o.TLS); err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	return nil
}

// validateOutputs checks names on every entry, because routes refer to them,
// and all other fields on enabled entries only.
func (c *Config) validateOutputs() error {
	seen := make(map[string]bool, len(c.LogOutputs))
	for i := range c.LogOutputs {
		entry := &c.LogOutputs[i]
		name := strings.TrimSpace(entry.Name)
		var err error
		switch {
		case name == "":
			err = errors.New("name is required")
		case strings.EqualFold(name, APILogRemoteOutputName):
			err = fmt.Errorf("name %q is reserved", APILogRemoteOutputName)
		case seen[name]:
			err = errors.New("duplicate name")
		case entry.Enabled:
			err = entry.validate()
		}
		if err != nil {
			return fmt.Errorf("logoutputs[%d] (name %q): %w", i, entry.Name, err)
		}
		seen[name] = true
	}
	return nil
}

// validateRoutes rejects routes that would silently drop data.
func (c *Config) validateRoutes() error {
	enabled := map[string]bool{APILogRemoteOutputName: c.APILogRemote.Enabled}
	for i := range c.LogOutputs {
		enabled[c.LogOutputs[i].Name] = c.LogOutputs[i].Enabled
	}

	for i := range c.WebAPI {
		entry := &c.WebAPI[i]
		if !entry.Enabled {
			continue
		}
		routed := make(map[string]bool, len(entry.Outputs))
		for _, name := range entry.Outputs {
			isEnabled, known := enabled[name]
			var err error
			switch {
			case !known:
				err = fmt.Errorf("unknown output %q", name)
			case !isEnabled:
				err = fmt.Errorf("output %q is disabled", name)
			case routed[name]:
				err = fmt.Errorf("output %q is listed more than once", name)
			}
			if err != nil {
				return fmt.Errorf("webapi[%d] (name %q): %w", i, entry.Name, err)
			}
			routed[name] = true
		}
	}
	return nil
}

// EnabledOutputNames returns every enabled output, starting with apilogremote.
func (c *Config) EnabledOutputNames() []string {
	var names []string
	if c.APILogRemote.Enabled {
		names = append(names, APILogRemoteOutputName)
	}
	for i := range c.LogOutputs {
		if c.LogOutputs[i].Enabled {
			names = append(names, c.LogOutputs[i].Name)
		}
	}
	return names
}

// RoutesFor returns the outputs a source delivers to: all enabled outputs when
// its list is omitted, and none when the list is explicitly empty.
func (c *Config) RoutesFor(w *WebAPI) []string {
	if w.Outputs == nil {
		return c.EnabledOutputNames()
	}
	return w.Outputs
}

// OutputWarnings returns startup warnings for insecure enabled outputs. They
// name the output and never include its URL.
func (c *Config) OutputWarnings() []string {
	var warnings []string
	for i := range c.LogOutputs {
		entry := &c.LogOutputs[i]
		if !entry.Enabled {
			continue
		}
		if entry.Type == OutputTypeSyslog {
			if entry.Network == SyslogNetworkTCP || entry.Network == SyslogNetworkUDP {
				warnings = append(warnings, fmt.Sprintf("log output %q uses plaintext syslog over %s", entry.Name, entry.Network))
			}
		} else if u, err := url.Parse(entry.URL); err == nil && u.Scheme == "http" {
			warnings = append(warnings, fmt.Sprintf("log output %q uses plaintext http", entry.Name))
		}
		if entry.TLS.SkipVerify {
			warnings = append(warnings, fmt.Sprintf("log output %q has TLS verification disabled (skip_verify: true)", entry.Name))
		}
	}
	return warnings
}

// hasValidPort reports whether the URL's port, if any, is in 1-65535. It is
// separate from isHTTPURL so that legacy sections keep their existing checks.
func hasValidPort(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Port() == "" {
		return true
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	return err == nil && port != 0
}

// isHeaderName reports whether s is a valid HTTP token.
func isHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isAlnum := c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !isAlnum && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

// isHeaderValue rejects control characters, which net/http would refuse on
// every send.
func isHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}
