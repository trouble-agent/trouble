# Repository traceability doctrine

This repository treats a completion claim as a claim about a reproducible change, not as a status
string. Every completion claim MUST point to repository evidence: a test, a documented inspection,
or another committed artifact that a reviewer can open and verify. A claim also needs a witness for
an external or independent observation. When no witness applies, say so explicitly as
`witness=none:<reason>`; a blank, omitted, or bare `none` is not evidence.

## The trace marker

Use one single-line marker to connect the work, its promise, its proof, and its history:

`ch:trace row=<row-id> spec=<spec-or-none> wave=<wave-or-none> test=<test-or-none> doc=<doc-path> prompt=<prompt-or-none> evidence=<repository-artifact> witness=<witness-or-none> verdict=<verdict-id-or-none> commit=<published-commit> memory=<memory-key-or-none>`

The marker fields are:

- `row` identifies the repository work item.
- `spec` identifies the governing specification or records why none applies.
- `wave` identifies the dispatch or implementation wave, or explains why none applies.
- `test` identifies the test or verification procedure.
- `doc` identifies the documentation surface changed or consulted.
- `prompt` identifies the task brief or explains why none applies.
- `evidence` identifies the repository artifact that supports the claim.
- `witness` identifies an independent observation, or uses `none:<reason>` when none applies.
- `verdict` identifies the review or judgment attached to the work.
- `commit` identifies the published commit containing the claimed result.
- `memory` identifies a durable memory record, or explains why none applies.

Every field must be present. Values that do not apply use `none:<reason>`, never a blank or bare
`none`. `evidence` is not a pointer to another claim: it is the artifact or reproducible procedure
that makes the claim checkable. A witness is not a substitute for repository evidence.

A verdict is valid only when it binds to the exact `commit` it judged, and that commit is published
and reachable from the repository's published history. A verdict from an uncommitted worktree or a
superseded commit cannot close a completion claim. Reviewers should therefore read the committed
tree, rerun the named test or inspection where practical, and confirm that the marker's links agree.

The minimum closeout is `STATUS`, `TIER`, `EVIDENCE`, followed by the complete marker. If a claim
cannot name repository evidence, a witness, or a published commit-bound verdict, it is not complete;
report the gap instead of converting it into a success statement.
