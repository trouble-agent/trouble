// Package scrub_test drives the scrubbing engine and the ledger writer together.
//
// The test lives in an external test package because internal/ledger imports
// internal/scrub: an in-package test would close an import cycle. It therefore
// uses only the exported engine surface (the §2 interface) plus the ledger's
// exported surface, which is exactly the public chain a caller sees.
package scrub_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/ledger"
	"github.com/totalwindupflightsystems/trouble/internal/scrub"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

const (
	testPubKeyA = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	testPubKeyB = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
)

func testProjects() []types.Project {
	return []types.Project{
		{ID: "1", Slug: "alpha", PublicKey: testPubKeyA, SecretKey: "00112233445566778899aabbccddeeff", Enabled: true},
		{ID: "2", Slug: "beta", PublicKey: testPubKeyB, Enabled: true},
	}
}

func newTestEngine(t *testing.T, cfg string) *scrub.Engine {
	t.Helper()
	e, err := scrub.New([]byte(cfg), testProjects())
	if err != nil {
		t.Fatalf("scrub.New: %v", err)
	}
	return e
}

func mustMkdirTemp(t *testing.T, prefix string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(wd, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

func byRuleString(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+itoa(m[k]))
	}
	return strings.Join(parts, ",")
}

// SPEC-02 §7 e2e_ledger_test.go: the "no unredacted secret reached the ledger"
// test.
//
// SPEC-04's sentinel and SPEC-12's lifecycle do not exist yet, so this test
// drives the real chain that exists today: the real scrubbing engine, the real
// ledger writer (group commit, rotation, canonical JSON) and real files on disk
// under one state root. Every payload is pushed through the scrubber exactly as
// §4 requires (parse → UTF-8 → scrub → sig → ledger) and then appended through
// ledger.Append with the boundary re-scan armed (assert_scrubbed), which is the
// persistence-boundary backstop of §3.4 point 3.
//
// 17 seeded secrets ride the 10 call-site shapes of §4 (SDK payload, collector
// journal tail, config snapshot, skill candidate, issue body, board row,
// research context, spool enqueue, satellite forward, fail-closed refusal).

type seededSecret struct {
	rule string
	// literal is the exact byte sequence that must never appear under the state
	// root. It is seeded into a payload that reaches disk through one path.
	literal string
	// id is the call site the secret rides.
	id string
}

func seededSecrets() []seededSecret {
	return []seededSecret{
		{"env_assign", "hunter2swordfish", "sdk payload (env dump)"},
		{"kv_secret_assign", "abc123xyz", "sdk payload (contexts)"},
		{"dsn_secret", "0123456789abcdef0123456789abcdef", "sdk payload (exception message)"},
		{"url_basic_auth", "regpassw0rd", "sdk payload (request url)"},
		{"conn_string_password", "s3cr3t", "sdk payload (stack)"},
		{"auth_header", "dXNlcjpwYXNzd29yZA==", "sdk payload (headers)"},
		{"private_key_block", "MIIEowIBAAKCAQEAx3Zk9sQmT1xW", "collector journal tail"},
		{"private_key_inline", "aVeryLongBase64LookingValue12345678", "config snapshot"},
		{"cli_flag_secret", "abcdef123456", "skill candidate play TOML"},
		{"bearer_token", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sigpart", "issue body"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhZG1pbiJ9.U0lHTkFUVVJF", "research context"},
		{"cloud_key_shape", "AKIAIOSFODNN7EXAMPLE", "board row brief"},
		{"entropy_token", "Qw9zXk2Lm7Tp4Rv8Bn1Yh6Jd3Fg0Sa5Ce2Ui9Ol4Wq7", "sdk payload (token field)"},
		{"pii_email_ip", "alice@example.com", "board row brief"},
		{"pii_identity_kv", "bobbysmith", "sdk payload (user)"},
		{"path_home_root", "appuser", "sdk payload (file path)"},
		{"dsn_any", "deploysecretvalue", "spool envelope (sentry-scheme uri)"},
	}
}

const e2ePubKey = testPubKeyA
const e2eEventID = "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f"

// testSecretKey is project 1's secret half (SPEC-02 §3.5: echoing it into a
// record is the config-snapshot trap).
const testSecretKey = "00112233445566778899aabbccddeeff"

func TestE2ENoUnredactedSecretReachedTheLedger(t *testing.T) {
	state := mustMkdirTemp(t, ".scrub-e2e-")
	eng := newTestEngine(t, "")
	actor := types.Actor{Kind: types.ActorDaemon, ID: "troubled", Version: "0.1.0"}
	origin := types.Origin{HostID: "7f3a91c2d4e5b607", Source: "sentinel:payment-worker"}

	l, err := ledger.Open(context.Background(), ledger.Options{
		Root:           filepath.Join(state, "ledger"),
		Rotation:       ledger.DefaultRotationPolicy(),
		Retention:      ledger.DefaultRetentionPolicy(),
		Index:          ledger.DefaultIndexOptions(),
		Writer:         actor,
		MaxSchema:      1,
		Now:            time.Now,
		HostID:         "7f3a91c2d4e5b607",
		Zone:           "loopback",
		AssertScrubbed: true,
		ScrubVerify: func(ctx context.Context, b []byte) error {
			if err := eng.Verify(ctx, b); err != nil {
				t.Logf("REFUSED LINE: %s", b)
				return err
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	ctx := context.Background()
	firedRules := map[string]bool{}
	secrets := seededSecrets()
	totalRedactions := 0

	// push scrubs a record's payload through the real scrubber and then appends
	// it through the real ledger writer.
	push := func(kind types.RecordKind, sig string, projectID string, payload map[string]any, targets map[string]types.ScrubTarget) types.Record {
		t.Helper()
		rec := types.Record{Kind: kind, Sig: sig, Origin: origin, Actor: actor, Payload: payload}
		res, err := eng.ScrubRecordFor(ctx, projectID, &rec, targets)
		if err != nil {
			t.Fatalf("ScrubRecordFor(%s): %v", kind, err)
		}
		for name, n := range res.ByRule {
			if n > 0 {
				firedRules[name] = true
			}
		}
		totalRedactions += res.Redactions
		got, err := l.Append(ctx, types.RecordDraft{
			Kind: kind, Sig: sig, Origin: origin, Actor: actor,
			Redactions: rec.Redactions, Payload: rec.Payload,
		})
		if err != nil {
			t.Fatalf("ledger.Append(%s): %v", kind, err)
		}
		return got
	}

	// 1. sentinel envelope: exception message with a DSN (pubkey + secret half),
	//    a request url with basic auth, an env dump and a stack with a conn
	//    string — one ScrubFields call, four targets (§2).
	sdk := map[string]types.ScrubTarget{
		"message": types.TgEventMsg, "stack": types.TgStack,
		"headers": types.TgHeader, "env": types.TgEnv,
	}
	rec := push(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "1", map[string]any{
		"event_id": e2eEventID,
		"message":  "failed to send envelope to http://" + e2ePubKey + ":" + secrets[2].literal + "@hooks.example:7643/7",
		"stack":    "Traceback:\n  File \"/home/" + secrets[15].literal + "/src/trouble/x.go\", line 12\n  postgres://app:" + secrets[4].literal + "@db.internal:5432/app\n  token " + secrets[12].literal,
		"headers":  "accept: application/json\nAuthorization: Basic " + secrets[5].literal + "\nx-sentry-auth: sentry_key=" + e2ePubKey,
		"env":      "PATH=/usr/bin\nPASSWORD=" + secrets[0].literal + "\nHOME=/home/" + secrets[15].literal,
		"request":  map[string]any{"url": "https://deploy:" + secrets[3].literal + "@registry.internal/v2/"},
		// the SDK serialises its contexts and its user object as JSON strings, so a
		// sensitive name travels with its value: the scrubber is name-driven, and a
		// bare value under a sensitive JSON key is the write-boundary re-scan's job
		// (fail closed), not the scrubber's
		"contexts_json": `{"token":"` + secrets[1].literal + `"}`,
		"user_json":     `{"user":"` + secrets[14].literal + `"}`,
	}, func() map[string]types.ScrubTarget {
		m := map[string]types.ScrubTarget{}
		for k, v := range sdk {
			m[k] = v
		}
		m["request.url"] = types.TgEventMsg
		m["contexts_json"] = types.TgEventMsg
		m["user_json"] = types.TgEnv
		return m
	}())
	if rec.Redactions == 0 {
		t.Error("the SDK payload recorded 0 redactions")
	}

	// 2. collector journal tail carrying a private key block
	push(types.KEvent, "journald:sha256v1:aa11bb22cc33dd44", "1", map[string]any{
		"op":      "collector",
		"message": "-----BEGIN RSA PRIVATE KEY-----\n" + secrets[6].literal + "\n-----END RSA PRIVATE KEY-----\n",
	}, map[string]types.ScrubTarget{"message": types.TgJournalTail})

	// 3. config snapshot: a resolved-config dump that echoes Project.SecretKey
	//    and (in the TOML form rule 2 owns) an inline private key value
	push(types.KConfig, "", "1", map[string]any{
		"key": "sentinel",
		"value": "project = \"1\"\npublic_key = \"" + e2ePubKey + "\"\nsecret_key = \"" + testSecretKey + "\"\n" +
			"secret_key: " + secrets[7].literal + "\n",
	}, map[string]types.ScrubTarget{"value": types.TgDSN})

	// 4. skill candidate whose play TOML embeds a CLI flag secret
	push(types.KSkill, "", "1", map[string]any{
		"name": "restart_worker",
		"play": "[[step]]\ncmd = \"/usr/bin/worker --api-key=" + secrets[8].literal + "\"\n",
	}, map[string]types.ScrubTarget{"play": types.TgSkill})

	// 5. issue body with an auth header line AND a bare bearer token
	push(types.KIssue, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "1", map[string]any{
		"title": "queue wedge: pool exhausted",
		"body": "repro:\nAuthorization: Basic " + secrets[5].literal + "\n" +
			"upstream call rejected: Bearer " + secrets[9].literal + " is expired\n",
	}, map[string]types.ScrubTarget{"title": types.TgIssue, "body": types.TgIssue})

	// 6. board row whose brief embeds an email and a public IP
	push(types.KFlow, "", "1", map[string]any{
		"brief":  "owner " + secrets[13].literal + " should look at this from 203.0.113.9",
		"reason": "aws credential " + secrets[11].literal + " rotated",
	}, map[string]types.ScrubTarget{"brief": types.TgBoard, "reason": types.TgBoard})

	// 7. research context with a JWT
	push(types.KResearch, "", "1", map[string]any{
		"question": "why does the worker hold a stale credential " + secrets[10].literal + "?",
	}, map[string]types.ScrubTarget{"question": types.TgStack})

	// 8. spool enqueue: the serialized ForwardEnvelope is scrubbed before the
	//    file write (§4), so spool bytes on disk are always scrubbed.
	env := types.ForwardEnvelope{
		ProtocolVersion: 1,
		IdempotencyKey:  "sentinel:sha256v1:9f2c1d3e4b5a6c7d|1|7f3a91c2d4e5b607",
		HostID:          "7f3a91c2d4e5b607",
		HubID:           "c0ffee1234567890",
		Origin:          origin,
		Records: []types.Record{{
			Kind: types.KEvent, Sig: "sentinel:sha256v1:9f2c1d3e4b5a6c7d",
			Origin: origin, Actor: actor,
			Payload: map[string]any{
				"message": "spooled envelope with sentry://deployuser:" + secrets[16].literal + "@hooks.example/1",
				"token":   secrets[12].literal,
			},
		}},
		Ack: 41207,
	}
	envRes, err := eng.ScrubEnvelope(ctx, &env, map[string]types.ScrubTarget{
		"message": types.TgSpool, "token": types.TgSpool,
	})
	if err != nil {
		t.Fatalf("ScrubEnvelope: %v", err)
	}
	for name, n := range envRes.ByRule {
		if n > 0 {
			firedRules[name] = true
		}
	}
	totalRedactions += envRes.Redactions
	spoolBytes, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Verify(ctx, spoolBytes); err != nil {
		t.Errorf("the scrubbed spool envelope does not pass the boundary re-scan: %v", err)
	}
	writeFileT(t, filepath.Join(state, "spool", "pending", "01J9Z6Q0M2X4T8V1K7B3N5R8WD.json"), string(spoolBytes))

	// also record the envelope's records in the ledger (the hub side)
	for i := range env.Records {
		_, err := l.Append(ctx, types.RecordDraft{
			Kind: env.Records[i].Kind, Sig: env.Records[i].Sig, Origin: origin, Actor: actor,
			Redactions: env.Records[i].Redactions, Payload: env.Records[i].Payload,
		})
		if err != nil {
			t.Fatalf("hub append %d: %v", i, err)
		}
	}

	// 9. skills-local: a pulled artifact is written under the state root only
	//    after the scrubber has seen it.
	skillBytes, err := json.Marshal(map[string]any{
		"name": "restart_worker", "play": "cmd = \"--api-key=" + secrets[8].literal + "\"",
	})
	if err != nil {
		t.Fatal(err)
	}
	scrubbedSkill, _, err := eng.ScrubBytes(ctx, types.TgSkill, "1", skillBytes)
	if err != nil {
		t.Fatalf("scrub skill artifact: %v", err)
	}
	writeFileT(t, filepath.Join(state, "skills-local", "restart_worker", "SKILL.json"), string(scrubbedSkill))

	// 10. fail-closed: a payload whose rule times out is NOT persisted — the
	//     caller writes metadata only plus a gap record with cause scrub_failed.
	// a 1 ns rule budget makes any rule evaluation exceed rule_timeout, which the
	// engine treats as SCRUB-005 and fails closed (§3.8)
	timingEng := newTestEngine(t, "[scrub]\nrule_timeout = \"1ns\"\n")
	failPayload := "PASSWORD=" + secrets[0].literal + " and a timeout"
	_, _, serr := timingEng.ScrubBytes(ctx, types.TgEventMsg, "1", []byte(failPayload))
	if serr == nil {
		t.Fatal("the timing-out engine did not fail closed")
	}
	if scrub.CodeOf(serr) != types.CodeScrub005 {
		t.Fatalf("code = %s, want %s (%v)", scrub.CodeOf(serr), types.CodeScrub005, serr)
	}
	gapRec := types.Record{Kind: types.KGap, Origin: origin, Actor: actor, Payload: map[string]any{
		"sensor": "sentinel", "scope": "ingest", "est_lost": 1,
		"cause": "scrub_failed", "error_code": string(types.CodeScrub005),
	}}
	if _, err := eng.ScrubRecordFor(ctx, "1", &gapRec, map[string]types.ScrubTarget{"cause": types.TgEventMsg}); err != nil {
		t.Fatalf("gap record scrub: %v", err)
	}
	if _, err := l.Append(ctx, types.RecordDraft{
		Kind: types.KGap, Origin: origin, Actor: actor, Payload: gapRec.Payload,
	}); err != nil {
		t.Fatalf("gap record append: %v", err)
	}
	// the same for the boundary class: a record refused by the write-boundary
	// re-scan leaves a metadata-only lifecycle note (never the offending bytes)
	if _, err := l.Append(ctx, types.RecordDraft{
		Kind: types.KLifecycle, Origin: origin, Actor: actor,
		Payload: map[string]any{
			"event":      "record_refused",
			"cause":      "boundary_rescan_hit",
			"error_code": string(types.CodeScrub008),
		},
	}); err != nil {
		t.Fatalf("refusal note append: %v", err)
	}

	if err := l.Close(ctx); err != nil {
		t.Fatalf("ledger.Close: %v", err)
	}
	if totalRedactions == 0 {
		t.Fatal("the run recorded 0 redactions")
	}

	// --- assertions -------------------------------------------------------

	// 1. no seeded secret appears anywhere under the state root
	for _, s := range secrets {
		if hits := filesContaining(t, state, s.literal); len(hits) > 0 {
			t.Errorf("SECRET LEAK: %q (%s, rule %s) reached %v", s.literal, s.id, s.rule, hits)
		}
	}
	// the DSN secret of the config snapshot is the same trap
	if hits := filesContaining(t, state, testSecretKey); len(hits) > 0 {
		t.Errorf("SECRET LEAK: Project.SecretKey reached %v", hits)
	}

	// 2. the allowed credential and the identifier must be present
	if hits := filesContaining(t, state, e2ePubKey); len(hits) == 0 {
		t.Error("the DSN public key is absent from the state root: the test is proving the wrong thing")
	}
	if hits := filesContaining(t, state, e2eEventID); len(hits) == 0 {
		t.Error("the Sentry event id is absent from the state root: the test is proving the wrong thing")
	}

	// 3. every ledger file that carried a scrubbed payload carries markers
	// every record file (the day files) carries markers; HEAD/LOCK and the
	// quarantine dir are ledger metadata, not record storage
	var ledgerFiles []string
	for _, f := range filesUnder(t, filepath.Join(state, "ledger")) {
		if filepath.Ext(f) == ".jsonl" {
			ledgerFiles = append(ledgerFiles, f)
		}
	}
	if len(ledgerFiles) == 0 {
		t.Fatal("no ledger record file was written")
	}
	var markerTotal int
	for _, f := range ledgerFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n := strings.Count(string(b), "[REDACTED:")
		if n == 0 {
			t.Errorf("ledger file %s carries no marker", f)
		}
		markerTotal += n
	}
	if markerTotal == 0 {
		t.Error("no [REDACTED: marker was persisted")
	}
	wantRules := []string{
		"env_assign", "kv_secret_assign", "dsn_secret", "dsn_any", "conn_string_password",
		"url_basic_auth", "cli_flag_secret", "auth_header", "private_key_block",
		"private_key_inline", "bearer_token", "jwt", "cloud_key_shape",
		"entropy_token", "pii_email_ip", "pii_identity_kv", "path_home_root",
	}
	for _, r := range wantRules {
		if !firedRules[r] {
			t.Errorf("rule %s never fired in the run", r)
		}
	}
	t.Logf("fired %d rules, %d redactions, %d ledger files, %d markers",
		len(firedRules), totalRedactions, len(ledgerFiles), markerTotal)

	// 4. the read path: a second, independent check that the persisted bytes are
	//    scrubbed, not merely that the seed strings are absent
	for _, f := range ledgerFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if line == "" {
				continue
			}
			if err := eng.Verify(ctx, []byte(line)); err != nil {
				t.Errorf("%s:%d does not pass the boundary re-scan: %v", filepath.Base(f), i+1, err)
			}
		}
	}

	// 5. the fail-closed path: the refused payload left no trace
	if hits := filesContaining(t, state, "and a timeout"); len(hits) > 0 {
		t.Errorf("a fail-closed payload reached %v", hits)
	}
	if hits := filesContaining(t, state, "scrub_failed"); len(hits) == 0 {
		t.Error("no gap record with cause scrub_failed was persisted")
	}
}

// filesContaining is the grep -rF equivalent: every file under root that
// contains needle, as absolute paths.
func filesContaining(t *testing.T, root, needle string) []string {
	t.Helper()
	var out []string
	for _, f := range filesUnder(t, root) {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), needle) {
			out = append(out, f)
		}
	}
	return out
}

func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
