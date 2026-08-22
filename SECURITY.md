# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately via GitHub Security Advisories
(Security → Report a vulnerability on the repository) rather than public
issues. You should receive a response within a week.

## Security model

FireDrill is designed so that a drill can never endanger production:

- **Read-only sources.** FireDrill only ever downloads backups. Use
  read-only credentials for backup storage (`credentialsRef` resolves via
  the standard AWS chain / named profiles; secrets are never written to
  specs, evidence, or logs).
- **Isolated sandboxes.** Docker sandboxes run on a dedicated bridge
  network with the database port published on `127.0.0.1` only. Kubernetes
  sandboxes run in a dedicated namespace under a deny-all-egress
  NetworkPolicy (requires a NetworkPolicy-enforcing CNI). Credentials are
  random per drill and discarded with the sandbox.
- **Guaranteed teardown.** Sandboxes are destroyed via deferred cleanup on
  every code path plus an independent TTL watchdog.
- **No secrets in process lists.** Database passwords are passed to
  in-sandbox tooling via environment (`MYSQL_PWD` derived inside the
  container; `process.env` for mongosh) or via a config file the sandbox
  writes with a shell builtin (the MongoDB Database Tools), never argv.
  Notification endpoints are named by env var (`webhookEnv`, `urlEnv`), so
  webhook URLs stay out of specs, evidence and logs.
- **Tamper-evident evidence.** Evidence records are signed with ed25519
  twice over: a detached `.sig` envelope and an in-toto/DSSE attestation
  (`.intoto.jsonl`) verifiable with `cosign verify-blob-attestation`.
  `firedrill verify-evidence` checks both. The signing key lives at
  `~/.config/firedrill/firedrill.key` (0600) and is never copied into
  sandboxes or evidence.
- **Ransomware canary.** The `canary` check pins a pre-planted sentinel
  value that must restore byte-exact; encrypted-at-source or silently
  corrupted backups fail the drill. The sentinel is never recorded in
  evidence or logs.
- **User-supplied queries run in the sandbox only.** `rowCount`/`smoke`
  queries are user-authored by design and execute exclusively against the
  disposable restored copy — SQL through `database/sql`, mongosh
  expressions inside the container. Checksum identifiers are validated
  before interpolation.
- **Bounded downloads.** `maxBytes` caps the transferred artifact and
  `maxUncompressedBytes` (default 100x) caps what a compressed artifact may
  expand to, so a wrong prefix or a crafted archive cannot fill the runner's
  disk. Prefix discovery (`select: latest`) needs list permission on the
  prefix — still read-only.

## Supply chain

- CI runs `golangci-lint` (including gosec) and `govulncheck` on every push.
- Dependabot keeps Go modules and GitHub Actions current.
- Release binaries are built by GoReleaser in CI from tagged commits with
  checksums published alongside.
