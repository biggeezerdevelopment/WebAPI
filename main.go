package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

// options are the command-line settings.
// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

type options struct {
	configPath  string
	checkConfig bool
	version     bool
}

// parseFlags reads the command line. Usage and errors go to stderr.
func parseFlags(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("webapi-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.configPath, "config", "config.yml", "path to the configuration file")
	fs.BoolVar(&opts.checkConfig, "check-config", false, "validate the configuration, build outputs and collectors, and exit without polling or writing state")
	fs.BoolVar(&opts.version, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("unexpected argument %q", fs.Arg(0))
		fmt.Fprintln(stderr, err)
		fs.Usage()
		return options{}, err
	}
	return opts, nil
}

func main() {
	// SIGINT is also what NSSM sends on Windows (Ctrl+C); SIGTERM is systemd's.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop() // A second signal now ends the process at once.
	}()
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the whole agent: it returns the process exit code. 2 is a usage
// error and 1 any other failure.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		return 2
	}
	if opts.version {
		fmt.Fprintf(stdout, "webapi-agent %s\n", version)
		return 0
	}
	logger := log.New(stderr, "", log.LstdFlags)

	cfg, err := config.LoadConfig(opts.configPath)
	if err != nil {
		logger.Printf("Failed to load config: %v", err)
		return 1
	}
	for _, warning := range cfg.OutputWarnings() {
		logger.Printf("WARNING: %s", warning)
	}

	a, err := build(cfg, opts.checkConfig)
	if err != nil {
		logger.Print(err)
		return 1
	}
	if opts.checkConfig {
		if err := a.router.Close(); err != nil {
			logger.Printf("Closing outputs: %v", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: configuration OK\n", opts.configPath)
		return 0
	}

	var wg sync.WaitGroup
	a.start(ctx, &wg)
	<-ctx.Done()
	logger.Printf("Shutting down: waiting up to %s for sources and outputs", shutdownTimeout)
	if err := shutdown(wg.Wait, a.router.Close, shutdownTimeout); err != nil {
		logger.Printf("Shutdown: %v", err)
		return 1
	}
	logger.Printf("Shutdown complete")
	return 0
}

// agent holds everything built from a configuration before polling starts.
type agent struct {
	cfg        *config.Config
	router     *output.Router
	reg        *status.Registry
	collectors []*collector.Collector
	plain      []plainSource
	heartbeat  *output.Dispatcher
}

// plainSource is an enabled source without a collector.
type plainSource struct {
	cfg        *config.WebAPI
	client     *api.Client
	dispatcher *output.Dispatcher
	status     pollStatus
}

// build creates the outputs, collectors, sources and heartbeat. Collectors
// are built before anything polls, so bad state stops startup. With readOnly
// no state file is written. On error the router is closed.
func build(cfg *config.Config, readOnly bool) (a *agent, err error) {
	router, err := output.NewRouter(cfg)
	if err != nil {
		return nil, fmt.Errorf("Failed to create log outputs: %w", err)
	}
	defer func() {
		if err != nil {
			_ = router.Close()
		}
	}()
	a = &agent{cfg: cfg, router: router, reg: status.NewRegistry(cfg)}

	for i := range cfg.WebAPI {
		apiCfg := &cfg.WebAPI[i]
		if !apiCfg.Enabled {
			continue
		}
		client, err := api.NewClient(apiCfg)
		if err != nil {
			return nil, fmt.Errorf("webapi[%d] (name %q): %w", i, apiCfg.Name, err)
		}
		src := a.reg.SourceAt(i)
		if apiCfg.Collector == nil {
			dispatcher, err := router.ObservedDispatcher(apiCfg, src)
			if err != nil {
				return nil, fmt.Errorf("Failed to route log outputs: %w", err)
			}
			a.plain = append(a.plain, plainSource{cfg: apiCfg, client: client, dispatcher: dispatcher, status: src})
			continue
		}
		routes, err := router.Routes(apiCfg)
		if err != nil {
			return nil, fmt.Errorf("Failed to route log outputs: %w", err)
		}
		c, err := collector.New(collector.Options{
			Agent:    cfg.JarvisAgent,
			Source:   apiCfg,
			StateDir: cfg.StateDir,
			Fetcher:  client,
			Outputs:  routes,
			Status:   src,
			ReadOnly: readOnly,
		})
		if err != nil {
			return nil, fmt.Errorf("Failed to start collector %q: %w", apiCfg.Name, err)
		}
		a.collectors = append(a.collectors, c)
	}

	if cfg.Heartbeat.Enabled {
		a.heartbeat, err = router.HeartbeatDispatcher(cfg.Heartbeat.SourceName(), cfg.Heartbeat.Outputs)
		if err != nil {
			return nil, fmt.Errorf("Failed to route heartbeat: %w", err)
		}
	}
	return a, nil
}

// start launches the heartbeat and every source. Each goroutine is added to
// wg and returns once ctx is canceled.
func (a *agent) start(ctx context.Context, wg *sync.WaitGroup) {
	if a.heartbeat != nil {
		ticker := time.NewTicker(time.Duration(a.cfg.Heartbeat.IntervalSeconds()) * time.Second)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer ticker.Stop()
			heartbeat.Run(ctx, a.reg, a.heartbeat, a.cfg.JarvisAgent, a.cfg.Heartbeat.SourceName(), ticker.C)
		}()
	}
	for _, c := range a.collectors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Run(ctx)
		}()
	}
	for _, p := range a.plain {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pollLoop(ctx, time.Duration(p.cfg.SleepTime)*time.Second, func() {
				pollOnce(ctx, a.cfg.JarvisAgent, p.cfg.Name, p.client, p.dispatcher, p.status)
			})
		}()
	}
}
