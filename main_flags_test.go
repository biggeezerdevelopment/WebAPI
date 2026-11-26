package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFlagsDefaults(t *testing.T) {
	opts, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.configPath != "config.yml" || opts.checkConfig {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestParseFlagsConfigAndCheck(t *testing.T) {
	opts, err := parseFlags([]string{"-config", "/etc/webapi/agent.yml", "-check-config"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.configPath != "/etc/webapi/agent.yml" || !opts.checkConfig {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestParseFlagsRejectsUnknownAndPositional(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"config.yml"}} {
		if _, err := parseFlags(args, io.Discard); err == nil {
			t.Errorf("parseFlags(%q) succeeded", args)
		}
	}
}

const checkConfigTemplate = `
jarvisagent:
  name: "agent"
  id: "1"
state_dir: %STATE%
logoutputs:
  - name: "hook"
    enabled: true
    type: "webhook"
    url: "http://127.0.0.1:1/audit"
    method: "POST"
    timeout: 5
webapi:
  - enabled: true
    name: "github-audit"
    endpoint: "https://api.github.com/orgs/example-org/audit-log"
    method: "get"
    auth_type: "apikey"
    api_key_env: "CHECK_CONFIG_TOKEN"
    sleeptime: 60
    timeout: 30
    params:
      include: "all"
      per_page: "100"
      order: "asc"
    collector:
      type: "github_audit"
    outputs: ["hook"]
  - enabled: true
    name: "plain"
    endpoint: "http://127.0.0.1:1/metrics"
    method: "get"
    sleeptime: 60
    timeout: 5
heartbeat:
  enabled: true
  interval: 300
  outputs: ["hook"]
`

func writeCheckConfig(t *testing.T, stateDir string) string {
	t.Helper()
	t.Setenv("CHECK_CONFIG_TOKEN", "secret-token-value")
	text := strings.ReplaceAll(checkConfigTemplate, "%STATE%", `"`+filepath.ToSlash(stateDir)+`"`)
	path := filepath.Join(t.TempDir(), "agent.yml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckConfigSucceedsWithoutWritingState(t *testing.T) {
	stateDir := t.TempDir()
	path := writeCheckConfig(t, stateDir)
	var stdout, stderr bytes.Buffer

	code := run(context.Background(), []string{"-config", path, "-check-config"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "configuration OK") {
		t.Errorf("stdout = %q", stdout.String())
	}
	// Plaintext http outputs must be reported as warnings.
	if !strings.Contains(stderr.String(), "WARNING") {
		t.Errorf("stderr has no warnings: %q", stderr.String())
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("check-config wrote to state_dir: %v", entries)
	}
}

func TestCheckConfigLeavesExistingStateUnchanged(t *testing.T) {
	stateDir := t.TempDir()
	path := writeCheckConfig(t, stateDir)
	// Build once normally so that a state file exists.
	if code := run(context.Background(), []string{"-config", path, "-check-config"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	statePath := filepath.Join(stateDir, "github-audit.json")
	original := []byte(`{"source":"github-audit","outputs":{}}`)
	if err := os.WriteFile(statePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(statePath)

	var stderr bytes.Buffer
	code := run(context.Background(), []string{"-config", path, "-check-config"}, io.Discard, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	got, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(statePath)
	if !bytes.Equal(got, original) || !after.ModTime().Equal(info.ModTime()) {
		t.Fatalf("state file changed: %s", got)
	}
}

func TestCheckConfigFailsOnInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yml")
	if err := os.WriteFile(path, []byte("webapi:\n  - enabled: true\n    name: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"-config", path, "-check-config"}, io.Discard, &stderr); code == 0 {
		t.Fatal("exit = 0 for invalid config")
	}
	if !strings.Contains(stderr.String(), "name is required") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestCheckConfigFailsOnMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yml")
	if code := run(context.Background(), []string{"-config", missing, "-check-config"}, io.Discard, io.Discard); code == 0 {
		t.Fatal("exit = 0 for missing config")
	}
}

func TestCheckConfigErrorsDoNotLeakSecrets(t *testing.T) {
	stateDir := t.TempDir()
	path := writeCheckConfig(t, stateDir)
	var stdout, stderr bytes.Buffer
	run(context.Background(), []string{"-config", path, "-check-config"}, &stdout, &stderr)
	if strings.Contains(stdout.String()+stderr.String(), "secret-token-value") {
		t.Fatal("output contains the token value")
	}
}

func TestRunBadFlagExitsTwo(t *testing.T) {
	if code := run(context.Background(), []string{"-nope"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
