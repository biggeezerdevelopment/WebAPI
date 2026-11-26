package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestVersionFlagPrintsVersionWithoutLoadingConfig(t *testing.T) {
	old := version
	version = "v1.2.3"
	t.Cleanup(func() { version = old })

	missing := filepath.Join(t.TempDir(), "absent.yml")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-config", missing, "-version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "webapi-agent v1.2.3\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

func TestVersionDefaultsToDev(t *testing.T) {
	if version != "dev" {
		t.Fatalf("version = %q, want %q for an unstamped build", version, "dev")
	}
}
