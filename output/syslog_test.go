package output

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

const syslogTestWait = 5 * time.Second

func syslogConfig(network, address string) *config.LogOutput {
	return &config.LogOutput{
		Name: "soc", Enabled: true, Type: config.OutputTypeSyslog, Network: network,
		Address: address, Facility: "local0", Severity: "info", Timeout: 5,
	}
}

func newTestSyslog(t *testing.T, cfg *config.LogOutput) *Syslog {
	t.Helper()
	s, err := NewSyslog(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// readFrame parses one octet-counted frame.
func readFrame(r *bufio.Reader) (string, error) {
	prefix, err := r.ReadString(' ')
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimSuffix(prefix, " "))
	if err != nil {
		return "", fmt.Errorf("bad frame length %q", prefix)
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return "", err
	}
	return string(msg), nil
}

// streamServer collects frames. With dropFirst, it closes the first
// connection after one frame.
type streamServer struct {
	ln     net.Listener
	frames chan string
	conns  atomic.Int32
}

func startStreamServer(t *testing.T, tlsCfg *tls.Config, dropFirst bool) *streamServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	}
	srv := &streamServer{ln: ln, frames: make(chan string, 1024)}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var open []net.Conn
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			open = append(open, conn)
			mu.Unlock()
			drop := dropFirst && srv.conns.Add(1) == 1
			if !dropFirst {
				srv.conns.Add(1)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					frame, err := readFrame(r)
					if err != nil {
						return
					}
					srv.frames <- frame
					if drop {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, conn := range open {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return srv
}

func (s *streamServer) next(t *testing.T) string {
	t.Helper()
	select {
	case frame := <-s.frames:
		return frame
	case <-time.After(syslogTestWait):
		t.Fatal("timed out waiting for frame")
		return ""
	}
}

func assertEnvelope(t *testing.T, msg string) {
	t.Helper()
	parts := strings.SplitN(msg, " ", 8)
	if len(parts) != 8 || !strings.HasPrefix(msg, "<134>1 ") {
		t.Fatalf("malformed message %q", msg)
	}
	var payload api.LogPayload
	if err := json.Unmarshal([]byte(parts[7]), &payload); err != nil {
		t.Fatalf("MSG is not JSON: %v", err)
	}
	if payload.AgentID != "id" || payload.Source != "src" {
		t.Errorf("payload = %+v", payload)
	}
}

func TestSyslogFormatExact(t *testing.T) {
	cfg := syslogConfig("tcp", "localhost:514")
	cfg.AppName = "webapi-agent"
	rec := testRecord()
	rec.Timestamp = time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.FixedZone("x", 3600))
	got, err := newTestSyslog(t, cfg).format(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := `<134>1 2026-01-02T02:04:05.123456Z agent webapi-agent - - - ` +
		`{"agent_id":"id","agent_name":"agent","timestamp":` + strconv.FormatInt(rec.Timestamp.Unix(), 10) +
		`,"source":"src","api_results":{"k":1}}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestSyslogFormatDefaultsAndNilValues(t *testing.T) {
	s := newTestSyslog(t, syslogConfig("tcp", "localhost:514"))
	got, err := s.format(Record{Source: "my source"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "<134>1 - - my_source - - - {"; !strings.HasPrefix(string(got), want) {
		t.Errorf("got %s, want prefix %s", got, want)
	}
}

func TestSyslogMessageIsSingleLine(t *testing.T) {
	rec := testRecord()
	rec.Data = []byte("{\n  \"k\": \"v\"\n}\n")
	got, err := newTestSyslog(t, syslogConfig("tcp", "localhost:514")).format(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(got), "\r\n") {
		t.Errorf("message has a line break: %q", got)
	}
}

func TestSanitizeSyslogField(t *testing.T) {
	tests := []struct {
		name, in string
		limit    int
		want     string
	}{
		{"empty", "", 48, "-"},
		{"blank", " \t ", 48, "-"},
		{"plain", "web-api", 48, "web-api"},
		{"space", "web api", 48, "web_api"},
		{"control", "a\nb\x7fc", 48, "a_b_c"},
		{"unicode is one replacement", "aéb", 48, "a_b"},
		{"exact limit", strings.Repeat("a", 48), 48, strings.Repeat("a", 48)},
		{"app name limit", strings.Repeat("a", 49), maxSyslogAppName, strings.Repeat("a", 48)},
		{"hostname limit", strings.Repeat("h", 300), maxSyslogHostname, strings.Repeat("h", 255)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeSyslogField(tt.in, tt.limit); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFrameOctetCounted(t *testing.T) {
	if got := string(frameOctetCounted([]byte("héllo"))); got != "6 héllo" {
		t.Errorf("got %q", got)
	}
}

func TestSyslogDefaultServerName(t *testing.T) {
	for address, want := range map[string]string{
		"syslog.example.com:6514": "syslog.example.com",
		"[::1]:6514":              "::1",
		"[fe80::1%en0]:6514":      "fe80::1",
	} {
		if got := newTestSyslog(t, syslogConfig("tls", address)).tlsConfig.ServerName; got != want {
			t.Errorf("%s: ServerName = %q, want %q", address, got, want)
		}
	}
	cfg := syslogConfig("tls", "10.0.0.1:6514")
	cfg.TLS.ServerName = "override.example.com"
	if got := newTestSyslog(t, cfg).tlsConfig.ServerName; got != "override.example.com" {
		t.Errorf("ServerName = %q", got)
	}
}

func TestSyslogOverUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	s := newTestSyslog(t, syslogConfig("udp", pc.LocalAddr().String()))
	for i := 0; i < 2; i++ {
		if err := s.Send(context.Background(), testRecord()); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 70000)
		_ = pc.SetReadDeadline(time.Now().Add(syslogTestWait))
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		assertEnvelope(t, string(buf[:n])) // One whole message, with no framing.
	}
}

func TestSyslogOversizedUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	counting := &countingOutput{Output: newTestSyslog(t, syslogConfig("udp", pc.LocalAddr().String()))}
	rec := testRecord()
	rec.Data = []byte(strings.Repeat("x", maxSyslogDatagram+1))
	err = newRetrying(counting, 5*time.Second).Send(context.Background(), rec)
	if !errors.Is(err, ErrSyslogMessageTooLarge) {
		t.Fatalf("error = %v", err)
	}
	if got := counting.calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
	_ = pc.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, _, err := pc.ReadFrom(make([]byte, 10)); err == nil {
		t.Errorf("a %d byte datagram was sent", n)
	}
}

type countingOutput struct {
	Output
	calls atomic.Int32
}

func (c *countingOutput) Send(ctx context.Context, rec Record) error {
	c.calls.Add(1)
	return c.Output.Send(ctx, rec)
}

func TestSyslogOverStreams(t *testing.T) {
	b := testcerts.New(t)
	serverTLS := &tls.Config{Certificates: []tls.Certificate{b.ServerCert}}
	mutualTLS := &tls.Config{
		Certificates: []tls.Certificate{b.ServerCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    b.CAPool,
	}
	tests := []struct {
		name, network string
		server        *tls.Config
		client        tlsconfig.Options
	}{
		{"tcp", "tcp", nil, tlsconfig.Options{}},
		{"tls custom CA", "tls", serverTLS, tlsconfig.Options{CAFile: b.CAFile}},
		{"mutual tls", "tls", mutualTLS, tlsconfig.Options{CAFile: b.CAFile, CertFile: b.ClientCertFile, KeyFile: b.ClientKeyFile}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startStreamServer(t, tt.server, false)
			cfg := syslogConfig(tt.network, srv.ln.Addr().String())
			cfg.TLS = tt.client
			s := newTestSyslog(t, cfg)
			for i := 0; i < 3; i++ {
				if err := s.Send(context.Background(), testRecord()); err != nil {
					t.Fatal(err)
				}
				assertEnvelope(t, srv.next(t))
			}
			if got := srv.conns.Load(); got != 1 {
				t.Errorf("connections = %d, want 1 reused", got)
			}
		})
	}
}

func TestSyslogRejectsUntrustedServer(t *testing.T) {
	b, other := testcerts.New(t), testcerts.New(t)
	srv := startStreamServer(t, &tls.Config{Certificates: []tls.Certificate{b.ServerCert}}, false)
	address := srv.ln.Addr().String()
	cfg := syslogConfig("tls", address)
	cfg.TLS.CAFile = other.CAFile

	err := newTestSyslog(t, cfg).Send(context.Background(), testRecord())
	if err == nil || !strings.Contains(err.Error(), "certificate verification failed") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), address) {
		t.Errorf("error leaks address: %v", err)
	}
	select {
	case frame := <-srv.frames:
		t.Errorf("untrusted server received %q", frame)
	case <-time.After(50 * time.Millisecond):
	}
}

// logCapture collects the standard logger's output for one test.
type logCapture struct {
	mu   sync.Mutex
	text strings.Builder
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}

func captureLog(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	previous := log.Writer()
	log.SetOutput(c)
	t.Cleanup(func() { log.SetOutput(previous) })
	return c
}

// TLS 1.3 clients finish the handshake before the server checks their
// certificate, so this rejection only ever reaches the watcher.
func TestSyslogLogsPostHandshakeRejection(t *testing.T) {
	b := testcerts.New(t)
	logs := captureLog(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(syslogTestWait))
		_ = tls.Server(raw, &tls.Config{
			Certificates: []tls.Certificate{b.ServerCert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    b.CAPool,
			MinVersion:   tls.VersionTLS13,
		}).Handshake()
		// Holding the socket open keeps a reset from racing the alert.
		<-release
	}()
	defer func() {
		_ = ln.Close()
		close(release)
		<-done
	}()

	address := ln.Addr().String()
	cfg := syslogConfig("tls", address)
	cfg.TLS.CAFile = b.CAFile
	s := newTestSyslog(t, cfg)
	// Send may succeed or fail depending on when the alert arrives.
	_ = s.Send(context.Background(), testRecord())

	deadline := time.Now().Add(syslogTestWait)
	for !strings.Contains(logs.String(), "TLS handshake rejected by server") {
		if time.Now().After(deadline) {
			t.Fatalf("rejection was not logged: %q", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := logs.String(); !strings.Contains(got, `"soc"`) || strings.Contains(got, address) {
		t.Errorf("log = %q, want output name and no address", got)
	}
}

func TestSyslogLocalCloseIsNotLogged(t *testing.T) {
	logs := captureLog(t)
	srv := startStreamServer(t, nil, false)
	s := newTestSyslog(t, syslogConfig("tcp", srv.ln.Addr().String()))
	if err := s.Send(context.Background(), testRecord()); err != nil {
		t.Fatal(err)
	}
	srv.next(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); got != "" {
		t.Errorf("local close logged %q", got)
	}
}

func TestSyslogReconnectsAfterDrop(t *testing.T) {
	srv := startStreamServer(t, nil, true)
	s := newTestSyslog(t, syslogConfig("tcp", srv.ln.Addr().String()))
	if err := s.Send(context.Background(), testRecord()); err != nil {
		t.Fatal(err)
	}
	assertEnvelope(t, srv.next(t))

	deadline := time.Now().Add(syslogTestWait)
	for {
		s.mu.Lock()
		dropped := s.conn == nil
		s.mu.Unlock()
		if dropped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dropped connection was not detected")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := s.Send(context.Background(), testRecord()); err != nil {
		t.Fatal(err)
	}
	assertEnvelope(t, srv.next(t))
	if got := srv.conns.Load(); got != 2 {
		t.Errorf("connections = %d, want 2", got)
	}
}

func TestSyslogReconnectsAfterWriteFailure(t *testing.T) {
	srv := startStreamServer(t, nil, false)
	s := newTestSyslog(t, syslogConfig("tcp", srv.ln.Addr().String()))
	if err := s.Send(context.Background(), testRecord()); err != nil {
		t.Fatal(err)
	}
	assertEnvelope(t, srv.next(t))

	// Closing the socket underneath the output makes the next write fail
	// unless the watcher already noticed; the retry must deliver either way.
	s.mu.Lock()
	_ = s.conn.Close()
	s.mu.Unlock()
	if err := newRetrying(s, 5*time.Second).Send(context.Background(), testRecord()); err != nil {
		t.Fatal(err)
	}
	assertEnvelope(t, srv.next(t))
}

func TestSyslogConcurrentSendsKeepFramesIntact(t *testing.T) {
	const senders, each = 16, 25
	srv := startStreamServer(t, nil, false)
	s := newTestSyslog(t, syslogConfig("tcp", srv.ln.Addr().String()))

	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				rec := testRecord()
				rec.Data = []byte(fmt.Sprintf(`{"id":"%d-%d","pad":"%s"}`, i, j, strings.Repeat("p", 2000)))
				if err := s.Send(context.Background(), rec); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}

	seen := make(map[string]bool, senders*each)
	for n := 0; n < senders*each; n++ {
		msg := srv.next(t)
		assertEnvelope(t, msg)
		var payload struct {
			APIResults struct {
				ID string `json:"id"`
			} `json:"api_results"`
		}
		if err := json.Unmarshal([]byte(strings.SplitN(msg, " ", 8)[7]), &payload); err != nil {
			t.Fatal(err)
		}
		if seen[payload.APIResults.ID] {
			t.Errorf("duplicate frame %s", payload.APIResults.ID)
		}
		seen[payload.APIResults.ID] = true
	}
	wg.Wait()
}

// stalledListener accepts at the kernel level but never reads.
func stalledListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// stallingRecord is a record larger than the socket buffers. Sent to a server
// that never reads, it stalls the write on Linux. Windows buffers one whole
// send of any size while its send buffer is empty, so there the second such
// send is the one that stalls.
func stallingRecord() Record {
	rec := testRecord()
	rec.Data = []byte(strings.Repeat("x", 32<<20))
	return rec
}

func TestSyslogCloseUnblocksStalledWrite(t *testing.T) {
	cfg := syslogConfig("tcp", stalledListener(t))
	cfg.Timeout = 60
	s := newTestSyslog(t, cfg)
	rec := stallingRecord()

	// Send until a send stalls, then wait until it has been stuck far longer
	// than formatting the record takes, so Close interrupts the write itself.
	var sendStarted atomic.Int64
	done := make(chan error, 1)
	go func() {
		for {
			sendStarted.Store(time.Now().UnixNano())
			if err := s.Send(context.Background(), rec); err != nil {
				done <- err
				return
			}
		}
	}()
	const stuck = 2 * time.Second
	giveUp := time.Now().Add(syslogTestWait + stuck)
	for started := sendStarted.Load(); started == 0 || time.Since(time.Unix(0, started)) < stuck; started = sendStarted.Load() {
		select {
		case err := <-done:
			t.Fatalf("Send failed before it stalled: %v", err)
		default:
		}
		if time.Now().After(giveUp) {
			t.Fatal("no send stalled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errSyslogClosed) {
			t.Errorf("error = %v", err)
		}
	case <-time.After(syslogTestWait):
		t.Fatal("Send did not return after Close")
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := s.Send(context.Background(), testRecord()); !errors.Is(err, errSyslogClosed) {
		t.Errorf("Send after Close = %v", err)
	}
}

func TestSyslogWriteTimesOut(t *testing.T) {
	cfg := syslogConfig("tcp", stalledListener(t))
	cfg.Timeout = 1
	s := newTestSyslog(t, cfg)
	rec := stallingRecord()
	// The first send stalls on Linux. Windows may buffer it whole, and then
	// the second one stalls (see stallingRecord).
	var err error
	for attempt := 0; attempt < 2 && err == nil; attempt++ {
		err = s.Send(context.Background(), rec)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		t.Error("connection kept after a failed write")
	}
}

func TestFanOutWithFailedOrHungSyslog(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := closed.Addr().String()
	_ = closed.Close()

	for name, address := range map[string]string{"failing": refused, "hanging": stalledListener(t)} {
		t.Run(name, func(t *testing.T) {
			received := make(chan struct{}, 8)
			hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				received <- struct{}{}
			}))
			defer hook.Close()

			syslogEntry := *syslogConfig("tcp", address)
			syslogEntry.Name, syslogEntry.Timeout = "syslog", 2
			webhookEntry := *webhookConfig(hook.URL)
			cfg := &config.Config{LogOutputs: []config.LogOutput{syslogEntry, webhookEntry}}
			router, err := NewRouter(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer router.Close()
			dispatcher, err := router.Dispatcher(&config.WebAPI{Name: "src", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}

			rec := testRecord()
			rec.Data = []byte(strings.Repeat("x", 16<<20))
			for i := 0; i < 3; i++ {
				dispatcher.Deliver(context.Background(), rec)
				select {
				case <-received:
				case <-time.After(syslogTestWait):
					t.Fatalf("webhook did not receive record %d", i)
				}
			}
		})
	}
}
