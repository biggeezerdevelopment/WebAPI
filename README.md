# WebAPI Agent

A Go-based agent that polls web APIs and audit logs and forwards each result to webhook, syslog or remote log outputs.

## Overview

This project implements a configurable agent that can:
- Poll multiple web APIs at once, each on its own `sleeptime`
- Collect audit logs from GitHub, Atlassian organizations, Jira Cloud and Stripe, with checkpointed, at-least-once delivery
- Route each source to named webhook or syslog outputs, or to `apilogremote`
- Read credentials from environment variables instead of the config file
- Send a periodic heartbeat so a SIEM can tell the agent is alive

## Prerequisites

- Go 1.23.1 or later. `go.mod` sets `toolchain go1.26.11`, so an older Go downloads and uses go1.26.11 to build, unless `GOTOOLCHAIN=local` is set.
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
    api_timeout: 30              # Seconds (1-3600), default 30, covering all retries of one record
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
      skip_verify: true         # Shorthand for tls.skip_verify. Do not combine with tls
      # tls:                    # Same options and rules as an output's tls
      #   ca_file: "/etc/webapi/certs/internal-ca.pem"
      #   server_name: "api.internal"
      sleeptime: 30            # Time between requests in seconds
      timeout: 30              # Request timeout in seconds
      params:
        metrics: "20"          # Query parameters
      body:
        - "metrics"            # Request body parameters
      outputs: ["siem-webhook"] # Optional routing, see Log Outputs
```

**Source TLS.** A `webapi` entry accepts an optional `tls` section with `ca_file`, `cert_file` and `key_file` (mutual TLS), `server_name` and `skip_verify`. The rules match output TLS (see **TLS** under Log Outputs), including TLS 1.2 or later. The top-level `skip_verify` is shorthand for `tls.skip_verify`. Setting a top-level `skip_verify`, even `false`, together with `tls` is a startup error, so there is one place to look. A bad TLS file is a startup error that names the source and the file.

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
      # spool:                  # Optional disk queue, see Spool below
      #   enabled: true
      #   max_size_mb: 256      # 1-102400, default 256
      #   max_age_hours: 72     # 0-87600, default 72, 0 means never expire
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

The agent logs a startup warning for plaintext `http` webhooks and sources, plaintext `tcp` or `udp` syslog, and `skip_verify: true` on an output or source.

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

**Delivery.** A source delivers to its outputs concurrently, so a slow or failed output does not block another. Each record is sent as one request or message and retried up to 3 times with exponential backoff within the output's `timeout` (`api_timeout` for `apilogremote`). A redirect that is not followed (301, 302, 303, 307 or 308) fails after one attempt, because every retry would fail the same way. Failures are logged with the source and output names. Unless the output has a spool, records are not queued on disk. A record is dropped for such an output when its retries are exhausted or, for a source with several outputs, when that output's bounded in-memory queue is full. Each drop is logged. For a spooled output, a plain source's records are written to disk and sent in order, at least once, so they are not dropped for exhausted retries or a full queue. A crash, or a shutdown during a send, means that record is sent again after the next start. After a failure the worker backs off from 1 s, doubling up to 5 min, and never drops a record because a send failed. The spool's size and age limits, and corruption, can still drop records (see Spool). A permanent error, such as an oversized UDP syslog message or a refused redirect, is logged and the record is skipped. A disk error while appending is logged and counted as a delivery failure. The optional heartbeat (see Heartbeat) reports these outcomes. Collector sources (`github_audit`, `atlassian_org_audit`, `jira_audit`, `stripe_activity_logs`) use the stronger delivery described below.

**Spool.** Any `logoutputs` entry, and `apilogremote`, can have a `spool` block that queues records on disk until the output accepts them. Only plain sources are spooled. Collectors (which have their own checkpoints) and heartbeats bypass the spool.

- An enabled spool requires `state_dir`. Without it startup fails with `spool requires state_dir`. The limits are validated even when `enabled` is `false`.
- A `spool` block with no `enabled` key leaves spooling off, and startup logs `output "<name>" has a spool block with no "enabled" key; spooling is off` (also shown by `-check-config`). Set `enabled: true` to spool.
- Each spool is a directory `<state_dir>/spool/output-<hash>`, where the hash is derived from the output name.
- Renaming or disabling an output, or disabling its spool, leaves its backlog in `<state_dir>/spool/output-<hash>` unsent. That backlog holds plaintext record data. Remove such directories with `-purge-spools` (see Running). It treats the spool of a disabled output, or of an output whose spool is off, as stale, so any unsent records in it are deleted. Run it only while the agent is stopped, or after the agent was restarted with the current config.
- Files are split into segments of at most 8 MiB, and no more than a quarter of `max_size_mb`. When the spool is full, the oldest whole segments are dropped. Records older than `max_age_hours` are dropped when they are read, and records in a corrupt segment are dropped. Each drop is logged and counted in the heartbeat.
- A record larger than `max_size_mb` cannot be spooled. It is logged, counted as a delivery failure, and lost for that output.
- Spool files hold record data in plain text. The agent creates directories with mode 0700 and files with mode 0600. Restrict access to `state_dir`.
- If the read position cannot be saved to the `ack` file, as on a full disk, the spool keeps delivering and keeps enforcing its size limit. The position moves in memory, and the `ack` file can lag behind it. After a crash the agent sends records again but never skips any. The failure is logged once for each run of failures.
- At startup, if the `ack` file names a segment that is missing or was already removed, the agent resumes at the next segment that exists. When the missing segment lies between existing ones it logs a warning. An `ack` file that points past the last segment still stops startup.
- In the heartbeat, each spooled record is credited to the source that produced it. A source is identified by its name, subject, endpoint and method, so reordering `webapi` entries does not misattribute records. A record left by an older version, or one whose source no longer exists or is ambiguous, is still delivered. Its failures are reported to every source routed to that output, and its success is credited to no source. Two enabled sources with the same name, subject, endpoint and method get a startup warning, and neither is credited.

### Heartbeat

The agent can send a periodic heartbeat record to one or more outputs so a SIEM can tell that it is alive. Each heartbeat lists every enabled `webapi` entry with its last pull, its last successful pull, and the pull and delivery errors since the previous heartbeat.

```yaml
heartbeat:
    enabled: true
    interval: 300               # Seconds, 30-3600, default 300
    name: "webapi-heartbeat"    # Record Source, default "heartbeat"
    outputs: ["siem-webhook"]   # Required
```

- When `enabled` is false or the block is omitted, nothing changes. `config.yml` ships a disabled example.
- `outputs` is required. Each name must be an enabled output, including `apilogremote`, and must be listed only once.
- `name` cannot match the name of an enabled `webapi` entry.
- A bad value stops the agent at startup.
- The agent logs a startup warning when the heartbeat routes to a UDP syslog output, because UDP cannot confirm delivery.

**Schedule.**

- One heartbeat is sent at startup, after all collectors are built, so a restart is visible.
- After that, one heartbeat is sent every `interval`, independent of each source's `sleeptime`.
- The startup heartbeat can go out before the first polls finish, so its last-seen fields may be `null`.

**Payload.** The record's Source is `name` and its timestamp is the snapshot time. Its data holds:

```json
{
  "type": "heartbeat",
  "healthy": false,
  "started_at": "2026-11-25T20:00:00Z",
  "uptime_seconds": 7800,
  "interval_seconds": 300,
  "sources": [
    {
      "name": "github-audit",
      "kind": "collector",
      "collector_type": "github_audit",
      "sleeptime_seconds": 60,
      "last_poll_at": "2026-11-25T22:10:00Z",
      "last_success_at": "2026-11-25T22:10:00Z",
      "errors_since_last": 2,
      "last_error": "unexpected status code: 502",
      "last_error_at": "2026-11-25T22:05:00Z",
      "outputs": [
        {
          "name": "siem-webhook",
          "last_delivery_at": "2026-11-25T22:10:01Z",
          "delivery_failures_since_last": 0,
          "last_delivery_error": null,
          "last_delivery_error_at": null,
          "checkpoint": "2026-11-25T22:09:58Z"
        }
      ]
    },
    {
      "name": "qualys",
      "kind": "plain",
      "collector_type": null,
      "sleeptime_seconds": 30,
      "last_poll_at": "2026-11-25T22:09:30Z",
      "last_success_at": "2026-11-25T22:09:30Z",
      "errors_since_last": 0,
      "last_error": null,
      "last_error_at": null,
      "outputs": [
        {
          "name": "siem-webhook",
          "last_delivery_at": "2026-11-25T22:09:31Z",
          "delivery_failures_since_last": 0,
          "last_delivery_error": null,
          "last_delivery_error_at": null
        }
      ]
    }
  ],
  "spools": [
    {
      "output": "siem-webhook",
      "records": 0,
      "bytes": 0,
      "dropped_since_last": 0
    }
  ]
}
```

- Times are RFC 3339 in UTC. A value that has not been seen yet is `null`.
- `errors_since_last` and `delivery_failures_since_last` reset after every heartbeat. Last-seen times and the most recent error are kept.
- Plain entries have `kind: "plain"`, `collector_type: null` and no `checkpoint` field.
- `spools` has one entry per spooled output, with the records and bytes waiting on disk and the records dropped by the size or age limit, or because of corruption, since the previous heartbeat. `dropped_since_last` resets after every heartbeat. The array is omitted when no output has a spool.
- `checkpoint` appears only on collector outputs. It is seeded from the state file at startup. It is the newest delivered event time as saved in the state file, or the start of the `initial_lookback_hours` window for an output with no saved state. It advances as events are delivered.

**What counts as a pull.**

- **Plain entry:** the request returned no error. A failed request counts as an error.
- **Collector:** the poll made at least one fetch. It is an error when the poll returns one and a success otherwise.
- A collector poll that made no fetch is not counted. This happens when every output was busy or backing off, or when the collector is waiting out a rate limit. A poll ended by shutdown is not counted either.
- A collector "no progress" stall is different: it is returned as a poll error and is counted.

**What counts as a delivery.**

- **Plain entries:** each record sent to an output is a delivery or a failure. A record dropped because that output's queue was full counts as a failure. For a spooled output, a record that matches no single source counts as a failure for every source routed to that output, and its success counts for none (see Spool).
- **Collectors:** each delivered event counts as a delivery and updates `checkpoint`. A failed batch counts as one failure.
- Work interrupted by shutdown is never counted as an error or a failure.

**Healthy flag.** `healthy` is `false` while a source or output has failed more recently than it last succeeded, or when any `errors_since_last` or `delivery_failures_since_last` is above 0. It stays `false` across heartbeats, even after the counts reset, until the source has a successful pull or the output has a successful delivery. A `spools[].dropped_since_last` above 0 also makes it `false`. A spool backlog alone does not. One SIEM rule on this field covers every source.

**Error text.** Errors are cleaned before they are stored, so credentials in `params` or URLs do not reach the SIEM:

- Query strings, fragments and `user:pass@` are removed from URLs.
- URL paths are kept, so do not put secrets in `endpoint` paths.
- A URL that cannot be parsed is replaced with `scheme://[redacted]`.
- Control characters such as newlines become spaces.
- Text is cut to 512 bytes on a character boundary and ends with `…`.

**Heartbeat delivery.** Heartbeats use the normal retries within each output's `timeout` (`api_timeout` for `apilogremote`). A failed heartbeat is logged, and the heartbeat's own deliveries are not tracked in later heartbeats. The SIEM sees a missing heartbeat instead, which is the signal to alert on.

**Suggested SIEM rules.**

- Alert when no record with Source `name` arrives for twice `interval`, for example 10 minutes at `interval: 300`. This means the agent is down or cannot reach the output.
- Alert on any heartbeat with `healthy: false`, and use `sources[].last_error` and `outputs[].last_delivery_error` to find the cause.
- Optionally, alert when a source's `last_success_at` is older than a few multiples of its `sleeptime_seconds`.

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
- A fetch reads at most 100 pages, counted from the page that passes the checkpoint's timestamp. Pages before that do not count, so a very busy second, or a source that must start below the checkpoint, cannot stall an output. If 10,000 pages do not pass it, the poll logs a "no progress" error, delivers nothing to the outputs at that checkpoint, retries them with a doubling delay, and still serves other outputs. `per_page` must be 1 to 100. Keep it at `100` to avoid this and to limit rate-limit use.
- State is saved atomically in `state_dir`, one file per source. A corrupt file, or a checkpoint that is negative or in the future, stops startup. Run only one agent per `state_dir`.
- Collector delivery to `apilogremote` treats any redirect as a failed send, without retries, because a followed redirect can drop the request body. Plain sources keep following redirects on that output.
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

### Stripe Activity Logs Collector

The `stripe_activity_logs` collector polls a Stripe account's security history through the Activity Logs API (`GET https://api.stripe.com/v2/iam/activity_logs`) and sends each record, unchanged, to the routed outputs. It uses the same engine as `github_audit`. The API is a preview and can change without notice.

```yaml
webapi:
  - enabled: true
    name: "stripe-activity-logs"
    subject: "StripeActivityLogs"
    endpoint: "https://api.stripe.com/v2/iam/activity_logs"
    method: "get"
    auth_type: "apikey"
    api_key_env: "STRIPE_ACTIVITY_LOGS_KEY"
    sleeptime: 300
    timeout: 30
    headers:
      - key: "Stripe-Version"
        value: "2026-11-18.preview"
    params:
      limit: "100"              # optional, 1-100, default 100
      #"action_groups[0]": "api_key"
    collector:
      type: "stripe_activity_logs"
      initial_lookback_hours: 24
      settle_minutes: 30        # optional, 10-1440, default 30
    outputs: ["siem-webhook"]
```

- Create a restricted key with the **Activity logs: Read** permission. It is sent as `Authorization: Bearer <key>`. Only `auth_type: apikey` is accepted.
- A `Stripe-Version` header is required. Use `2026-11-18.preview`. Without the header Stripe returns 400, and a non-preview version returns 404.
- The collector manages `page`, so it cannot be set in `params`. `limit` must be a whole number from 1 to 100.
- `actions`, `action_groups` and `logs_from` are passed through. Set a filter bare with one value (`actions: "api_key_created"`), or with indexes for several values (`actions[0]`, `actions[1]`). Indexes must start at 0 with no gaps. A filter cannot be both bare and indexed. Stripe rejects `actions[]`, so the collector refuses it at startup. Other parameters are refused at startup.
- A filter value Stripe does not recognize makes every poll fail with status 400. Test new filters with a single request first.
- The API has no time filter, so each poll walks the list from the oldest retained record. Stripe keeps records for 6 months. A poll costs about `ceil(N / limit) + 1` requests for `N` matching records. Use `sleeptime: 300` or more, and on a large account narrow the walk with `action_groups`.
- The list is in record-ID order, and `created` can step back by about a second between records. Each record's checkpoint time is therefore the highest `created` seen so far in the walk. The record is still delivered unchanged. A late record listed after everything already delivered is still delivered.
- Records created within the last `settle_minutes` are held back until a later poll. Stripe says records take at least 10 minutes to appear and gives no upper bound. The hold-back reduces, but does not remove, the risk of missing a record that Stripe inserts behind an output's checkpoint. `settle_minutes` is accepted only by this collector type.
- Changing the filters changes the running maximum, so records can be re-sent.
- A record with no `id`, or a missing or invalid RFC 3339 `created`, fails the poll.
- `next_page_url` is a path relative to the API host. The collector resolves it against the current page. Links to another host are refused. A walk ends on a page shorter than `limit`, because Stripe returns `next_page_url` even on the last page.
- Records contain personal data, including email addresses, IP addresses, locations and user agents. Route them only to outputs that are approved for that data.

## Building

To build the project:

```bash
go build -o webapi-agent
```

A local build reports its version as `dev`. To set the version, pass it at build time:

```bash
go build -trimpath -ldflags "-s -w -X main.version=v1.2.3" -o webapi-agent
```

Pushing a `v*` tag runs the release workflow (`.github/workflows/release.yml`). It builds `webapi-agent_<tag>_<os>_<arch>` (`.exe` on Windows) for linux/amd64, linux/arm64, windows/amd64 and darwin/arm64 with `CGO_ENABLED=0`, writes a `SHA256SUMS` file, and publishes everything as a GitHub release. A tag with a hyphen, such as `v1.2.3-rc1`, is published as a pre-release.

CI (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`, `govulncheck` and `go test -race` on Linux and Windows for every push to main and every pull request.

## Running

1. Configure the `config.yml` file according to your needs
2. Run the agent:
```bash
./webapi-agent
```

The agent reads `config.yml` from its working directory. Command-line flags:

- `-config <path>` reads another file. A relative path, like a relative `state_dir`, resolves against the working directory, not the config file's folder.
- `-check-config` loads and validates the config, prints warnings, builds the outputs, collectors and heartbeat, and exits without polling. It exits 0 and prints `configuration OK` when the config is valid, 1 on an error, and 2 on a usage error. It validates `spool` blocks and never writes checkpoint or spool files, but like a normal start it creates `state_dir` if missing and checks that it is writable.
- `-version` prints `webapi-agent <version>` and exits 0 without loading the config. The version is `dev` unless it was set at build time (see Building).
- `-purge-spools` loads and validates the config, prints warnings, removes every `<state_dir>/spool/output-*` directory that no enabled spooled output uses, and exits without polling. It prints `removed <dir> (<n> bytes)` for each directory, then a summary, or `no stale spool directories`. It requires `state_dir` and exits 1 on an error, such as a failed removal. Combined with `-check-config` it is a usage error (exit code 2). Spools of outputs that are disabled, or whose spool is off, count as stale, so purging deletes any unsent records in them. Run it only while the agent is stopped, or after the agent has been restarted with the current config.

```bash
./webapi-agent -config /etc/webapi/config.yml -check-config
```

The agent stops gracefully on SIGINT or SIGTERM (see Error Handling). To run it as a systemd or Windows service and keep credentials out of the config, see [DEPLOYMENT.md](DEPLOYMENT.md).

## Features

- Multiple API monitoring, each source polled independently
- Audit log collectors: `github_audit`, `atlassian_org_audit`, `jira_audit`, `stripe_activity_logs`
- Per-output checkpoints saved atomically in `state_dir`
- Named outputs: `webhook` and `syslog` (`tls`, `tcp` or `udp`), plus `apilogremote`
- Per-source routing with `outputs`
- Retries with exponential backoff within each output's `timeout` (`api_timeout` for `apilogremote`)
- Authentication:
  - API Key
  - Basic Authentication
- Secrets from the environment: `api_key_env`, `api_password_env`, header `value_env`
- Customizable headers, query parameters and request body
- TLS 1.2+ with verification by default, custom CA (`ca_file`), mutual TLS and `server_name`, for sources and outputs
- Configurable request intervals and timeouts
- Heartbeat with per-source poll and delivery status
- Graceful shutdown on SIGINT or SIGTERM, bounded at 30 seconds
- `-config`, `-check-config`, `-version` and `-purge-spools` command-line flags
- CI on Linux and Windows, and release binaries with SHA256 checksums for `v*` tags

## Project Structure

```
.
├── api/                # API client implementations
├── collector/          # Checkpointed audit log collectors (GitHub, Atlassian, Jira, Stripe)
├── config/             # Configuration loading and validation
├── heartbeat/          # Periodic heartbeat sender
├── internal/testcerts/ # Throwaway certificates for tests
├── output/             # Named log outputs, routing, and delivery
├── status/             # Per-source poll and delivery status for the heartbeat
├── tlsconfig/          # Shared client TLS configuration builder
├── main.go             # Main application entry point
├── config.yml          # Configuration file
├── DEPLOYMENT.md       # Running as a service and protecting credentials
├── go.mod              # Go module definition
└── go.sum              # Go module checksums
```

## Error Handling

- A failed plain API request is logged and still forwarded, with null data. Request and connection errors from sources and `apilogremote` leave out the request URL, so query parameters such as `params` do not reach the logs.
- A failed collector poll is logged and forwards nothing. The next poll resumes from each output's checkpoint.
- Output sends, including `apilogremote`, are retried up to 3 times within the output's `timeout` (`api_timeout` for `apilogremote`). A redirect that is not followed is not retried. Failures and dropped records are logged with the source and output names.
- A collector output that keeps failing is retried with a doubling delay of up to one hour.
- An invalid config, an unset or empty environment variable, a bad TLS file on a source or output, or a corrupt state file stops startup. Errors name the variable, never its value.
- The heartbeat reports poll and delivery errors, with credentials removed from the error text.
- On SIGINT or SIGTERM the agent stops polling and waits up to 30 seconds for in-flight requests and deliveries, then closes its outputs. It exits 0, or 1 if that wait timed out. A second signal exits at once. A poll or delivery cut off by shutdown is not reported or forwarded, and its checkpoint does not advance.

## Contributing

1. Fork the repository
2. Create your feature branch
3. Commit your changes
4. Push to the branch
5. Create a new Pull Request

## License

[Add your license information here] 