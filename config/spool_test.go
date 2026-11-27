package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func intPtr(n int) *int { return &n }

func spoolConfig(stateDir string, sp *Spool) *Config {
	return &Config{
		StateDir: stateDir,
		LogOutputs: []LogOutput{{
			Name: "hook", Enabled: true, Type: OutputTypeWebhook,
			URL: "https://example.com/hook", Method: "POST", Timeout: 5, Spool: sp,
		}},
	}
}

func TestSpoolDefaults(t *testing.T) {
	var none *Spool
	if none.IsEnabled() || none.MaxBytes() != 256<<20 || none.MaxAge() != 72*time.Hour {
		t.Fatalf("nil spool: enabled=%v bytes=%d age=%v", none.IsEnabled(), none.MaxBytes(), none.MaxAge())
	}
	sp := &Spool{Enabled: true, MaxSizeMB: intPtr(10), MaxAgeHours: intPtr(0)}
	if !sp.IsEnabled() || sp.MaxBytes() != 10<<20 || sp.MaxAge() != 0 {
		t.Fatalf("spool: enabled=%v bytes=%d age=%v", sp.IsEnabled(), sp.MaxBytes(), sp.MaxAge())
	}
}

func TestSpoolLimitValidation(t *testing.T) {
	cases := map[string]struct {
		sp   *Spool
		want string
	}{
		"size zero":    {&Spool{Enabled: true, MaxSizeMB: intPtr(0)}, "max_size_mb must be between 1 and 102400, got 0"},
		"size too big": {&Spool{Enabled: true, MaxSizeMB: intPtr(102401)}, "max_size_mb must be between 1 and 102400, got 102401"},
		"age negative": {&Spool{Enabled: true, MaxAgeHours: intPtr(-1)}, "max_age_hours must be between 0 and 87600, got -1"},
		"age too big":  {&Spool{Enabled: true, MaxAgeHours: intPtr(87601)}, "max_age_hours must be between 0 and 87600, got 87601"},
		"disabled bad": {&Spool{MaxSizeMB: intPtr(0)}, "max_size_mb must be between 1 and 102400, got 0"},
		"valid":        {&Spool{Enabled: true, MaxSizeMB: intPtr(1), MaxAgeHours: intPtr(0)}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := spoolConfig(t.TempDir(), tc.sp).Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `logoutputs[0] (name "hook")`) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSpoolRequiresStateDir(t *testing.T) {
	err := spoolConfig("", &Spool{Enabled: true}).Validate()
	if err == nil || err.Error() != "spool requires state_dir" {
		t.Fatalf("Validate = %v", err)
	}
	if err := spoolConfig("", &Spool{Enabled: false}).Validate(); err != nil {
		t.Fatalf("disabled spool: %v", err)
	}
}

func TestSpoolOnDisabledOutputIgnored(t *testing.T) {
	cfg := spoolConfig("", &Spool{Enabled: true, MaxSizeMB: intPtr(0)})
	cfg.LogOutputs[0].Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestSpoolOnAPILogRemote(t *testing.T) {
	cfg := &Config{APILogRemote: APILogRemote{
		Enabled: true, APIURL: "https://example.com/logs", APIMethod: "POST",
		Spool: &Spool{Enabled: true, MaxAgeHours: intPtr(-5)},
	}}
	if err := cfg.Validate(); err == nil || !strings.HasPrefix(err.Error(), "apilogremote: spool: max_age_hours") {
		t.Fatalf("Validate = %v", err)
	}
	cfg.APILogRemote.Spool.MaxAgeHours = nil
	if err := cfg.Validate(); err == nil || err.Error() != "spool requires state_dir" {
		t.Fatalf("Validate = %v", err)
	}
}

func TestSpoolValidateCreatesNoSpoolDir(t *testing.T) {
	dir := t.TempDir()
	if err := spoolConfig(dir, &Spool{Enabled: true}).Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "spool")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spool dir exists after Validate: %v", err)
	}
}

func TestSpoolYAML(t *testing.T) {
	text := `
apilogremote:
  spool: {enabled: true}
logoutputs:
  - name: hook
    spool:
      enabled: true
      max_size_mb: 64
      max_age_hours: 0
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(text), &cfg); err != nil {
		t.Fatal(err)
	}
	sp := cfg.LogOutputs[0].Spool
	if !sp.IsEnabled() || sp.MaxBytes() != 64<<20 || sp.MaxAge() != 0 {
		t.Fatalf("logoutputs spool = %+v", sp)
	}
	if !cfg.APILogRemote.Spool.IsEnabled() || cfg.APILogRemote.Spool.MaxAge() != 72*time.Hour {
		t.Fatalf("apilogremote spool = %+v", cfg.APILogRemote.Spool)
	}
}

func TestSpoolDirHashesName(t *testing.T) {
	a, b := SpoolDir("/state", "a/../b"), SpoolDir("/state", "c")
	if filepath.Dir(a) != filepath.Join("/state", "spool") || !strings.HasPrefix(filepath.Base(a), "output-") {
		t.Fatalf("SpoolDir = %q", a)
	}
	if len(filepath.Base(a)) != len("output-")+32 || a == b {
		t.Fatalf("SpoolDir a=%q b=%q", a, b)
	}
}
