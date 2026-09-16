package issues

import (
	"context"
	"fmt"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Link wires the sig's issue to a board row (SPEC-08) and/or a research brief
// (SPEC-07) with no manual step (§3.12, AC-22). A value is only ever set from an
// id the caller owns: the desk never invents a tsk_/res_ id.
func (d *Desk) Link(ctx context.Context, ref types.IssueRef, taskID, researchID string) (types.IssueRef, error) {
	if taskID == "" && researchID == "" {
		return ref, nil
	}
	drv, ok := d.drivers[ref.Driver]
	if !ok {
		return ref, newErr(types.CodeIssues003, ReasonUnknownDriver, 0, false, "driver %q is not enabled", ref.Driver)
	}
	dc := d.drvCfg[ref.Driver]

	lines := make([]string, 0, 2)
	if taskID != "" {
		lines = append(lines, fmt.Sprintf("linked: %s (board-jsonl)", taskID))
	}
	if researchID != "" {
		lines = append(lines, fmt.Sprintf("research: %s", researchID))
	}
	idemKey := commentIdem(ref.ID, "link|"+taskID+"|"+researchID)
	body, _, err := d.scrubBody(ctx, joinLines(lines)+"\n\n"+IdemMarker(idemKey), d.anchorProject(ref))
	if err != nil {
		return ref, err
	}
	var out types.IssueRef
	attempts, callErr := d.withRetry(ctx, ref.Driver, dc, "link", idemKey, func(c context.Context) error {
		r, err := drv.Comment(c, ref, body)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	payload := map[string]any{
		"op": "link", "driver": ref.Driver, "sig": ref.Sig, "idem_key": idemKey,
		"result": "linked", "task_id": taskID, "research_id": researchID,
		"external_id": ref.ExternalID, "attempt": attempts, "http_status": 200, "error_code": "",
	}
	if callErr != nil {
		e := asError(callErr)
		payload["result"] = "failed"
		payload["error_code"] = string(e.Code)
		payload["retryable"] = e.Retryable
		payload["http_status"] = e.HTTPStatus
		_, _ = d.record(ctx, types.KIssue, ref.Sig, "", payload)
		return ref, e
	}
	if out.ID == "" {
		out = ref
	}
	out.Driver = ref.Driver
	out.Sig = ref.Sig
	if taskID != "" {
		out.TaskID = taskID
	}
	if researchID != "" {
		out.ResearchID = researchID
	}
	_, _ = d.record(ctx, types.KIssue, ref.Sig, "", payload)

	d.mu.Lock()
	for _, a := range d.anchors {
		if a.sig == ref.Sig && a.driver == ref.Driver {
			a.ref = out
			a.updatedTS = types.FormatUTC(d.now())
		}
	}
	d.handled[idemKey] = true
	d.mu.Unlock()
	return out, nil
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// AnchorForRow is the SPEC-08 ordering-race lookup: SPEC-08 fills
// BoardRow.IssueRefs[] with the iss_ ids the desk returned for a sig, and uses
// this when the row is written before the issue.
func (d *Desk) AnchorForRow(sig string) (types.IssueRef, bool) { return d.Anchor(sig) }
