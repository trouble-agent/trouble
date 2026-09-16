# registry schemas (generated — do not edit by hand)

`internal/registry/schema/<module>@<version>.json` is the committed, generated
JSON Schema (draft 2020-12) of every shipped module's args struct.

Regenerate with `make schema`; `make schema-check` fails when a descriptor edit
lands without a regenerated artifact (SPEC-06 §3.2). At boot the registry
regenerates each schema in memory and compares it byte for byte with the file
here: a mismatch is `TROUBLE-REGISTRY-014` and the daemon exits, so a stale or
tampered schema never reaches a call.

This README exists so the directory is embeddable (`//go:embed schema`) before
the first generation run.
