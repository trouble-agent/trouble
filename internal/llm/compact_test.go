package llm

import (
	"context"
	"strings"
	"testing"
)

// The CONTEXT-COMPACTION HOOK (SPEC-05 §3.7a): over budget triggers a capped,
// single-shot, map-style summarisation pass whose in/out token counts are recorded;
// a pass that cannot be capped refuses. There is no path that returns a shorter
// context without an accounting.

// compactingConfig is a config whose compaction hook is on and cheap to drive.
func compactingConfig(urls ...string) Config {
	cfg := testConfig(urls...)
	cfg.MaxTokens = 256
	cfg.Compact = CompactConfig{
		Enabled:      true,
		BudgetTokens: 100,
		ChunkTokens:  40,
		MaxChunks:    8,
		MaxTokens:    128,
	}
	return cfg
}

func TestCompact_UnderBudgetTouchesNothing(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("ok", 1, 1)})
	client := mustClient(t, compactingConfig(urls[0]))

	chunks := []string{"a small context", "with two chunks"}
	got, acct, err := client.MaybeCompact(context.Background(), chunks)
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if acct.Applied {
		t.Errorf("applied = true for a context inside the budget: %+v", acct)
	}
	if rec.count() != 0 {
		t.Errorf("HTTP requests = %d, want 0: an in-budget context sends no summarisation call", rec.count())
	}
	if len(got) != len(chunks) || got[0] != chunks[0] || got[1] != chunks[1] {
		t.Errorf("context = %v, want it byte-identical", got)
	}
	if acct.InTokens != EstimateChunks(chunks) {
		t.Errorf("in_tokens = %d, want the measured %d", acct.InTokens, EstimateChunks(chunks))
	}
	if acct.OutTokens != 0 || acct.Chunks != 0 {
		t.Errorf("accounting = %+v, want zero out/chunks when nothing was compacted", acct)
	}
}

func TestCompact_OverBudgetRunsOneSingleShotCallPerChunk(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("SUMMARY", 30, 5)})
	client := mustClient(t, compactingConfig(urls[0]))

	// 8 chunks x 400 bytes ≈ 800 tokens, over the 100-token budget; 40-token groups
	// make each 400-byte chunk its own group.
	chunks := make([]string, 8)
	for i := range chunks {
		chunks[i] = "fragment-" + itoa(i) + " " + strings.Repeat("y", 390)
	}
	got, acct, err := client.MaybeCompact(context.Background(), chunks)
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if !acct.Applied {
		t.Fatalf("applied = false for an over-budget context: %+v", acct)
	}
	if acct.Chunks != 8 {
		t.Errorf("chunks = %d, want 8 (one group per oversize fragment)", acct.Chunks)
	}
	if rec.count() != acct.Chunks {
		t.Errorf("HTTP requests = %d, want exactly one summarisation call per group (%d) — single-shot, no retries",
			rec.count(), acct.Chunks)
	}
	if acct.InTokens != EstimateChunks(chunks) || acct.InTokens <= 100 {
		t.Errorf("in_tokens = %d, want the measured over-budget count (%d)", acct.InTokens, EstimateChunks(chunks))
	}
	if acct.OutTokens == 0 {
		t.Errorf("out_tokens = 0, want the summaries' token count recorded")
	}
	if acct.MaxChunks != 8 {
		t.Errorf("max_chunks = %d, want the configured cap recorded with the accounting", acct.MaxChunks)
	}
	if acct.Candidate != "c1" || acct.Model != "test-model-c1" {
		t.Errorf("compaction candidate = %q/%q, want the serving chain entry recorded", acct.Candidate, acct.Model)
	}
	if len(got) != 8 {
		t.Fatalf("summaries = %d, want one per group", len(got))
	}
	for i, s := range got {
		if s != "SUMMARY" {
			t.Fatalf("summary %d = %q, want the upstream's summary", i, s)
		}
	}
	// Each call must carry ITS OWN fragment: a summarisation that saw the wrong
	// group would silently rewrite the context.
	reqs := rec.all()
	for i, r := range reqs {
		msg, _ := r.Body["messages"].([]any)
		user, _ := msg[0].(map[string]any)
		content, _ := user["content"].(string)
		if !strings.Contains(content, "fragment-"+itoa(i)) {
			t.Errorf("summarisation call %d did not carry fragment-%d; content=%q", i, i, truncate(content, 120))
		}
		if !strings.Contains(content, "of 8") {
			t.Errorf("summarisation call %d does not name the group count: %q", i, truncate(content, 120))
		}
		if mt, _ := r.Body["max_tokens"].(float64); int(mt) != 128 {
			t.Errorf("summarisation call %d max_tokens = %v, want the compact cap 128", i, r.Body["max_tokens"])
		}
		if stream, ok := r.Body["stream"].(bool); !ok || stream {
			t.Errorf("summarisation call %d did not pin stream=false", i)
		}
	}
}

func TestCompact_TooManyGroupsRefusesInsteadOfTruncating(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("SUMMARY", 1, 1)})
	cfg := compactingConfig(urls[0])
	cfg.Compact.MaxChunks = 2
	client := mustClient(t, cfg)

	chunks := make([]string, 6)
	for i := range chunks {
		chunks[i] = strings.Repeat("z", 400) // one group each → 6 groups > 2
	}
	got, acct, err := client.MaybeCompact(context.Background(), chunks)
	if err == nil {
		t.Fatal("MaybeCompact compacted a context that needs more groups than max_chunks; it must refuse instead of dropping content")
	}
	if got := ClassOf(err); got != ClassCompaction {
		t.Errorf("class = %q, want %q", got, ClassCompaction)
	}
	if !strings.Contains(err.Error(), "never truncated") {
		t.Errorf("the refusal does not state that the context is never truncated: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("context = %v, want nil: a refused pass returns nothing, not a shortened context", got)
	}
	if acct.Applied {
		t.Errorf("applied = true on a refused pass: %+v", acct)
	}
	if rec.count() != 0 {
		t.Errorf("HTTP requests = %d, want 0: the pass refuses before spending a call", rec.count())
	}
	if acct.InTokens == 0 {
		t.Error("in_tokens = 0, want the measured context size recorded even on a refusal")
	}
}

func TestCompact_DisabledRefusesAnOverBudgetContext(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("SUMMARY", 1, 1)})
	cfg := compactingConfig(urls[0])
	cfg.Compact.Enabled = false
	client := mustClient(t, cfg)

	chunks := []string{strings.Repeat("q", 2000)}
	if _, _, err := client.MaybeCompact(context.Background(), chunks); err == nil {
		t.Fatal("MaybeCompact accepted an over-budget context with compaction disabled")
	} else if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("refusal = %v, want it to name the disabled hook", err)
	}
	if rec.count() != 0 {
		t.Errorf("HTTP requests = %d, want 0", rec.count())
	}
}

func TestCompact_SummarisationFailureRefusesTheStage(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 500, ctype: "application/json", body: `{"error":"boom"}`})
	client := mustClient(t, compactingConfig(urls[0]))

	chunks := []string{strings.Repeat("w", 800)}
	got, _, err := client.MaybeCompact(context.Background(), chunks)
	if err == nil {
		t.Fatal("MaybeCompact reported success after the summarisation call failed")
	}
	if got := ClassOf(err); got != ClassCompaction {
		t.Errorf("class = %q, want %q", got, ClassCompaction)
	}
	if len(got) != 0 {
		t.Errorf("context = %v, want nil (no partial summarisation is handed on)", got)
	}
}

func TestCompact_CompleteSendsSummariesNotTheRawContext(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("SUMMARY", 30, 5)})
	client := mustClient(t, compactingConfig(urls[0]))

	chunks := []string{strings.Repeat("r", 400), strings.Repeat("s", 400)}
	resp, err := client.Complete(context.Background(), Request{Prompt: "fix it", Context: chunks})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !resp.Compaction.Applied {
		t.Fatalf("compaction accounting = %+v, want applied", resp.Compaction)
	}
	// The last request is the stage completion; it must carry the summaries.
	last := rec.all()[rec.count()-1]
	msg, _ := last.Body["messages"].([]any)
	user, _ := msg[0].(map[string]any)
	content, _ := user["content"].(string)
	if !strings.Contains(content, "fix it") {
		t.Errorf("the stage prompt is missing: %q", truncate(content, 80))
	}
	if !strings.Contains(content, "SUMMARY") {
		t.Errorf("the stage request does not carry the summaries: %q", truncate(content, 200))
	}
	if strings.Contains(content, strings.Repeat("r", 100)) {
		t.Errorf("the stage request still carries the raw over-budget context: %q", truncate(content, 200))
	}
	if resp.Compaction.InTokens <= 0 || resp.Compaction.OutTokens <= 0 {
		t.Errorf("token accounting = %+v, want both counts recorded", resp.Compaction)
	}
}

func TestCompact_AccountingIsPresentEvenWhenNothingWasCompacted(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("ok", 1, 1)})
	client := mustClient(t, compactingConfig(urls[0]))

	resp, err := client.Complete(context.Background(), Request{Prompt: "x", Context: []string{"tiny"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Compaction.Applied {
		t.Errorf("applied = true for an in-budget context: %+v", resp.Compaction)
	}
	if resp.Compaction.InTokens != EstimateTokens("tiny") {
		t.Errorf("in_tokens = %d, want the measured %d", resp.Compaction.InTokens, EstimateTokens("tiny"))
	}
	if resp.Compaction.MaxChunks != 8 {
		t.Errorf("max_chunks = %d, want the configured cap recorded with every accounting", resp.Compaction.MaxChunks)
	}
}

func TestCompact_GroupingKeepsEveryChunkInOrder(t *testing.T) {
	chunks := []string{
		strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40),
		strings.Repeat("d", 40), strings.Repeat("e", 40), strings.Repeat("f", 40),
	}
	groups, err := groupChunks(chunks, EstimateTokens(chunks[0])*2, 8)
	if err != nil {
		t.Fatalf("groupChunks: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3 (two chunks of 10 tokens each per 20-token group)", len(groups))
	}
	// Order and completeness: every chunk appears exactly once, in input order.
	var seen []string
	for _, g := range groups {
		seen = append(seen, g...)
	}
	if len(seen) != len(chunks) {
		t.Fatalf("grouped %d chunks, want %d (no chunk may be dropped)", len(seen), len(chunks))
	}
	for i := range chunks {
		if seen[i] != chunks[i] {
			t.Errorf("chunk %d = %q, want %q (order is preserved)", i, truncate(seen[i], 8), truncate(chunks[i], 8))
		}
	}
}

func TestCompact_GroupingRefusesZeroCap(t *testing.T) {
	if _, err := groupChunks([]string{"x"}, 10, 0); err == nil {
		t.Fatal("groupChunks accepted max_chunks=0")
	}
}

func TestCompact_HugeSingleChunkIsOneGroupNotASplit(t *testing.T) {
	// A single fragment far larger than the group target is its own group: the
	// pass does not cut a chunk in half, because half a fragment is the silent
	// truncation the hook must never do.
	groups, err := groupChunks([]string{strings.Repeat("m", 4000)}, 100, 3)
	if err != nil {
		t.Fatalf("groupChunks: %v", err)
	}
	if len(groups) != 1 || len(groups[0]) != 1 {
		t.Fatalf("groups = %v, want one group holding the whole chunk", len(groups))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
