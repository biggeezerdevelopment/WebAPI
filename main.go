package main

import (
	"log"
	"sync"
	"time"

	"webapi/api"
	"webapi/config"
)

func main() {
	// Load configuration
	cfg, err := config.LoadConfig("config.yml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create log client if enabled
	var logClient *api.LogClient
	if cfg.APILogRemote.Enabled {
		logClient = api.NewLogClient(&cfg.APILogRemote)
	}

	var wg sync.WaitGroup

	// Initialize API clients for each enabled WebAPI
	for _, apiConfig := range cfg.WebAPI {
		if !apiConfig.Enabled {
			continue
		}

		client := api.NewClient(&apiConfig)

		wg.Add(1)
		// Start a goroutine for each API client
		go func(apiCfg config.WebAPI) {
			defer wg.Done()
			for {
				// Here you would collect the metrics or data to send

				// Send request to the API
				body, err := client.SendRequest()
				if err != nil {
					log.Printf("Error sending request to %s: %v", apiCfg.Name, err)
				}

				// If log forwarding is enabled, send the results to the log API
				if logClient != nil {
					if err := logClient.SendLog(
						cfg.JarvisAgent.ID,
						cfg.JarvisAgent.Name,
						apiCfg.Name,
						body,
					); err != nil {
						log.Printf("Error sending log for %s: %v", apiCfg.Name, err)
					}
				}

				time.Sleep(time.Duration(apiCfg.SleepTime) * time.Second)
			}
		}(apiConfig)
	}

	// Wait for all goroutines to complete (they won't in this case, but it's good practice)
	wg.Wait()
}
