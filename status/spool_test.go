package status

import (
	"strings"
	"testing"

	"webapi/config"
)

func TestSnapshotOmitsSpoolsWithoutProvider(t *testing.T) {
	reg := newRegistry(&config.Config{}, (&fakeClock{t: start}).now)
	if got := marshal(t, reg.Snapshot()); strings.Contains(got, "spools") {
		t.Fatalf("heartbeat = %s", got)
	}
}

func TestSnapshotIncludesSpools(t *testing.T) {
	reg := newRegistry(&config.Config{}, (&fakeClock{t: start}).now)
	stats := []SpoolSnapshot{{Output: "hook", Records: 3, Bytes: 51}}
	reg.SetSpoolStats(func() []SpoolSnapshot { return stats })
	hb := reg.Snapshot()
	if !hb.Healthy {
		t.Fatal("a spool backlog alone made the heartbeat unhealthy")
	}
	want := `"spools":[{"output":"hook","records":3,"bytes":51,"dropped_since_last":0}]`
	if got := marshal(t, hb); !strings.Contains(got, want) {
		t.Fatalf("heartbeat = %s", got)
	}
	stats = []SpoolSnapshot{{Output: "hook", DroppedSinceLast: 2}}
	if reg.Snapshot().Healthy {
		t.Fatal("spool drops did not make the heartbeat unhealthy")
	}
}
