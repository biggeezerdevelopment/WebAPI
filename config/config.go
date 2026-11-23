package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

var validHTTPMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodConnect: true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
}

type Header struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"`
}

// Body is a request body configured as a YAML string or list of strings.
// A string is already serialized and is sent unchanged; a list is sent as a
// JSON array.
type Body struct {
	value any
}

// UnmarshalYAML accepts a scalar or a sequence of scalars.
func (b *Body) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var text string
		if err := node.Decode(&text); err != nil {
			return err
		}
		if text != "" {
			b.value = text
		}
	case yaml.SequenceNode:
		var items []string
		if err := node.Decode(&items); err != nil {
			return err
		}
		if len(items) > 0 {
			b.value = items
		}
	default:
		return fmt.Errorf("line %d: body must be a string or a list of strings", node.Line)
	}
	return nil
}

// IsEmpty reports whether no request body is configured.
func (b Body) IsEmpty() bool {
	return b.value == nil
}

// Payload returns the bytes to send, or nil when no body is configured.
func (b Body) Payload() ([]byte, error) {
	switch v := b.value.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(v), nil
	default:
		return json.Marshal(v)
	}
}

type WebAPI struct {
	Enabled     bool              `yaml:"enabled"`
	Name        string            `yaml:"name"`
	Subject     string            `yaml:"subject"`
	Endpoint    string            `yaml:"endpoint"`
	Method      string            `yaml:"method"`
	Headers     []Header          `yaml:"headers"`
	AuthType    string            `yaml:"auth_type"`
	APIKey      string            `yaml:"api_key"`
	APIUsername string            `yaml:"api_username"`
	APIPassword string            `yaml:"api_password"`
	SkipVerify  bool              `yaml:"skip_verify"`
	SleepTime   int               `yaml:"sleeptime"`
	Timeout     int               `yaml:"timeout"`
	Body        Body              `yaml:"body"`
	Params      map[string]string `yaml:"params"`
}

type APILogRemote struct {
	Enabled    bool     `yaml:"enabled"`
	APIURL     string   `yaml:"api_url"`
	APIMethod  string   `yaml:"api_method"`
	APIHeaders []Header `yaml:"api_headers"`
	SkipVerify bool     `yaml:"skip_verify"`
}

type JarvisAgent struct {
	Name string `yaml:"name"`
	ID   string `yaml:"id"`
}

type Config struct {
	JarvisAgent  JarvisAgent  `yaml:"jarvisagent"`
	APILogRemote APILogRemote `yaml:"apilogremote"`
	WebAPI       []WebAPI     `yaml:"webapi"`
}

func LoadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("error parsing config file: %v", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &config, nil
}

// Validate checks enabled entries and returns an error naming the first bad
// one. Disabled entries are ignored. URL values are left out of errors because
// they can embed credentials.
func (c *Config) Validate() error {
	if c.APILogRemote.Enabled {
		if !isHTTPURL(c.APILogRemote.APIURL) {
			return errors.New("apilogremote: api_url must be an absolute http or https URL")
		}
		if !isHTTPMethod(c.APILogRemote.APIMethod) {
			return fmt.Errorf("apilogremote: invalid api_method %q", c.APILogRemote.APIMethod)
		}
	}

	for i := range c.WebAPI {
		entry := &c.WebAPI[i]
		if !entry.Enabled {
			continue
		}
		if err := entry.validate(); err != nil {
			return fmt.Errorf("webapi[%d] (name %q): %w", i, entry.Name, err)
		}
	}
	return nil
}

func (w *WebAPI) validate() error {
	if strings.TrimSpace(w.Name) == "" {
		return errors.New("name is required")
	}
	if !isHTTPURL(w.Endpoint) {
		return errors.New("endpoint must be an absolute http or https URL")
	}
	if !isHTTPMethod(w.Method) {
		return fmt.Errorf("invalid method %q", w.Method)
	}
	if w.SleepTime <= 0 {
		return fmt.Errorf("sleeptime must be greater than 0, got %d", w.SleepTime)
	}
	if w.Timeout <= 0 {
		return fmt.Errorf("timeout must be greater than 0, got %d", w.Timeout)
	}
	return nil
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

// isHTTPMethod is case-insensitive because the HTTP clients upper-case methods.
func isHTTPMethod(method string) bool {
	return validHTTPMethods[strings.ToUpper(method)]
}
