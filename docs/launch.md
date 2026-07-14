# Launch kit (drafts — post manually)

## Show HN

**Title:** Show HN: FireDrill – continuously prove your backups actually restore

**Body:**

A backup that has never been restored is just hope stored on disk. Most teams
discover corruption, ransomware-encrypted backups, or 4-hour restore times
during the actual disaster — because backups are verified on write, almost
never on read.

FireDrill is an open-source tool that restores your real backups into a
disposable, isolated sandbox on a schedule, verifies the data actually came
back (row counts, checksums, a planted ransomware canary that must restore
byte-exact, custom smoke SQL), measures the true RTO/RPO, and emits signed
evidence — then destroys the sandbox.

It deliberately does NOT back anything up. It's the verification layer on top
of whatever you already run: pg_dump, mysqldump, Velero (it restores whole
Kubernetes namespaces into ephemeral namespaces), pgBackRest, RDS snapshots.

The part I haven't seen productized elsewhere: evidence is emitted as signed
in-toto/DSSE attestations (verifiable with cosign) and `firedrill controls`
aggregates runs into an auditor-ready matrix mapped to ISO 27001 A.8.13 /
SOC 2 A1.2 — so recovery testing stops being a screenshot in Confluence once
a year.

Ships as a single Go binary, a Kubernetes operator (`RecoveryDrill` CRD with
cron scheduling), Prometheus metrics + Grafana dashboard, Slack alerts.
Sandboxes are network-isolated (loopback-only Docker network / deny-all-egress
NetworkPolicy), TTL-bounded, and always destroyed.

Repo: https://github.com/kirilurbonas/FireDrill
Would love feedback — especially on what other backup formats deserve drivers.

## r/devops · r/PostgreSQL · r/kubernetes

**Title:** I built an open-source tool that fire-drills your backups — restore into a sandbox, verify the data, signed evidence for auditors

**Body:** (shorter, same skeleton — lead with the ransomware-canary angle for
r/devops, the pg_dump/RTO angle for r/PostgreSQL, the Velero/operator angle
for r/kubernetes. One GIF, one spec YAML, link.)

## Checklist before posting

- [ ] Re-record GIF if output changed since v0.6
- [ ] Pin the repo on your GitHub profile
- [ ] Post Tue–Thu, ~14:00–16:00 UTC for HN
- [ ] Be around for the first 2 hours to answer comments
- [ ] Expected first questions: "how is this different from pgbackrest_auto /
      AWS Backup restore testing?" (answer is in the README comparison table),
      "does it work with WAL archives / PITR?" (roadmap), "large DBs?"
      (subset restores are on the roadmap; document size caps honestly)
