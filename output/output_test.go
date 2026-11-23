package output

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/internal/testcerts"
	"webapi/tlsconfig"
)

func testRecord() Record {
	return Record{AgentID: "id", AgentName: "agent", Source: "src", Timestamp: time.Unix(1700000000, 0), Data: []byte(`{"k":1}`)}
}

func webhookConfig(url string) *config.LogOutput {
	return &config.LogOutput{Name: "hook", Enabled: true, Type: config.OutputTypeWebhook, URL: url, Method: "post", Timeout: 5}
}

func newTestWebhook(t *testing.T, cfg *config.LogOutput) *Webhook {
	t.Helper()
	w, err := NewWebhook(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestWebhookSendsEnvelopeAndHeaders(t *testing.T) {
	t.Setenv("WEBHOOK_TEST_AUTH", "Bearer from-env")
	var got api.LogPayload
	var method, contentType, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, contentType, auth = r.Method, r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	cfg := webhookConfig(srv.URL)
	cfg.Headers = []config.OutputHeader{
		{Key: "Content-Type", Value: "application/json"},
		{Key: "Authorization", ValueEnv: "WEBHOOK_TEST_AUTH"},
	}
	if err := newTestWebhook(t, cfg).Send(context.Background(), testRecord()); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || contentType != "application/json" || auth != "Bearer from-env" {
		t.Errorf("method=%q content-type=%q auth=%q", method, contentType, auth)
	}
	results, _ := json.Marshal(got.APIResults)
	if got.AgentID != "id" || got.AgentName != "agent" || got.Source != "src" || got.Timestamp != 1700000000 || string(results) != `{"k":1}` {
		t.Errorf("payload = %+v results=%s", got, results)
	}
}

func TestWebhookForwardsNullForFailedPoll(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = string(data)
	}))
	defer srv.Close()

	rec := testRecord()
	rec.Data = nil
	if err := newTestWebhook(t, webhookConfig(srv.URL)).Send(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"api_results":null`) {
		t.Errorf("body = %s", body)
	}
}

func TestWebhookErrorsOmitURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close() // connection refused from here on

	url := "http://user:hunter2@" + addr + "/secret-path?token=abc"
	err := newTestWebhook(t, webhookConfig(url)).Send(context.Background(), testRecord())
	if err == nil {
		t.Fatal("expected error")
	}
	for _, leak := range []string{"hunter2", addr, "secret-path", "token"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
}

func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer srv.Close()

	err := newTestWebhook(t, webhookConfig(srv.URL)).Send(context.Background(), testRecord())
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("got %v", err)
	}
	if followed.Load() != 0 {
		t.Error("redirect was followed")
	}
}

func newTLSServer(t *testing.T, b *testcerts.Bundle, clientAuth tls.ClientAuthType) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{b.ServerCert}, ClientAuth: clientAuth, ClientCAs: b.CAPool}
	srv.Config.ErrorLog = nil
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestWebhookCustomCA(t *testing.T) {
	b := testcerts.New(t)
	srv, hits := newTLSServer(t, b, tls.NoClientCert)
	cfg := webhookConfig(srv.URL)
	cfg.TLS = tlsconfig.Options{CAFile: b.CAFile, ServerName: "webhook.test"}
	if err := newTestWebhook(t, cfg).Send(context.Background(), testRecord()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d", hits.Load())
	}
}

func TestWebhookRejectsUntrustedCertificate(t *testing.T) {
	b := testcerts.New(t)
	other := testcerts.New(t)
	srv, hits := newTLSServer(t, b, tls.NoClientCert)

	for name, opts := range map[string]tlsconfig.Options{
		"wrong CA":     {CAFile: other.CAFile},
		"system roots": {},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := webhookConfig(srv.URL)
			cfg.TLS = opts
			err := newTestWebhook(t, cfg).Send(context.Background(), testRecord())
			if err == nil || !strings.Contains(err.Error(), "certificate verification") {
				t.Fatalf("got %v", err)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("handler reached %d times", hits.Load())
	}
}

func TestWebhookMutualTLS(t *testing.T) {
	b := testcerts.New(t)
	srv, hits := newTLSServer(t, b, tls.RequireAndVerifyClientCert)

	cfg := webhookConfig(srv.URL)
	cfg.TLS = tlsconfig.Options{CAFile: b.CAFile, CertFile: b.ClientCertFile, KeyFile: b.ClientKeyFile}
	if err := newTestWebhook(t, cfg).Send(context.Background(), testRecord()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d", hits.Load())
	}

	noCert := webhookConfig(srv.URL)
	noCert.TLS = tlsconfig.Options{CAFile: b.CAFile}
	if err := newTestWebhook(t, noCert).Send(context.Background(), testRecord()); err == nil {
		t.Fatal("expected rejection without client certificate")
	}
	if hits.Load() != 1 {
		t.Errorf("handler reached without client certificate: %d", hits.Load())
	}
}

func TestRetryNon2xxThenSuccess(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	r := newRetrying(newTestWebhook(t, webhookConfig(srv.URL)), 5*time.Second)
	r.backoff = time.Millisecond
	if err := r.Send(context.Background(), testRecord()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d", calls.Load())
	}
}

func TestRetryStopsAfterThreeRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	r := newRetrying(newTestWebhook(t, webhookConfig(srv.URL)), 5*time.Second)
	r.backoff = time.Millisecond
	err := r.Send(context.Background(), testRecord())
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("got %v", err)
	}
	if calls.Load() != 1+maxRetries {
		t.Errorf("calls = %d, want %d", calls.Load(), 1+maxRetries)
	}
}

type funcOutput struct {
	send func(context.Context, Record) error
}

func (f funcOutput) Send(ctx context.Context, rec Record) error { return f.send(ctx, rec) }
func (f funcOutput) Close() error                               { return nil }

func TestRetryBoundedByTimeoutAndContext(t *testing.T) {
	failing := funcOutput{send: func(context.Context, Record) error { return errors.New("boom") }}

	r := newRetrying(failing, 30*time.Millisecond)
	r.backoff = time.Hour
	start := time.Now()
	if err := r.Send(context.Background(), testRecord()); err == nil {
		t.Fatal("expected error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout not honored: %v", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	counted := funcOutput{send: func(context.Context, Record) error { calls.Add(1); return errors.New("boom") }}
	if err := newRetrying(counted, time.Minute).Send(ctx, testRecord()); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls after cancel = %d", calls.Load())
	}
}

func TestFanOutFailedOutputDoesNotAffectOther(t *testing.T) {
	received := make(chan Record, 3)
	failing := funcOutput{send: func(context.Context, Record) error { return errors.New("down") }}
	healthy := funcOutput{send: func(_ context.Context, rec Record) error { received <- rec; return nil }}

	d := newDispatcher("src", []target{{"bad", failing}, {"good", healthy}}, 8)
	defer d.Close()
	for i := 0; i < 3; i++ {
		d.Deliver(context.Background(), testRecord())
	}
	for i := 0; i < 3; i++ {
		select {
		case <-received:
		case <-time.After(5 * time.Second):
			t.Fatal("healthy output did not receive record")
		}
	}
}

func TestFanOutHungOutputDoesNotBlockOther(t *testing.T) {
	release := make(chan struct{})
	received := make(chan Record, 8)
	hung := funcOutput{send: func(context.Context, Record) error { <-release; return nil }}
	healthy := funcOutput{send: func(_ context.Context, rec Record) error { received <- rec; return nil }}

	d := newDispatcher("src", []target{{"hung", hung}, {"good", healthy}}, 1)
	defer d.Close()
	defer close(release)

	// More records than the hung lane can hold: Deliver must never block.
	for i := 0; i < 4; i++ {
		done := make(chan struct{})
		go func() {
			d.Deliver(context.Background(), testRecord())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Deliver blocked on hung output")
		}
		select {
		case <-received:
		case <-time.After(5 * time.Second):
			t.Fatal("healthy output did not receive record")
		}
	}
}

func TestDispatcherCloseDrainsQueues(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	counter := func(name string) Output {
		return funcOutput{send: func(context.Context, Record) error {
			mu.Lock()
			defer mu.Unlock()
			counts[name]++
			return nil
		}}
	}
	d := newDispatcher("src", []target{{"a", counter("a")}, {"b", counter("b")}}, 8)
	for i := 0; i < 5; i++ {
		d.Deliver(context.Background(), testRecord())
	}
	d.Close()
	d.Close() // idempotent
	d.Deliver(context.Background(), testRecord())
	if counts["a"] != 5 || counts["b"] != 5 {
		t.Errorf("counts = %v", counts)
	}
}

func TestRouterFanOutOverHTTP(t *testing.T) {
	var good atomic.Int32
	goodSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { good.Add(1) }))
	defer goodSrv.Close()
	stop := make(chan struct{})
	hungSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer hungSrv.Close()
	defer close(stop)

	hungCfg, goodCfg := webhookConfig(hungSrv.URL), webhookConfig(goodSrv.URL)
	hungCfg.Name, hungCfg.Timeout = "hung", 1
	goodCfg.Name = "good"
	cfg := &config.Config{LogOutputs: []config.LogOutput{*hungCfg, *goodCfg}}
	source := &config.WebAPI{Name: "src"}

	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := router.Dispatcher(source)
	if err != nil {
		t.Fatal(err)
	}
	d.Deliver(context.Background(), testRecord())

	deadline := time.Now().Add(900 * time.Millisecond)
	for good.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if good.Load() != 1 {
		t.Errorf("healthy webhook hits = %d before hung output timed out", good.Load())
	}
	if err := router.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRouterLegacyOutputIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &config.Config{APILogRemote: config.APILogRemote{Enabled: true, APIURL: srv.URL, APIMethod: "POST"}}
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	d, err := router.Dispatcher(&config.WebAPI{Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}

	rec := testRecord()
	rec.Data = nil
	d.Deliver(context.Background(), rec) // synchronous for a single output
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
	if !strings.Contains(body, `"api_results":null`) {
		t.Errorf("body = %s", body)
	}
}

func TestRouterEmptyRoutesDeliverNothing(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()

	cfg := &config.Config{LogOutputs: []config.LogOutput{*webhookConfig(srv.URL)}}
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	d, err := router.Dispatcher(&config.WebAPI{Name: "none", Outputs: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	d.Deliver(context.Background(), testRecord())
	if calls.Load() != 0 {
		t.Errorf("calls = %d", calls.Load())
	}
}
