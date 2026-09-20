package scrub

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// SPEC-02 §7 fixedpoint_test.go: for each of the 18 markers
// Scrub(Scrub(x)) == Scrub(x) and Redactions(second) == 0, plus a property test
// over 10,000 generated payloads. Idempotence is AC-22's mechanism: it is what
// lets a satellite scrub locally and the hub re-scrub the same record without
// changing the sig.

// markerContexts puts every one of the 18 markers in a context that would
// otherwise trigger its own rule.
func markerContexts(pubkey string) []struct {
	id     string
	target types.ScrubTarget
	in     string
} {
	return []struct {
		id     string
		target types.ScrubTarget
		in     string
	}{
		{"private_key_block", types.TgJournalTail, "[REDACTED:private_key_block]\nnext line"},
		{"private_key_inline", types.TgConfigSnapshot, `secret_key: [REDACTED:private_key_inline]`},
		{"dsn_secret", types.TgEventMsg, "http://" + pubkey + ":[REDACTED:dsn_secret]@hooks.example:7643/7"},
		{"dsn_any", types.TgEventMsg, "https://[REDACTED:dsn_any]@hooks.example:7643/7"},
		{"conn_string_password", types.TgEventMsg, "postgres://app:[REDACTED:conn_string_password]@db.internal:5432/app"},
		{"url_basic_auth", types.TgEventMsg, "https://deploy:[REDACTED:url_basic_auth]@registry.internal/v2/"},
		{"env_assign", types.TgEventMsg, "PASSWORD=[REDACTED:env_assign]"},
		{"kv_secret_assign", types.TgEventMsg, `{"token": [REDACTED:kv_secret_assign]}`},
		{"cli_flag_secret", types.TgEventMsg, "--api-key=[REDACTED:cli_flag_secret]"},
		{"auth_header", types.TgHeader, "Authorization: [REDACTED:auth_header]"},
		{"bearer_token", types.TgEventMsg, "Bearer [REDACTED:bearer_token]"},
		{"jwt", types.TgEventMsg, "login failed for [REDACTED:jwt]"},
		{"cloud_key_shape", types.TgEventMsg, "aws key [REDACTED:cloud_key_shape] rejected"},
		{"entropy_token", types.TgEventMsg, "[REDACTED:entropy_token]"},
		{"pii_email_ip", types.TgEventMsg, "mail [REDACTED:pii_email_ip] bounced"},
		{"pii_identity_kv", types.TgEventMsg, `{"user": [REDACTED:pii_identity_kv]}`},
		{"path_home_root", types.TgEventMsg, "[REDACTED:path_home_root]/projects/trouble/internal/scrub/engine.go"},
		{"path_disclosure", types.TgEventMsg, "open [REDACTED:path_disclosure] failed"},
		{"truncated", types.TgEventMsg, "line one\nline two [SCRUB-TRUNCATED:4096]"},
	}
}

func TestFixedPointMarkers(t *testing.T) {
	for _, mode := range []string{"", "[scrub]\npath_mode = \"redact\"\n"} {
		e := newTestEngine(t, mode)
		for _, c := range markerContexts(testPubKeyA) {
			once, res1 := scrubString(t, e, c.target, "1", c.in)
			twice, res2 := scrubString(t, e, c.target, "1", once)
			if twice != once {
				t.Errorf("path_mode=%q %s: second pass changed the output\n  1st %q\n  2nd %q", mode, c.id, once, twice)
			}
			if res2.Redactions != 0 || len(res2.ByRule) != 0 {
				t.Errorf("path_mode=%q %s: second pass reported %d redactions (%s), want 0",
					mode, c.id, res2.Redactions, byRuleString(res2.ByRule))
			}
			if len(once) == 0 && len(c.in) > 0 {
				t.Errorf("%s: empty output for %q", c.id, c.in)
			}
			_ = res1
		}
		// and the outputs of the positive vectors are fixed points too
		for _, v := range positiveVectors() {
			twice, res := scrubString(t, e, v.target, "1", v.out)
			if twice != v.out {
				t.Errorf("vector %s output is not a fixed point: %q → %q", v.id, v.out, twice)
			}
			if res.Redactions != 0 {
				t.Errorf("vector %s output reported %d redactions on re-scrub (%s)",
					v.id, res.Redactions, byRuleString(res.ByRule))
			}
		}
	}
}

// TestFixedPointProperty scrubs 10,000 generated payloads twice: the second pass
// must be a no-op with Redactions == 0.
func TestFixedPointProperty(t *testing.T) {
	e := newTestEngine(t, "")
	targets := []types.ScrubTarget{types.TgEventMsg, types.TgStack, types.TgJournalTail, types.TgHeader}
	payloads := generatePayloads(10000)
	changed := 0
	for i, p := range payloads {
		tg := targets[i%len(targets)]
		once, _, err := e.ScrubString(context.Background(), tg, "1", p)
		if err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		twice, res, err := e.ScrubString(context.Background(), tg, "1", once)
		if err != nil {
			t.Fatalf("payload %d second pass: %v", i, err)
		}
		if twice != once {
			changed++
			if changed < 5 {
				t.Errorf("payload %d is not a fixed point:\n  in   %q\n  1st  %q\n  2nd  %q", i, p, once, twice)
			}
		}
		if res.Redactions != 0 {
			t.Errorf("payload %d: second pass reported %d redactions (%s)", i, res.Redactions, byRuleString(res.ByRule))
		}
	}
	if changed != 0 {
		t.Errorf("%d of %d payloads were not fixed points", changed, len(payloads))
	}
}

// generatePayloads builds a deterministic corpus of secret-shaped payloads.
func generatePayloads(n int) []string {
	rng := rand.New(rand.NewSource(20260916))
	shapes := []string{
		`PASSWORD=%s`,
		`{"token": "%s"}`,
		`Authorization: Bearer %s`,
		`--api-key=%s`,
		`http://a1b2c3d4e5f60718293a4b5c6d7e8f90:%s@hooks.example:7643/7`,
		`postgres://app:%s@db.internal:5432/app`,
		`https://deploy:%s@registry.internal/v2/`,
		`secret_key: "%s"`,
		"-----BEGIN RSA PRIVATE KEY-----\n%s\n-----END RSA PRIVATE KEY-----\n",
		`user bob <bob@%s.example> from 203.0.113.7`,
		`{"user": "%s", "ip_address": "10.0.0.%d"}`,
		`open /home/user/%s/config.toml failed`,
		`aws AKIAIOSFODNN7EXAMPLE and gh token ghp_%s`,
		`file %s and a plain sentence with no secret at all`,
	}
	words := []string{
		"hunter2", "abc123xyz", "aVeryLongBase64LookingValue12345678",
		"Qw9zXk2Lm7Tp4Rv8Bn1Yh6Jd3Fg0Sa5Ce2Ui9Ol4Wq7",
		"[REDACTED:env_assign]", "[REDACTED:kv_secret_assign]", "[SCRUB-TRUNCATED:4096]",
		"0123456789abcdef0123456789abcdef", "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f",
		"d1e8f0a9b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2", "src", "engine.go",
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		shape := shapes[rng.Intn(len(shapes))]
		w := words[rng.Intn(len(words))]
		switch strings.Count(shape, "%") {
		case 0:
			out = append(out, shape)
		case 1:
			out = append(out, fmt.Sprintf(shape, w))
		case 2:
			out = append(out, fmt.Sprintf(shape, w, rng.Intn(256)))
		}
	}
	return out
}
