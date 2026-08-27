# b2-verify

A lightweight backup-freshness watchdog for B2 buckets and local directories,
with Prometheus metrics and Telegram alerts. Written in Go with **zero
runtime dependencies** (standard library only).

## Why

Silent backup rot is the worst kind of failure: backups look fine until the
day you actually need them. In August 2026, an audit found a Nightscout
database backup that had been broken for **20 days**. The backup job had been
"running", but nothing was watching whether the backup output was actually
*fresh*. Monitoring "the job ran" is not the same as monitoring "the backup is
fresh".

b2-verify watches the *output* of backup pipelines: the newest object in a B2
bucket, or the newest file in a directory, must be younger than a configured
limit. If it is not, you get an alert within minutes of the limit being
exceeded — not weeks later during an audit.

## What it checks

Each configured destination is a **leg**:

- **`b2` legs** query the Backblaze B2 native API
  (`b2_authorize_account` → `b2_list_buckets` → `b2_list_file_names`) and take
  the newest object's `uploadTimestamp` under a prefix. This covers database
  dumps, rclone backups and any other pipeline that writes to B2.
- **`file` legs** use a path's modification time — suitable for rclone-synced
  marker files, git snapshot directories, or database dump directories.

A leg is **fresh** while the newest object is at most `max_age_hours` old.
Anything older is **stale**. A leg that cannot be checked (missing path, bad
credentials, no objects under the prefix) is in an **error** state, which is
reported as worse than stale.

## Install

```
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=v0.1.0"
```

This produces a single static binary. Drop it somewhere on `$PATH`, e.g.
`/usr/local/bin/b2-verify`.

## Configuration

The configuration is JSON (see [`config.example.json`](config.example.json)).
The file may be a bare array of legs, or an object with a `legs` array plus
optional settings:

| Setting | Default | Meaning |
|---|---|---|
| `listen` | `":9118"` | Address for the `/metrics` and `/healthz` endpoints |
| `interval_seconds` | `300` | How often legs are re-checked in `run` mode |

Per leg:

| Field | Applies to | Meaning |
|---|---|---|
| `name` | all | Unique leg name; used in metrics labels and alert messages |
| `kind` | all | `"b2"` or `"file"` |
| `max_age_hours` | all | Maximum allowed age of the newest object |
| `bucket` | `b2` | B2 bucket name |
| `prefix` | `b2` | Object prefix to filter on (e.g. `database-dumps/`) |
| `key_id_env` | `b2` | Name of the environment variable holding the B2 application key ID |
| `key_env` | `b2` | Name of the environment variable holding the B2 application key |
| `path` | `file` | Directory or file to stat |

**Credentials are never stored in the config.** `key_id_env` and `key_env`
name environment variables that hold the actual secrets, so the config file is
safe to commit and review.

## Usage

```
b2-verify check -config /etc/b2-verify/config.json
b2-verify run   -config /etc/b2-verify/config.json
```

- `check` runs once and exits: **0** = all legs fresh, **1** = at least one
  leg stale, **2** = a leg failed to check or the config is invalid. This is
  cron-friendly.
- `run` starts the continuous watchdog: it checks immediately, then every
  `interval_seconds`, and serves `/metrics` (plus `/healthz`) on `listen`.
- `-dry-run` evaluates legs without sending alerts (log-only).

## Metrics

Prometheus text format on `:9118/metrics`, hand-rolled with no client library
— every family is self-documented with `# HELP` / `# TYPE` lines:

| Metric | Meaning |
|---|---|
| `b2_verify_leg_age_seconds{leg}` | Age of the newest object per leg, in seconds |
| `b2_verify_leg_fresh{leg}` | 1 = fresh, 0 = stale or error |
| `b2_verify_last_run_timestamp_seconds` | Unix time the last check completed |
| `b2_verify_scrape{legs,version}` | Info: configured leg count and binary version |

Example scrape config for Prometheus:

```yaml
scrape_configs:
  - job_name: b2-verify
    static_configs:
      - targets: ["localhost:9118"]
```

## Alerting

b2-verify sends a Telegram message on each *transition* into stale or error,
and a recovery message when the leg comes back fresh. One alert per
transition — no spam while a leg stays broken.

Set `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` in the service environment.
If they are unset, the tool runs log-only (metrics and exit codes still work,
which is enough for cron-based use).

Setting up Telegram:

1. Talk to [@BotFather](https://t.me/BotFather) and create a bot; it gives
   you a bot token.
2. Send your bot a message (any message) from the chat that should receive
   alerts.
3. Find the chat ID:

```
curl "https://api.telegram.org/bot<YOUR_BOT_TOKEN>/getUpdates"
```

The chat ID appears in the `chat` object of the update for your message.

## systemd

```ini
[Unit]
Description=b2-verify backup freshness watchdog
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/b2-verify run -config /etc/b2-verify/config.json
EnvironmentFile=/etc/b2-verify/env
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
```

`/etc/b2-verify/env` holds the secrets (chmod 600):

```
TELEGRAM_BOT_TOKEN=***
TELEGRAM_CHAT_ID=YOUR_CHAT_ID
B2_KEY_ID=YOUR_KEY_ID
B2_APPLICATION_KEY=YOUR_A…_KEY
```

## Design notes

- **Stdlib only.** The Prometheus exposition format is a few dozen lines; a
  client library dependency is not worth the supply-chain surface for a
  watchdog.
- **Output-side monitoring.** b2-verify watches what was *written*, not what
  a job claims to have done — it catches jobs that exit 0 while producing
  nothing, dead credentials, and full buckets.
- **Auth per run.** Each `b2` leg authorises fresh with its own environment
  credentials on every check; the tool keeps no persistent sessions.
- **Pagination.** `b2_list_file_names` is walked up to 5 pages of 1,000
  objects so the newest `uploadTimestamp` is found even in busy prefixes.
- **Transition-based alerts.** An alert fires once per state change. If
  delivery fails, the transition is not recorded and the alert is retried on
  the next run — a failed alert is never silently dropped.
- **Env indirection for credentials.** Config names variables, never secrets;
  nothing secret is written to disk by b2-verify.
- **Error legs.** A leg that cannot be checked is an error (exit 2), not
  stale — a broken checker should scream, not shrug.

## Fleet conventions

- Conventional commits, British English, no emoji in commit messages.
- CI is a 4-line caller of the shared
  [`go-ci.yml`](https://github.com/NovaLux12/.github) reusable workflow.

## License

MIT — see [LICENSE](LICENSE).