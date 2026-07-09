# FireDrill 🔥

[![CI](https://github.com/kirilurbonas/FireDrill/actions/workflows/ci.yml/badge.svg)](https://github.com/kirilurbonas/FireDrill/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/kirilurbonas/FireDrill)](https://goreportcard.com/report/github.com/kirilurbonas/FireDrill)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

**Fire drills for your backups. Prove recovery before you need it.**

FireDrill restores your real backups into a disposable, isolated sandbox, verifies the data actually came back intact, measures the true recovery time, and emits signed, audit-grade evidence — then destroys the sandbox. It answers the question every backup tool quietly dodges: *if today were the disaster, would you actually get your data back — and how long would it take?*

FireDrill **does not back anything up**. It is the verification layer on top of whatever backup you already run (`pg_dump`, pgBackRest, RDS snapshots, …): backup-agnostic recovery verification with audit-grade proof.

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

## How it works

1. **Declare** recovery targets in `firedrill.yaml` — source backup, restore method, RTO/RPO objectives, checks.
2. **Provision** a throwaway Docker sandbox: own bridge network, loopback-only port, random one-off credentials, hard TTL.
3. **Restore** the latest backup into it (local file or `s3://…`), timed — that's your *measured* RTO.
4. **Verify** — restore succeeded, freshness vs RPO, row counts, order-independent checksums, custom smoke SQL.
5. **Report** — a JSON evidence record signed with ed25519, mapped to compliance controls (`ISO27001-A.8.13`, `SOC2-A1.2`, …).
6. **Destroy** the sandbox — guaranteed by both a deferred teardown and an in-process TTL watchdog.

See [examples/firedrill.yaml](examples/firedrill.yaml) for the full spec format and [docs/architecture.md](docs/architecture.md) for design.

## Evidence

Every drill writes `evidence/<drill>-<timestamp>.json` plus a detached `.sig` envelope. Any tampering with the evidence fails verification:

```
$ firedrill verify-evidence evidence/payments-db-2026-07-08T19-25-12Z.json
✓ signature valid — evidence is intact
```

Auditors can verify with `--public-key ~/.config/firedrill/firedrill.pub` to additionally pin the signing key.

With `report.html: true`, a self-contained HTML report (`<evidence>.html`) is written next to the JSON — shareable with anyone who won't read JSON.

## Metrics

Drill results export as Prometheus metrics via `report.sinks`:

```yaml
report:
  sinks:
    - { type: prometheus, textfileDir: /var/lib/node_exporter/textfile }  # node_exporter textfile collector
    - { type: pushgateway, url: http://pushgateway:9091 }                 # for scrape-based setups
```

Exported (per drill): `firedrill_drill_verified`, `firedrill_restore_duration_seconds` (measured RTO), `firedrill_backup_age_seconds` (RPO), `firedrill_rto_met`, `firedrill_rpo_met`, `firedrill_check_passed{check=…}`, `firedrill_drill_timestamp_seconds`. Alert on `firedrill_drill_verified == 0` or a rising `restore_duration` trend. Sink failures are warnings — they never fail a drill.

## Guardrails

| Risk | Mitigation |
|---|---|
| Accidentally touching production | Sandbox on its own network, published to `127.0.0.1` only; sources are read-only (FireDrill only downloads) |
| Sandbox left running | Deferred destroy on every code path **and** a TTL watchdog that force-removes the container past the deadline |
| "Restore ran" ≠ "data is back" | Data-level checks: row counts, checksums, user smoke SQL — not just exit codes |
| Secrets leaking into evidence | Credentials referenced by name (`credentialsRef` → AWS profile), never inlined or persisted |
| Corrupt/garbage backups passing | A failed restore fails the drill; dependent checks report `SKIP`, never false `PASS` |

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
make e2e     # full drill loop against real Docker (also runs in CI)
make lint    # golangci-lint (incl. gosec)
```

## Roadmap

v0.3 MySQL + Velero drivers, `RecoveryDrill` CRD + operator (scheduled drills, history, Slack) · v0.4 cloud sandboxes (Terraform/RDS), sigstore/cosign attestations, compliance-control export. See [firedrill-plan.md](firedrill-plan.md).

## License

Apache-2.0
