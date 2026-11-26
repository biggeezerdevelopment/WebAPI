package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"webapi/api"
	"webapi/collector"
	"webapi/config"
	"webapi/heartbeat"
	"webapi/output"
	"webapi/status"
)

// shutdownTimeout bounds how long the agent waits, after a stop signal, for
// sources and outputs to finish. A hung request cannot block exit past it.
const shutdownTimeout = 30 * time.Second

// errShutdownTimeout reports that shutdown did not finish in time.
var errShutdownTimeout = errors.New("shutdown did not finish in time")

// fetcher performs one poll of a source.
type fetcher interface {
	SendRequest(ctx context.Context) ([]byte, error)
}

// deliverer hands a polled record to the source's outputs.
type deliverer interface {
	Deliver(ctx context.Context, rec output.Record)
}

// pollStatus records each plain poll's outcome for the heartbeat.
// *status.Source implements it.
type pollStatus interface {
	PollStarted()
	PollSucceeded()
	PollFailed(err error)
}

// pollOnce fetches once and forwards the result. A failed poll is still
// forwarded, with nil data, so that receivers see a null result. A poll ended
// by shutdown is neither reported nor forwarded.
func pollOnce(ctx context.Context, agent config.JarvisAgent, name string, f fetcher, d deliverer, st pollStatus) {
	st.PollStarted()
	body, err := f.SendRequest(ctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		log.Printf("Error sending request to %s: %v", name, err)
		st.PollFailed(err)
	} else {
		st.PollSucceeded()
	}
	d.Deliver(ctx, output.Record{
		AgentID:   agent.ID,
		AgentName: agent.Name,
		Source:    name,
		Timestamp: time.Now(),
		Data:      body,
	})
}

// pollLoop calls poll, then waits interval, until ctx is canceled.
func pollLoop(ctx context.Context, interval time.Duration, poll func()) {
	for {
		poll()
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// shutdown runs wait and then closeFn, giving up after timeout. On a timeout
// closeFn may still be running; the caller exits anyway. Checkpoints are
// replaced atomically, so exiting during a save leaves the old or new state.
func shutdown(wait func(), closeFn func() error, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		wait()
		done <- closeFn()
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errShutdownTimeout
	}
}

func main() {
	// Load configuration
	cfg, err := config.LoadConfig("config.yml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	for _, warning := range cfg.OutputWarnings() {
		log.Printf("WARNING: %s", warning)
	}

	router, err := output.NewRouter(cfg)
	if err != nil {
		log.Fatalf("Failed to create log outputs: %v", err)
	}

	// SIGINT is also what NSSM sends on Windows (Ctrl+C); SIGTERM is systemd's.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var wg sync.WaitGroup
	reg := status.NewRegistry(cfg)

	// Build every collector first, so that bad state stops startup before any
	// source has begun polling.
	collectors := make(map[int]*collector.Collector)
	for i := range cfg.WebAPI {
		apiCfg := &cfg.WebAPI[i]
		if !apiCfg.Enabled || apiCfg.Collector == nil {
			continue
		}
		routes, err := router.Routes(apiCfg)
		if err != nil {
			log.Fatalf("Failed to route log outputs: %v", err)
		}
		c, err := collector.New(collector.Options{
			Agent:    cfg.JarvisAgent,
			Source:   apiCfg,
			StateDir: cfg.StateDir,
			Fetcher:  api.NewClient(apiCfg),
			Outputs:  routes,
			Status:   reg.SourceAt(i),
		})
		if err != nil {
			log.Fatalf("Failed to start collector %q: %v", apiCfg.Name, err)
		}
		collectors[i] = c
	}

	if cfg.Heartbeat.Enabled {
		hb, err := router.HeartbeatDispatcher(cfg.Heartbeat.SourceName(), cfg.Heartbeat.Outputs)
		if err != nil {
			log.Fatalf("Failed to route heartbeat: %v", err)
		}
		ticker := time.NewTicker(time.Duration(cfg.Heartbeat.IntervalSeconds()) * time.Second)
		defer ticker.Stop()
		wg.Add(1)
		go func() {
			defer wg.Done()
			heartbeat.Run(ctx, reg, hb, cfg.JarvisAgent, cfg.Heartbeat.SourceName(), ticker.C)
		}()
	}

	// Initialize API clients for each enabled WebAPI
	for i := range cfg.WebAPI {
		apiCfg := &cfg.WebAPI[i]
		if !apiCfg.Enabled {
			continue
		}
		if c, ok := collectors[i]; ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.Run(ctx)
			}()
			continue
		}

		client := api.NewClient(apiCfg)
		src := reg.SourceAt(i)
		dispatcher, err := router.ObservedDispatcher(apiCfg, src)
		if err != nil {
			log.Fatalf("Failed to route log outputs: %v", err)
		}

		wg.Add(1)
		// Start a goroutine for each API client
		go func() {
			defer wg.Done()
			pollLoop(ctx, time.Duration(apiCfg.SleepTime)*time.Second, func() {
				pollOnce(ctx, cfg.JarvisAgent, apiCfg.Name, client, dispatcher, src)
			})
		}()
	}

	<-ctx.Done()
	stop() // A second signal now ends the process at once.
	log.Printf("Shutting down: waiting up to %s for sources and outputs", shutdownTimeout)
	if err := shutdown(wg.Wait, router.Close, shutdownTimeout); err != nil {
		log.Printf("Shutdown: %v", err)
		os.Exit(1)
	}
	log.Printf("Shutdown complete")
}
