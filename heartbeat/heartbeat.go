// Package heartbeat sends a periodic liveness record built from the status
// registry, so a SIEM can alert when the agent stops or a source fails.
package heartbeat

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"webapi/config"
	"webapi/output"
	"webapi/status"
)

// Snapshotter returns the current status and resets its interval counts.
// *status.Registry implements it.
type Snapshotter interface {
	Snapshot() status.Heartbeat
}

// Deliverer sends a record to the heartbeat outputs. *output.Dispatcher
// implements it.
type Deliverer interface {
	Deliver(ctx context.Context, rec output.Record)
}

// Send delivers one heartbeat now. Delivery failures are logged by the
// Deliverer. The SIEM detects them as a missing heartbeat.
func Send(ctx context.Context, snap Snapshotter, d Deliverer, agent config.JarvisAgent, source string) {
	hb := snap.Snapshot()
	data, err := json.Marshal(hb)
	if err != nil {
		log.Printf("Error encoding heartbeat: %v", err)
		return
	}
	d.Deliver(ctx, output.Record{
		AgentID:   agent.ID,
		AgentName: agent.Name,
		Source:    source,
		Timestamp: hb.At,
		Data:      data,
	})
}

// Run sends a heartbeat immediately, so a restart is visible, and then one per
// tick until ctx is done.
func Run(ctx context.Context, snap Snapshotter, d Deliverer, agent config.JarvisAgent, source string, ticks <-chan time.Time) {
	Send(ctx, snap, d, agent, source)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			Send(ctx, snap, d, agent, source)
		}
	}
}
