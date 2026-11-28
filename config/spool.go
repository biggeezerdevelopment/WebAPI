package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultSpoolMaxSizeMB is the spool size limit when max_size_mb is omitted.
	DefaultSpoolMaxSizeMB = 256
	// MaxSpoolMaxSizeMB is the largest accepted max_size_mb (100 GiB).
	MaxSpoolMaxSizeMB = 102400
	// DefaultSpoolMaxAgeHours is the record age limit when max_age_hours is omitted.
	DefaultSpoolMaxAgeHours = 72
	// MaxSpoolMaxAgeHours is the largest accepted max_age_hours (10 years).
	MaxSpoolMaxAgeHours = 87600
)

// Spool configures an output's on-disk spool. Records from plain sources
// routed to the output are written to disk first and sent in order, so an
// output outage does not lose them. Collectors and heartbeats bypass it.
type Spool struct {
	Enabled bool `yaml:"enabled"`
	// MaxSizeMB caps the spool's size on disk. nil means the default.
	MaxSizeMB *int `yaml:"max_size_mb"`
	// MaxAgeHours drops records older than this many hours. nil means the
	// default and 0 means records never expire.
	MaxAgeHours *int `yaml:"max_age_hours"`

	// enabledSet records that the YAML block had an enabled key, so a block
	// that leaves it out can be warned about.
	enabledSet bool
}

// UnmarshalYAML decodes the block and notes whether it set enabled.
func (s *Spool) UnmarshalYAML(value *yaml.Node) error {
	type plain Spool
	if err := value.Decode((*plain)(s)); err != nil {
		return err
	}
	s.enabledSet = false
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "enabled" {
				s.enabledSet = true
			}
		}
	}
	return nil
}

// spoolWarnings flags enabled outputs whose spool block has no enabled key,
// which leaves spooling off.
func (c *Config) spoolWarnings() []string {
	var warnings []string
	check := func(name string, sp *Spool) {
		if sp != nil && !sp.Enabled && !sp.enabledSet {
			warnings = append(warnings, fmt.Sprintf("output %q has a spool block with no \"enabled\" key; spooling is off", name))
		}
	}
	if c.APILogRemote.Enabled {
		check(APILogRemoteOutputName, c.APILogRemote.Spool)
	}
	for i := range c.LogOutputs {
		if c.LogOutputs[i].Enabled {
			check(c.LogOutputs[i].Name, c.LogOutputs[i].Spool)
		}
	}
	return warnings
}

// IsEnabled reports whether s is set and enabled.
func (s *Spool) IsEnabled() bool { return s != nil && s.Enabled }

// MaxBytes returns the size limit in bytes.
func (s *Spool) MaxBytes() int64 {
	mb := DefaultSpoolMaxSizeMB
	if s != nil && s.MaxSizeMB != nil {
		mb = *s.MaxSizeMB
	}
	return int64(mb) << 20
}

// MaxAge returns the record age limit; 0 means none.
func (s *Spool) MaxAge() time.Duration {
	hours := DefaultSpoolMaxAgeHours
	if s != nil && s.MaxAgeHours != nil {
		hours = *s.MaxAgeHours
	}
	return time.Duration(hours) * time.Hour
}

// validate checks the limits, which are checked even when the spool is
// disabled so that turning it on cannot reveal an old mistake.
func (s *Spool) validate() error {
	if s == nil {
		return nil
	}
	if s.MaxSizeMB != nil && (*s.MaxSizeMB < 1 || *s.MaxSizeMB > MaxSpoolMaxSizeMB) {
		return fmt.Errorf("spool: max_size_mb must be between 1 and %d, got %d", MaxSpoolMaxSizeMB, *s.MaxSizeMB)
	}
	if s.MaxAgeHours != nil && (*s.MaxAgeHours < 0 || *s.MaxAgeHours > MaxSpoolMaxAgeHours) {
		return fmt.Errorf("spool: max_age_hours must be between 0 and %d, got %d", MaxSpoolMaxAgeHours, *s.MaxAgeHours)
	}
	return nil
}

// SpoolDir is the directory of the named output's spool. The name is hashed
// because output names can hold characters, or be reserved names, that are
// not valid in a path on every platform.
func SpoolDir(stateDir, output string) string {
	sum := sha256.Sum256([]byte(output))
	return filepath.Join(stateDir, "spool", "output-"+hex.EncodeToString(sum[:16]))
}

// validateSpools checks the spool blocks of enabled outputs, and state_dir
// when any of them is enabled. It does not create the spool directories.
func (c *Config) validateSpools() error {
	enabled := false
	if c.APILogRemote.Enabled {
		if err := c.APILogRemote.Spool.validate(); err != nil {
			return fmt.Errorf("apilogremote: %w", err)
		}
		enabled = c.APILogRemote.Spool.IsEnabled()
	}
	for i := range c.LogOutputs {
		entry := &c.LogOutputs[i]
		if !entry.Enabled {
			continue
		}
		if err := entry.Spool.validate(); err != nil {
			return fmt.Errorf("logoutputs[%d] (name %q): %w", i, entry.Name, err)
		}
		enabled = enabled || entry.Spool.IsEnabled()
	}
	if !enabled {
		return nil
	}
	if strings.TrimSpace(c.StateDir) == "" {
		return errors.New("spool requires state_dir")
	}
	if err := checkStateDir(c.StateDir); err != nil {
		return fmt.Errorf("spool: %w", err)
	}
	return nil
}
