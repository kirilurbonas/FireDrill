# Architecture

## Overview

```
 firedrill.yaml            ┌──────────────────┐
 (RecoveryDrill spec) ────▶│  pkg/drill (orchestrator)
                           └───┬────┬────┬────┬───────┐
                               ▼    ▼    ▼    ▼       ▼
                          spec  source drivers verify report
                                │      │
                                │  ┌───▼──────────────────────┐
                                └─▶│ pkg/sandbox/docker        │
                                   │ isolated · TTL · 127.0.0.1│
                                   │ restore → verify → 🔥      │
                                   └───────────────────────────┘
```

One drill = `pkg/drill.Run`: fetch backup → provision sandbox → restore (timed) → verify → write + sign evidence → destroy sandbox.

## Packages

- **`pkg/spec`** — the `RecoveryDrill` document: strict YAML decoding (unknown fields rejected), structural validation, typed durations. The spec is the only user input; everything else derives from it.
- **`pkg/source`** — read-only backup fetchers (`file`, `s3`). Returns a local path plus the backup's modification time, which drives the freshness check and RPO measurement. S3 credentials come from the standard AWS chain or a named profile (`credentialsRef`); secrets never appear in the spec or evidence.
- **`pkg/sandbox/docker`** — ephemeral Postgres sandboxes. Each gets its own bridge network, a port published only on `127.0.0.1`, and random one-off credentials. `Destroy` is idempotent (`sync.Once`); a background TTL watchdog force-removes the container even if the calling process hangs. `Exec` runs commands inside the container over the Docker API.
- **`pkg/drivers/postgres`** — restores a dump into the sandbox by streaming it to `pg_restore`/`psql` *inside* the container (format auto-detected from the `PGDMP` magic). No Postgres client needed on the host, and tool versions always match the sandbox image. Wall-clock restore duration is the measured RTO.
- **`pkg/verify`** — checks run against the restored database: `restoreSucceeded`, `freshness`, `rowCount`, `checksum` (order-independent md5 over a column; identifiers regex-validated before interpolation), `smoke` (user SQL + row-count assertion). When the restore fails, data checks report `SKIP` — skipped checks count as unproven, so the drill cannot pass.
- **`pkg/metrics`** — exports the finished evidence as Prometheus metrics to configured sinks: node_exporter textfile (written atomically: temp file + rename) or Pushgateway (grouped by `drill`; the grouping key supplies the label, so pushed metrics omit it). Sink failures surface as warnings — a monitoring outage must not fail a recovery drill.
- **`pkg/report`** — the evidence record (objectives vs measured RTO/RPO, per-check results, sandbox lifecycle, control mappings), written as deterministic JSON and signed with ed25519 (detached `.sig` envelope carrying the public key + fingerprint). `keygen` / `verify-evidence` round-trip in the CLI.

## Key decisions

- **Restore inside the sandbox container** — zero host dependencies, version-matched tooling, and the sandbox stays the only place backup data ever materializes.
- **Restore failure is a drill result, not a crash** — a corrupt backup produces evidence with `verified: false`, which is precisely the product's job. Only infrastructure problems (Docker down, backup unfetchable) are execution errors (exit 2).
- **Native ed25519 over cosign for v0.1** — stdlib-only, offline, easily verifiable. Sigstore/in-toto attestations are the v0.2 upgrade path.
- **The whole drill is capped by the sandbox TTL** — the run context times out when the sandbox would be torn down anyway.

## Extension points (roadmap)

`source.Fetch`, the driver, and the sandbox provider are selected by spec fields (`from.type`, `source.driver`, `sandbox.provider`) with validation rejecting unknown values — adding Velero/Kubernetes (v0.2/0.3) means new packages plus a switch arm, no orchestrator changes.
