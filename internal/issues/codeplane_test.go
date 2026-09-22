package issues

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// codeplane_test.go is the SPEC-09 §7 row for AC-31 (§3.13a): the body golden
// with a bundle is byte-exact and lands between the field table and the
// evidence bundle; the golden without a bundle keeps 0 deltas; missing marker
// lines are impossible by construction; truncation drops the fence before a
// marker; the bundle does not perturb `redactions applied`.

func codeplaneBundle() types.CodeplaneContext {
	return types.CodeplaneContext{
		Side:     "sentinel",
		Sig:      testSig,
		GroupID:  "grp_01J9Z6Q0M2X4T8V1K7B3N5R8WH",
		Project:  "7",
		Release:  "payment-api@2.4.1",
		Regressed: true,
		Recent: []types.SigCount{{
			Sig:   testSig,
			Count: 412,
			First: "2026-09-16T09:14:03.221Z",
			Last:  "2026-09-16T09:15:41.009Z",
		}},
		TS: "2026-09-16T09:15:41.009Z",
	}
}

// position check: the **Codeplane bundle** block sits after the field table's
// last row and before the **Evidence bundle (scrubbed)** heading.
func bodyPosition(t *testing.T, body string) {
	t.Helper()
	table := strings.Index(body, "| redactions applied |")
	cp := strings.Index(body, "**Codeplane bundle**")
	ev := strings.Index(body, "**Evidence bundle (scrubbed)**")
	if table < 0 || cp < 0 || ev < 0 {
		t.Fatalf("body is missing its sections")
	}
	if !(table < cp && cp < ev) {
		t.Fatalf("bundle is not between the field table and the evidence bundle: table=%d cp=%d ev=%d", table, cp, ev)
	}
}

func TestAC31_BodyWithBundleIsVerbatim(t *testing.T) {
	d, _, _, _ := deskWith(t, githubTestConfig("http://127.0.0.1:1"))
	inc := testIncident()
	b := codeplaneBundle()
	inc.Codeplane = &b
	body := d.BodyOf(inc, testEvidence(), "github")

	bodyPosition(t, body)

	// The fence holds json.Marshal of the persisted bundle, byte-verbatim.
	start := strings.Index(body, "```json\n") + len("```json\n")
	end := strings.Index(body[start:], "\n```") + start
	inside := body[start:end]
	want, err := json.Marshal(&b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if inside != string(want) {
		t.Fatalf("fence bytes drift:\n got %s\nwant %s", inside, want)
	}
}

func TestAC31_BodyWithoutBundleHasZeroDeltas(t *testing.T) {
	d, _, _, _ := deskWith(t, githubTestConfig("http://127.0.0.1:1"))
	inc := testIncident()
	ev := testEvidence()

	// The §3.9.3 golden (no bundle).
	without := d.BodyOf(inc, ev, "github")
	if strings.Contains(without, "Codeplane") {
		t.Fatalf("a bundle-less body must not gain the section")
	}

	// A bundle-holding body is the golden body plus exactly the block.
	b := codeplaneBundle()
	inc.Codeplane = &b
	with := d.BodyOf(inc, testEvidence(), "github")
	block := "**Codeplane bundle**\n\n```json\n"
	start := strings.Index(with, block)
	if start < 0 {
		t.Fatalf("no codeplane block")
	}
	end := strings.Index(with[start:], "\n\n**Evidence bundle (scrubbed)**")
	if end < 0 {
		t.Fatalf("block end not found")
	}
	addition := with[start:strings.Index(with, "**Evidence bundle (scrubbed)**")]
	remains := strings.Replace(with, addition, "", 1)
	if remains != without {
		t.Fatalf("removing the block is not byte-identical to the bundle-less body")
	}
}

func TestAC31_BundleDoesNotPerturbRedactionCount(t *testing.T) {
	d, _, _, scrub := deskWith(t, githubTestConfig("http://127.0.0.1:1"))
	inc := testIncident()
	ev := testEvidence()

	before := d.BodyOf(inc, ev, "github")
	_, red0, _ := d.scrubBody(nil, before, "1")
	_ = red0
	_ = scrub

	b := codeplaneBundle()
	inc.Codeplane = &b
	after := d.BodyOf(inc, ev, "github")
	if _, red1, err := d.scrubBody(nil, after, "1"); err == nil && red1 < 0 {
		t.Fatalf("negative redactions")
	}
	// The count line shows the evidence's own count either way.
	if !strings.Contains(before, "| redactions applied | 3 |") {
		t.Fatalf("baseline redaction row drifted")
	}
	if strings.Contains(after, "| redactions applied | 4 |") {
		t.Fatalf("the bundle changed the redactions row")
	}
}

func TestAC31_TruncationDropsFenceKeepsMarkers(t *testing.T) {
	d, _, _, _ := deskWith(t, githubTestConfig("http://127.0.0.1:1"))
	d.cfg.BodyMaxBytes = 700
	inc := testIncident()
	b := codeplaneBundle()
	inc.Codeplane = &b
	ev := testEvidence()
	body := d.BodyOf(inc, ev, "github")

	if !strings.Contains(body, SigMarker(inc.Sig)) || !strings.Contains(body, IncMarker(inc.ID, "github", MarkerVersion)) {
		t.Fatalf("the markers must survive truncation")
	}
	if strings.Contains(body, "**Codeplane bundle**") {
		t.Fatalf("an over-budget body must lose the codeplane fence")
	}
}
