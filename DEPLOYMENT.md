# Deployment: Running as a Service and Protecting Credentials

This guide covers running `webapi-agent` as a long-lived service on Linux (systemd) and Windows (NSSM). It also covers how to keep API keys and tokens out of config files, logs and source control.

## How the Agent Loads Configuration and Secrets

### Configuration

- By default the agent loads `config.yml` from its **current working directory**. `-config <path>` selects another file; a relative path also resolves against the working directory.
- Without an absolute `-config`, the service definition must set the working directory to the folder that holds `config.yml`.
- A relative `state_dir` resolves against the working directory. Use an absolute `state_dir` for services.

### Secrets

The agent reads credentials from one of two places:

| Source | Use |
|---|---|
| Inline config values (`api_key`, `api_password`, output header `value`) | Do not use for secrets |
| Environment variables named by `api_key_env`, `api_password_env` and output header `value_env` | Recommended |

- The agent does not load `.env` files and does not read secrets from files. The service manager must put the variables in the process environment.
- Setting both the inline key and its `_env` form is an error.
- The agent checks each named variable when it loads the config. If a variable is unset, empty or contains invalid characters, the agent refuses to start. The error names the variable, never its value.

This guide uses these example variables. The names are whatever your `config.yml` refers to:

```
STRIPE_ACTIVITY_LOGS_KEY
GITHUB_AUDIT_TOKEN
ATLASSIAN_ORG_API_KEY
JIRA_API_TOKEN
SIEM_WEBHOOK_AUTH
```

### Logging and Shutdown

- The agent writes its logs to stderr.
- On SIGTERM (systemd) or Ctrl+C (NSSM), the agent stops polling and waits up to 30 seconds for in-flight requests and deliveries, then exits. A second signal ends it at once.
- Give the service manager a stop timeout above 30 seconds. systemd's default of 90 seconds is enough. NSSM needs `AppStopMethodConsole` raised (see Install the Service).
- A forced stop, or a shutdown that times out, is still safe, because checkpoints are written atomically (temp file, fsync, rename). At worst, events delivered since the last checkpoint are sent again (at-least-once delivery).

**Heartbeat.** If the heartbeat is enabled (see [README.md](README.md#heartbeat)):

- Stopping or restarting the service stops heartbeats. The SIEM's missing-heartbeat alert fires if the service is down longer than the alert threshold, so plan for it or suppress it during maintenance.
- On restart the agent sends a heartbeat at startup, after all collectors are built, which makes the restart visible.
- Counts and last-seen values are kept in memory only. They start empty after a restart, so the startup heartbeat's last-poll fields may be `null`.

### Build

Linux:

```bash
go build -o webapi-agent
```

Windows, cross-compiled from Linux or macOS:

```bash
GOOS=windows GOARCH=amd64 go build -o webapi-agent.exe
```

Windows, built in PowerShell:

```powershell
$env:GOOS = "windows"; $env:GOARCH = "amd64"; go build -o webapi-agent.exe
```

## Linux (systemd)

### Layout

| Path | Owner | Mode | Contents |
|---|---|---|---|
| `/opt/webapi/webapi-agent` | `root:root` | `0755` | Binary |
| `/etc/webapi/` | `root:webapi` | `0750` | Working directory |
| `/etc/webapi/config.yml` | `root:webapi` | `0640` | Configuration, no secrets |
| `/etc/webapi/webapi.env` | `root:root` | `0600` | Secrets as `KEY=value` lines |
| `/var/lib/webapi/state` | `webapi:webapi` | `0700` | Collector checkpoints |

The service runs with `WorkingDirectory=/etc/webapi`. In `config.yml`, set an absolute state directory:

```yaml
state_dir: "/var/lib/webapi/state"
```

### Install

```bash
# Dedicated system user and group, with no login shell
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin webapi

# Binary
sudo install -d -o root -g root -m 0755 /opt/webapi
sudo install -o root -g root -m 0755 webapi-agent /opt/webapi/webapi-agent

# Configuration
sudo install -d -o root -g webapi -m 0750 /etc/webapi
sudo install -o root -g webapi -m 0640 config.yml /etc/webapi/config.yml

# Secrets file (empty, readable by root only)
sudo install -o root -g root -m 0600 /dev/null /etc/webapi/webapi.env

# State
sudo install -d -o webapi -g webapi -m 0700 /var/lib/webapi/state
```

Add the secrets with `sudoedit /etc/webapi/webapi.env`, one `KEY=value` per line:

```
STRIPE_ACTIVITY_LOGS_KEY=rk_live_...
GITHUB_AUDIT_TOKEN=ghp_...
ATLASSIAN_ORG_API_KEY=ATCTT...
JIRA_API_TOKEN=...
SIEM_WEBHOOK_AUTH=Bearer ...
```

- systemd reads `EnvironmentFile` as root before it starts the agent as `webapi`. The file can therefore stay `root:root 0600`, and the `webapi` user cannot read it directly.
- systemd interprets quotes, backslashes and `$` in values. Wrap a value in single quotes if it contains any of these.

### Unit File

Create `/etc/systemd/system/webapi.service`:

```ini
[Unit]
Description=WebAPI Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=webapi
Group=webapi
WorkingDirectory=/etc/webapi
EnvironmentFile=/etc/webapi/webapi.env
ExecStart=/opt/webapi/webapi-agent
Restart=on-failure
RestartSec=10

NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/var/lib/webapi/state

[Install]
WantedBy=multi-user.target
```

`ProtectSystem=strict` makes the whole file system read-only for the agent, and `ReadWritePaths` reopens only the state directory. The agent never writes to its working directory.

### Enable and Monitor

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now webapi
sudo systemctl status webapi
journalctl -u webapi -f
```

For remote liveness, enable the heartbeat described in [README.md](README.md#heartbeat).

### Rotate a Key

```bash
sudoedit /etc/webapi/webapi.env
sudo systemd-run --pipe --wait -p User=webapi -p WorkingDirectory=/etc/webapi \
  -p EnvironmentFile=/etc/webapi/webapi.env /opt/webapi/webapi-agent -check-config
sudo systemctl restart webapi
journalctl -u webapi -n 50
```

The `systemd-run` line checks the config with the service's user, directory and variables. Restart only after it prints `configuration OK`.

Revoke the old key at the provider only after the agent has polled successfully with the new one.

## Windows (NSSM)

The agent cannot run as a native Windows service, so it needs a wrapper. Use [NSSM](https://nssm.cc/), which starts the agent, captures its output and restarts it if it exits.

Run every command below in an elevated PowerShell.

> **PowerShell history:** PSReadLine saves typed commands to disk, including any secret typed inline. The examples read secrets with `Read-Host -AsSecureString`, so no secret appears in a command line.

### Layout

| Path | `svc-webapi` access | Contents |
|---|---|---|
| `C:\Program Files\WebAPI\webapi-agent.exe` | Read and execute (default) | Binary |
| `C:\ProgramData\WebAPI\config.yml` | Read | Configuration, no secrets |
| `C:\ProgramData\WebAPI\state\` | Modify | Collector checkpoints |
| `C:\ProgramData\WebAPI\logs\` | Modify | Agent output captured by NSSM |

In `config.yml`, wrap Windows paths in single quotes. YAML treats backslashes in double quotes as escapes:

```yaml
state_dir: 'C:\ProgramData\WebAPI\state'
```

### Service Account and Folders

1. Create a dedicated local account:

   ```powershell
   $pw = Read-Host -AsSecureString "Password for svc-webapi"
   New-LocalUser -Name "svc-webapi" -Password $pw -PasswordNeverExpires -UserMayNotChangePassword
   ```

2. Create the folders and copy the files:

   ```powershell
   New-Item -ItemType Directory -Force "C:\Program Files\WebAPI", "C:\ProgramData\WebAPI\state", "C:\ProgramData\WebAPI\logs"
   Copy-Item .\webapi-agent.exe "C:\Program Files\WebAPI\"
   Copy-Item .\config.yml "C:\ProgramData\WebAPI\"
   ```

3. Remove inherited permissions from the data folder, so ordinary users lose access. Then give the service account read access to the folder and modify rights only on `state` and `logs`:

   ```powershell
   icacls "C:\ProgramData\WebAPI" /inheritance:r `
     /grant:r "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F" "svc-webapi:(OI)(CI)RX"
   icacls "C:\ProgramData\WebAPI\state" /grant "svc-webapi:(OI)(CI)M"
   icacls "C:\ProgramData\WebAPI\logs"  /grant "svc-webapi:(OI)(CI)M"
   ```

### Install the Service

1. Register the binary and set the working directory:

   ```powershell
   nssm install webapi "C:\Program Files\WebAPI\webapi-agent.exe"
   nssm set webapi AppDirectory "C:\ProgramData\WebAPI"
   nssm set webapi AppStopMethodConsole 35000
   ```

   NSSM stops the agent with Ctrl+C and by default waits only 1500 ms before forcing it. 35000 ms lets the agent's 30-second shutdown finish.

2. Capture the agent's output. The agent logs to stderr, so send both streams to one file, and rotate it at 10 MB while the service runs:

   ```powershell
   nssm set webapi AppStdout "C:\ProgramData\WebAPI\logs\webapi.log"
   nssm set webapi AppStderr "C:\ProgramData\WebAPI\logs\webapi.log"
   nssm set webapi AppRotateFiles 1
   nssm set webapi AppRotateOnline 1
   nssm set webapi AppRotateBytes 10485760
   ```

3. Run the service as the dedicated account:

   ```powershell
   $plain = [System.Net.NetworkCredential]::new("", $pw).Password
   nssm set webapi ObjectName ".\svc-webapi" $plain
   Remove-Variable plain, pw
   ```

   If the service later fails to start with error 1069, give `svc-webapi` the **Log on as a service** right in `secpol.msc` (Local Policies → User Rights Assignment).

4. Set the secrets without typing them on a command line:

   ```powershell
   function Read-Secret($name) {
       $s = Read-Host -AsSecureString $name
       [System.Net.NetworkCredential]::new("", $s).Password
   }
   nssm set webapi AppEnvironmentExtra `
     "STRIPE_ACTIVITY_LOGS_KEY=$(Read-Secret 'STRIPE_ACTIVITY_LOGS_KEY')" `
     "GITHUB_AUDIT_TOKEN=$(Read-Secret 'GITHUB_AUDIT_TOKEN')" `
     "ATLASSIAN_ORG_API_KEY=$(Read-Secret 'ATLASSIAN_ORG_API_KEY')" `
     "JIRA_API_TOKEN=$(Read-Secret 'JIRA_API_TOKEN')" `
     "SIEM_WEBHOOK_AUTH=$(Read-Secret 'SIEM_WEBHOOK_AUTH')"
   ```

5. Lock down the registry key that holds the secrets. This step is required, as explained below the code:

   ```powershell
   $key = 'HKLM:\SYSTEM\CurrentControlSet\Services\webapi\Parameters'
   $acl = Get-Acl $key
   $acl.SetAccessRuleProtection($true, $false)   # drop inherited rules
   foreach ($rule in @($acl.Access)) { [void]$acl.RemoveAccessRule($rule) }
   $inherit = [System.Security.AccessControl.InheritanceFlags]::ContainerInherit
   $none    = [System.Security.AccessControl.PropagationFlags]::None
   foreach ($id in 'NT AUTHORITY\SYSTEM', 'BUILTIN\Administrators') {
       $acl.AddAccessRule([System.Security.AccessControl.RegistryAccessRule]::new($id, 'FullControl', $inherit, $none, 'Allow'))
   }
   $acl.AddAccessRule([System.Security.AccessControl.RegistryAccessRule]::new("$env:COMPUTERNAME\svc-webapi", 'ReadKey', $inherit, $none, 'Allow'))
   Set-Acl $key $acl
   (Get-Acl $key).Access | Format-Table IdentityReference, RegistryRights
   ```

   NSSM stores `AppEnvironmentExtra` in plain text under `HKLM\SYSTEM\CurrentControlSet\Services\webapi\Parameters`. By default every local user can read the keys under `Services`. After this step, only SYSTEM, Administrators and the service account can read it. The service account needs read access because NSSM runs as that account and reads its settings from the key. `nssm set` keeps these permissions, but removing and reinstalling the service resets them.

6. Start the service:

   ```powershell
   nssm start webapi
   ```

### Monitor

```powershell
nssm status webapi
Get-Content "C:\ProgramData\WebAPI\logs\webapi.log" -Wait -Tail 50
```

For remote liveness, enable the heartbeat described in [README.md](README.md#heartbeat).

### Rotate a Key

`nssm set webapi AppEnvironmentExtra` replaces the whole list. Supply every variable again, not only the one that changed. Then restart the service:

```powershell
nssm set webapi AppEnvironmentExtra `
  "STRIPE_ACTIVITY_LOGS_KEY=$(Read-Secret 'STRIPE_ACTIVITY_LOGS_KEY')" `
  "GITHUB_AUDIT_TOKEN=$(Read-Secret 'GITHUB_AUDIT_TOKEN')" `
  "ATLASSIAN_ORG_API_KEY=$(Read-Secret 'ATLASSIAN_ORG_API_KEY')" `
  "JIRA_API_TOKEN=$(Read-Secret 'JIRA_API_TOKEN')" `
  "SIEM_WEBHOOK_AUTH=$(Read-Secret 'SIEM_WEBHOOK_AUTH')"
nssm restart webapi
```

`Read-Secret` is the function defined in step 4. Define it again first in a new PowerShell session.

## Containers

This repository has no Dockerfile. If you build an image, pass secrets with `--env-file`, mount the configuration and state, and set the working directory to the folder that holds `config.yml`:

```bash
docker run -d --name webapi \
  --env-file /etc/webapi/webapi.env \
  -v /etc/webapi/config.yml:/etc/webapi/config.yml:ro \
  -v /var/lib/webapi/state:/var/lib/webapi/state \
  -w /etc/webapi \
  <image>
```

Docker's `--env-file` takes each value literally, including any quotes. This differs from systemd.

Orchestrators can inject the same variables from their secret stores.

Kubernetes:

```yaml
env:
  - name: GITHUB_AUDIT_TOKEN
    valueFrom:
      secretKeyRef:
        name: webapi-secrets
        key: github-audit-token
```

AWS ECS task definition, from Secrets Manager or SSM Parameter Store:

```json
"secrets": [
  { "name": "GITHUB_AUDIT_TOKEN",
    "valueFrom": "arn:aws:secretsmanager:<region>:<account>:secret:webapi/github-audit-token" },
  { "name": "JIRA_API_TOKEN",
    "valueFrom": "arn:aws:ssm:<region>:<account>:parameter/webapi/jira-api-token" }
]
```

## Protecting Keys

### Keep Secrets out of `config.yml`

Use `api_key_env`, `api_password_env` and `value_env` for every credential. Then `config.yml` holds no secrets and can be reviewed, versioned and shared.

```yaml
# Source credential
auth_type: "apikey"
api_key_env: "GITHUB_AUDIT_TOKEN"

# Output header credential
headers:
  - key: "Authorization"
    value_env: "SIEM_WEBHOOK_AUTH"
```

### Grant Least Privilege per Source

| Source | Credential |
|---|---|
| GitHub audit log | Token with only the `read:audit_log` scope, from an organization or enterprise owner |
| Stripe Activity Logs | Restricted key with only **Activity logs: Read** |
| Atlassian organization audit | Organization admin API key scoped to `read:events:admin` |
| Jira audit records | API token for an account with the **Administer Jira** global permission |
| Webhook output | A credential that can only write to the ingest endpoint |

### Manage the Key Lifecycle

- Use separate keys for each environment (dev, staging, production), so a leaked test key exposes nothing in production.
- Set an expiry wherever the provider supports one.
- Rotate keys on a schedule with the steps above, and revoke each old key once the new one works.

### Keep Secret Files out of the Repository

- Store `webapi.env` and similar files outside the repository folder.
- The project's `.gitignore` ignores `.env`, `.env.*` and `*.env`. A secrets file with any other name can still be committed by accident.

### Run Without Full Privileges

- Do not run the agent as `root` on Linux or as `LocalSystem` on Windows.
- Use the dedicated `webapi` or `svc-webapi` account, which can write only to the state directory and, on Windows, the logs directory.

### Know the Limits of Environment Variables

- On Linux, root and the process's own user can read its environment through `/proc/<pid>/environ`. On Windows, administrators can read a process's environment.
- Child processes inherit the environment. The agent starts none.
- A dedicated service account keeps other accounts on the host from reading the secrets.

## Verification Checklist

- [ ] `config.yml` contains no inline keys, passwords or header secrets.
- [ ] Every credential uses `api_key_env`, `api_password_env` or `value_env`.
- [ ] The service's working directory is the folder that holds `config.yml`.
- [ ] `state_dir` is an absolute path that the service account can write to.
- [ ] `webapi-agent -check-config`, run with the service's environment, prints `configuration OK` before each restart.
- [ ] The stop timeout is above 30 seconds (systemd `TimeoutStopSec`, default 90, or NSSM `AppStopMethodConsole`).
- [ ] Linux: `webapi.env` is `root:root 0600`, `config.yml` is `root:webapi 0640`, and the state directory is `webapi:webapi 0700`.
- [ ] Windows: `C:\ProgramData\WebAPI` has no inherited permissions, and `svc-webapi` can modify only `state` and `logs`.
- [ ] Windows: the `Services\webapi\Parameters` registry key grants access only to SYSTEM, Administrators and `svc-webapi`.
- [ ] The service runs as `webapi` or `svc-webapi`, not as root or LocalSystem.
- [ ] No secret files are inside the repository folder.
- [ ] No secrets appear in PSReadLine history (`(Get-PSReadLineOption).HistorySavePath`).
- [ ] Each key has the minimum scope listed above and belongs to one environment.
- [ ] The logs (`journalctl -u webapi` or `webapi.log`) show successful polls and no environment-variable errors.
- [ ] After a restart, collectors resume from their checkpoints and send no duplicates.
