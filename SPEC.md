# Open Heartbeat Protocol (OHP) — version 0.2

Status: draft. Feedback welcome through GitHub issues.

The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are to be
interpreted as described in [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119).

## 1. Purpose

Scheduled jobs (cron entries, backups, ETL pipelines, queue workers) fail
silently: when a job stops running, nothing happens, so nothing alerts.
Heartbeat monitoring (also called "dead man's switch" or "cron monitoring")
inverts the check: the job tells a monitoring server that it ran, and the
server alerts when an expected signal does not arrive in time.

Many services offer this, and they converged on a similar design, but every
one of them exposes its own URLs, suffixes and limits. Job wrappers, backup
tools and schedulers therefore need a separate integration per vendor, and
switching vendor means editing every job.

OHP standardizes the wire protocol between a job and a heartbeat server, so
that:

- a job can be monitored by any conforming server by changing one URL;
- tools (backup software, schedulers, CI systems, wrappers) can integrate once;
- a plain `curl` remains a complete client.

OHP deliberately formalizes the de facto design already used by existing
services (most closely the Healthchecks.io ping API), so that existing servers
can become conforming with little or no change.

Out of scope for 0.1: how checks are created and configured (schedules, grace
periods, alert channels), alerting, and user interfaces. These are server
concerns. Section 9 defines an optional discovery document.

## 2. Terminology

- **Check**: a monitored job, as known by the server.
- **Check URL**: the absolute URL that identifies a check. Opaque to clients.
- **Signal**: a single HTTP request from a client to a check URL.
- **Run**: one execution of the job, delimited by a `start` signal and a
  completion signal.
- **Run ID**: a client-chosen identifier that ties the signals of one run
  together.
- **Client**: anything that sends signals (a job, a wrapper, `curl`).
- **Server**: the service that receives signals and evaluates check state.

## 3. Check URL

A check URL is an absolute `https` URL. Servers MAY also accept `http` on
loopback addresses and private networks.

A check URL MUST contain enough secret entropy that it cannot be guessed:
at least 128 bits (for example a random UUID), or a slug placed under a
secret key of at least 128 bits (`https://example.com/ping/<key>/<slug>`).

Clients MUST treat the check URL as opaque. The only transformations a client
performs are appending a signal suffix to the path (section 4) and adding
query parameters (section 5). A check URL MUST NOT end with a `/`; clients
SHOULD remove a trailing `/` before appending a suffix.

## 4. Signals

A signal is selected by the path suffix appended to the check URL:

| Request path       | Signal    | Meaning                                       |
|--------------------|-----------|-----------------------------------------------|
| `{check}`          | success   | The job completed successfully.               |
| `{check}/start`    | start     | The job started. Opens a run.                 |
| `{check}/fail`     | fail      | The job failed.                               |
| `{check}/log`      | log       | Informational message. Does not change state. |
| `{check}/{code}`   | exit code | The job exited with status `code`.            |

`{code}` is a decimal integer from `0` to `255`. Clients MUST NOT send
leading zeros. Servers MUST reject values above `255`, SHOULD reject leading
zeros and MAY instead read them as the decimal value (`01` as `1`).
Exit code `0` is equivalent to success; any other exit code is equivalent to
fail. Servers SHOULD store the exit code.

Any other suffix MUST be rejected with `400 Bad Request` or `404 Not Found`.

### 4.1 Methods

Servers MUST accept `GET`, `HEAD` and `POST` for every signal, with identical
effect. `GET` keeps the simplest clients (`curl`, `wget`, embedded HTTP
libraries) conforming. Clients SHOULD use `POST`.

Signals are not idempotent: a server records every request it accepts.
Clients MUST NOT send the same signal twice on purpose, but retries after a
network failure (section 7) are allowed, and servers MUST tolerate them.

## 5. Query parameters

- `rid` — Run ID: a UUID ([RFC 9562](https://www.rfc-editor.org/rfc/rfc9562))
  in its textual form, 32 hexadecimal digits in groups of 8-4-4-4-12
  separated by hyphens, in either case. Clients SHOULD use random (version 4)
  or time-ordered (version 7) UUIDs. When present on a `start` signal and on a
  completion signal (success, fail or exit code), the server MUST treat them
  as the same run. This allows overlapping runs of the same check. A server
  MUST reject an invalid `rid` with `400 Bad Request`.
- `create` — When `create=1` is present and the check URL is a slug under a
  valid key, a server MAY create the check on first use. Servers that do not
  support creation MUST ignore the parameter.

Servers MUST ignore unknown query parameters.

## 6. Request body and metadata

### 6.1 Body

A `POST` signal MAY carry a body: the job's output or an error message, which
the server stores with the signal. The body is treated as text; clients SHOULD
send UTF-8 with `Content-Type: text/plain; charset=utf-8`.

Servers MUST accept bodies of at least 10 000 bytes and SHOULD accept
100 000 bytes. A server MUST NOT reject a signal because its body is too
large: it MUST record the signal and truncate the body, keeping the first
bytes, and SHOULD include the response header `OHP-Log-Truncated: 1`.

Because servers keep the beginning of the body, clients that capture job
output SHOULD send its **last** bytes (the tail), where errors usually are.

### 6.2 Metadata headers

Clients MAY send these request headers. Servers MAY store them and MUST
ignore those they do not support:

| Header          | Value                                   | Example                     |
|-----------------|-----------------------------------------|-----------------------------|
| `OHP-Version`   | Protocol version implemented by client  | `0.1`                       |
| `OHP-Duration`  | Run duration in seconds, decimal        | `12.304`                    |
| `OHP-Host`      | Host name that ran the job              | `backup-01`                 |
| `OHP-Timestamp` | Client time of the event, RFC 3339      | `2026-11-07T22:00:03Z`      |

The server's own receive time is authoritative. `OHP-Timestamp` and
`OHP-Duration` are informational: a server that measured a duration from a
matching `start` signal SHOULD prefer its own measurement.

## 7. Responses

| Status                    | Meaning                                              |
|---------------------------|------------------------------------------------------|
| `200 OK`                  | Signal recorded.                                     |
| `201 Created`             | Check created by `create=1`, signal recorded.        |
| `400 Bad Request`         | Invalid suffix, exit code or `rid`.                  |
| `404 Not Found`           | No such check.                                       |
| `429 Too Many Requests`   | Rate limited. SHOULD include `Retry-After`.          |
| `5xx`                     | Server error. The signal may not have been recorded. |

A successful response body is the text `OK` (or `Created`), followed by an
optional newline. Servers SHOULD include the response header
`OHP-Version: 0.1`.

### 7.1 Client behaviour

- A client SHOULD time out each attempt after 10 seconds.
- On a network error, a `5xx` response or a `429` response, a client SHOULD
  retry up to 5 attempts in total with exponential backoff starting at
  1 second. For `429`, a `Retry-After` value takes precedence when it is not
  larger than 60 seconds.
- A client MUST NOT retry after any other `4xx` response.
- A wrapper that runs a job MUST NOT change the job's outcome because a signal
  could not be delivered: the job runs anyway, and the wrapper exits with the
  job's own exit status.
- A client SHOULD read the check URL from the `OHP_URL` environment variable
  when none is given explicitly.

## 8. Check state (informative)

How a server turns signals into alerts is up to the server. For predictable
behaviour across implementations, servers SHOULD follow this model:

- A check has an expected **schedule** (a period, or a cron expression with a
  time zone) and a **grace** time.
- `new`: no signal received yet.
- `up`: the last completion was a success, and the next one is not yet due.
- `late`: a completion is overdue, but by less than the grace time.
- `down`: the last completion was a failure, a completion is overdue by more
  than the grace time, or a run started and did not complete within the grace
  time.
- `log` signals never change state. A `start` signal marks the check as
  running without changing its state.

Servers SHOULD alert on transitions to `down` and back to `up`.

## 9. Discovery (optional)

A server MAY publish a discovery document at
`/.well-known/ohp` on the origin of its check URLs:

```json
{
  "versions": ["0.2"],
  "max_body_bytes": 100000,
  "create": true,
  "documentation": "https://example.com/docs/heartbeats"
}
```

`versions` is REQUIRED; the other members are OPTIONAL. Clients MUST NOT
require this document to send signals.

## 10. Security considerations

- A check URL is a bearer credential: anyone who knows it can send signals.
  It MUST NOT be committed to public repositories or printed in public logs.
  Clients SHOULD redact it in their own error messages.
- Servers MUST use TLS for non-local traffic and SHOULD rate limit per check
  and per source address.
- Job output can contain secrets. Clients that capture output SHOULD let the
  user disable or limit it; servers SHOULD let operators set retention.
- A server MUST NOT follow URLs or execute content found in a body.

## 11. Conformance

- A **conforming server** implements sections 3 to 7.
- A **conforming client** implements sections 4 to 7.1.

The reference implementation ships an automated conformance runner
(`ohp conform <check-url>`) that tests a server's behaviour on the wire.

## 12. Versioning

Until 1.0 the protocol is a draft and a new 0.x version may change the wire
format; every change is listed in [CHANGELOG.md](CHANGELOG.md). From 1.0,
minor versions only add optional features, and breaking changes require a new
major version and a new discovery entry in `versions`.

Changes in 0.2: `rid` must be a UUID, and servers may read exit codes with
leading zeros. Both follow the behaviour of the most widely deployed
implementation (Appendix A).

## Appendix A. Relation to existing services (informative)

OHP follows the design that existing cron monitoring services converged on:
a unique ping URL per job, with `/start` and `/fail` endpoints. Services that
use different paths can become conforming by adding the OHP paths as aliases.

**Healthchecks.io**, tested on 2026-11-09 with `ohp conform -pace 15s`
against a hosted check, meets every MUST requirement of this version. It
differs on SHOULD requirements: an unknown check UUID is answered with
`200 OK (not found)` (and a malformed one with `400`) rather than `404`, exit codes with leading zeros are
read as decimal, and responses carry no `OHP-Version` or
`OHP-Log-Truncated` header. Its ping API also rate-limits each check;
clients that retry on `429` (section 7.1) cope with it.
