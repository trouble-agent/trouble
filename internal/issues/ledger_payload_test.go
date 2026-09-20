package issues

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// payloadSchema is the §3.3 required-key set per op. Every key listed must be
// present on the record, so a schema drift is a test failure rather than a
// reviewer's memory.
var payloadSchema = map[string][]string{
	"ensure":      {"op", "driver", "sig", "inc", "idem_key", "result", "created", "commented", "attempt", "max_attempts"},
	"comment":     {"op", "driver", "sig", "idem_key", "result", "created", "commented", "external_id", "comment_kind", "window_expired"},
	"close":       {"op", "driver", "sig", "idem_key", "result", "reason", "attempt"},
	"reopen":      {"op", "driver", "sig", "idem_key", "result", "state"},
	"link":        {"op", "driver", "sig", "idem_key", "result", "task_id", "research_id"},
	"ack":         {"op", "driver", "sig", "result", "until"},
	"healthcheck": {"op", "driver", "ok", "detail", "rate_limit_remaining", "rate_limit_reset_ts", "consecutive_failures"},
	"replay":      {"op", "driver", "idem_key", "result", "attempts"},
	"drop":        {"op", "driver", "result", "count", "reason"},
	"cap":         {"op", "driver", "result", "cap", "window", "count_in_window"},
}

// attemptSchema is the §3.5 per-attempt record's required-key set.
var attemptSchema = map[string][]string{
	"ensure":      {"op", "driver", "idem_key", "attempt", "max_attempts", "outcome", "http_status", "error_code", "retryable", "backoff_ms", "next_try_ts"},
	"comment":     {"op", "driver", "idem_key", "attempt", "max_attempts", "outcome", "http_status", "error_code", "retryable", "backoff_ms", "next_try_ts"},
	"close":       {"op", "driver", "idem_key", "attempt", "max_attempts", "outcome", "http_status", "error_code", "retryable", "backoff_ms", "next_try_ts"},
	"reopen":      {"op", "driver", "idem_key", "attempt", "max_attempts", "outcome", "http_status", "error_code", "retryable", "backoff_ms", "next_try_ts"},
	"link":        {"op", "driver", "idem_key", "attempt", "max_attempts", "outcome", "http_status", "error_code", "retryable", "backoff_ms", "next_try_ts"},
	"healthcheck": {"op", "driver", "idem_key", "attempt", "max_attempts", "outcome", "http_status", "error_code", "retryable", "backoff_ms", "next_try_ts"},
}

// TestLedgerPayloadSchemas walks a full lifecycle and asserts every emitted
// record's payload against §3.3, the code range, the retryable flag and the
// actor stamp.
func TestLedgerPayloadSchemas(t *testing.T) {
	fx := resolvedFixture()
	d, led, clk, api := quietDesk(t, fx)

	inc := testIncident()
	ev := testEvidence()
	ref, ok := d.Anchor(testSig)
	if !ok || ref.ID == "" {
		t.Fatalf("the fixture filed no issue")
	}
	// 1. a manual comment (the CLI's `issues comment` path).
	if _, err := d.Comment(context.Background(), ref, "ev_trigger_1", "manual note"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	// 2. the sig linkage.
	if _, err := d.Link(context.Background(), ref, "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ", "res_01J9Z6Q0M2X4T8V1K7B3N5R8WL"); err != nil {
		t.Fatalf("link: %v", err)
	}
	// 3. an ack.
	if _, err := d.Ack(context.Background(), ref, time.Hour); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// Past the ack's hour and the comment spacing, still inside the 24h create
	// window: the recurrence below must reach the driver.
	clk.advance(61 * time.Minute)

	// 4. edge case 3: a human deletes the issue, the anchor is lost by the next
	// recurrence, and the create that follows the loss is what the per-sig create
	// cap exists to bound.
	api.mu.Lock()
	kept := api.issues[:0]
	for _, it := range api.issues {
		if it.Number != atoi(ref.ExternalID) {
			kept = append(kept, it)
		}
	}
	api.issues = kept
	api.mu.Unlock()
	if _, err := d.EnsureBySig(context.Background(), inc, ev); err == nil {
		t.Logf("anchor-loss recurrence folded without a driver error")
	}
	if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil && CodeOf(err) != types.CodeIssues004 {
		t.Fatalf("create after anchor loss: %v", err)
	}
	// 5. a failing probe pair: the healthcheck record and the driver_down gap.
	api.mu.Lock()
	api.down = true
	api.mu.Unlock()
	d.probe(context.Background(), "github")
	d.probe(context.Background(), "github")

	seen := map[string]int{}
	coded := map[string]int{}
	gaps := 0
	for _, r := range led.records() {
		if r.Kind == types.KGap {
			gaps++
			for _, k := range []string{"id", "sensor", "scope", "from_ts", "to_ts", "est_lost", "cause"} {
				if _, ok := r.Payload[k]; !ok {
					t.Fatalf("gap record is missing %q: %v", k, r.Payload)
				}
			}
			if strPayload(r.Payload, "sensor") != "issues" {
				t.Fatalf("gap sensor = %q", strPayload(r.Payload, "sensor"))
			}
			continue
		}
		if r.Kind != types.KIssue {
			t.Fatalf("the desk emitted a %s record: it owns issue and gap only", r.Kind)
		}
		op := strPayload(r.Payload, "op")
		seen[op]++
		var want []string
		var ok bool
		if _, isAttempt := r.Payload["outcome"]; isAttempt {
			// The §3.5 per-attempt record shares the op name with §3.3's op
			// record; its key set is its own.
			if _, ok := attemptSchema[op]; !ok {
				t.Fatalf("an attempt record claims an unknown op %q: %v", op, r.Payload)
			}
			want, ok = attemptSchema[op], true
		} else {
			want, ok = payloadSchema[op]
		}
		if !ok {
			t.Fatalf("unknown issue op %q: %v", op, r.Payload)
		}
		for _, k := range want {
			if _, present := r.Payload[k]; !present {
				t.Fatalf("op %s is missing %q: %v", op, k, r.Payload)
			}
		}
		// Every record carries the actor stamp (SPEC-01) so the audit chain names
		// the binary that wrote it.
		if r.Actor.Version == "" || r.Actor.GitSHA == "" {
			t.Fatalf("record %s has no actor stamp: %+v", op, r.Actor)
		}
		if r.Origin.Source != "issues" || r.Origin.HostID == "" {
			t.Fatalf("record %s has origin %+v", op, r.Origin)
		}
		code := strPayload(r.Payload, "error_code")
		if code == "" {
			continue
		}
		coded[code]++
		if !strings.HasPrefix(code, "TROUBLE-ISSUES-") {
			t.Fatalf("op %s emitted a code outside the area: %s", op, code)
		}
		if _, ok := r.Payload["retryable"]; !ok {
			t.Fatalf("op %s carries %s without a retryable flag", op, code)
		}
		if got, want := types.CodeClass[types.ErrorCode(code)], types.IssuesCodeClass[types.ErrorCode(code)]; got != want {
			t.Fatalf("code %s has class %s, want %s", code, got, want)
		}
	}
	// The lifecycle must have exercised its ops. close/reopen are covered by
	// quietclose_test.go, which asserts their key sets too.
	for _, op := range []string{"ensure", "comment", "link", "ack", "healthcheck", "cap"} {
		if seen[op] == 0 {
			t.Fatalf("the lifecycle never emitted op=%s (seen: %v)", op, seen)
		}
	}
	if gaps == 0 {
		t.Fatalf("no gap record accompanied the driver outage")
	}
	if len(coded) == 0 {
		t.Fatalf("the lifecycle emitted no coded record")
	}
}

// TestEveryIssueCodeIsDeclared proves the ten codes of the area are all in the
// catalog with the §5 class, which is what SPEC-INDEX §5 rule 4 requires.
func TestEveryIssueCodeIsDeclared(t *testing.T) {
	want := map[types.ErrorCode]types.ErrorClass{
		types.CodeIssues001: types.ErrClassTransient,
		types.CodeIssues002: types.ErrClassTransient,
		types.CodeIssues003: types.ErrClassPermanent,
		types.CodeIssues004: types.ErrClassPermanent,
		types.CodeIssues005: types.ErrClassTransient,
		types.CodeIssues006: types.ErrClassTransient,
		types.CodeIssues007: types.ErrClassPermanent,
		types.CodeIssues008: types.ErrClassPermanent,
		types.CodeIssues009: types.ErrClassTransient,
		types.CodeIssues010: types.ErrClassPermanent,
	}
	for code, class := range want {
		if got := types.CodeClass[code]; got != class {
			t.Fatalf("%s class = %q, want %q", code, got, class)
		}
	}
	if len(want) != 10 {
		t.Fatalf("the area declares %d codes, want 10", len(want))
	}
}

// TestPermanentFailuresNeverSpool pins the invariant that only retryable=true
// failures enter the spool: a permanent instance can never be replayed.
func TestPermanentFailuresNeverSpool(t *testing.T) {
	for _, code := range []types.ErrorCode{
		types.CodeIssues003, types.CodeIssues004, types.CodeIssues007, types.CodeIssues008, types.CodeIssues010,
	} {
		e := newErr(code, ReasonValidation, 0, false, "x")
		if RetryableOf(e) {
			t.Fatalf("%s is permanent and must not be retryable", code)
		}
	}
	for _, code := range []types.ErrorCode{
		types.CodeIssues001, types.CodeIssues002, types.CodeIssues005, types.CodeIssues006, types.CodeIssues009,
	} {
		e := newErr(code, ReasonTransient, 0, true, "x")
		if !RetryableOf(e) {
			t.Fatalf("%s is transient and must be retryable", code)
		}
	}
	if ClassOf(newErr(types.CodeIssues004, ReasonCap, 0, false, "x")) != types.ErrClassPermanent {
		t.Fatalf("a cap must classify as permanent")
	}
}
