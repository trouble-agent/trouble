package skills

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// decodePublicKey parses a base64 std 32-byte ed25519 public key.
func decodePublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("public_key is not base64 std: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public_key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// canaryRecord is one canary outcome (§4.4).
type canaryRecord struct {
	Result        string `json:"result"` // green | failed | inconclusive | ""
	HostID        string `json:"host_id"`
	TS            string `json:"ts"`
	ApplyMode     bool   `json:"apply_mode"`
	DaemonVersion string `json:"daemon_version"`
}

// installRow is one (name, version) row of the local index (§3.5).
type installRow struct {
	Name           string       `json:"name"`
	Version        int          `json:"version"`
	State          string       `json:"state"`
	Origin         string       `json:"origin"` // local | pull
	Dir            string       `json:"dir"`
	SignerKeyID    string       `json:"signer_key_id"`
	CanonicalSV    string       `json:"canonical_sha256"`
	PlaySHA        string       `json:"play_sha256"`
	SigsSHA        string       `json:"sigs_sha256"`
	InstalledTS    string       `json:"installed_ts"`
	Canary         canaryRecord `json:"canary"`
	Refusals       int          `json:"refusals"`
	RefusalCode    string       `json:"refusal_code"`
	Reason         string       `json:"reason"`
	CanaryOverride bool         `json:"canary_override,omitempty"`
	Failures       int          `json:"failures"`
	RejectedHash   string       `json:"rejected_play_sha256,omitempty"`
	Sigs           int          `json:"sigs"`
	Provenance     []string     `json:"provenance,omitempty"`
	// SkillID is the local candidate id a promotion came from, so the registry's
	// authorize hook can resolve `skill:<id>` back to an installed artifact.
	SkillID string `json:"skill_id,omitempty"`
}

// localIndex is the whole local state (§3.5). It is local persistence only: never
// a wire format, never a ledger payload.
type localIndex struct {
	SchemaVersion int          `json:"schema_version"`
	HostID        string       `json:"host_id"`
	Revision      int          `json:"revision"`
	Rows          []installRow `json:"rows"`
}

// statsFile is stats.json (§3.6): local counters, never artifact content.
type statsFile struct {
	SchemaVersion int                `json:"schema_version"`
	HostID        string             `json:"host_id"`
	Rows          []types.SkillStats `json:"rows"`
	// RunsDay/Runs are the per-UTC-day application counter the
	// `[guards].max_runs` budget needs (§4.5 rule 4): host-local state, kept out
	// of the artifact, which is exactly the contradiction SPEC-11 §1 fixes.
	RunsDay string         `json:"runs_day"`
	Runs    map[string]int `json:"runs"`
}

// holdEntry is one open verify-window hold (§4.5 rule 3).
type holdEntry struct {
	Name      string `json:"name"`
	Version   int    `json:"version"`
	Sig       string `json:"sig"`
	WindowEnd string `json:"window_end"`
	HeldTS    string `json:"held_ts"`
	Expired   bool   `json:"expired"`
}

// pullReport is the result of one pull (§4.1).
type pullReport struct {
	Ref            string   `json:"ref"`
	RefMode        string   `json:"ref_mode"`
	ResolvedSHA    string   `json:"resolved_sha"`
	Seen           int      `json:"seen"`
	Installed      int      `json:"installed"`
	Pending        int      `json:"pending_review"`
	Held           int      `json:"held"`
	Refused        int      `json:"refused"`
	MissingModules []string `json:"missing_modules"`
	Noop           bool     `json:"noop"`
	ErrorCode      string   `json:"error_code,omitempty"`
}

// matchResult is one resolution (§2's Resolver.Match).
type matchResult struct {
	Name        string
	Version     int
	Skill       types.Skill
	Play        types.Play
	PlayBytes   []byte
	Origin      string
	Candidates  int
	Conflict    bool
	ConflictWhy string
	Winner      string
	Loser       string
	Demoted     bool
	Reason      string
}

// Store is the local persistence of §3.5 plus the read surfaces of §3.7.
type Store struct {
	cfg    types.SkillsConfig
	dir    string
	hostID string
	ver    string
	now    func() time.Time

	mu              sync.Mutex
	idx             localIndex
	stats           statsFile
	dirty           bool
	lastFlush       time.Time
	holds           []holdEntry
	refused         map[string]int // dedup key → accumulated count
	lastRefusedEmit map[string]time.Time
}

func newStore(cfg types.SkillsConfig, stateRoot, hostID, daemonVersion string, now func() time.Time) (*Store, error) {
	dir := cfg.StateDir
	if dir == "" {
		dir = filepath.Join(stateRoot, "skills-local")
	}
	s := &Store{
		cfg: cfg, dir: dir, hostID: hostID, ver: daemonVersion, now: now,
		idx:             localIndex{SchemaVersion: 1, HostID: hostID},
		stats:           statsFile{SchemaVersion: 1, HostID: hostID},
		refused:         map[string]int{},
		lastRefusedEmit: map[string]time.Time{},
	}
	for _, sub := range []string{"repo", "staging", "pending", "installed", "quarantine", "candidates"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, newErr(types.CodeSkills014, ReasonStatsWrite, "state dir %s: %v", dir, err)
		}
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir is the resolved skills-local root.
func (s *Store) Dir() string { return s.dir }

func (s *Store) indexPath() string { return filepath.Join(s.dir, "index.json") }
func (s *Store) statsPath() string { return filepath.Join(s.dir, "stats.json") }

func (s *Store) load() error {
	if raw, err := os.ReadFile(s.indexPath()); err == nil {
		var idx localIndex
		if err := json.Unmarshal(raw, &idx); err == nil {
			s.idx = idx
			if s.idx.HostID == "" {
				s.idx.HostID = s.hostID
			}
		} else {
			return newErr(types.CodeSkills002, ReasonTampered, "index.json is unreadable: %v", err)
		}
	}
	if raw, err := os.ReadFile(s.statsPath()); err == nil {
		var st statsFile
		if err := json.Unmarshal(raw, &st); err == nil {
			s.stats = st
		}
	}
	return nil
}

// saveIndex writes the index atomically (temp → fsync → rename).
func (s *Store) saveIndex() error {
	s.mu.Lock()
	s.idx.Revision++
	raw, err := json.Marshal(s.idx)
	s.mu.Unlock()
	if err != nil {
		return newErr(types.CodeSkills014, ReasonStatsWrite, "index encode: %v", err)
	}
	return atomicWrite(s.indexPath(), raw)
}

func atomicWrite(path string, raw []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return newErr(types.CodeSkills014, ReasonStatsWrite, "temp file in %s: %v", dir, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return newErr(types.CodeSkills014, ReasonStatsWrite, "chmod: %v", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return newErr(types.CodeSkills014, ReasonStatsWrite, "write: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return newErr(types.CodeSkills014, ReasonStatsWrite, "fsync: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return newErr(types.CodeSkills014, ReasonStatsWrite, "close: %v", err)
	}
	if err := os.Rename(name, path); err != nil {
		return newErr(types.CodeSkills014, ReasonStatsWrite, "rename: %v", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// saveStats flushes stats.json at most every 5 s (§3.6) and on demand.
func (s *Store) saveStats(force bool) error {
	s.mu.Lock()
	if !force && s.now().Sub(s.lastFlush) < 5*time.Second {
		s.mu.Unlock()
		return nil
	}
	s.lastFlush = s.now()
	s.dirty = false
	raw, err := json.Marshal(s.stats)
	s.mu.Unlock()
	if err != nil {
		return newErr(types.CodeSkills014, ReasonStatsWrite, "stats encode: %v", err)
	}
	return atomicWrite(s.statsPath(), raw)
}

// Flush forces a stats flush (shutdown path).
func (s *Store) Flush() error { return s.saveStats(true) }

// Rows returns a copy of the index rows, sorted by (name, version).
func (s *Store) Rows() []installRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]installRow, len(s.idx.Rows))
	copy(out, s.idx.Rows)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// row finds one (name, version) row.
func (s *Store) row(name string, version int) (installRow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.idx.Rows {
		if r.Name == name && r.Version == version {
			return r, true
		}
	}
	return installRow{}, false
}

// putRow inserts or replaces a row.
func (s *Store) putRow(r installRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.idx.Rows {
		if s.idx.Rows[i].Name == r.Name && s.idx.Rows[i].Version == r.Version {
			s.idx.Rows[i] = r
			return
		}
	}
	s.idx.Rows = append(s.idx.Rows, r)
}

// HighestInstalled is the highest installed version for a name (§3.3).
func (s *Store) HighestInstalled(name string) (installRow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best installRow
	found := false
	for _, r := range s.idx.Rows {
		if r.Name != name {
			continue
		}
		switch r.State {
		case types.SkillInstalled, types.SkillPendingReview, types.SkillHeld,
			types.SkillCanaryBlocked, types.SkillFloorBlocked, types.SkillQuarantined, types.SkillConflict:
		default:
			continue
		}
		if !found || r.Version > best.Version {
			best, found = r, true
		}
	}
	return best, found
}

// MaxVersion is the highest version the local index holds for a name, whatever
// the state: local allocation is `1 + max` over every row (§3.3), so an int is
// never recycled.
func (s *Store) MaxVersion(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	max := 0
	for _, r := range s.idx.Rows {
		if r.Name == name && r.Version > max {
			max = r.Version
		}
	}
	return max
}

// NameOwnedByRelease reports whether a name has ever been installed from the
// channel: a locally promoted name may not collide with one (§3.3).
func (s *Store) NameOwnedByRelease(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.idx.Rows {
		if r.Name == name && r.Origin == "pull" {
			return true
		}
	}
	return false
}

// CanaryGreen reports whether a green canary for (name, version) exists inside
// the validity window and from the configured canary host (§4.4).
func (s *Store) CanaryGreen(name string, version int, canaryHostID string, validity time.Duration) (canaryRecord, bool) {
	r, ok := s.row(name, version)
	if !ok || r.Canary.Result != "green" {
		return canaryRecord{}, false
	}
	if canaryHostID != "" && r.Canary.HostID != canaryHostID {
		return canaryRecord{}, false
	}
	if r.Canary.DaemonVersion != "" {
		if got, ok := parseSemver(r.Canary.DaemonVersion); ok {
			if want, ok2 := parseSemver(r.CanonicalSV); ok2 && false {
				_ = want
			}
			_ = got
		}
	}
	if ts, err := types.ParseUTC(r.Canary.TS); err == nil && validity > 0 {
		if s.now().Sub(ts) > validity {
			return canaryRecord{}, false
		}
	}
	return r.Canary, true
}

// SetCanary records a canary outcome, creating the row when the version has not
// been indexed yet (a canary run can finish before the artifact is installed
// anywhere else).
func (s *Store) SetCanary(name string, version int, rec canaryRecord) {
	r, ok := s.row(name, version)
	if !ok {
		r = installRow{Name: name, Version: version, State: types.SkillHeld}
	}
	r.Canary = rec
	s.putRow(r)
	if err := s.saveIndex(); err != nil {
		_ = err
	}
}

// RunsToday is the number of applications of (name, version) for a sig today.
func (s *Store) RunsToday(name string, version int, sig string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stats.RunsDay != dayKey(s.now()) {
		return 0
	}
	return s.stats.Runs[runKey(name, version, sig)]
}

// noteRun counts one application against today's budget.
func (s *Store) noteRun(name string, version int, sig string) {
	s.mu.Lock()
	if s.stats.RunsDay != dayKey(s.now()) {
		s.stats.RunsDay = dayKey(s.now())
		s.stats.Runs = map[string]int{}
	}
	if s.stats.Runs == nil {
		s.stats.Runs = map[string]int{}
	}
	s.stats.Runs[runKey(name, version, sig)]++
	s.dirty = true
	s.mu.Unlock()
	_ = s.saveStats(false)
}

func runKey(name string, version int, sig string) string {
	return fmt.Sprintf("%s|%d|%s|%s", name, version, sig, "")
}

// Stats returns one (name, version) stats row (§3.6).
func (s *Store) Stats(name string, version int) types.SkillStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.stats.Rows {
		if r.Name == name && r.Version == version {
			return r
		}
	}
	return types.SkillStats{Name: name, Version: version}
}

// bumpStats applies one run outcome. A flush failure is TROUBLE-SKILLS-014 and the
// counters stay in memory: a play is never blocked by stats.
func (s *Store) bumpStats(name string, version int, success bool, refusals int) {
	s.mu.Lock()
	found := false
	for i := range s.stats.Rows {
		if s.stats.Rows[i].Name == name && s.stats.Rows[i].Version == version {
			s.stats.Rows[i].Applied++
			if success {
				s.stats.Rows[i].Success++
			}
			s.stats.Rows[i].Refusals += refusals
			s.stats.Rows[i].LastUsedTS = types.FormatUTC(s.now())
			found = true
			break
		}
	}
	if !found {
		row := types.SkillStats{Name: name, Version: version, Applied: 1, LastUsedTS: types.FormatUTC(s.now())}
		if success {
			row.Success = 1
		}
		row.Refusals = refusals
		s.stats.Rows = append(s.stats.Rows, row)
	}
	s.dirty = true
	s.mu.Unlock()
	_ = s.saveStats(false)
}

// StatsFlushError is the last stats write failure (TROUBLE-SKILLS-014), exposed
// so /health and the CLI can report it without failing anything.
func (s *Store) StatsFlushError() error { return s.saveStats(true) }

// Status is `trouble skills status --json` (§3.7).
func (s *Store) Status(pull pullState) types.SkillsStatus {
	src, _ := EffectiveSource(s.cfg)
	st := types.SkillsStatus{
		HostID:        s.hostID,
		DaemonVersion: s.ver,
		Enabled:       s.cfg.Enabled,
		Source:        src.Path,
		Ref:           s.cfg.SourceRef,
		RefMode:       s.cfg.RefMode,
		LastPullTS:    pull.LastPullTS,
		LastPullSHA:   pull.LastPullSHA,
		PullFailures:  pull.Failures,
		NextPullTS:    pull.NextPullTS,
		Degraded:      pull.Failures >= 5,
		Approve:       s.cfg.Approve,
		CanaryHostID:  s.cfg.CanaryHostID,
		Signers:       SignerList(s.cfg),
	}
	for _, r := range s.Rows() {
		st.Skills = append(st.Skills, s.RowView(r))
	}
	return st
}

// RowView renders one index row as the public `SkillRow`.
func (s *Store) RowView(r installRow) types.SkillRow {
	stats := s.Stats(r.Name, r.Version)
	return types.SkillRow{
		Name:        r.Name,
		Version:     r.Version,
		State:       r.State,
		Origin:      r.Origin,
		SignerKeyID: r.SignerKeyID,
		Sigs:        r.Sigs,
		Applied:     stats.Applied,
		Success:     stats.Success,
		Refusals:    stats.Refusals + r.Refusals,
		ErrorCode:   r.RefusalCode,
		Reason:      r.Reason,
		InstalledTS: r.InstalledTS,
	}
}

// recordRefusal stores a refusal row (state refused) with its accumulated count.
func (s *Store) recordRefusal(name string, version int, code types.ErrorCode, reason string) {
	row, ok := s.row(name, version)
	if !ok {
		row = installRow{Name: name, Version: version, Sigs: 0}
	}
	row.State = types.SkillRefused
	row.RefusalCode = string(code)
	row.Reason = reason
	row.Refusals++
	row.InstalledTS = types.FormatUTC(s.now())
	s.putRow(row)
}

// RefusalCount is the accumulated count for a refusal dedup key.
func (s *Store) RefusalCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused[key]
}

// noteRefusal accumulates a refusal dedup key and reports whether the ledger
// record should be emitted now (at most once per 24 h per key, §4.7).
func (s *Store) noteRefusal(key string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused[key]++
	n := s.refused[key]
	last, seen := s.lastRefusedEmit[key]
	if !seen || s.now().Sub(last) >= 24*time.Hour {
		s.lastRefusedEmit[key] = s.now()
		return n, true
	}
	return n, false
}

// OpenHold remembers an open verify-window hold (§4.5 rule 3).
func (s *Store) OpenHold(sig, name string, version int, windowEnd string) holdEntry {
	h := holdEntry{Name: name, Version: version, Sig: sig, WindowEnd: windowEnd, HeldTS: types.FormatUTC(s.now())}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.holds {
		if s.holds[i].Sig == sig && s.holds[i].Name == name && s.holds[i].Version == version {
			return s.holds[i]
		}
	}
	s.holds = append(s.holds, h)
	return h
}

// ExpireHolds converts holds older than max_hold into refusals (§4.5 rule 3).
func (s *Store) ExpireHolds(maxHold time.Duration) []holdEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []holdEntry
	kept := s.holds[:0]
	for _, h := range s.holds {
		held, err := types.ParseUTC(h.HeldTS)
		if err == nil && s.now().Sub(held) > maxHold {
			h.Expired = true
			expired = append(expired, h)
			continue
		}
		kept = append(kept, h)
	}
	s.holds = kept
	return expired
}

// ReleaseHolds drops the holds for a sig (the window closed).
func (s *Store) ReleaseHolds(sig string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.holds[:0]
	for _, h := range s.holds {
		if h.Sig != sig {
			kept = append(kept, h)
		}
	}
	s.holds = kept
}

// Holds returns the open holds.
func (s *Store) Holds() []holdEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]holdEntry, len(s.holds))
	copy(out, s.holds)
	return out
}

// InstallDir is the on-disk directory of one installed version.
func (s *Store) InstallDir(name string, version int) string {
	return filepath.Join(s.dir, "installed", name, fmt.Sprint(version))
}

// stageDir is one pull's staging root.
func (s *Store) stageDir(id string) string { return filepath.Join(s.dir, "staging", id) }

// readArtifact reads SKILL.toml plus the play it names from a directory.
func readArtifact(dir string) (types.Skill, []byte, []byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "SKILL.toml"))
	if err != nil {
		return types.Skill{}, nil, nil, newErr(types.CodeSkills001, ReasonValidation, "SKILL.toml: %v", err)
	}
	// The play path needs the name/version, so decode the header twice: once
	// loosely for the path, once strictly through LoadArtifact.
	var head struct {
		Name    string `toml:"name"`
		Version int    `toml:"version"`
		PlayRef string `toml:"play_ref"`
	}
	if err := decodeTOML(raw, &head); err != nil {
		return types.Skill{}, nil, nil, newErr(types.CodeSkills001, ReasonValidation, "SKILL.toml: %v", err)
	}
	if strings.TrimSpace(head.PlayRef) != "" {
		if strings.Contains(head.PlayRef, "..") || strings.HasPrefix(head.PlayRef, "/") {
			return types.Skill{}, nil, nil, newErr(types.CodeSkills001, ReasonFieldRule,
				"play_ref %q escapes the artifact tree", head.PlayRef)
		}
	}
	playPath := filepath.Join(dir, filepath.FromSlash(head.PlayRef))
	playBytes, err := os.ReadFile(playPath)
	if err != nil {
		return types.Skill{}, nil, nil, newErr(types.CodeSkills001, ReasonPlayMissing,
			"play_ref %q does not resolve: %v", head.PlayRef, err)
	}
	skill, err := LoadArtifact(raw, playBytes)
	if err != nil {
		return types.Skill{}, nil, nil, err
	}
	return skill, playBytes, raw, nil
}

// reverify re-checks every installed row against its recorded digests and
// quarantines a row that no longer matches (§3.5). Local tampering is detected,
// never executed.
func (s *Store) reverify(ctx context.Context, emit func(installRow, types.Skill, error) error) (int, error) {
	quarantined := 0
	for _, r := range s.Rows() {
		if r.State != types.SkillInstalled && r.State != types.SkillPendingReview {
			continue
		}
		dir := filepath.Join(s.dir, filepath.FromSlash(r.Dir))
		skill, playBytes, _, err := readArtifact(dir)
		if err != nil {
			if qerr := s.quarantine(r, types.CodeSkills002, ReasonTampered); qerr != nil {
				return quarantined, qerr
			}
			quarantined++
			if emit != nil {
				_ = emit(r, skill, newErr(types.CodeSkills002, ReasonTampered, "%s@%d: %v", r.Name, r.Version, err))
			}
			continue
		}
		canon, err := CanonicalSHA256(skill, playBytes)
		if err != nil || canon != r.CanonicalSV || PlaySHA256(playBytes) != r.PlaySHA {
			if qerr := s.quarantine(r, types.CodeSkills002, ReasonTampered); qerr != nil {
				return quarantined, qerr
			}
			quarantined++
			if emit != nil {
				_ = emit(r, skill, newErr(types.CodeSkills002, ReasonTampered,
					"%s@%d: digest mismatch (canonical %s vs %s)", r.Name, r.Version, short(canon), short(r.CanonicalSV)))
			}
		}
	}
	if quarantined > 0 {
		if err := s.saveIndex(); err != nil {
			return quarantined, err
		}
	}
	return quarantined, nil
}

// quarantine moves a version's directory under quarantine/ and marks the row.
func (s *Store) quarantine(r installRow, code types.ErrorCode, reason string) error {
	src := filepath.Join(s.dir, filepath.FromSlash(r.Dir))
	dst := filepath.Join(s.dir, "quarantine", r.Name, fmt.Sprint(r.Version))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return newErr(types.CodeSkills014, ReasonStatsWrite, "quarantine dir: %v", err)
	}
	_ = os.RemoveAll(dst)
	if err := os.Rename(src, dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return newErr(types.CodeSkills014, ReasonStatsWrite, "quarantine %s@%d: %v", r.Name, r.Version, err)
	}
	row := r
	row.State = types.SkillQuarantined
	row.Dir = filepath.ToSlash(filepath.Join("quarantine", r.Name, fmt.Sprint(r.Version)))
	row.RefusalCode = string(code)
	row.Reason = reason
	row.Refusals++
	s.putRow(row)
	return nil
}

// installRowFromSkill is the row a successful install writes.
func (s *Store) installRowFromSkill(skill types.Skill, playBytes []byte, dir, state, origin string, canary canaryRecord) (installRow, error) {
	canon, err := CanonicalSHA256(skill, playBytes)
	if err != nil {
		return installRow{}, err
	}
	prov := append([]string{}, skill.Provenance.Incidents...)
	prov = append(prov, skill.Provenance.Research...)
	return installRow{
		Name:        skill.Name,
		Version:     skill.Version,
		State:       state,
		Origin:      origin,
		Dir:         filepath.ToSlash(dir),
		SignerKeyID: skill.SignerKeyID,
		CanonicalSV: canon,
		PlaySHA:     PlaySHA256(playBytes),
		SigsSHA:     SigsSHA256(skill),
		InstalledTS: types.FormatUTC(s.now()),
		Canary:      canary,
		Sigs:        len(skill.Sigs),
		Provenance:  prov,
	}, nil
}

// retentionPrune removes older installed versions beyond retain_versions (§3.3).
func (s *Store) retentionPrune(name string) []int {
	keep := s.cfg.RetainVersions
	if keep <= 0 {
		keep = 2
	}
	var installed []int
	for _, r := range s.Rows() {
		if r.Name == name && r.State == types.SkillInstalled {
			installed = append(installed, r.Version)
		}
	}
	sort.Ints(installed)
	var removed []int
	for len(installed) > keep {
		v := installed[0]
		installed = installed[1:]
		_ = os.RemoveAll(s.InstallDir(name, v))
		s.mu.Lock()
		kept := s.idx.Rows[:0]
		for _, r := range s.idx.Rows {
			if r.Name == name && r.Version == v {
				continue
			}
			kept = append(kept, r)
		}
		s.idx.Rows = kept
		s.mu.Unlock()
		removed = append(removed, v)
	}
	return removed
}

// recordRejectedHash marks a play content hash as rejected: no promotion may
// reuse it, and auto-accept may not accept it (§3.4).
func (s *Store) recordRejectedHash(name string, version int, hash string) {
	row, ok := s.row(name, version)
	if !ok {
		row = installRow{Name: name, Version: version}
	}
	row.RejectedHash = hash
	s.putRow(row)
	_ = s.saveIndex()
}

// RejectedHash reports whether a play content hash was rejected before.
func (s *Store) RejectedHash(name, hash string) bool {
	if hash == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.idx.Rows {
		if r.Name == name && r.RejectedHash == hash {
			return true
		}
	}
	return false
}

// InstalledByID resolves a `skill:<id>` source back to its installed row.
func (s *Store) InstalledByID(id string) (installRow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.idx.Rows {
		if r.State != types.SkillInstalled {
			continue
		}
		if r.SkillID == id || fmt.Sprintf("%s@%d", r.Name, r.Version) == id || r.Name == id {
			return r, true
		}
	}
	return installRow{}, false
}

// short renders a digest for a human line.
func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
