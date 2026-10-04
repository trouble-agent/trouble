# The agent stage's LLM client: `internal/llm`

Authority: SPEC-05 §4.3a ("The `[llm]` table") and §3.7a ("The agent stage's per-call
budgets and context compaction"). This page documents the implementation as it
exists in `internal/llm/{client,config,compact,errors}.go`; where this page and
the spec disagree, the spec wins.

## What the package is

`internal/llm` is the ladder's budgeted, buffered LLM client for the agent stage.
It is deliberately small and stdlib-only (`net/http`, no SDK): one request and one
response per stage call, an ordered fallback chain of OpenAI-compatible
candidates, and every budget enforced rather than merely reported.

What the ladder sees instead: `internal/llm` is opaque to the ladder's contract.
The ladder depends on the `AgentPort` interface — one method,
`RunAgent(ctx, prompt) (AgentOutcome, error)` — and on the shared
`types.AgentOutcome` record; it never imports this package's types. That is what
keeps the client swappable and the `agent_run` record shape stable (SPEC-TYPES
§3.15.12a).

## Exported API

| Symbol | What it is |
|---|---|
| `Config` | the resolved `[llm]` table: candidates, caps, compaction |
| `Candidate` | one chain entry (`name`, `base_url`, `model`, `key_ref`, per-candidate `max_tokens`/`timeout`) |
| `CompactConfig` | the compaction hook's four caps + enabled flag |
| `Request` | one buffered completion (`system`, `prompt`, `context` chunks, optional `max_tokens`) |
| `Response` | the completion text plus accounting: serving candidate/model/endpoint, `usage`, `compaction`, per-attempt records, latency |
| `New(cfg Config) (*Client, error)` | builds and validates; a buildable-but-invalid config is a construction error, never a surprise on the first call |
| `Client.Complete(ctx, Request)` | one buffered completion over the ordered chain |
| `Client.RunAgent(ctx, prompt)` | the `AgentPort` implementation: completion + `AgentOutcome` (outcome returned WITH the error, so the record never depends on an error string) |
| `Client.MaybeCompact(ctx, chunks)` | the compaction hook; returns the context plus its `LLMCompaction` accounting |
| `errors.go` classes | `transport`, `timeout`, `rate_limited`, `server_error`, `credential`, `client_error`, `contract`, `budget`, `compaction` |
| `Retryable(class)` | whether the chain may move to the next candidate |

`Retryable` is true for `transport`, `timeout`, `rate_limited`, `server_error`
and `credential` — the last two because a candidate-local failure (one unset
env var, one 401) must not take down a chain whose next candidate is healthy.
`client_error` is NOT retryable: a malformed request is malformed for every
candidate. `budget`, `contract` and `compaction` are decisions this client
already made and are reported, never retried.

## The `[llm]` config table

Declared as a top-level `[llm]` table and decoded strictly (an unknown key inside
`[llm]` is refused by name). Defaults, all safe and none fleet-specific:

| Key | Default | Meaning |
|---|---|---|
| `max_tokens` | `4096` | HARD completion cap of one stage call (`> 0` validated) |
| `timeout` | `120s` | wall-clock cap of ONE attempt, as a context deadline |
| `max_attempts` | `0` | chain entries one stage run may try; `0` = the whole chain (bounded failover) |
| `max_response_bytes` | `1048576` | buffered response cap; a larger body is refused, not read |
| `fallback_chain` | `[]` | ordered candidate NAMES; omitted = `[[llm.candidates]]` declaration order |
| `candidates[].name` | required | stable chain-entry id the `agent_run` record carries |
| `candidates[].base_url` | required | OpenAI-compatible root (`http`/`https`, no embedded credentials) |
| `candidates[].model` | required | model id sent in the body |
| `candidates[].key_ref` | required | NAME of the env var holding the key |
| `candidates[].max_tokens` | `0` | candidate cap; `0` inherits `max_tokens`; a larger value is refused |
| `candidates[].timeout` | `0` | candidate wall clock; `0` inherits `timeout` |
| `compact.enabled` | `false` | the compaction hook; off means an over-budget context is refused |
| `compact.budget_tokens` | `24000` | assembled-context budget |
| `compact.chunk_tokens` | `6000` | target size of one summarisation group |
| `compact.max_chunks` | `8` | summarisation-call cap; a context needing more groups is refused |
| `compact.max_tokens` | `800` | summarisation completion cap, bounded by `max_tokens` |

The compiled default chain is **empty**: with no table declared, `Deps.Agent`
stays unwired and the agent stage refuses with TROUBLE-LADDER-021
(`reason=no_agent_port`) instead of inventing a model. No default endpoint, no
default model, no default key is ever compiled in.

## `key_ref` is a NAME, never a value

`key_ref` must match `^[A-Z][A-Z0-9_]{2,63}$` — the NAME of an environment
variable. Anything else (a provider token, a base64 blob, a path) is refused at
construction. The value is read from the process environment at call time
(`EnvKeyResolver`), so rotation needs no restart, and no config dump, boot
record, record payload or error message can carry the key.

## Strict decode and boot refusal

`LoadConfig` decodes the `[llm]` table strictly: unknown keys are refused by
name (a typo that silently keeps a cap is how a budget stops existing). A
declared-but-unbuildable table is a **boot refusal** (SPEC-12 §3.1c), recorded
with TROUBLE-LIFECYCLE-001 naming the key's own message — never a silent
fallback to "no model".

## Request enforcement order

`Complete` enforces in this order:

1. **Token cap** — an over-cap request is refused before it is built; nothing is
   sent. (The provider's reported `usage.completion_tokens` over the cap fails
   the stage afterwards; the text is discarded, never truncated.)
2. **Context budget** — the assembled context is measured. At or below
   `compact.budget_tokens` nothing happens. Above it: compaction if the hook is
   enabled, refusal (`compaction`) if not. Compaction is a single-shot MAP pass:
   at most `max_chunks` groups of about `chunk_tokens` each, in order — no chunk
   cut in half, none dropped — each group summarised by exactly one capped
   completion through the same ordered chain, summaries replacing the groups. A
   split needing more groups than `max_chunks`, or a failing summarisation call,
   REFUSES the stage. There is no code path that returns a shorter context than
   it was given without an accounting that says so; silent truncation is the one
   outcome the hook exists to prevent. Estimated token counts are flagged
   `estimated:true` and never presented as measurements.
3. **Per-attempt wall clock** — each attempt runs under its own context
   deadline; a hung upstream costs exactly the configured cap and classifies as
   `timeout`.
4. **Ordered fallback chain** — candidates are tried in `fallback_chain` order
   (or declaration order), moving on after a retryable failure, bounded by
   `max_attempts`. A stage whose caller deadline expires stops.

Every attempt is classified into exactly one error class (see the table above);
the serving candidate — or the last attempt's class — is what the ledger records.

## Example

Invented but schema-valid values; no real endpoints or keys:

```toml
[llm]
max_tokens     = 4096
timeout        = "120s"
max_attempts   = 2
fallback_chain = ["primary", "backup"]

[[llm.candidates]]
name     = "primary"
base_url = "https://llm.example.internal/v1"
model    = "example-large"
key_ref  = "EXAMPLE_LLM_KEY"

[[llm.candidates]]
name       = "backup"
base_url   = "https://fallback.example.internal/v1"
model      = "example-small"
key_ref    = "EXAMPLE_LLM_KEY_BACKUP"
max_tokens = 2048
timeout    = "60s"

[llm.compact]
enabled        = true
budget_tokens  = 24000
chunk_tokens   = 6000
max_chunks     = 8
max_tokens     = 800
```
