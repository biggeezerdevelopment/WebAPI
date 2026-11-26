package collector

import (
	"os"
	"testing"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/output"
)

func TestNewReadOnlyDoesNotSaveInitialCheckpoints(t *testing.T) {
	cfg := cursorConfig("https://example.invalid/events")
	dir := t.TempDir()
	_, err := newCollectorWithSource(Options{
		Agent: config.JarvisAgent{ID: "id"}, Source: cfg, StateDir: dir,
		Fetcher: api.NewClient(cfg), Outputs: []output.NamedOutput{{Name: "o", Output: &fakeOutput{}}},
		ReadOnly: true,
	}, &cursorSource{endpoint: cfg.Endpoint}, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath(dir, cfg.Name)); !os.IsNotExist(err) {
		t.Fatalf("state file exists after read-only build (stat err = %v)", err)
	}
}
