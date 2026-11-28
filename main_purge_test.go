package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"webapi/config"
)

func makeSpoolDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0000000000000001.seg"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeSpoolsRemovesOnlyStaleDirs(t *testing.T) {
	stateDir := t.TempDir()
	path := writeSpoolConfig(t, stateDir)
	live := config.SpoolDir(stateDir, "hook")
	stale := config.SpoolDir(stateDir, "renamed-output")
	other := filepath.Join(stateDir, "spool", "notes")
	for _, d := range []string{live, stale, other} {
		makeSpoolDir(t, d)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-config", path, "-purge-spools"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale dir still there: %v", err)
	}
	for _, d := range []string{live, other} {
		if _, err := os.Stat(filepath.Join(d, "0000000000000001.seg")); err != nil {
			t.Fatalf("%s was touched: %v", d, err)
		}
	}
	out := stdout.String()
	if !strings.Contains(out, "removed "+filepath.Clean(stale)+" (5 bytes)") || !strings.Contains(out, "removed 1 stale spool directory, 5 bytes") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestPurgeSpoolsWithNothingStale(t *testing.T) {
	stateDir := t.TempDir()
	path := writeSpoolConfig(t, stateDir)
	var stdout bytes.Buffer
	if code := run(context.Background(), []string{"-config", path, "-purge-spools"}, &stdout, io.Discard); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.TrimSpace(stdout.String()) != "no stale spool directories" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestPurgeSpoolsRequiresStateDir(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "agent.yml")
	text := "jarvisagent:\n  name: \"agent\"\n  id: \"1\"\nlogoutputs:\n  - name: \"hook\"\n    enabled: true\n    type: \"webhook\"\n    url: \"https://127.0.0.1:1/audit\"\n    method: \"POST\"\n    timeout: 5\n"
	if err := os.WriteFile(cfgPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"-config", cfgPath, "-purge-spools"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires state_dir") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPurgeSpoolsCannotCombineWithCheckConfig(t *testing.T) {
	if _, err := parseFlags([]string{"-purge-spools", "-check-config"}, io.Discard); err == nil {
		t.Fatal("parseFlags accepted -purge-spools with -check-config")
	}
	if code := run(context.Background(), []string{"-purge-spools", "-check-config"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
