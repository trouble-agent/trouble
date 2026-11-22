package ladder

// error_seam_shape_test.go — TRBL-144: *Error carries the dashboard write-seam
// shape (Error.ErrorCode/ErrorDetail/ErrorReason + RefusedTransition)
// itself, so a raw ladder error on any seam path maps to a typed code
// (4xx/503) instead of the generic 500 action_failed. These tests pin the
// per-class mapping and prove the shape through an errors.As walk — exactly
// the lookup mapActionError performs.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestLadderErrorCarriesSeamShape(t *testing.T) {
	// Permanent + no reason: the wire reason falls back to the guard detail
	// (newErr's format string), matching the adapter's behaviour.
	e := newErr(types.CodeLadder012, "", "incident %s is not in the index", "inc_01J")
	if got := e.ErrorCode(); got != types.CodeLadder012 {
		t.Fatalf("ErrorCode = %s, want %s", got, types.CodeLadder012)
	}
	if got := e.ErrorDetail(); got != "ladder_012" {
		t.Fatalf("ErrorDetail = %q, want ladder_012", got)
	}
	if got := e.ErrorReason(); got != "incident inc_01J is not in the index" {
		t.Fatalf("ErrorReason = %q, want the guard detail (reason fallback)", got)
	}
	if !e.RefusedTransition() {
		t.Fatal("permanent-class refusal must mark the transition refused (409)")
	}

	// A reason set beats the detail.
	er := newErr(types.CodeLadder018, reasonKillSwitch, "checkpoint blocked")
	if got := er.ErrorReason(); got != reasonKillSwitch {
		t.Fatalf("ErrorReason = %q, want %s", got, reasonKillSwitch)
	}
	if got := er.ErrorDetail(); got != "ladder_018" {
		t.Fatalf("ErrorDetail = %q, want ladder_018", got)
	}

	// An empty detail carries no machine-readable detail at all.
	eb := newErr(types.CodeLadder001, reasonQuarantined, "")
	if got := eb.ErrorDetail(); got != "" {
		t.Fatalf("ErrorDetail = %q, want empty when Detail is unset", got)
	}

	// Transient class: a 503 candidate, never a refused transition.
	et := newErr(types.CodeLadder015, reasonParkFailed, "window not parked")
	if et.RefusedTransition() {
		t.Fatal("transient-class error must NOT mark the transition refused")
	}
	if got := et.ErrorDetail(); got != "ladder_015" {
		t.Fatalf("ErrorDetail = %q, want ladder_015", got)
	}
}

// TestEveryLadderCodeRefusalClassification walks the whole ladder catalog
// (SPEC-05 §5): every permanent code is a refused transition, every transient
// one is not. A new code inherits the mapping automatically.
func TestEveryLadderCodeRefusalClassification(t *testing.T) {
	if len(types.LadderCodeClass) < 18 {
		t.Fatalf("catalog shrank to %d codes: the sweep must cover the ladder range", len(types.LadderCodeClass))
	}
	for code, class := range types.LadderCodeClass {
		e := &Error{Code: code, Class: class, Reason: reasonQuarantined}
		if got := e.RefusedTransition(); got != (class == types.ErrClassPermanent) {
			t.Fatalf("%s (class %s): RefusedTransition = %v", code, class, got)
		}
	}
}

// TestSeamShapeFoundThroughWrapChain: mapActionError resolves the shape with
// errors.As, so the shape must survive fmt.Errorf-style wrapping.
func TestSeamShapeFoundThroughWrapChain(t *testing.T) {
	raw := newErr(types.CodeLadder012, "", "incident %s is not in the index", "inc_01J")
	wrapped := fmt.Errorf("drive: %w", raw)

	var shape interface {
		ErrorCode() types.ErrorCode
		ErrorDetail() string
		ErrorReason() string
		RefusedTransition() bool
	}
	if !errors.As(wrapped, &shape) {
		t.Fatal("errors.As lost the seam shape after wrapping")
	}
	if shape.ErrorCode() != types.CodeLadder012 || shape.ErrorDetail() != "ladder_012" || !shape.RefusedTransition() {
		t.Fatalf("shape through wrap = code %s detail %q refused %v", shape.ErrorCode(), shape.ErrorDetail(), shape.RefusedTransition())
	}
}
