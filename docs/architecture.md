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
- **`pkg/source`** — read-only backup fetchers (`file`, `s3`). Returns a local path plus the backup's modification time, which drives the freshness check and RPO measurement. S3 credentials come from the standard AWS chain or a named profile (`credentialsRef`); secrets never appear in the spec or evidence. An optional `endpoint` targets S3-compatible stores (MinIO/Ceph) with path-style addressing. **Discovery** (`select: latest` + a `match` glob) resolves a prefix or directory to its newest object, and the resolved URI is recorded in evidence. **Compression** is detected from magic bytes and expanded transparently (gzip/zstd/bzip2) — for S3 during the download, so a compressed artifact is never staged twice; `maxBytes` bounds the transfer and `maxUncompressedBytes` the expansion.
- **`pkg/drivers`** — the engine abstraction. A `Driver` supplies everything engine-specific: container env, listen port, readiness commands, restore tooling, `database/sql` driver + DSN, and the checksum dialect. Implementations self-register via `init()`; the orchestrator resolves them by the spec's `source.driver`. `postgres` restores logical dumps via `pg_restore`/`psql` (format sniffed from the `PGDMP` magic) and physical `pg_basebackup` tars via a **cold-start sandbox**: the container/pod starts with `sleep infinity`, the driver untars into PGDATA, rewrites pg_hba to trust (sandbox-only), starts Postgres in the background and waits for crash recovery — the whole sequence is the measured RTO, and verification connects with the restored cluster's own identity (`RestoreResult.DSN` override). `mysql` streams `mysqldump` SQL through the container's `mysql` client and checksums with order-independent `BIT_XOR(CRC32(col))` (GROUP_CONCAT+md5 would silently truncate). `mongodb` streams a `mongodump --archive` into `mongorestore` (excluding `admin.*`/`config.*`, so the source's user catalog cannot lock the drill out of the sandbox) and brings its own verification engine. The engine-specific query surface is split out of `Driver`: `SQLCapable` (driver name + DSN + checksum dialect) for engines that speak `database/sql`, `Verifier` for engines that don't.
- **`pkg/sandbox`** — the provider abstraction; `docker` and `kubernetes` implement it. **docker**: own bridge network, port published only on `127.0.0.1`, random one-off credentials; `Destroy` is idempotent (`sync.Once`) and a TTL watchdog force-removes the container even if the calling process hangs. **kubernetes**: sandbox pod in a dedicated namespace with a deny-all-egress NetworkPolicy; exec via SPDY (like `kubectl exec`), connectivity via pod IP in-cluster or a port-forward from outside; TTL force-deletes the pod.
- **`pkg/drivers/velero`** — namespace-level drills. Unlike engine drivers, Velero performs the restore itself: the driver validates the `Backup` CR (completion timestamp = RPO), creates an ephemeral namespace (deny-egress NetworkPolicy) and a `Restore` CR with `namespaceMapping`, polls its phase (wall clock = measured RTO; Failed/PartiallyFailed is a drill result, not a crash), and deletes the namespace on teardown. Talks to Velero CRDs via the dynamic client — no velero CLI dependency. The orchestrator branches to this path (`pkg/drill/velero.go`) and verification uses K8s checks (`podsReady`, `resourceCount`) instead of SQL.
- **`pkg/operator`** — controller-runtime reconciler for the `RecoveryDrill` CRD (`firedrill operator` subcommand — one binary, one image). The CR `spec:` is converted to the CLI's `spec.Drill` (JSON is valid YAML, so the same strict decoder and validation apply — CLI and operator cannot drift). Cron scheduling via `robfig/cron`; outcomes land in `.status` (`phase`, `verified`, measured RTO/RPO). Works on unstructured objects — no codegen.
- **`pkg/verify`** — checks run against the restored data: `restoreSucceeded`, `freshness`, `rowCount`, `checksum` (order-independent hash over one column; identifiers regex-validated before interpolation), `smoke` (user query + row-count assertion), `canary` (planted sentinel, never echoed into evidence). Checks are written once against a `verify.Engine` — `NewSQL` wraps a `database/sql` handle, the MongoDB driver evaluates mongosh inside the sandbox — so a new engine costs an Engine implementation, not a parallel check family. When the restore fails, data checks report `SKIP` — skipped checks count as unproven, so the drill cannot pass.
- **`pkg/notify`** — notification sinks: Slack incoming webhooks, and a generic `webhook` that POSTs the canonical evidence JSON with the outcome in an `X-FireDrill-Event` header. URLs come from env vars named in the spec (`webhookEnv`/`urlEnv`), never the spec itself; `onlyFailures` suppresses noise for verified drills. Like metric sinks, notification failures are warnings only.
- **`pkg/metrics`** — exports the finished evidence as Prometheus metrics to configured sinks: node_exporter textfile (written atomically: temp file + rename) or Pushgateway (grouped by `drill`; the grouping key supplies the label, so pushed metrics omit it). Sink failures surface as warnings — a monitoring outage must not fail a recovery drill.
- **`pkg/report`** — the evidence record (objectives vs measured RTO/RPO, per-check results, sandbox lifecycle, control mappings), written as deterministic JSON and signed with ed25519 (detached `.sig` envelope carrying the public key + fingerprint). `keygen` / `verify-evidence` round-trip in the CLI. Signed drills additionally emit an in-toto/DSSE attestation (`.intoto.jsonl`, stdlib-only PAE + ed25519) whose subject digest pins the evidence file — verifiable by `verify-evidence` and by `cosign verify-blob-attestation` using the PKIX `firedrill.cosign.pub` that keygen writes. `BuildControlReport` aggregates an evidence directory into a per-control matrix (`firedrill controls`), re-validating each file's signature so auditors can distinguish signed from unsigned evidence. `Gate` answers the question a report cannot — *is recovery still being proven?* — by evaluating required subjects (drills from a spec file, or controls) against a freshness window and failing on subjects with no evidence at all. History, controls and gate share one evidence scanner.

## Key decisions

- **Restore inside the sandbox container** — zero host dependencies, version-matched tooling, and the sandbox stays the only place backup data ever materializes.
- **Restore failure is a drill result, not a crash** — a corrupt backup produces evidence with `verified: false`, which is precisely the product's job. Only infrastructure problems (Docker down, backup unfetchable) are execution errors (exit 2).
- **Native ed25519 over cosign for v0.1** — stdlib-only, offline, easily verifiable. Sigstore/in-toto attestations are the v0.2 upgrade path.
- **The whole drill is capped by the sandbox TTL** — the run context times out when the sandbox would be torn down anyway.
- **Checks are engine-agnostic, queries are not** — one `verify.Engine` interface keeps check semantics identical across engines while the query strings stay in each engine's own dialect. Inventing a portable query language would be a worse lie than asking for SQL where SQL is what you have.

## Extension points (roadmap)

`source.Fetch`, the driver, and the sandbox provider are selected by spec fields (`from.type`, `source.driver`, `sandbox.provider`) with validation rejecting unknown values — adding Velero/Kubernetes (v0.2/0.3) means new packages plus a switch arm, no orchestrator changes.
