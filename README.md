# FireDrill 🔥

[![CI](https://github.com/kirilurbonas/FireDrill/actions/workflows/ci.yml/badge.svg)](https://github.com/kirilurbonas/FireDrill/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/kirilurbonas/FireDrill)](https://goreportcard.com/report/github.com/kirilurbonas/FireDrill)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

**Fire drills for your backups. Prove recovery before you need it.**

FireDrill restores your real backups into a disposable, isolated sandbox, verifies the data actually came back intact, measures the true recovery time, and emits signed, audit-grade evidence — then destroys the sandbox. It answers the question every backup tool quietly dodges: *if today were the disaster, would you actually get your data back — and how long would it take?*

FireDrill **does not back anything up**. It is the verification layer on top of whatever backup you already run (`pg_dump`, `mysqldump`, Velero, pgBackRest, RDS snapshots, …): backup-agnostic recovery verification with audit-grade proof. Postgres, MySQL and Velero (Kubernetes namespaces) are supported today; the driver interface is built for more.

## Demo

![FireDrill demo](demo/demo.gif)

## Install

Download a binary from [releases](https://github.com/kirilurbonas/FireDrill/releases), or build from source:

```sh
git clone https://github.com/kirilurbonas/FireDrill && cd FireDrill && make build
```

## Quickstart

Requirements: Docker running locally. No Postgres client needed on the host — restore tooling runs inside the sandbox container.

```sh
make build                        # builds bin/firedrill
./bin/firedrill keygen            # one-time: create the evidence-signing keypair
./examples/make-demo-backup.sh    # generate a realistic demo pg_dump
./bin/firedrill validate -f examples/firedrill.yaml
./bin/firedrill run payments-db -f examples/firedrill.yaml
./bin/firedrill verify-evidence evidence/payments-db-*.json
```

Exit codes: `0` recovery verified · `1` drill ran but recovery not verified · `2` drill could not execute.

**Fleets**: a drill file can contain multiple drills as YAML documents (`---`). `firedrill run <name>` picks one; `firedrill run --all` runs everything and prints a scorecard:

```
DRILL          RESULT     RESTORE  RTO RPO  EVIDENCE
payments-db    verified   3m50s    ✓   ✓    evidence/payments-db-….json
orders-db      FAILED     1m02s    ✓   ✓    evidence/orders-db-….json

2 drill(s): 1 verified, 1 failed, 0 errored
```

**S3-compatible stores** (MinIO, Ceph, Wasabi, …): add `endpoint` to the source and FireDrill switches to path-style addressing:

```yaml
from: { type: s3, uri: "s3://backups/pg/latest.dump", endpoint: "http://minio.internal:9000" }
```

## How it works

1. **Declare** recovery targets in `firedrill.yaml` — source backup, restore method, RTO/RPO objectives, checks.
2. **Provision** a throwaway sandbox — a Docker container (own bridge network, loopback-only port) or a Kubernetes pod (dedicated namespace, deny-all-egress NetworkPolicy) — with random one-off credentials and a hard TTL.
3. **Restore** the latest backup into it (local file or `s3://…`), timed — that's your *measured* RTO.
4. **Verify** — restore succeeded, freshness vs RPO, row counts, order-independent checksums, custom smoke SQL.
5. **Report** — a JSON evidence record signed with ed25519, mapped to compliance controls (`ISO27001-A.8.13`, `SOC2-A1.2`, …).
6. **Destroy** the sandbox — guaranteed by both a deferred teardown and an in-process TTL watchdog.

See [examples/firedrill.yaml](examples/firedrill.yaml) (Postgres) and [examples/firedrill-mysql.yaml](examples/firedrill-mysql.yaml) (MySQL) for the spec format, and [docs/architecture.md](docs/architecture.md) for design.

## Evidence

Every drill writes `evidence/<drill>-<timestamp>.json` plus a detached `.sig` envelope. Any tampering with the evidence fails verification:

```
$ firedrill verify-evidence evidence/payments-db-2026-07-08T19-25-12Z.json
✓ signature valid — evidence is intact
```

Auditors can verify with `--public-key ~/.config/firedrill/firedrill.pub` to additionally pin the signing key.

With `report.html: true`, a self-contained HTML report (`<evidence>.html`) is written next to the JSON — shareable with anyone who won't read JSON.

**in-toto/DSSE attestations** — alongside the `.sig`, every signed drill emits `<evidence>.intoto.jsonl`: a DSSE envelope over an in-toto Statement whose predicate is the evidence. `verify-evidence` checks it automatically, and it's verifiable with standard supply-chain tooling:

```sh
cosign verify-blob-attestation \
  --key ~/.config/firedrill/firedrill.cosign.pub \
  --type https://firedrill.dev/drill-evidence/v1 \
  --signature evidence/payments-db-….json.intoto.jsonl \
  --insecure-ignore-tlog \
  evidence/payments-db-….json
```

(`keygen` writes `firedrill.cosign.pub`, a PKIX copy of the key for cosign/openssl. `--insecure-ignore-tlog` because drill evidence is signed offline, not logged to Rekor.)

## Ransomware canary

Plant a sentinel value in production before backups run, then have every drill prove it restores **byte-exact**:

```sql
create table firedrill_canary (token text);
insert into firedrill_canary values ('fd-canary-2f8a91c4');
```

```yaml
verify:
  - canary: { sql: "select token from firedrill_canary", expect: "fd-canary-2f8a91c4" }
```

Row counts and freshness can't catch a backup that was encrypted at the source or silently corrupted — a known token that must match exactly can. The sentinel value itself is never written into evidence.

## Compliance-control export

Drills declare which controls they evidence (`report.controls: [ISO27001-A.8.13, SOC2-A1.2]`). `firedrill controls` aggregates an evidence directory into an auditor-ready matrix — per control: every run, its result, measured restore time, RTO/RPO status, and whether the evidence signature validates:

```sh
firedrill controls                          # markdown to stdout
firedrill controls --format json -o controls.json
```

Hand the markdown (or JSON) straight to your GRC team at audit time instead of screenshots and a Confluence page.

`firedrill history` shows past runs with an RTO trend, so restore-time regressions are visible before they become incidents:

```
WHEN (UTC)         DRILL          RESULT  RESTORE  RTO RPO  TREND
2026-07-11 03:00   payments-db    ok      3m50s    ✓   ✓    ▇▇▇▇▇▇▇▇
2026-07-12 03:00   payments-db    ok      4m10s    ✓   ✓    ▇▇▇▇▇▇▇▇▇
2026-07-13 03:00   payments-db    FAILED  9m02s    ✗   ✓    ▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇
```

## Kubernetes

Two levels of Kubernetes support:

**Sandbox provider** — set `sandbox.provider: kubernetes` and the drill provisions the sandbox as a pod (dedicated namespace, deny-all-egress NetworkPolicy, random credentials, TTL force-delete) instead of a Docker container. The CLI reaches it through a port-forward; in-cluster it uses the pod IP.

**Operator** — declare drills as `RecoveryDrill` custom resources and let the operator run them on a cron schedule:

```sh
kubectl apply -f deploy/crd.yaml
kubectl apply -f deploy/operator.yaml      # or run `firedrill operator` with a kubeconfig
kubectl apply -f deploy/example-recoverydrill.yaml
kubectl get drills -n firedrill-system     # NAME  PHASE  VERIFIED  LAST RUN  SCHEDULE
```

The CR's `spec:` block is exactly the `firedrill.yaml` spec — the operator validates and runs it with the same code as the CLI, records the outcome (`phase`, `verified`, measured RTO/RPO) in `.status`, and emits Kubernetes Events (`DrillVerified` / `DrillFailed` / `DrillError`) so `kubectl describe drill` tells the story.

The operator image is published to `ghcr.io/kirilurbonas/firedrill` (multi-arch) by the release workflow — `deploy/operator.yaml` uses it out of the box; pin a version tag in production.

**Velero drills** — if your backups are Velero backups, FireDrill can drill whole namespaces: it restores the backup into an **ephemeral namespace** via a Velero `Restore` with `namespaceMapping` (production is never touched), verifies the workloads actually came back, and deletes the namespace:

```yaml
source:
  driver: velero
  from: { type: velero, backup: shop-nightly, namespace: shop }
sandbox: { provider: kubernetes, ttl: 20m }
verify:
  - restoreSucceeded: {}
  - podsReady: { timeout: 5m }                     # every restored pod reaches Ready
  - resourceCount: { kind: deployments, min: 1 }   # objects actually came back
```

Requires Velero installed in the cluster. Try it locally: `examples/velero/setup-velero-kind.sh` stands up Velero + MinIO + a demo backup in a kind cluster, then `firedrill run shop-ns -f examples/firedrill-velero.yaml`.

## Metrics

Drill results export as Prometheus metrics via `report.sinks`:

```yaml
report:
  sinks:
    - { type: prometheus, textfileDir: /var/lib/node_exporter/textfile }  # node_exporter textfile collector
    - { type: pushgateway, url: http://pushgateway:9091 }                 # for scrape-based setups
```

Exported (per drill): `firedrill_drill_verified`, `firedrill_restore_duration_seconds` (measured RTO), `firedrill_backup_age_seconds` (RPO), `firedrill_rto_met`, `firedrill_rpo_met`, `firedrill_check_passed{check=…}`, `firedrill_drill_timestamp_seconds`. Alert on `firedrill_drill_verified == 0` or a rising `restore_duration` trend. Sink failures are warnings — they never fail a drill.

A ready-made Grafana dashboard (verification history, RTO/RPO trends, time-since-last-drill) ships at [deploy/grafana-dashboard.json](deploy/grafana-dashboard.json) — import it and point it at your Prometheus datasource.

## Slack notifications

Add a `slack` sink to get drill outcomes in a channel. The webhook URL is read from an environment variable — it never appears in the spec:

```yaml
report:
  sinks:
    - { type: slack, webhookEnv: SLACK_WEBHOOK_URL, onlyFailures: true }
```

`onlyFailures: true` keeps the channel quiet until a drill actually fails — usually what you want for the 3 a.m. pager channel.

## Guardrails

| Risk | Mitigation |
|---|---|
| Accidentally touching production | Docker: own network, published to `127.0.0.1` only. Kubernetes: deny-all-egress NetworkPolicy. Sources are read-only (FireDrill only downloads) |
| Sandbox left running | Deferred destroy on every code path **and** a TTL watchdog that force-removes the container/pod past the deadline |
| "Restore ran" ≠ "data is back" | Data-level checks: row counts, checksums, user smoke SQL — not just exit codes |
| Secrets leaking into evidence | Credentials referenced by name (`credentialsRef` → AWS profile), never inlined or persisted |
| Corrupt/garbage backups passing | A failed restore fails the drill; dependent checks report `SKIP`, never false `PASS` |
| Secrets in process lists | Database passwords reach in-sandbox tooling via environment, never argv |

See [SECURITY.md](SECURITY.md) for the full security model and how to report vulnerabilities.

## How is this different from …?

| Tool | What it does | What it doesn't |
|---|---|---|
| [pgbackrest_auto](https://github.com/vitabaks/pgbackrest_auto) | Automated restore + validate for pgBackRest | pgBackRest-only, bash, no signed evidence |
| [AWS Backup restore testing](https://docs.aws.amazon.com/aws-backup/latest/devguide/restore-testing.html) | Managed periodic restore tests | AWS resources only, evidence stays in AWS |
| Backup tools with verify (pgBackRest, pg_probackup, …) | Checksum their **own** backups | Verify writes, not end-to-end recovery |
| **FireDrill** | Backup-**agnostic** recovery drills with measured RTO/RPO and **signed, control-mapped evidence** | Doesn't back anything up — by design |

A backup that has never been restored is just hope stored on disk. FireDrill turns that hope into a signed record an auditor can check.

## Development

```sh
make test    # unit tests
make e2e     # full drill loops against real Docker + a Kubernetes cluster (kind); k8s tests skip if no cluster is reachable
make lint    # golangci-lint (incl. gosec)
```

CI runs all of it — lint (with e2e files), `govulncheck`, unit tests, and the Docker/Kubernetes/Velero/operator e2e suites against a kind cluster. Dependabot keeps dependencies current (PRs auto-merge when CI passes). See [CONTRIBUTING.md](CONTRIBUTING.md).

## Roadmap

Next up: cloud sandboxes (Terraform/RDS). See [firedrill-plan.md](firedrill-plan.md).

## License

Apache-2.0
