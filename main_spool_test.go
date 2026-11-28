package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSpoolConfig is writeCheckConfig with spooling enabled on output "hook".
func writeSpoolConfig(t *testing.T, stateDir string) string {
	t.Helper()
	path := writeCheckConfig(t, stateDir)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(data), "    timeout: 5\nwebapi:", "    timeout: 5\n    spool:\n      enabled: true\nwebapi:", 1)
	if text == string(data) {
		t.Fatal("template did not change; update the replacement")
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckConfigWithSpoolWritesNothing(t *testing.T) {
	stateDir := t.TempDir()
	path := writeSpoolConfig(t, stateDir)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-config", path, "-check-config"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "spool")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check-config created the spool dir: %v", err)
	}
}
