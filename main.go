package main

import (
	"context"
	"log"
	"sync"
	"time"

	"webapi/api"
	"webapi/collector"
	"webapi/config"
	"webapi/heartbeat"
	"webapi/output"
	"webapi/status"
)

// fetcher performs one poll of a source.
type fetcher interface {
	SendRequest() ([]byte, error)
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
// forwarded, with nil data, so that receivers see a null result.
func pollOnce(ctx context.Context, agent config.JarvisAgent, name string, f fetcher, d deliverer, st pollStatus) {
	st.PollStarted()
	body, err := f.SendRequest()
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
	defer router.Close()

	ctx := context.Background()
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
			for {
				pollOnce(ctx, cfg.JarvisAgent, apiCfg.Name, client, dispatcher, src)
				time.Sleep(time.Duration(apiCfg.SleepTime) * time.Second)
			}
		}()
	}

	// Wait for all goroutines to complete (they won't in this case, but it's good practice)
	wg.Wait()
}
