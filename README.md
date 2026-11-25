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

**Delivery.** A source delivers to its outputs concurrently, so a slow or failed output does not block another. Each record is sent as one request or message and retried up to 3 times with exponential backoff within the output's `timeout`. Failures are logged with the source and output names. Records are not queued on disk. A record is dropped for an output when its retries are exhausted or, for a source with several outputs, when that output's bounded in-memory queue is full. Each drop is logged. GitHub audit collectors use the stronger delivery described below.

### GitHub Audit Log Collector

A `webapi` entry with a `collector` polls a GitHub organization audit log and delivers each event as its own record, in `@timestamp` order. Delivery is at least once to outputs that confirm receipt, such as a webhook. Syslog cannot confirm receipt, as described below. The API requires an organization plan that includes it and a token with audit log access.

```yaml
state_dir: "./state"

webapi:
    - enabled: true
      name: "github-audit"
      subject: "GitHubAudit"
      endpoint: "https://api.github.com/orgs/example-org/audit-log"
      method: "get"
      auth_type: "apikey"
      api_key_env: "GITHUB_AUDIT_TOKEN"  # Read from the environment
      sleeptime: 60
      timeout: 30
      headers:
        - key: "Accept"
          value: "application/vnd.github+json"
        - key: "X-GitHub-Api-Version"
          value: "2022-11-28"
      params:
        include: "all"
        per_page: "100"
        order: "asc"            # Must be asc or omitted
      collector:
        type: "github_audit"
        initial_lookback_hours: 24  # 1-4320, default 24
      outputs: ["soc-syslog", "siem-webhook"]
```

- `api_key_env` works on any `webapi` entry and cannot be combined with `api_key`. The variable must be set and non-empty at startup.
- `api_password_env` does the same for the basic-auth password (for example an API token) and cannot be combined with `api_password`. With `auth_type: basic`, startup fails unless `api_username` is set and a password resolves. Errors name the variable but never its value:

  ```yaml
  - enabled: true
    name: "example"
    endpoint: "https://example.atlassian.net/rest/api/3/myself"
    method: "get"
    auth_type: "basic"
    api_username: "svc-audit@example.com"
    api_password_env: "JIRA_API_TOKEN"
  ```

- Each collector type sets the auth it needs. `github_audit` requires `method: get`, `auth_type: apikey`, a usable `state_dir`, and at least one output. Its endpoint must have no query string. It manages `after`, `before`, and the `created:` qualifier, so they cannot be configured. Other `phrase` filters are kept.
- Each output has its own checkpoint: the newest delivered timestamp and the event IDs delivered at that timestamp. An output's batch stops at its first failed record and resumes from its checkpoint on a later poll, so one output being down does not block another or lose its backlog while GitHub retains the events.
- Caught-up outputs are fetched first. An output that keeps failing is retried with a doubling delay of up to one hour, so its backlog fetches do not use up the GitHub rate limit. The delay resets when the output makes progress or the agent restarts.
- An output with no checkpoint starts `initial_lookback_hours` back. That start is saved immediately, and adding an output does not change any other output's checkpoint.
- A failed poll forwards nothing. `Link` pagination, `Retry-After` (on rate-limit and 5xx responses), and rate-limit headers are honored. Pagination links to another origin and redirects are refused so the token stays with the configured host.
- A fetch normally stops at 100 pages, but keeps paging until it passes the checkpoint's timestamp so a very busy second cannot stall an output. If 10,000 pages do not pass it, the poll logs a "no progress" error, delivers nothing to the outputs at that checkpoint, retries them with a doubling delay, and still serves other outputs. `per_page` must be 1 to 100. Keep it at `100` to avoid this and to limit rate-limit use.
- State is saved atomically in `state_dir`, one file per source. A corrupt file, or a checkpoint that is negative or in the future, stops startup. Run only one agent per `state_dir`.
- Collector delivery to `apilogremote` treats any redirect as a failed send, because a followed redirect can drop the request body. Plain sources keep following redirects on that output.
- A record that fails permanently, such as one too large for UDP syslog, holds that output at that record until the cause is fixed.
- Syslog has no application-level acknowledgement, so the agent logs a startup warning when a collector routes to it on any transport. Over `tls` or `tcp`, a write succeeds once it reaches the local socket buffer, so events written during a silent peer hang or network partition can be lost even though their checkpoint advances. A closed connection is detected and retried. UDP confirms nothing. Use a webhook output where at-least-once delivery is required.
- An event GitHub publishes with a timestamp older than an output's checkpoint is not delivered to that output, because delivering it would break timestamp order.

### Atlassian Organization Audit Log Collector

The `atlassian_org_audit` collector polls an Atlassian organization's audit log through the events-stream API (`GET https://api.atlassian.com/admin/v1/orgs/{orgId}/events-stream`) and sends each event, unchanged, to the routed outputs. It uses the same engine as `github_audit`, so the checkpoint, retry, backoff and fan-out rules above apply to it as well.

```yaml
webapi:
  - enabled: true
    name: "atlassian-org-audit"
    subject: "AtlassianOrgAudit"
    endpoint: "https://api.atlassian.com/admin/v1/orgs/<org-id>/events-stream"
    method: "get"
    auth_type: "apikey"
    api_key_env: "ATLASSIAN_ORG_API_KEY"
    sleeptime: 60
    timeout: 30
    params:
      limit: "500"              # optional, 1-500, default 500
    collector:
      type: "atlassian_org_audit"
      initial_lookback_hours: 24
    outputs: ["siem-webhook"]
```

- Create an organization API key with the `read:events:admin` scope. The key is sent as `Authorization: Bearer <key>`. Only `auth_type: apikey` is accepted.
- The collector manages `from`, `to`, `cursor` and `sortOrder`, so they cannot be set in `params`. `limit` must be a whole number from 1 to 500.
- Events are keyed by `data[].id` and ordered by `attributes.processedAt`. An event with no `id`, or a missing or invalid `processedAt`, fails the poll, as does a page whose `processedAt` values decrease.
- The first request of a window asks for `from` one hour before the checkpoint, with `sortOrder=asc`. Atlassian does not document which timestamp `from` filters on, and an event's time can be earlier than its processing time. The overlap covers that, and the checkpoint filters out events that were already delivered.
- Pages are followed with `cursor` set to `meta.next`. The API may return `meta.next` on the last page so that clients can poll for new events, so the window also ends when a page holds fewer than `limit` events.

### Jira Cloud Audit Records Collector

The `jira_audit` collector polls a Jira Cloud site's audit records (`GET https://<site>.atlassian.net/rest/api/3/auditing/record`) and sends each record, unchanged, to the routed outputs. It uses the same engine as `github_audit`.

```yaml
webapi:
  - enabled: true
    name: "jira-audit"
    subject: "JiraAudit"
    endpoint: "https://example.atlassian.net/rest/api/3/auditing/record"
    method: "get"
    auth_type: "basic"
    api_username: "svc-audit@example.com"
    api_password_env: "JIRA_API_TOKEN"
    sleeptime: 300
    timeout: 30
    params:
      limit: "1000"            # optional, 1-1000, default 1000
      #filter: "user management"
    collector:
      type: "jira_audit"
      initial_lookback_hours: 24
    outputs: ["siem-webhook"]
```

- The account needs the **Administer Jira** global permission. Authenticate with its email as `api_username` and an API token in `api_password_env`. Only `auth_type: basic` is accepted.
- The collector manages `from`, `to` and `offset`, so they cannot be set in `params`. `limit` must be a whole number from 1 to 1000. `filter` is passed through.
- Each window fixes `to` at now minus 60 seconds and `from` at the checkpoint rounded down to the second, both formatted as `2006-01-02T15:04:05.000+0000` in UTC. It requests `offset=0&limit=1` to read `total`, then reads pages from the last offset back to 0 and reverses each one, so records come out oldest first.
- Atlassian does not document the sort order. The collector expects newest first and fails the poll if a page, or the boundary between two pages, is in another order. It also fails the poll if `total` changes during the window or a page holds the wrong number of records. The engine retries on the next cycle.
- Records are keyed by `records[].id` (an integer) and ordered by `created`, which may use a `+0000`-style offset or RFC 3339. A record with a missing or invalid `id` or `created` fails the poll.

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
├── collector/      # Checkpointed GitHub audit log collection
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