# Open Heartbeat Protocol

[![CI](https://github.com/AndreaAcanfora/openheartbeat/actions/workflows/ci.yml/badge.svg)](https://github.com/AndreaAcanfora/openheartbeat/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/AndreaAcanfora/openheartbeat/ohp.svg)](https://pkg.go.dev/github.com/AndreaAcanfora/openheartbeat/ohp)

An open, vendor-neutral protocol for monitoring cron jobs and scheduled tasks,
with a reference client, server and conformance suite in Go.

Scheduled jobs fail silently: when a backup stops running, nothing alerts.
Heartbeat monitoring fixes that by having the job report to a server, which
alerts when a report does not arrive in time. Many services do this, with
similar but incompatible APIs, so every tool and every job is tied to one
vendor. **OHP standardizes the wire protocol**, so that a job can switch
monitoring server by changing one URL, and tools can integrate once.

- **Specification:** [SPEC.md](SPEC.md) (version 0.1, draft)
- **Compatible by design:** it formalizes the design that existing services
  already converged on, so adopting it is mostly a matter of aliases.
- **`curl` is a complete client.**

## The protocol in 30 seconds

Every job gets a secret check URL. The job sends signals to it:

```sh
curl -fsS -m 10 --retry 5 https://hb.example.com/ping/<key>/backup/start   # started
curl -fsS -m 10 --retry 5 https://hb.example.com/ping/<key>/backup         # succeeded
curl -fsS -m 10 --retry 5 https://hb.example.com/ping/<key>/backup/fail    # failed
curl -fsS -m 10 --retry 5 https://hb.example.com/ping/<key>/backup/$?      # exit code
curl -fsS -m 10 --retry 5 --data-binary @job.log https://hb.example.com/ping/<key>/backup/log
```

A `rid` query parameter ties a start to its completion, so overlapping runs
and durations are tracked correctly. A POST body carries the job's output.
See [SPEC.md](SPEC.md) for the details: methods, limits, retries, responses,
check states and security.

## The `ohp` command

Download a binary for Linux, macOS, Windows or FreeBSD from the
[releases](https://github.com/AndreaAcanfora/openheartbeat/releases), or
install with Go 1.26 or later:

```sh
go install github.com/AndreaAcanfora/openheartbeat/cmd/ohp@latest
```

### Wrap a job

```sh
export OHP_URL=https://hb.example.com/ping/<key>/nightly-backup
ohp run -- pg_dump -f /backups/db.sql mydb
```

`ohp run` sends `start`, runs the command with its output passed through
untouched, then sends the exit code with the last 10 KB of output and the
duration. It always exits with the job's own exit code: if the monitoring
server is unreachable, the job is not affected.

In a crontab:

```cron
OHP_URL=https://hb.example.com/ping/<key>/nightly-backup
0 3 * * * ohp run -- /usr/local/bin/backup.sh
```

### Send one signal

```sh
ohp ping start
ohp ping -data build.log fail
ohp ping 0
```

### Run a reference server

```sh
ohp serve -addr 127.0.0.1:8080 -check backup:24h:1h
```

An in-memory server for development and tests. Unknown checks are created on
first use with `?create=1`; states are listed at `/api/checks`.

### Test a server

```sh
ohp conform https://hb.example.com/ping/<key>/conformance-test
```

Sends real signals to an existing check and reports which requirements of the
spec pass. Use a dedicated check: the suite leaves it in the `up` state, but
its history will show the test signals.

```
PASS  MUST   §4.1 success with GET
PASS  MUST   §4.1 success with HEAD
PASS  MUST   §6.1 success with POST and a body
PASS  MUST   §5   start then exit code 0 with the same rid
...
conforming to OHP 0.1
```

## Go library

```go
import "github.com/AndreaAcanfora/openheartbeat/ohp"

c := ohp.NewClient()
rid := ohp.NewRunID()
c.Send(ctx, checkURL, ohp.Ping{Signal: ohp.Start, RunID: rid})
if err := job(); err != nil {
	c.Send(ctx, checkURL, ohp.Ping{Signal: ohp.Fail, RunID: rid, Body: []byte(err.Error())})
} else {
	c.Send(ctx, checkURL, ohp.Ping{Signal: ohp.Success, RunID: rid})
}
```

The client retries network errors, `5xx` and `429` with exponential backoff,
and never includes the check URL (a secret) in its errors.

## Repository layout

| Path           | Contents                                       |
|----------------|------------------------------------------------|
| `SPEC.md`      | The protocol                                   |
| `ohp/`         | Go client library                              |
| `server/`      | Reference server (in memory)                   |
| `conformance/` | Wire conformance suite                         |
| `cmd/ohp/`     | The `ohp` command                              |

## Roadmap

- Cron expressions and time zones in the reference server.
- Adapters in `ohp run` for services that do not speak OHP yet.
- OHP support in [uptimer](https://github.com/AndreaAcanfora/uptimer).
- An optional management profile (creating checks and schedules).

Feedback on the spec is the most useful contribution right now: open an issue,
especially if you run a heartbeat service or a tool that sends heartbeats.

## License

MIT
