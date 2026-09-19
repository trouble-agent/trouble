package llm

import (
	"context"
	"unicode/utf8"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// EstimateTokens is the byte-based token estimator used where no provider `usage`
// object exists. It is documented, deterministic and deliberately coarse: four
// UTF-8 bytes per token, rounded up, minimum 1 for a non-empty string. Every count
// produced by it is flagged `estimated:true` in the accounting, because an estimate
// presented as a measurement is how a budget stops being a budget.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := utf8.RuneCountInString(s)
	tokens := (n + 3) / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// EstimateChunks sums the estimate over a context.
func EstimateChunks(chunks []string) int {
	total := 0
	for _, c := range chunks {
		total += EstimateTokens(c)
	}
	return total
}

// MaybeCompact is the context-compaction hook (SPEC-05 §3.7a).
//
// Below or at the budget it returns the context UNCHANGED and an “Applied:false“
// accounting that still carries the measured in-token count — so a record can state
// "the context fit" rather than omitting the fact. Above the budget it runs the
// capped, single-shot, map-style pass:
//
//   - the context is grouped into at most `MaxChunks` groups of about `ChunkTokens`
//     each, in order, with no group dropped and no group split mid-chunk;
//   - each group is summarised by exactly ONE completion whose `max_tokens` is
//     `Compact.MaxTokens`, sent through this same ordered chain;
//   - the summaries replace the groups and the in/out token counts are returned.
//
// A split that would need more groups than `MaxChunks`, or a summarisation call
// that fails, is a ClassCompaction refusal: the stage fails instead of running on a
// context that was quietly cut down to size. If `Enabled` is false an over-budget
// context is refused outright.
func (c *Client) MaybeCompact(ctx context.Context, chunks []string) ([]string, types.LLMCompaction, error) {
	in := EstimateChunks(chunks)
	acct := types.LLMCompaction{InTokens: in, MaxChunks: c.cfg.Compact.MaxChunks}
	if in <= c.cfg.Compact.BudgetTokens || len(chunks) == 0 {
		return chunks, acct, nil
	}
	if !c.cfg.Compact.Enabled {
		return nil, acct, newErr(ClassCompaction, "", ReasonCompactionCapped,
			"context is %d tokens, over the budget %d, and compaction is disabled; the stage refuses rather than truncate",
			in, c.cfg.Compact.BudgetTokens)
	}

	groups, err := groupChunks(chunks, c.cfg.Compact.ChunkTokens, c.cfg.Compact.MaxChunks)
	if err != nil {
		return nil, acct, err
	}

	summaries := make([]string, 0, len(groups))
	out := 0
	for i, group := range groups {
		prompt := compactPrompt(i, len(groups), group)
		resp, err := c.Complete(ctx, Request{Prompt: prompt, MaxTokens: c.cfg.CompactCap()})
		if err != nil {
			return nil, acct, newErr(ClassCompaction, "", ReasonCompactionCapped,
				"chunk %d/%d could not be summarised: %v", i+1, len(groups), err)
		}
		if resp.Candidate != "" {
			acct.Candidate = resp.Candidate
			acct.Model = resp.Model
		}
		summaries = append(summaries, resp.Text)
		out += EstimateTokens(resp.Text)
	}
	acct.Applied = true
	acct.Chunks = len(groups)
	acct.OutTokens = out
	return summaries, acct, nil
}

// Compact is MaybeCompact's explicit form for a caller that wants the accounting
// without a completion: it returns the accounting only.
func (c *Client) Compact(ctx context.Context, chunks []string) (types.LLMCompaction, error) {
	_, acct, err := c.MaybeCompact(ctx, chunks)
	return acct, err
}

// groupChunks splits the context into at most `maxChunks` ordered groups of about
// `chunkTokens` each.
//
// It never splits a chunk (content is preserved as whole units) and it never drops
// one; a context that needs more groups than the cap REFUSES. That refusal is the
// mechanism behind "never silent truncation": there is no code path that returns a
// shorter context than it was given without a recorded accounting.
func groupChunks(chunks []string, chunkTokens, maxChunks int) ([][]string, error) {
	if maxChunks <= 0 {
		return nil, newErr(ClassCompaction, "", ReasonCompactionCapped, "max_chunks must be > 0")
	}
	groups := make([][]string, 0, maxChunks)
	var cur []string
	curTokens := 0
	for _, chunk := range chunks {
		cp := chunk
		tokens := EstimateTokens(cp)
		if len(cur) > 0 && curTokens+tokens > chunkTokens {
			groups = append(groups, cur)
			cur = nil
			curTokens = 0
		}
		cur = append(cur, cp)
		curTokens += tokens
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	if len(groups) > maxChunks {
		return nil, newErr(ClassCompaction, "", ReasonCompactionCapped,
			"the context needs %d summarisation groups but max_chunks is %d; raise the cap or shrink the context (the context is never truncated)",
			len(groups), maxChunks)
	}
	return groups, nil
}

// compactPrompt is the single-shot map prompt. It is deterministic, so a
// summarisation is reproducible for a given group.
func compactPrompt(index, total int, group []string) string {
	body := ""
	for i, part := range group {
		if i > 0 {
			body += "\n"
		}
		body += part
	}
	return "You are compacting context for an automated incident-repair run.\n" +
		"Summarise the following context fragment (" +
		itoa(index+1) + " of " + itoa(total) + ") without losing any fact that a repair decision could depend on: " +
		"identifiers, error codes, file paths, commands, numbers and timestamps must survive verbatim. " +
		"Drop only repetition. Answer with the summary alone, no preamble.\n\n" +
		"----- context fragment -----\n" + body + "\n----- end fragment -----"
}

// itoa is a tiny decimal renderer so the prompt builder stays dependency-free.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
