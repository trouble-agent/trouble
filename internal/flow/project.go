package flow

// project.go — the registration proof (SPEC-08 §3.5).
//
// A row in a project the scheduler does not tick is a row nobody works: the
// fleet's inert-feature class and the exact lie a green board tells. So filing
// refuses (TROUBLE-FLOW-018, zero bytes written) unless the proof holds, and the
// proof is a probe against the scheduler rather than a config assertion.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// schedulerProject is the slice of the scheduler's project row the proof reads.
type schedulerProject struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	BoardPath string `json:"board_path"`
	Ticked    bool   `json:"ticked"`
}

// projectFor resolves the project a row belongs to: by board path first (the
// row's own identity), then by repo root.
func (f *Flow) projectFor(row types.BoardRow) (types.FlowProject, bool) {
	f.mu.Lock()
	names := make([]string, 0, len(f.cfg.Projects))
	for name := range f.cfg.Projects {
		names = append(names, name)
	}
	f.mu.Unlock()
	if row.Repo != "" {
		for _, name := range names {
			p := f.cfg.Projects[name]
			if p.Repo != "" && cleanPath(p.Repo) == cleanPath(row.Repo) {
				return p, true
			}
		}
	}
	if b := boardOf(row.Repo, f.cfg.BoardPath); b != "" {
		for _, name := range names {
			p := f.cfg.Projects[name]
			if p.BoardPath != "" && cleanPath(p.BoardPath) == cleanPath(b) {
				return p, true
			}
		}
	}
	if len(names) == 1 {
		return f.cfg.Projects[names[0]], true
	}
	return types.FlowProject{}, false
}

// projectByName resolves a configured project by its key.
func (f *Flow) projectByName(name string) (types.FlowProject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.cfg.Projects[name]
	return p, ok
}

// ProjectNames lists the configured project keys (the composition root's
// single-project repo resolution uses it).
func (f *Flow) ProjectNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.cfg.Projects))
	for name := range f.cfg.Projects {
		names = append(names, name)
	}
	return names
}

// ProjectByName is the exported lookup of projectByName.
func (f *Flow) ProjectByName(name string) (types.FlowProject, bool) {
	return f.projectByName(name)
}

// registrationHolds is the three-check proof plus the staleness rule (§3.5).
func (f *Flow) registrationHolds(p types.FlowProject) bool {
	if !p.Enabled {
		return false
	}
	if !p.Registered {
		return false
	}
	// A probe that could not reach the scheduler is transient: the last known
	// value holds for registration_stale_max, after which filing refuses.
	if p.LastProbeTS == "" {
		return false
	}
	ts, err := types.ParseUTC(p.LastProbeTS)
	if err != nil {
		return false
	}
	stale := f.cfg.RegistrationStaleMax.Std()
	if stale <= 0 {
		stale = time.Hour
	}
	if f.clock().Now().Sub(ts) > stale {
		return false
	}
	return true
}

// probeProject runs the three mandatory checks and returns the updated project.
func (f *Flow) probeProject(ctx context.Context, p types.FlowProject) types.FlowProject {
	p.LastProbeTS = types.FormatUTC(f.clock().Now())
	if f.cfg.SchedulerEndpoint == "" {
		p.Registered = false
		p.Reason = "scheduler_endpoint_unset"
		return p
	}
	url := strings.TrimRight(f.cfg.SchedulerEndpoint, "/") + "/projects"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		p.Registered = false
		p.Reason = "probe_request_invalid"
		return p
	}
	if tok := readToken(f.cfg.SchedulerTokenFile); tok != "" {
		// The token comes from the 0600 file and never appears in a URL or argv.
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Unreachable is NOT a registration failure: the last known value holds
		// until registration_stale_max (§3.5).
		p.Reason = "probe_unreachable"
		return p
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		p.Reason = fmt.Sprintf("probe_http_%d", resp.StatusCode)
		return p
	}
	var list []schedulerProject
	if err := json.Unmarshal(body, &list); err != nil {
		var wrapper struct {
			Projects []schedulerProject `json:"projects"`
		}
		if err2 := json.Unmarshal(body, &wrapper); err2 != nil {
			p.Reason = "probe_body_unparsable"
			return p
		}
		list = wrapper.Projects
	}
	want := p.Scheduler
	if want == "" {
		want = p.Name
	}
	for _, sp := range list {
		if sp.Name != want {
			continue
		}
		p.Ticked = sp.Ticked
		if !sp.Enabled {
			p.Registered = false
			p.Reason = "scheduler_project_disabled"
			return p
		}
		if sp.BoardPath != "" && cleanPath(sp.BoardPath) != cleanPath(p.BoardPath) {
			p.Registered = false
			p.Reason = "board_path_mismatch"
			return p
		}
		p.Registered = true
		p.Reason = ""
		return p
	}
	p.Registered = false
	p.Reason = "scheduler_project_absent"
	return p
}

// boardOf derives a repo's board directory (the configured default or the
// repo-relative convention).
func boardOf(repo, configured string) string {
	if repo == "" {
		return configured
	}
	if configured != "" {
		if cleanPath(configured) == cleanPath(repo) {
			return configured
		}
	}
	return repo + "/.board"
}

// readToken reads a 0600 token file. A missing or over-permissive file yields no
// token: the probe then runs unauthenticated and fails loudly instead of sending
// a secret from a world-readable path.
func readToken(path string) string {
	if path == "" {
		return ""
	}
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if st.Mode().Perm()&0o077 != 0 {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
