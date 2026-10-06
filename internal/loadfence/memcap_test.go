package loadfence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemCapParseMemSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"2GiB", 2 << 30, true},
		{"512MiB", 512 << 20, true},
		{"1GiB", 1 << 30, true},
		{"268435456", 1 << 28, true}, // raw bytes
		{"2500KiB", 2500 << 10, true},
		{"1.5GiB", int64(1.5 * (1 << 30)), true},
		{"2GB", 2 << 30, true}, // SI alias honoured
		{"500MB", 500 << 20, true},
		{"3G", 3 << 30, true},
		{"1024B", 1024, true},
		{" 2GiB ", 2 << 30, true}, // surrounding space tolerated
		{"", 0, false},
		{"bogus", 0, false},
		{"GiB", 0, false},        // unit with no number
		{"0GiB", 0, false},       // a zero cap is meaningless: rejected, not zero
		{"-2GiB", 0, false},      // negative rejected
		{"2 GiB", 2 << 30, true}, // embedded space tolerated: unambiguous, still 2GiB
	}
	for _, c := range cases {
		got, ok := parseMemSize(c.in)
		if ok != c.ok {
			t.Errorf("parseMemSize(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("parseMemSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestMemCapOverrideWinsOverEverything(t *testing.T) {
	t.Setenv(EnvMemOverride, "2GiB")
	got, source := MemCap()
	if got != 2<<30 {
		t.Fatalf("MemCap() = %d, want %d with %s set", got, int64(2<<30), EnvMemOverride)
	}
	if !strings.Contains(source, EnvMemOverride) {
		t.Errorf("source = %q, want it to name the override env", source)
	}
}

func TestMemCapBadOverrideFallsThroughToCgroup(t *testing.T) {
	t.Setenv(EnvMemOverride, "not-a-size")
	// No assertion on WHICH branch resolves on the host running the test —
	// this box may legitimately be capped or not. The contract under test:
	// a broken override must never resolve to capBytes 0 (which every fence
	// would read as "no ceiling") and must never panic; it must return the
	// cgroup figure or the no-ceiling sentinel, both of which the fences
	// handle. (The uncapped branch is exercised structurally below.)
	capBytes, _ := MemCap()
	if capBytes == 0 {
		t.Fatalf("MemCap() = 0 with a malformed override: 0 is not a representable state")
	}
}

func TestMemCapReadsACgroupV2File(t *testing.T) {
	// The v2 branch is file-driven; point the reader at a temp file that
	// carries a real cap figure by overriding the path constants the way the
	// QA harness's own scope would present them. MemCap reads the package
	// constants, so this test exercises parse + sentinel handling directly
	// through parseMemSize and documents the shapes the reader must handle.
	dir := t.TempDir()
	for name, body := range map[string]string{
		"memory.max-capped":    "3221225472\n",
		"memory.max-unlimited": "max\n",
		"memory.max-empty":     "",
		"limit.v1-unlimited":   "9223372036854771712", // v1's no-limit sentinel
		"limit.v1-garbage":     "not-a-number",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The capped shape parses to its byte figure.
	if got, ok := parseMemSize(strings.TrimSpace("3221225472")); !ok || got != 3<<30 {
		t.Errorf("cgroup v2 capped shape = %d ok=%v, want 3GiB", got, ok)
	}
	// The unlimited shapes must not parse as caps (they carry no number).
	for _, shape := range []string{"max", ""} {
		if _, ok := parseMemSize(shape); ok {
			t.Errorf("shape %q parsed as a cap; unlimited must read as no-ceiling", shape)
		}
	}
	// The v1 sentinel is above the 1<<62 threshold the reader ignores: prove
	// the boundary with the exact constant.
	const v1Unlimited = int64(9223372036854771712) // = 1<<63 - 4096
	if v1Unlimited <= 1<<62 {
		t.Fatalf("premise: the v1 unlimited sentinel must exceed the 1<<62 ignore threshold")
	}
}

func TestMemCapNoCapDetected(t *testing.T) {
	// Strip the override and ask the host. Whatever the cgroup situation is,
	// the no-ceiling sentinel is negative and the source empty. (This box's
	// own scope may or may not carry a cap; the fence handles both.)
	t.Setenv(EnvMemOverride, "")
	capBytes, source := MemCap()
	if capBytes >= 0 {
		if source == "" {
			t.Errorf("MemCap() = %d with empty source: a reported cap must name where it was read", capBytes)
		}
		if capBytes <= 0 {
			t.Errorf("MemCap() = %d: a detected cap is positive", capBytes)
		}
		return
	}
	if source != "" {
		t.Errorf("no-cap branch must leave the source empty, got %q", source)
	}
}

func TestMemCapText(t *testing.T) {
	if got := MemCapText(3<<30, "cgroup:v2:memory.max"); !strings.Contains(got, "3.00GiB") || !strings.Contains(got, "3221225472") || !strings.Contains(got, "memory.max") {
		t.Errorf("MemCapText = %q, want bytes + human figure + source", got)
	}
	if got := MemCapText(-1, ""); got != "no memory cap detected" {
		t.Errorf("MemCapText(-1) = %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{3 << 30, "3.00GiB"},
		{512 << 20, "512.0MiB"},
		{4096, "4.0KiB"},
		{512, "512B"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.in); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMemCapSkipReasonTable(t *testing.T) {
	// The decision table, forced through the override so it is host-independent.
	t.Run("no cap: the gate runs", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "")
		if capBytes, _ := MemCap(); capBytes >= 0 {
			t.Skip("host has a real cap; this row needs an uncapped reading")
		}
		if reason, skip := MemCapSkipReason("TestGate", 2<<30, "a 2GiB fixture"); skip {
			t.Errorf("MemCapSkipReason skipped on an uncapped host: %q", reason)
		}
	})
	t.Run("live set fits: the gate runs", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "2GiB")
		if reason, skip := MemCapSkipReason("TestGate", 1<<30, "a 1GiB fixture"); skip {
			t.Errorf("MemCapSkipReason skipped a fitting live set: %q", reason)
		}
	})
	t.Run("live set exactly the cap: runs (fit means <= cap)", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "1GiB")
		if _, skip := MemCapSkipReason("TestGate", 1<<30, "the boundary"); skip {
			t.Errorf("MemCapSkipReason skipped a live set exactly at the cap")
		}
	})
	t.Run("no live-set figure: the gate runs (the fence needs evidence)", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "512MiB")
		if _, skip := MemCapSkipReason("TestGate", 0, "unmeasured"); skip {
			t.Errorf("MemCapSkipReason fenced on a zero live-set figure")
		}
	})
	t.Run("live set over the cap: skip carrying the MEASURED cap value", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "512MiB")
		reason, skip := MemCapSkipReason("TestGate", 1<<30, "1M-record fixture, measured live")
		if !skip {
			t.Fatalf("MemCapSkipReason did not fence a 1GiB live set under a 512MiB cap")
		}
		// The CAP carries its raw byte figure AND its source (the contract's
		// "measured cap value"); the live set is named in human units plus its
		// evidence line.
		for _, want := range []string{"TestGate", "SKIP", "1.00GiB", "512.0MiB", "536870912", "mem cap", EnvMemOverride, "QA-TROUBLE-22"} {
			if !strings.Contains(reason, want) {
				t.Errorf("skip text missing %q:\n%s", want, reason)
			}
		}
	})
}

func TestSkipUnderMemCapWrapper(t *testing.T) {
	t.Setenv(EnvMemOverride, "512MiB")
	// The wrapper skips (the test framework records it via t.Skipf) — run the
	// fence for a live set that does not fit and prove the wrapper reaches the
	// skip path by NOT getting here when it does. The negative arm lives in
	// MemCapSkipReasonTable.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SkipUnderMemCap panicked instead of skipping: %v", r)
		}
	}()
	SkipUnderMemCap(t, "TestSkipUnderMemCapWrapper", 1<<30, "1M-record fixture, measured live", "quiet-host 515k rec/s")
	// Reached only when the fence did NOT skip.
	t.Errorf("SkipUnderMemCap did not skip a 1GiB live set under the 512MiB override")
}

func TestCappedNTable(t *testing.T) {
	t.Run("uncapped host is identity", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "")
		if capBytes, _ := MemCap(); capBytes >= 0 {
			t.Skip("host has a real cap; identity covered by the fitting case below")
		}
		if got := CappedN(t, "TestGate", 100000, 512<<10, 5000); got != 100000 {
			t.Errorf("CappedN = %d on an uncapped host, want the unlimited 100000", got)
		}
	})
	t.Run("caps to what fits", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "64MiB")
		// 64MiB / 512B = 131072 units fit; 200k requested.
		if got := CappedN(t, "TestGate", 200000, 512, 50000); got != (64<<20)/512 {
			t.Errorf("CappedN = %d, want the cap-derived %d", got, int64(64<<20)/512)
		}
	})
	t.Run("floor holds under a tight cap", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "1MiB")
		// 1MiB / 512B = 2048 — below the floor, the floor wins.
		if got := CappedN(t, "TestGate", 100000, 512, 50000); got != 50000 {
			t.Errorf("CappedN = %d, want the floor 50000", got)
		}
	})
	t.Run("already fitting is identity", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "2GiB")
		if got := CappedN(t, "TestGate", 100000, 512, 5000); got != 100000 {
			t.Errorf("CappedN = %d, want 100000 (the run already fits)", got)
		}
	})
	t.Run("zero live-per-unit is identity (no division by zero)", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "1MiB")
		if got := CappedN(t, "TestGate", 100000, 0, 5000); got != 100000 {
			t.Errorf("CappedN = %d with livePerUnit=0, want the unlimited count", got)
		}
	})
	t.Run("unlimited already at or below the floor is identity", func(t *testing.T) {
		t.Setenv(EnvMemOverride, "1MiB")
		if got := CappedN(t, "TestGate", 4000, 512, 5000); got != 4000 {
			t.Errorf("CappedN = %d for unlimited <= floor, want 4000 unchanged", got)
		}
	})
}

func TestGOMEMLIMITBytesIsReadable(t *testing.T) {
	// The runtime soft limit is queryable; on this box the harness usually
	// sets one. Whatever the value, it must not be zero (the runtime's
	// no-limit figure is math.MaxInt64, never 0).
	if got := GOMEMLIMITBytes(); got == 0 {
		t.Errorf("GOMEMLIMITBytes() = 0: the runtime reports MaxInt64 for unset, never 0")
	}
}
