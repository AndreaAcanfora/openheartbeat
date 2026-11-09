# Changelog

## v0.2.0 — 2026-11-09

- Spec 0.2 (draft): `rid` must be a UUID, as Healthchecks.io requires; servers may read exit codes with leading zeros. Appendix A reports a conformance run against Healthchecks.io: every MUST passes.
- `ohp conform -pace` spaces requests out for rate-limited services.
- The conformance suite checks that non-UUID run IDs are rejected; leading zeros are now a SHOULD.

## v0.1.0 — 2026-11-08

- OHP specification, version 0.1 (draft).
- Go client library with retries and secret-safe errors.
- `ohp` command: `run`, `ping`, `serve`, `conform`.
- In-memory reference server and wire conformance suite.
- Release binaries for Linux, macOS, Windows and FreeBSD.
