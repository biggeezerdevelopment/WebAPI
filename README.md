# WebAPI Agent

A Go-based agent that collects and forwards metrics from various web APIs to a centralized logging system.

## Overview

This project implements a configurable agent that can:
- Monitor multiple web APIs simultaneously
- Collect metrics from configured endpoints
- Forward collected data to a remote logging system
- Support various authentication methods
- Handle different API request methods and content types

## Prerequisites

- Go 1.23.1 or later
- Git

## Installation

1. Clone the repository:
```bash
git clone <repository-url>
cd WebAPI
```

2. Install dependencies:
```bash
go mod download
```

## Configuration

The project uses a YAML configuration file (`config.yml`) with the following structure:

### JarvisAgent Configuration
```yaml
jarvisagent:
    name: "jarvis-agent-windows"  # Name of the agent
    id: "1234567890"             # Unique identifier for the agent
```

### API Log Remote Configuration
```yaml
apilogremote:
    enabled: false               # Enable/disable remote logging
    api_url: "http://localhost:8080/api/v1/logs"
    api_method: "POST"
    skip_verify: false           # Skip TLS certificate verification (default: false)
    api_headers:
        - key: "Content-Type"
          value: "application/json"
```

TLS certificates from the remote log server are verified by default. Set `skip_verify: true` only for a trusted server with a self-signed or otherwise unverifiable certificate, because it leaves log traffic and API headers open to interception.

### WebAPI Configuration
```yaml
webapi:
    - enabled: false            # Enable/disable this API
      name: "qualys"            # Name of the API
      subject: "QualysAPI"      # Subject identifier
      endpoint: "http://localhost:8080/api/v1/metrics"
      method: "post"
      headers:
        - key: "Content-Type"
          value: "application/json"
      auth_type: "apikey"       # Authentication type
      api_key: "1234567890"     # API key for authentication
      skip_verify: true         # Skip SSL verification
      sleeptime: 30            # Time between requests in seconds
      timeout: 30              # Request timeout in seconds
      params:
        metrics: "20"          # Query parameters
      body:
        - "metrics"            # Request body parameters
      outputs: ["siem-webhook"] # Optional routing, see Log Outputs
```

### Log Outputs

The top-level `logoutputs` list defines named destinations. The supported `type` values are `webhook` and `syslog`.

```yaml
logoutputs:
    - name: "siem-webhook"
      enabled: true
      type: "webhook"
      url: "https://hooks.example.com/audit"
      method: "POST"            # POST, PUT, or PATCH
      timeout: 10               # Seconds (1-3600), covering all retries of one record
      headers:
        - key: "Content-Type"
          value: "application/json"
        - key: "Authorization"
          value_env: "SIEM_WEBHOOK_AUTH"  # Read from the environment
      tls:
        ca_file: "/etc/webapi/certs/internal-ca.pem"
        # cert_file: "/etc/webapi/certs/client.pem"
        # key_file: "/etc/webapi/certs/client-key.pem"
        # server_name: "hooks.internal"
        # skip_verify: false
```

**Routing.** Each `webapi` entry selects its destinations with `outputs`:

| Setting | Records go to |
|---|---|
| `outputs` omitted | All enabled outputs |
| `outputs: []` | No outputs |
| `outputs: ["siem-webhook", "apilogremote"]` | The named outputs |

When `apilogremote` is enabled it is available as an output named `apilogremote`, and that name cannot be used in `logoutputs`. Routing to an unknown or disabled output is a startup error.

**Headers.** Each header sets exactly one of `value` or `value_env`. A variable named by `value_env` must be set and non-empty at startup. Prefer `value_env` for credentials so they stay out of the config file.

**TLS.** Connections require TLS 1.2 or later and verify the server by default.

- `ca_file` trusts only that PEM bundle. Without it, the system roots are used. Only blank lines and `#` comments may appear outside certificate blocks. Any other text, a damaged block, or a non-certificate block is a startup error.
- `cert_file` and `key_file` together provide a client certificate for mutual TLS.
- `server_name` overrides the name used for certificate verification.
- `skip_verify: true` disables verification and cannot be combined with `ca_file`.

The agent logs a startup warning for plaintext `http` webhooks, plaintext `tcp` or `udp` syslog, and `skip_verify: true`.

**Syslog.** A `syslog` output sends RFC 5424 messages to a remote server:

```yaml
logoutputs:
    - name: "soc-syslog"
      enabled: true
      type: "syslog"
      network: "tls"            # tls, tcp, or udp
      address: "syslog.example.com:6514"
      facility: "local0"        # kern ... clock, local0-local7
      severity: "info"          # emerg, alert, crit, err, warning, notice, info, debug
      app_name: "webapi-agent"  # Optional, defaults to the source name
      timeout: 10               # Seconds (1-3600), covering all retries of one record
      tls:                      # Only allowed with network: tls
        ca_file: "/etc/webapi/certs/syslog-ca.pem"
        # cert_file: "/etc/webapi/certs/client.pem"
        # key_file: "/etc/webapi/certs/client-key.pem"
        # server_name: "syslog.example.com"  # Defaults to the host in address
        # skip_verify: false
```

- `HOSTNAME` is `jarvisagent.name` and `MSG` is the single-line JSON log envelope. Header fields are limited to printable ASCII, with other characters replaced by `_`, and empty values are sent as `-`.
- `tls` and `tcp` reuse one connection with octet-counted framing (RFC 5425) and reconnect after a failure. A record that was resent after a failed write can arrive twice.
- `udp` sends one datagram per message. A message over 65,000 bytes is dropped with an error rather than truncated, and operating system limits on datagram size may be lower.

**Delivery.** A source delivers to its outputs concurrently, so a slow or failed output does not block another. Each record is sent as one request or message and retried up to 3 times with exponential backoff within the output's `timeout`. Failures are logged with the source and output names. Records are not queued on disk. A record is dropped for an output when its retries are exhausted or, for a source with several outputs, when that output's bounded in-memory queue is full. Each drop is logged.

## Building

To build the project:

```bash
go build -o webapi-agent
```

## Running

1. Configure the `config.yml` file according to your needs
2. Run the agent:
```bash
./webapi-agent
```

## Features

- Multiple API monitoring support
- Configurable request intervals
- Remote logging capability
- Support for various authentication methods:
  - API Key
  - Basic Authentication
- Customizable headers and request parameters
- SSL verification options
- Configurable timeouts

## Project Structure

```
.
├── api/            # API client implementations
├── config/         # Configuration handling
├── output/         # Named log outputs, routing, and delivery
├── tlsconfig/      # Shared client TLS configuration builder
├── main.go         # Main application entry point
├── config.yml      # Configuration file
├── go.mod          # Go module definition
└── go.sum          # Go module checksums
```

## Error Handling

The agent includes comprehensive error handling:
- Failed API requests are logged
- Remote logging failures are logged
- Configuration loading errors are handled gracefully

## Contributing

1. Fork the repository
2. Create your feature branch
3. Commit your changes
4. Push to the branch
5. Create a new Pull Request

## License

[Add your license information here] 