package scrub

import (
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// SPEC-02 §7: 14 positive vectors (one per rule family plus boundary cases) and
// 12 negative vectors, each asserting exact output bytes and the exact ByRule
// map. 100% exact-byte equality, 0 tolerance.

type vector struct {
	id      string
	target  types.ScrubTarget
	project string
	cfg     string
	in      string
	out     string
	byRule  map[string]int
}

func positiveVectors() []vector {
	return []vector{
		{
			id: "P1", target: types.TgEventMsg, in: `PASSWORD=hunter2`,
			out: `PASSWORD=[REDACTED:env_assign]`, byRule: map[string]int{"env_assign": 1},
		},
		{
			id: "P2", target: types.TgEventMsg, in: `MY_API_KEY=abcdef1234567890`,
			out: `MY_API_KEY=[REDACTED:env_assign]`, byRule: map[string]int{"env_assign": 1},
		},
		{
			id: "P3", target: types.TgEventMsg, in: `{"token": "abc123xyz"}`,
			out: `{"token": [REDACTED:kv_secret_assign]}`, byRule: map[string]int{"kv_secret_assign": 1},
		},
		{
			id: "P4", target: types.TgHeader,
			in:  `Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sigpart`,
			out: `Authorization: [REDACTED:auth_header]`, byRule: map[string]int{"auth_header": 1},
		},
		{
			id: "P5", target: types.TgEventMsg, in: `Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sigpart`,
			out: `Bearer [REDACTED:bearer_token]`, byRule: map[string]int{"bearer_token": 1},
		},
		{
			id: "P6", target: types.TgEventMsg, in: `--api-key=abcdef123456`,
			out: `--api-key=[REDACTED:cli_flag_secret]`, byRule: map[string]int{"cli_flag_secret": 1},
		},
		{
			id: "P7", target: types.TgJournalTail,
			in:  "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAx3Zk\n9sQmT1xW\n-----END RSA PRIVATE KEY-----\n",
			out: "[REDACTED:private_key_block]\n", byRule: map[string]int{"private_key_block": 1},
		},
		{
			id: "P8", target: types.TgConfigSnapshot,
			in:  `secret_key: "aVeryLongBase64LookingValue12345678"`,
			out: `secret_key: [REDACTED:private_key_inline]`, byRule: map[string]int{"private_key_inline": 1},
		},
		{
			id: "P9", target: types.TgEventMsg,
			in:     `DATABASE_URL=postgres://app:s3cr3t@db.internal:5432/app`,
			out:    `DATABASE_URL=postgres://app:[REDACTED:conn_string_password]@db.internal:5432/app`,
			byRule: map[string]int{"conn_string_password": 1},
		},
		{
			id: "P10", target: types.TgEventMsg, in: `https://deploy:hunter2@registry.internal/v2/`,
			out:    `https://deploy:[REDACTED:url_basic_auth]@registry.internal/v2/`,
			byRule: map[string]int{"url_basic_auth": 1},
		},
		{
			id: "P11", target: types.TgEventMsg,
			in:     `http://a1b2c3d4e5f60718293a4b5c6d7e8f90:0123456789abcdef0123456789abcdef@hooks.example:7643/7`,
			out:    `http://a1b2c3d4e5f60718293a4b5c6d7e8f90:[REDACTED:dsn_secret]@hooks.example:7643/7`,
			byRule: map[string]int{"dsn_secret": 1},
		},
		{
			id: "P12", target: types.TgEventMsg,
			in:     `2026-09-16T09:14:03Z host app[1]: aws credential AKIAIOSFODNN7EXAMPLE rejected`,
			out:    `2026-09-16T09:14:03Z host app[1]: aws credential [REDACTED:cloud_key_shape] rejected`,
			byRule: map[string]int{"cloud_key_shape": 1},
		},
		{
			id: "P13", target: types.TgEventMsg, project: "1",
			in:     testHomePath + ":118",
			out:    `[REDACTED:path_home_root]/projects/trouble/internal/scrub/engine.go:118`,
			byRule: map[string]int{"path_home_root": 1},
		},
		{
			id: "P14", target: types.TgEventMsg, in: `Qw9zXk2Lm7Tp4Rv8Bn1Yh6Jd3Fg0Sa5Ce2Ui9Ol4Wq7`,
			out: `[REDACTED:entropy_token]`, byRule: map[string]int{"entropy_token": 1},
		},
	}
}

func negativeVectors() []vector {
	return []vector{
		{id: "N1", target: types.TgEventMsg, in: `{"event_id":"9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f"}`},
		{id: "N2", target: types.TgEventMsg, in: `registered project a1b2c3d4e5f60718293a4b5c6d7e8f90 for hooks.example`},
		{id: "N3", target: types.TgEventMsg, in: `build 9c1f0ab and 40-hex d1e8f0a9b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2`},
		{id: "N4", target: types.TgEventMsg, in: `ev_01J9Z6Q0M2X4T8V1K7B3N5R8WD`},
		{id: "N5", target: types.TgEventMsg, in: `POSTGRES_MAX_CONNECTIONS=20`},
		{id: "N6", target: types.TgEventMsg, in: `password reset required for user alice`},
		{id: "N7", target: types.TgEventMsg, in: `127.0.0.1:7643 and 192.168.1.14 and [::1]:7644`},
		{id: "N8", target: types.TgEventMsg, in: `[REDACTED:env_assign] and [SCRUB-TRUNCATED:4096]`},
		{id: "N9", target: types.TgEventMsg, in: `token bucket refilled at 200/s`},
		{id: "N10", target: types.TgEventMsg, in: `c3ab8ff13720e8ad9047dd39466b3c8974e592c2fa383d4a3960714caef0c4f2`},
		{id: "N11", target: types.TgConfigSnapshot, in: `verify_window = "10m"`},
		{id: "N12", target: types.TgEventMsg, in: `https://hooks.example:7643/api/1/envelope/`},
	}
}

func TestVectorsPositive(t *testing.T) {
	e := newTestEngine(t, "")
	for _, v := range positiveVectors() {
		pid := v.project
		if pid == "" {
			pid = "1"
		}
		got, res := scrubString(t, e, v.target, pid, v.in)
		if got != v.out {
			t.Errorf("%s: output = %q, want %q", v.id, got, v.out)
		}
		if !sameByRule(res.ByRule, v.byRule) {
			t.Errorf("%s: by_rule = %s, want %s", v.id, byRuleString(res.ByRule), byRuleString(v.byRule))
		}
		if res.Redactions != len(v.byRule) && res.Redactions == 0 {
			t.Errorf("%s: redactions = %d", v.id, res.Redactions)
		}
		if res.BytesIn != len(v.in) {
			t.Errorf("%s: bytes_in = %d, want %d", v.id, res.BytesIn, len(v.in))
		}
		if len(res.Value) == 0 {
			t.Errorf("%s: result value is empty", v.id)
		}
	}
}

func TestVectorsNegative(t *testing.T) {
	e := newTestEngine(t, "")
	for _, v := range negativeVectors() {
		pid := v.project
		if pid == "" {
			pid = "1"
		}
		got, res := scrubString(t, e, v.target, pid, v.in)
		if got != v.in {
			t.Errorf("%s: output changed to %q, want byte-identical input", v.id, got)
		}
		if res.Redactions != 0 || len(res.ByRule) != 0 {
			t.Errorf("%s: redactions = %d by_rule = %s, want 0", v.id, res.Redactions, byRuleString(res.ByRule))
		}
	}
	// the loopback/RFC1918 exemption of §3.3 is counted, not silent
	before := e.ExemptValues()
	for _, v := range negativeVectors() {
		if v.id != "N7" {
			continue
		}
		_, _ = scrubString(t, e, v.target, "2", v.in)
	}
	if got := e.ExemptValues() - before; got == 0 {
		t.Error("N7 matched three exempt addresses but exempt_values did not move")
	}
}

// TestVectorsFromTestdata checks the same vectors as loaded from
// testdata/vectors/vectors.txt: the file is the human-readable copy of the
// table above, and a drift between the two is a defect.
func TestVectorsFromTestdata(t *testing.T) {
	data, err := readTestdata("vectors/vectors.txt")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	e := newTestEngine(t, "")
	lines := strings.Split(strings.TrimSpace(data), "\n")
	seen := map[string]bool{}
	for _, ln := range lines {
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		parts := strings.SplitN(ln, "\t", 4)
		if len(parts) != 4 {
			t.Fatalf("bad vector line %q", ln)
		}
		id, in, want, rules := parts[0], parts[1], parts[2], parts[3]
		seen[id] = true
		got, res := scrubString(t, e, types.TgEventMsg, "1", unescape(in))
		if got != unescape(want) {
			t.Errorf("%s: output = %q, want %q", id, got, unescape(want))
		}
		if byRuleString(res.ByRule) != rules {
			t.Errorf("%s: by_rule = %s, want %s", id, byRuleString(res.ByRule), rules)
		}
	}
	for _, v := range positiveVectors() {
		if !seen[v.id] {
			t.Errorf("vector %s is missing from testdata/vectors/vectors.txt", v.id)
		}
	}
}

func unescape(s string) string {
	r := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\\`, `\`)
	return r.Replace(s)
}
