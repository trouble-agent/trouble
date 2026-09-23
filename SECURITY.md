# Security policy

`trouble` is a self-hosted incident system: it ingests events from the outside
world, persists them, and can apply changes to the host it runs on. Security
reports are welcome and taken seriously.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting:

<https://github.com/trouble-agent/trouble/security/advisories/new>

If you cannot use advisories, mail `totalwindupflightsystems@gmail.com` with
`trouble security` in the subject. Please do not open a public issue for a
suspected vulnerability.

Include, as far as you can:

- the version triple (`trouble --version` prints release, git SHA, build time —
  `0.0.0-dev` means an unstamped build from a tagless tree),
- the subsystem involved (scrub gate, sentinel ingestion, ledger, registry
  modules, dashboard, lifecycle),
- a minimal reproduction: config, request/command, observed vs expected,
- your assessment of impact.

## What to expect

- Acknowledgement within 3 business days.
- An assessment (accepted / needs-more-info / out-of-scope) within 7 business
  days of a usable report.
- Fix or mitigation guidance for accepted reports as fast as the finding
  warrants; you will be credited in the advisory unless you ask otherwise.

## Scope

In scope:

- the scrub gate (`internal/scrub`) — anything that gets persisted should pass
  through it; a leak it fails to remove is a security bug,
- sentinel ingestion (`internal/sentinel`) — envelope and legacy `/store/`
  handlers, quotas, the on-ramp,
- the dashboard and its auth/identity seam (`internal/dashboard`),
- the registry action surface (`internal/registry`) — authorize → validate →
  dry-run → apply → verify → audit, and the do-not-touch floor,
- the ledger's durability and torn-line handling (`internal/ledger`),
- the CLI and lifecycle install/upgrade path (`cmd/`, `internal/lifecycle`).

Out of scope:

- vulnerabilities in third-party dependencies with no exploitable path through
  this project (report those upstream, tell us if we need to bump a pin),
- deployments that ignore the documented bind/token/tailnet matrix and expose
  the dashboard to the open internet without a token,
- host-measured performance gates failing on a loaded machine (that is a
  documented fence, not a vulnerability).

## Hardening notes for operators

- Never run the dashboard with write scope over an untrusted network: use the
  loopback bind, the tailnet seam or a proxy-header seam with a token file.
- Keep the state root at `0700` and token files at `0600`; the lifecycle checks
  refuse wider modes.
- The registry's do-not-touch floor is compiled in and additive-only: weakening
  it is refused by design.
