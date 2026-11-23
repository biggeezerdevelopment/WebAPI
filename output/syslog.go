package output

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/tlsconfig"
)

const (
	// maxSyslogDatagram is the largest message sent over UDP.
	maxSyslogDatagram = 65000

	// RFC 5424 header field limits.
	maxSyslogHostname = 255
	maxSyslogAppName  = 48

	syslogNilValue = "-"
	// syslogTimeFormat is RFC 3339 with the six-digit fraction RFC 5424 allows.
	syslogTimeFormat = "2006-01-02T15:04:05.000000Z07:00"
)

// ErrSyslogMessageTooLarge reports a message that cannot fit in one datagram.
// Retrying cannot help, so the retry wrapper gives up on it immediately.
var ErrSyslogMessageTooLarge = errors.New("syslog message exceeds the UDP size limit")

var errSyslogClosed = errors.New("syslog output is closed")

// Syslog sends records as RFC 5424 messages. Stream transports reuse one
// connection and use octet-counted framing; UDP sends one datagram per message.
type Syslog struct {
	name      string
	network   string
	address   string
	tlsConfig *tls.Config
	pri       int
	appName   string
	timeout   time.Duration

	// sendSlot serializes sends. It is a channel so that a sender waiting
	// behind a stalled write still honors its context.
	sendSlot chan struct{}
	closeCtx context.Context
	cancel   context.CancelFunc
	readers  sync.WaitGroup

	mu     sync.Mutex // guards conn and closed
	conn   net.Conn
	closed bool
}

// NewSyslog builds a syslog output from a validated configuration entry. It
// does not connect, so an unavailable server cannot stop the agent starting.
func NewSyslog(cfg *config.LogOutput) (*Syslog, error) {
	pri, ok := config.SyslogPriority(cfg.Facility, cfg.Severity)
	if !ok {
		return nil, errors.New("unknown facility or severity")
	}

	var tlsCfg *tls.Config
	if cfg.Network == config.SyslogNetworkTLS {
		var err error
		if tlsCfg, err = tlsconfig.Build(cfg.TLS); err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
		if tlsCfg.ServerName == "" {
			host, _, err := net.SplitHostPort(cfg.Address)
			if err != nil {
				return nil, errors.New("address must be a valid host:port")
			}
			// An IPv6 zone is local to this machine and never in a certificate.
			tlsCfg.ServerName, _, _ = strings.Cut(host, "%")
		}
	}

	closeCtx, cancel := context.WithCancel(context.Background())
	return &Syslog{
		name:      cfg.Name,
		network:   cfg.Network,
		address:   cfg.Address,
		tlsConfig: tlsCfg,
		pri:       pri,
		appName:   strings.TrimSpace(cfg.AppName),
		timeout:   time.Duration(cfg.Timeout) * time.Second,
		sendSlot:  make(chan struct{}, 1),
		closeCtx:  closeCtx,
		cancel:    cancel,
	}, nil
}

// Send writes one record. Returned errors never contain the server address.
func (s *Syslog) Send(ctx context.Context, rec Record) error {
	msg, err := s.format(rec)
	if err != nil {
		return err
	}
	payload := msg
	if s.network == config.SyslogNetworkUDP {
		if len(msg) > maxSyslogDatagram {
			return fmt.Errorf("%w: %d bytes, limit %d", ErrSyslogMessageTooLarge, len(msg), maxSyslogDatagram)
		}
	} else {
		payload = frameOctetCounted(msg)
	}

	select {
	case s.sendSlot <- struct{}{}:
		defer func() { <-s.sendSlot }()
	case <-ctx.Done():
		return sanitizeTransportError("syslog", ctx.Err())
	case <-s.closeCtx.Done():
		return errSyslogClosed
	}

	conn, err := s.connection(ctx)
	if err != nil {
		return s.sendError(ctx, err)
	}
	if err := s.write(ctx, conn, payload); err != nil {
		// A partial frame would corrupt the stream, so the connection is
		// never reused after a failed write.
		s.drop(conn)
		return s.sendError(ctx, err)
	}
	return nil
}

func (s *Syslog) sendError(ctx context.Context, err error) error {
	if s.closeCtx.Err() != nil {
		return errSyslogClosed
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return sanitizeTransportError("syslog", ctxErr)
	}
	return sanitizeTransportError("syslog", err)
}

// connection returns the open connection or dials a new one. The caller must
// hold sendSlot.
func (s *Syslog) connection(ctx context.Context) (net.Conn, error) {
	s.mu.Lock()
	conn, closed := s.conn, s.closed
	s.mu.Unlock()
	if closed {
		return nil, errSyslogClosed
	}
	if conn != nil {
		return conn, nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	stop := context.AfterFunc(s.closeCtx, cancel)
	defer stop()

	conn, err := s.dial(dialCtx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = conn.Close()
		return nil, errSyslogClosed
	}
	s.conn = conn
	if s.network != config.SyslogNetworkUDP {
		s.readers.Add(1)
		go s.watch(conn)
	}
	return conn, nil
}

func (s *Syslog) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{}
	switch s.network {
	case config.SyslogNetworkTLS:
		return (&tls.Dialer{NetDialer: dialer, Config: s.tlsConfig}).DialContext(ctx, "tcp", s.address)
	case config.SyslogNetworkTCP:
		return dialer.DialContext(ctx, "tcp", s.address)
	default:
		return dialer.DialContext(ctx, "udp", s.address)
	}
}

// watch discards anything the server sends and drops the connection when the
// peer closes it. Without this, the first write after a disconnect would
// succeed into the socket buffer and its record would be lost silently. Read
// errors are logged because a TLS 1.3 server can reject the client certificate
// after the client handshake returns, when Send has nothing to report.
func (s *Syslog) watch(conn net.Conn) {
	defer s.readers.Done()
	_, err := io.Copy(io.Discard, conn)
	s.drop(conn)
	// net.ErrClosed means this side closed the connection on purpose.
	if err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("Syslog output %q lost its connection: %v", s.name, sanitizeTransportError("syslog", err))
	}
}

func (s *Syslog) write(ctx context.Context, conn net.Conn, payload []byte) error {
	deadline := time.Now().Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetWriteDeadline(time.Now()) })
	defer stop()
	_, err := conn.Write(payload)
	return err
}

// drop closes conn if it is still the current connection.
func (s *Syslog) drop(conn net.Conn) {
	s.mu.Lock()
	if s.conn != conn {
		s.mu.Unlock()
		return
	}
	s.conn = nil
	s.mu.Unlock()
	_ = conn.Close()
}

// Close releases the connection and unblocks any dial or write in progress. It
// is safe to call concurrently with Send and more than once.
func (s *Syslog) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()

	s.cancel()
	if conn != nil {
		// A close error only means the peer is already gone, and its text
		// would include the server address.
		_ = conn.Close()
	}
	s.readers.Wait()
	return nil
}

// format renders rec as an RFC 5424 message whose MSG is the JSON envelope.
// encoding/json output never contains a raw newline, so MSG is a single line.
func (s *Syslog) format(rec Record) ([]byte, error) {
	body, err := api.EncodeLogPayload(rec.AgentID, rec.AgentName, rec.Source, rec.Timestamp.Unix(), rec.Data)
	if err != nil {
		return nil, fmt.Errorf("error marshaling log payload: %v", err)
	}

	timestamp := syslogNilValue
	if !rec.Timestamp.IsZero() {
		timestamp = rec.Timestamp.UTC().Format(syslogTimeFormat)
	}
	appName := s.appName
	if appName == "" {
		appName = rec.Source
	}

	// PROCID, MSGID, and STRUCTURED-DATA are not used.
	header := fmt.Sprintf("<%d>1 %s %s %s - - - ", s.pri, timestamp,
		sanitizeSyslogField(rec.AgentName, maxSyslogHostname),
		sanitizeSyslogField(appName, maxSyslogAppName))
	return append([]byte(header), body...), nil
}

// sanitizeSyslogField returns value as printable ASCII without spaces, at most
// limit bytes long. Each other character becomes '_', and an empty value
// becomes the RFC 5424 NILVALUE.
func sanitizeSyslogField(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return syslogNilValue
	}
	var b strings.Builder
	for _, r := range value {
		if b.Len() == limit {
			break
		}
		if r >= 33 && r <= 126 {
			b.WriteByte(byte(r))
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// frameOctetCounted applies RFC 5425 framing: "<length> <message>".
func frameOctetCounted(msg []byte) []byte {
	frame := make([]byte, 0, len(msg)+12)
	frame = strconv.AppendInt(frame, int64(len(msg)), 10)
	frame = append(frame, ' ')
	return append(frame, msg...)
}
