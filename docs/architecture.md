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
- **`pkg/drivers`** — the engine abstraction. A `Driver` supplies everything engine-specific: container env, listen port, readiness commands, restore tooling, `database/sql` driver + DSN, and the checksum dialect. Implementations self-register via `init()`; the orchestrator resolves them by the spec's `source.driver`. `postgres` restores via `pg_restore`/`psql` (format sniffed from the `PGDMP` magic); `mysql` streams `mysqldump` SQL through the container's `mysql` client and checksums with order-independent `BIT_XOR(CRC32(col))` (GROUP_CONCAT+md5 would silently truncate).
- **`pkg/sandbox`** — the provider abstraction; `docker` and `kubernetes` implement it. **docker**: own bridge network, port published only on `127.0.0.1`, random one-off credentials; `Destroy` is idempotent (`sync.Once`) and a TTL watchdog force-removes the container even if the calling process hangs. **kubernetes**: sandbox pod in a dedicated namespace with a deny-all-egress NetworkPolicy; exec via SPDY (like `kubectl exec`), connectivity via pod IP in-cluster or a port-forward from outside; TTL force-deletes the pod.
- **`pkg/operator`** — controller-runtime reconciler for the `RecoveryDrill` CRD (`firedrill operator` subcommand — one binary, one image). The CR `spec:` is converted to the CLI's `spec.Drill` (JSON is valid YAML, so the same strict decoder and validation apply — CLI and operator cannot drift). Cron scheduling via `robfig/cron`; outcomes land in `.status` (`phase`, `verified`, measured RTO/RPO). Works on unstructured objects — no codegen.
- **`pkg/verify`** — checks run against the restored database: `restoreSucceeded`, `freshness`, `rowCount`, `checksum` (order-independent md5 over a column; identifiers regex-validated before interpolation), `smoke` (user SQL + row-count assertion). When the restore fails, data checks report `SKIP` — skipped checks count as unproven, so the drill cannot pass.
- **`pkg/notify`** — notification sinks (Slack incoming webhooks). The webhook URL comes from an env var named in the spec (`webhookEnv`), never the spec itself; `onlyFailures` suppresses noise for verified drills. Like metric sinks, notification failures are warnings only.
- **`pkg/metrics`** — exports the finished evidence as Prometheus metrics to configured sinks: node_exporter textfile (written atomically: temp file + rename) or Pushgateway (grouped by `drill`; the grouping key supplies the label, so pushed metrics omit it). Sink failures surface as warnings — a monitoring outage must not fail a recovery drill.
- **`pkg/report`** — the evidence record (objectives vs measured RTO/RPO, per-check results, sandbox lifecycle, control mappings), written as deterministic JSON and signed with ed25519 (detached `.sig` envelope carrying the public key + fingerprint). `keygen` / `verify-evidence` round-trip in the CLI. `BuildControlReport` aggregates an evidence directory into a per-control matrix (`firedrill controls`), re-validating each file's signature so auditors can distinguish signed from unsigned evidence.

## Key decisions

- **Restore inside the sandbox container** — zero host dependencies, version-matched tooling, and the sandbox stays the only place backup data ever materializes.
- **Restore failure is a drill result, not a crash** — a corrupt backup produces evidence with `verified: false`, which is precisely the product's job. Only infrastructure problems (Docker down, backup unfetchable) are execution errors (exit 2).
- **Native ed25519 over cosign for v0.1** — stdlib-only, offline, easily verifiable. Sigstore/in-toto attestations are the v0.2 upgrade path.
- **The whole drill is capped by the sandbox TTL** — the run context times out when the sandbox would be torn down anyway.

## Extension points (roadmap)

`source.Fetch`, the driver, and the sandbox provider are selected by spec fields (`from.type`, `source.driver`, `sandbox.provider`) with validation rejecting unknown values — adding Velero/Kubernetes (v0.2/0.3) means new packages plus a switch arm, no orchestrator changes.
