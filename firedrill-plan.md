# FireDrill — build plan

**Fire drills for your backups. Prove recovery before you need it.**

FireDrill continuously restores your real backups into a disposable, isolated sandbox, verifies the data actually came back intact, measures the true recovery time, and emits signed, audit-grade evidence — then destroys the sandbox. It answers the one question every backup tool quietly dodges: *if today were the disaster, would you actually get your data back — and how long would it take?*

> Name rationale: "fire drill" instantly communicates *practicing for disaster*, it's already the language auditors and SREs use, and it's positive (you're prepared) rather than morbid. Strong, memorable, and clear to a non-expert recruiter at a glance.
>
> Alternatives if you want a different flavor: **Rehearsal** ("rehearse your recovery"), **Revenant** (edgier — "your data returns from the dead"), **Testament** (proof + last-will double meaning, audit-flavored).

---

## 1. The problem (and why now)

Backups are verified on *write*, almost never on *read*. Teams discover corruption, ransomware-encrypted backups, missing tables, or 48-hour restore times only during a real disaster. As the ops adage goes, a backup that has never been restored is just hope stored on disk.

Meanwhile every regulated org (SOC 2, ISO 27001, DORA, HIPAA) is *required* to demonstrate that recovery works — and today that "evidence" is usually a screenshot and a Confluence page written once a year. There is no cloud- and database-agnostic open-source tool that proves recoverability continuously and produces auditor-ready evidence. Velero + hand-rolled CronJobs is the closest pattern, and it's Kubernetes-only and DIY. Commercial options (e.g. Veeam SureBackup) are VM/Windows-world and paid.

That gap is the opening.

## 2. The wedge (positioning — read this twice)

**FireDrill does not back anything up.** It is the *verification layer* on top of whatever backup you already run (pg_dump, pgBackRest, Velero, restic, RDS snapshots, `mysqldump`, cloud-native snapshots). This is the single most important design decision:

- It sidesteps competition with entrenched, trusted backup tools — it complements them.
- It's immediately useful to everyone regardless of their backup stack.
- It targets the actual unmet need: *proof of recoverability*, not another way to copy bytes.

One sentence: **FireDrill is backup-agnostic recovery verification with audit-grade proof.**

## 3. How it works — the drill loop

```
 firedrill.yaml           ┌────────────────┐        ┌────────────────────────┐
 (RecoveryDrill spec) ───▶│  Orchestrator  │◀───────│ Scheduler / Operator   │
                          └───────┬────────┘        │ (CronJob / CRD, v0.3)  │
                                  │                 └────────────────────────┘
      ┌──────────────┬───────────┼────────────┬────────────────────┐
      ▼              ▼           ▼            ▼                    ▼
 Restore driver   Sandbox    Verify       Reporter             Metrics
 (plugin)         provider   engine       signed evidence      Prometheus
 postgres|velero  docker|k8s check[]      JSON + PDF, cosign   Grafana|Slack
      │              │
      │   ┌──────────▼────────────┐
      └──▶│  ephemeral sandbox     │  isolated · TTL-bounded · cannot reach prod
          │  restore → verify → 🔥 │  destroyed after every drill (cost control)
          └────────────────────────┘
```

1. **Declare** recovery targets in `firedrill.yaml` (source backup, restore method, objectives, checks, schedule).
2. **Provision** a throwaway sandbox, fully isolated from production, with a hard TTL.
3. **Restore** the latest backup into it.
4. **Verify** — restore succeeded, data integrity (row counts / checksums / schema), freshness vs RPO, an app-level smoke query, and *measured* restore time vs RTO.
5. **Report** — pass/fail + measured RTO/RPO + a cryptographically signed evidence record; push metrics and Slack.
6. **Destroy** the sandbox. Store the result to trend RTO over time and alert on regressions.

## 4. The differentiator — audit-grade evidence

Every drill emits a tamper-evident, timestamped, signed evidence record (cosign / in-toto attestation), for example: *"2026-07-08 03:04 UTC — backup `payments/latest.dump` restored to an isolated sandbox in 3m50s; 1,240,338 ledger rows verified; RPO 41m (target ≤1h); integrity checksum matched; sandbox destroyed."*

Records map to controls so a GRC/audit team can consume them directly:

| Framework | Example controls FireDrill evidences |
|---|---|
| ISO 27001:2022 | A.8.13 Information backup · A.5.30 ICT readiness for business continuity |
| SOC 2 | A1.2 / A1.3 (availability, recovery testing) · CC7.x system operations |
| DORA / HIPAA | Recovery testing & continuity evidence |

Nobody has productized "DR verification as continuous compliance evidence" in open source. That's the moat and the portfolio headline.

## 5. Tech stack

- **Language: Go.** Single static binary, native to the Kubernetes/operator ecosystem, and the credible choice for infra tooling. (It's also a deliberate level-up signal for your profile, which is Python/TS-heavy. If you want to ship the MVP faster, Python is a fine v0.1 fallback — but Go is the stronger portfolio move.)
- **CLI:** Cobra.
- **Restore/sandbox:** Docker SDK (v0.1), client-go / controller-runtime (K8s), Terraform for cloud sandboxes (v0.4).
- **Signing/evidence:** Sigstore cosign or in-toto attestations.
- **Observability:** Prometheus client, ready-made Grafana dashboard.
- **Testing:** Testcontainers-go (spin real Postgres in CI to test the drill end-to-end — dogfoods the product).

## 6. MVP scope — v0.1 (buildable in ~2 focused weekends, fully demoable)

- `firedrill run <target>` CLI + `firedrill.yaml` spec loader
- **One restore driver: Postgres** (restore a `pg_dump`/basebackup from S3 or local file)
- **One sandbox: local Docker** (zero infra needed to demo — runs on a laptop)
- **Verification:** restore-succeeded · freshness · row-count · checksum · custom smoke SQL · measured restore time
- **Output:** colored CLI summary + JSON evidence file + signed manifest
- Hard guardrails: sandbox TTL, prod-unreachable network, read-only source credentials

That's a complete, honest, screenshot-worthy story on its own.

## 7. Roadmap

| Milestone | Adds | Why it matters |
|---|---|---|
| **v0.2** | Velero driver (restore a K8s namespace into an ephemeral namespace) · Prometheus metrics · HTML report | Covers the K8s crowd; makes it observable |
| **v0.3** | `RecoveryDrill` CRD + operator (scheduled drills, history, Slack) · Grafana dashboard | Turns a CLI into a *platform*; shows operator skills |
| **v0.4** | Cloud driver (Terraform-provisioned temp RDS from a snapshot) · compliance-control export · ransomware/garbage-backup canary | Enterprise + regulated angle; ties to your IaC work |
| **v1.0** | Multi-target scorecards, RTO trend dashboards, per-drill cost reporting (FinOps) | The "recovery posture" product |

## 8. Config model (`firedrill.yaml`)

```yaml
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata:
  name: payments-db
spec:
  schedule: "0 3 * * *"            # nightly 03:00 (used by the operator, v0.3)
  objectives:
    rto: 15m                       # restore must complete within 15 minutes
    rpo: 1h                        # backup must be younger than 1 hour
  source:
    driver: postgres
    from: { type: s3, uri: s3://acme-backups/payments/latest.dump, credentialsRef: aws-backup-ro }
  sandbox:
    provider: docker               # docker | kubernetes | terraform
    image: postgres:16
    ttl: 30m                       # hard teardown guardrail
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 1h }
    - rowCount:  { query: "select count(*) from ledger", min: 1000000 }
    - checksum:  { table: ledger, column: id }
    - smoke:     { sql: "select 1 from accounts where status='active' limit 1", expectRows: ">=1" }
  report:
    sign: true
    controls: [ISO27001-A.8.13, SOC2-A1.2]
    sinks:
      - { type: prometheus }
      - { type: slack, channelRef: sre-alerts }
```

## 9. CLI experience (the demo)

```
$ firedrill run payments-db
▸ provision sandbox  docker postgres:16 ................ ok   2.1s
▸ restore  s3://acme-backups/payments/latest.dump ...... ok   3m48s
▸ verify   restore succeeded ........................... PASS
▸ verify   freshness  (age 41m ≤ 1h) .................. PASS
▸ verify   row count  ledger ≥ 1,000,000 (1,240,338) .. PASS
▸ verify   checksum   ledger.id ....................... PASS
▸ verify   smoke query ................................ PASS
▸ measured RTO 3m50s (target 15m ✓)   RPO 41m (target 1h ✓)
▸ evidence ./evidence/payments-db-2026-07-08.json  (signed ✓)
✔ RECOVERY VERIFIED — sandbox destroyed
```

Record this as an asciinema/GIF for the README. It sells the whole project in ten seconds.

## 10. Repo structure

```
firedrill/
├── cmd/firedrill/            # CLI entrypoint (cobra)
├── pkg/
│   ├── spec/                 # RecoveryDrill schema + loader
│   ├── drivers/              # restore drivers: postgres, velero, …
│   ├── sandbox/              # sandbox providers: docker, k8s, terraform
│   ├── verify/               # verification checks
│   ├── report/               # evidence + signing + HTML/PDF
│   └── metrics/              # prometheus exporter
├── operator/                 # controller-runtime (RecoveryDrill CRD)  [v0.3]
├── examples/                 # sample backup + drill for the demo
├── docs/architecture.md
└── .github/workflows/ci.yml  # dogfoods a real restore via testcontainers
```

## 11. Risks & mitigations (put this in the README — it signals maturity)

| Risk | Mitigation |
|---|---|
| Large datasets make restores slow/expensive | Size caps, subset/sampled restore, off-peak scheduling, scale-to-zero sandboxes |
| Accidentally touching production | Read-only source creds, network-isolated sandbox that cannot reach prod, dry-run mode, TTL teardown |
| "Restore ran" ≠ "app actually works" | User-supplied smoke checks (SQL/HTTP), schema + checksum verification, not just exit codes |
| Secrets handling for backup sources | Pull via existing secret stores (Vault / K8s secrets), never persisted in evidence |
| Verifying arbitrary app correctness generically | Pluggable check interface; ship sensible defaults, let teams extend |

## 12. Why this is *your* project

It sits on your rare SRE + regulated-compliance intersection (air-gapped, SOC2/ISO27001, fintech/healthcare). It reuses your Terraform, Kubernetes, Prometheus, and DevSecOps strengths, adds Go and an operator (fresh signals), and solves a problem people feel viscerally. The demo produces real numbers and a signed report — recruiter-proof, and genuinely useful open source.

## 13. First week — concrete next steps

1. `git init firedrill`, Cobra skeleton, `RecoveryDrill` spec struct + YAML loader.
2. Docker sandbox provider (create → wait healthy → destroy, with TTL).
3. Postgres driver: restore a local `pg_dump` into the sandbox; time it.
4. Three checks: restoreSucceeded, rowCount, freshness.
5. JSON evidence writer + cosign signing.
6. README with the architecture diagram + a recorded CLI GIF.

Ship v0.1 with just Postgres + Docker. That alone is a portfolio-grade repo.
