// Package dashboard implements SPEC-10: the read-mostly HTTP face of the
// ledger — one embedded server in the daemon binary serving server-rendered
// pages and htmx HTML fragments from the SPEC-01 in-memory index.
//
// The package owns four things and nothing else (SPEC-10 §1):
//
//  1. The v0.1 route set (§2.1) — eight read surfaces, three write actions,
//     seven polling partials, the embedded static assets and one deterministic
//     not-found behaviour.
//  2. Auth: Bearer header or cookie only, three scopes (read, write,
//     autonomy), CSRF on every POST, a bind matrix with default-deny off
//     loopback.
//  3. Live updates: 2s polling of partials with a self-describing
//     stale-render guard, sized so AC-19's "live incident appears within 2s"
//     holds at p100 with a stated margin.
//  4. Budget discipline: no handler mmaps or full-scans the ledger; every read
//     goes through the injected index (SPEC-01 §3.4); the dashboard's slice of
//     the ≤80 MB steady-RSS budget is ≤6 MB (§2.9).
//
// The dashboard is not a writer of the ledger. Every click it accepts is a
// call on the owning subsystem through the Deps seam; the composition root
// (cmd/trouble) adapts those interfaces onto internal/ladder and
// internal/lifecycle.
//
// Import discipline (SPEC-10 §8, wave contract): stdlib + internal/types only.
// internal/ledger, internal/lifecycle and every other subsystem arrive through
// Deps function fields or interfaces declared in this package.
package dashboard