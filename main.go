package main

import (
	"context"
	"log"
	"sync"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/output"
)

// fetcher performs one poll of a source.
type fetcher interface {
	SendRequest() ([]byte, error)
}

// deliverer hands a polled record to the source's outputs.
type deliverer interface {
	Deliver(ctx context.Context, rec output.Record)
}

// pollOnce fetches once and forwards the result. A failed poll is still
// forwarded, with nil data, so that receivers see a null result.
func pollOnce(ctx context.Context, agent config.JarvisAgent, name string, f fetcher, d deliverer) {
	body, err := f.SendRequest()
	if err != nil {
		log.Printf("Error sending request to %s: %v", name, err)
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

	// Initialize API clients for each enabled WebAPI
	for i := range cfg.WebAPI {
		apiCfg := &cfg.WebAPI[i]
		if !apiCfg.Enabled {
			continue
		}

		client := api.NewClient(apiCfg)
		dispatcher, err := router.Dispatcher(apiCfg)
		if err != nil {
			log.Fatalf("Failed to route log outputs: %v", err)
		}

		wg.Add(1)
		// Start a goroutine for each API client
		go func() {
			defer wg.Done()
			for {
				pollOnce(ctx, cfg.JarvisAgent, apiCfg.Name, client, dispatcher)
				time.Sleep(time.Duration(apiCfg.SleepTime) * time.Second)
			}
		}()
	}

	// Wait for all goroutines to complete (they won't in this case, but it's good practice)
	wg.Wait()
}
