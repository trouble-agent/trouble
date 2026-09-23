# Contributing to trouble

Thanks for looking. This project is built spec-first, and that is the single
rule that shapes everything below.

## The authority is the spec set, not the code

`SPECS-BRIEF.md`, `SPECS-BRIEF-v0.1.1.md` and `specs/SPEC-01..13 + SPEC-TYPES +
SPEC-INDEX` are the contract. Code lands against an acceptance criterion (AC)
that already exists there, or the spec is amended first with a letter-suffix
section (`§3.4a`, not a renumbering of `§3.4`).

Before you write code:

1. Find the AC that covers your change in `specs/SPEC-INDEX.md`.
2. If no AC covers it, amend the owning spec (letter suffix) and add or amend
   the matrix row in `SPEC-INDEX.md`.
3. Run `python3 specs/tools/selfcheck.py` — it fails when a matrix row names a
   spec that does not declare the AC, and when metadata drifts from the matrix.
   A spec change that does not pass selfcheck is not a spec change.

## Build and test

```bash
make bin                 # build bin/trouble + bin/troubled
make check               # the v0.1 exit gate: selfcheck + schema-check + vet
                         # + go test ./internal/... + host-measured gates
make check-host          # host-measured gates verbosely, with fence verdicts
make conformance         # the registry conformance gate (SPEC-06 §2.4)
make smoke               # version triple, config explain, topology
make smoke-e2e           # 24 live assertions against both binaries
make ac-matrix           # the AC matrix over the SPEC-INDEX contract
make schema-check        # a descriptor edit without a regenerated schema fails
```

Notes that save time:

- `go test -count=1` is the norm; caching hides exactly the races this project
  cares about.
- Several gates are **host-measured** (throughput floors, p99 latency, RSS).
  They pass, fail or explicitly SKIP behind `internal/loadfence.FenceLoadAvg`.
  A SKIP on a loaded machine is not a failure; a FAIL below the fence is. Run
  the tree with `-p 1` when the box is also running other work, or those
  numbers move for reasons that have nothing to do with your change.
- The strongest single command for a change to the ledger, scrub or sentinel is
  `make check` plus `make smoke-e2e`.

## Style

- Go: `gofmt` clean, `go vet` clean, doc comments on exported identifiers.
  Comments explain *why* a decision was made and name the measurement that
  justifies it — this codebase is read as much as it is run.
- Specs and docs: state what is measured, what is asserted, and what is
  explicitly not covered. "Not covered" is a first-class answer; silent gaps are
  not.
- Tests: name the AC or the spec section they verify. A test that only restates
  the implementation is not evidence.

## Commits and pull requests

- One logical change per commit; subject in the imperative mood, short, with the
  AC or spec section in the body when it applies.
- Commit author is the project identity only. Do not add co-author trailers or
  personal names/addresses to this repository.
- No secrets, tokens, private hostnames, home-directory paths or personal data
  in commits, tests, fixtures or docs. Tests use documentation-range values
  (`192.0.2.0/24`, `example.com`, `opuser`). The repo runs a secret scan; a hit
  fails the commit.
- PRs: say which AC the change satisfies, what you ran, and what you measured.
  Paste the real command output for the gate you are claiming — a green claim
  without the run is treated as unverified.

## Reporting bugs and proposing features

Use the issue templates. For anything that looks like a security problem, follow
`SECURITY.md` instead of opening an issue.

## License of contributions

By contributing you agree your contribution is licensed under the MIT License
(see `LICENSE`), inbound = outbound.

## Code of conduct

Participation is covered by `CODE_OF_CONDUCT.md`.
