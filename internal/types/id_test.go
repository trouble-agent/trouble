package types

import (
	"sort"
	"strings"
	"testing"
)

// TestNewIDIsUniqueAndIncreasing pins the identity contract every record
// depends on: ids minted by one process are distinct and strictly increasing in
// string order, including several minted inside one millisecond.
func TestNewIDIsUniqueAndIncreasing(t *testing.T) {
	const n = 512
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, NewID(PEv))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id minted: %s", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "ev_") || len(id) != 3+26 {
			t.Fatalf("id %q is not <prefix><26-char ULID>", id)
		}
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("ids are not increasing: mint[%d]=%s sorts at %d (%s)", i, ids[i], i, sorted[i])
		}
	}
}

// TestEncodeULIDLayout pins the 48-bit timestamp + 80-bit randomness layout: the
// leading characters carry the time so the string order matches time order.
func TestEncodeULIDLayout(t *testing.T) {
	var zero [10]byte
	early := encodeULID(1_000_000, zero)
	late := encodeULID(2_000_000, zero)
	if !(early < late) {
		t.Fatalf("a later timestamp must sort after an earlier one: %s vs %s", early, late)
	}
	withRand := encodeULID(1_000_000, [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	if !(encodeULID(1_000_000, zero) < withRand) {
		t.Fatal("the randomness must occupy the trailing characters (incrementing the last byte must increase the id)")
	}
}
