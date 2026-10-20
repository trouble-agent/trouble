# QA Use Cases Catalog

This catalog documents the primary user goals (use cases) and their regression cases (REG) for the trouble daemon. Each use case has a spec anchor, the exact commands to drive it, and the observable pass condition.

---

## UC-001: Quickstart boot → first event → durable ledger check

**User goal**: Boot the daemon, send a first event, and verify the ledger is durable.

**Spec anchor**: SPEC-01 §2 (deploy/README.md quickstart)

**Commands**:
```bash
bin/troubled --config config.yaml
bin/trouble ledger status
bin/trouble ledger verify
```

**Pass condition**: rc=0, writer_pid != -1, verify reports 0 mismatches.

---

## UC-002: Ledger daily checks (status/verify)

**User goal**: Run daily ledger health checks to ensure the daemon is writing correctly and the ledger is intact.

**Spec anchor**: SPEC-01 §2.4

**Commands**:
```bash
bin/trouble ledger status --json
bin/trouble ledger verify
```

**Pass condition**: writer_pid matches daemon, verify rc=0 with 0 mismatches.

---

## UC-003: Compaction and retention

**User goal**: Compact old ledger files to reduce disk usage while preserving data integrity.

**Spec anchor**: SPEC-01 §3.4

**Commands**:
```bash
bin/trouble ledger compact --day YYYY-MM-DD [--dry-run]
```

**Pass condition**: rc=0, lines_out <= lines_in.

---

## UC-004: Hub profile query

**User goal**: Query the hub profile to see configured sensors and capabilities.

**Spec anchor**: SPEC-HUB §1

**Commands**:
```bash
bin/trouble hub profile
```

**Pass condition**: rc=0, JSON output.

---

## UC-005: Dashboard health

**User goal**: Check the dashboard health endpoint to confirm the API is serving.

**Spec anchor**: SPEC-DASH §1

**Commands**:
```bash
curl http://localhost:7644/health.json
```

**Pass condition**: 200 OK, build stamp present.

---

## UC-006: Ingest authentication

**User goal**: Verify ingest endpoint authentication ladder.

**Spec anchor**: SPEC-INGEST §2

**Commands**:
```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost:7643/ingest
```

**Pass condition**: 200 OK with valid token, 401 without.

---

## Regression Cases

### REG-001: Payload-hash false positive (TRBL-106-R2)

**Defect**: TRBL-106-R2 (verify payload_hash false-positives on virgin ledger)

**Anchor**: UC-002

**Reproduce**:
```bash
TMPROOT=$(mktemp -d)
bin/troubled --state-root $TMPROOT --config /dev/null --daemon=false &
sleep 2
kill $!
bin/trouble --state-root $TMPROOT ledger verify
```

**Expected**: Exit code 0, payload_hash_mismatches=0.

---

### REG-002: Ledger read-only contract (TRBL-104/105)

**Defect**: TRBL-104/105 (read-only verbs must not take writer seat or leave LOCK residue)

**Anchor**: UC-002

**Reproduce**:
```bash
TMPROOT=$(mktemp -d)
# No daemon running
bin/trouble --state-root $TMPROOT ledger status --json
```

**Expected**: writer_pid=-1, no LOCK file created.

---

### REG-003: Hole-trail record after injected seq gap (INT-CI-2)

**Defect**: INT-CI-2 (verify must detect seq gaps, not silently pass)

**Anchor**: UC-002

**Reproduce**:
```bash
# Inject a seq gap into a ledger file
# Run verify
bin/trouble --state-root $TMPROOT ledger verify
```

**Expected**: Exit code 1, seq_holes reported.

---

## Catalog Usage

Dogfood/QA ticks should:
1. Start from a documented use case (UC-NNN)
2. Drive the application as a real user would
3. Record the UC id in the run history
4. For fixed defects, re-run the corresponding REG case and record the REG id

This ensures coverage is durable and re-provable across ticks.
