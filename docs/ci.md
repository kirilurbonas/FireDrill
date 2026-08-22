# Drills in CI

A drill that runs on someone's laptop proves nothing next quarter. This page
covers running FireDrill from CI and turning "we test our backups" into a
check that fails the build when it stops being true.

## The GitHub Action

```yaml
- uses: kirilurbonas/FireDrill@v0.11.1
  with:
    file: firedrill.yaml
    all: "true"
    gate-max-age: 48h
```

The action downloads the pinned release, **verifies it against the release's
`checksums.txt`** before executing it, runs the drill(s), and — when
`gate-max-age` is set — gates on recovery freshness afterwards.

| Input | Default | Meaning |
|---|---|---|
| `version` | `latest` | Release to run, e.g. `0.11.0`. Pin it in production. |
| `file` | `firedrill.yaml` | Drill spec. |
| `drill` | — | Name of a single drill to run. |
| `all` | `false` | Run every drill in the file and print a scorecard. |
| `evidence-dir` | `evidence` | Where evidence is written. |
| `args` | — | Extra arguments for `firedrill run`. |
| `gate-max-age` | — | When set, run `firedrill gate --from-spec <file> --max-age <value>`. |
| `gate-args` | — | Extra arguments for `firedrill gate`, e.g. `--require-signed`. |
| `signing-key` | — | PEM of the evidence-signing key, from a secret. Required when the spec sets `report.sign`. |
| `fail-on-unverified` | `true` | Set `false` to collect evidence now and gate in a later job. |

Outputs: `verified` (`true`/`false`) and `evidence-dir`.

### Signing evidence in CI

Every shipped spec sets `report.sign: true`, and signing needs a key. Do not
generate one on the runner: it is discarded when the job ends, so nothing can
be verified against it later, and every run would be signed by a different
stranger. Generate the key once, keep the private half in a secret, and
distribute the public half to whoever verifies:

```sh
firedrill keygen                                  # once, on a trusted machine
gh secret set FIREDRILL_SIGNING_KEY < ~/.config/firedrill/firedrill.key
```

```yaml
- uses: kirilurbonas/FireDrill@v0.11.1
  with:
    file: firedrill.yaml
    all: "true"
    signing-key: ${{ secrets.FIREDRILL_SIGNING_KEY }}
```

The key is written to a private file under `RUNNER_TEMP` for the duration of
the job and never lands in evidence, logs, or the workspace. Auditors then
verify with `firedrill verify-evidence --public-key firedrill.pub`, which
fails on anything signed by a different key.

The runner needs Docker — GitHub-hosted `ubuntu-latest` has it — plus
read-only credentials for wherever the backup lives. A complete nightly
workflow, including evidence upload, is in
[examples/ci/github-actions.yml](../examples/ci/github-actions.yml).

Evidence is disposable inside a CI job, so persist it: upload it as an
artifact, cache it between runs, or (best) write it to durable storage. The
gate can only see the history it is given.

## The recovery SLO gate

`firedrill controls` reports what happened. `firedrill gate` enforces what
must keep happening, and exits non-zero when it doesn't:

```sh
# Every drill in the spec must have a verified run in the last 24 hours.
firedrill gate --from-spec firedrill.yaml --max-age 24h

# Auditor mode: the evidence must also be signed — and, with --public-key,
# signed by the key you trust rather than merely self-consistent.
firedrill gate --from-spec firedrill.yaml --max-age 720h --require-signed
firedrill gate --from-spec firedrill.yaml --max-age 720h --public-key firedrill.pub

# Per compliance control instead of per drill.
firedrill gate --by control --control ISO27001-A.8.13 --max-age 720h
```

```
DRILL                     LAST RUN (UTC)     LAST VERIFIED      SIGNED  STATUS
payments-db               2026-08-22 03:04   2026-08-22 03:04   ✓       ok
orders-db                 2026-07-24 03:02   2026-07-24 03:02   ✓       FAIL: last verified run was 29d ago (max 24h)
ledger-db                 —                  —                 —       FAIL: no evidence — has this drill run at all?

3 drill(s): 1 ok, 2 failing
```

A subject fails the gate when:

- **it has no evidence at all** — the failure mode a report can never show
  you, because a drill that silently stopped running leaves nothing behind;
- **its latest run did not verify recovery** (relax with `--allow-unverified`);
- **its most recent verified run has aged out** of `--max-age`;
- **`--require-signed`** is set and that evidence carries no valid signature
  (`--public-key <pem>` goes further and pins *which* key must have signed it).

Naming the subjects is what makes it a guarantee. `--from-spec` (every drill
in a spec file), `--drill` and `--control` all assert existence; without them
the gate reports only on drills that already have evidence.

Exit codes match `run`: `0` clean, `1` violations, `2` could not execute.
`--format json` emits the same result for dashboards.

## Without the Action

Any CI system works — the binary and the container image are self-contained:

```sh
# binary
curl -sSfL https://github.com/kirilurbonas/FireDrill/releases/latest/download/checksums.txt -o checksums.txt
# … download the matching tar.gz, sha256sum --check --ignore-missing checksums.txt, extract …
firedrill run --all -f firedrill.yaml

# or the published image (needs the Docker socket for the sandbox)
docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD:/work" -w /work \
  ghcr.io/kirilurbonas/firedrill:v0.11.0 run --all -f firedrill.yaml
```

On a shared runner, add `firedrill gc` to the job's cleanup step so a killed
job never leaves a sandbox behind.

## Scheduling without CI

The [Kubernetes operator](../README.md#kubernetes) runs drills from a
`RecoveryDrill` cron schedule inside the cluster, and emits Events plus
Prometheus metrics. `firedrill gate` still applies — point it at the
operator's evidence PVC from a CronJob.
