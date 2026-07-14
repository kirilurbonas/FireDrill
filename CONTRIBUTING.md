# Contributing to FireDrill

Thanks for considering a contribution! FireDrill verifies backup recoverability — correctness and honest failure modes matter more here than in most projects: a false "RECOVERY VERIFIED" is worse than a crash.

## Development setup

Requirements: Go (version in `go.mod`), Docker. For Kubernetes work: `kind`, `kubectl`; for the Velero driver: the `velero` CLI.

```sh
make build   # bin/firedrill
make test    # unit tests
make lint    # golangci-lint incl. gosec (config in .golangci.yml)
make e2e     # real drills against Docker; k8s/velero tests skip without a cluster
```

Full local e2e including Kubernetes and Velero:

```sh
kind create cluster --name firedrill-dev
./examples/velero/setup-velero-kind.sh
make e2e
```

## Ground rules

- **Every change ships green**: `make lint test` plus the e2e suites your change touches. CI runs the whole matrix (including kind + Velero) and `govulncheck`.
- **Guardrails are non-negotiable**: sandboxes must always be destroyed (defer + TTL watchdog), sources stay read-only, secrets never enter specs/evidence/argv, and a failed restore must produce `verified: false` — never a false PASS (skipped checks count as unproven).
- **README stays current**: any user-visible change updates README.md (and docs/architecture.md) in the same commit.
- **New drivers/providers/checks**: follow the existing patterns — drivers self-register in `pkg/drivers`, sandbox providers implement `pkg/sandbox.Sandbox`, checks get spec validation + unit tests + an e2e assertion.

## Reporting issues

Bugs and feature requests via GitHub issues (templates provided). Security issues: see [SECURITY.md](SECURITY.md) — private disclosure only.
