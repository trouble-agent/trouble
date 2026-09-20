package hub

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// genRe is the SPEC-01 §3.4 file-naming shape, reused verbatim so the archival
// tier and the ledger agree about what a generation IS. A file that does not
// match is never an archival unit.
var genRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(?:\.p(\d{2}))?(?:\.(\d+)\.gen)?\.jsonl$`)

// GenerationFile is one candidate generation on the hot host.
type GenerationFile struct {
	File     string `json:"file"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	ModTime  string `json:"mtime"`
	Records  int64  `json:"records"`
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	MinTS    string `json:"min_ts"`
	MaxTS    string `json:"max_ts"`
	SHA256   string `json:"sha256"`
	// Closure is the evidence that the file is closed: "idx" when the
	// SPEC-01 §3.7a sidecar exists and agrees with the bytes, "not_live" when the
	// file is simply not the writer's current file.
	Closure string `json:"closure"`
	// Droppable is set by DroppableGenerations: the generation has a VERIFIED
	// marker and the retention sweep may delete it (SPEC-13 §3.5 step 5).
	Droppable bool `json:"droppable"`
}

// ArchivePlan is the SPEC-13 §3.5 step 1 plan for one generation.
type ArchivePlan struct {
	File      string `json:"file"`
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	GzipBytes int64  `json:"gzip_bytes"`
	Records   int64  `json:"records"`
	FirstSeq  uint64 `json:"first_seq"`
	LastSeq   uint64 `json:"last_seq"`
	MinTS     string `json:"min_ts"`
	MaxTS     string `json:"max_ts"`
	MarkerID  string `json:"marker_id"`
	SHA256    string `json:"sha256"`
	Namespace string `json:"namespace"`
	ObjectKey string `json:"object_key"`
	Closure   string `json:"closure"`
}

// Target is the archival object store.
//
// SPEC-13 §3.5 step 2 writes one object per generation into the DuckBrain
// namespace and steps 3/5 read it back and compare; that is exactly this
// interface. Keeping it a seam means the archival safety rules (verify before
// drop, never overwrite a conflicting object) are testable without a live tier,
// while the shipped implementation is a real HTTP client (kv.go).
type Target interface {
	// Describe names the target for logs and health (never a credential).
	Describe() string
	// Put writes an object. A retry of the same key with the same bytes is an
	// idempotent overwrite (SPEC-13 §3.5 step 3: "retried with the SAME
	// marker_id (idempotent overwrite by key), never a second object").
	Put(ctx context.Context, key string, data []byte) error
	// Get reads an object back.
	Get(ctx context.Context, key string) ([]byte, error)
}

// Queue is the append-only archival queue (`archive/queue.jsonl`, SPEC-13 §3.1).
type Queue struct {
	jobs    []archiveJob
	path    string
	skipped int
}

// archiveJob is one queue line: pending → exported → verified → dropped.
type archiveJob struct {
	MarkerID  string `json:"marker_id"`
	File      string `json:"file"`
	Bytes     int64  `json:"bytes"`
	GzipBytes int64  `json:"gzip_bytes"`
	Records   int64  `json:"records"`
	State     string `json:"state"`
	Namespace string `json:"namespace"`
	ObjectKey string `json:"object_key"`
	ErrorCode string `json:"error_code"`
	TS        string `json:"ts"`
}

// ArchiveQueue loads the queue (SPEC-13 §2.3). A missing file is an empty queue;
// a torn last line is truncated, never fatal (the same tolerance the ledger's
// readers apply).
func ArchiveQueue(stateRoot string) (Queue, error) {
	q := Queue{path: QueuePath(stateRoot)}
	f, err := os.Open(q.path)
	if err != nil {
		if os.IsNotExist(err) {
			return q, nil
		}
		return q, errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot read the archive queue", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	for i, line := range lines {
		if len(strings.TrimSpace(line)) == 0 {
			continue
		}
		var job archiveJob
		if err := json.Unmarshal([]byte(line), &job); err != nil {
			if i == len(lines)-1 {
				break
			}
			q.skipped++
			continue
		}
		q.jobs = append(q.jobs, job)
	}
	return q, nil
}

// Jobs returns every queue line in file order.
func (q Queue) Jobs() []archiveJob { return q.jobs }

// Depth is the number of jobs that are not finished: `pending` and `failed`
// (both are work the tier still owes).
func (q Queue) Depth() int64 {
	var n int64
	for _, j := range q.jobs {
		if j.State == "pending" || j.State == "failed" || j.State == "" {
			n++
		}
	}
	return n
}

// Pending lists the unfinished jobs.
func (q Queue) Pending() []archiveJob {
	var out []archiveJob
	for _, j := range q.jobs {
		if j.State == "pending" || j.State == "failed" || j.State == "" {
			out = append(out, j)
		}
	}
	return out
}

// LastTS is the timestamp of the newest queue line ("" when the queue is empty).
func (q Queue) LastTS() string {
	if len(q.jobs) == 0 {
		return ""
	}
	last := q.jobs[len(q.jobs)-1]
	return last.TS
}

// Skipped counts malformed interior lines, so a corrupted queue is visible.
func (q Queue) Skipped() int { return q.skipped }

func appendJob(stateRoot string, job archiveJob) error {
	if stateRoot == "" {
		return errf(types.CodeHub009, ReasonArchiveCfg, "the archive queue has no state root")
	}
	if err := EnsureStateDirs(stateRoot); err != nil {
		return err
	}
	b, err := json.Marshal(job)
	if err != nil {
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot encode queue job", err)
	}
	f, err := os.OpenFile(QueuePath(stateRoot), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot open the archive queue", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot append the queue job", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot fsync the archive queue", err)
	}
	return f.Close()
}

// ScanGenerations lists the closed generations of a ledger directory.
//
// Closure is the spec's archival precondition (§3.5 step 1, 011 when a generation
// is not closed). Two evidentiary forms are accepted, in this order:
//
//   - `idx`: a `<file>.idx` sidecar exists and its Bytes/Sha256 agree with the
//     file. SPEC-01 §3.7a says "an `.idx` never gets to describe a file it does
//     not match", so a disagreeing sidecar is a refusal, not a closure.
//   - `not_live`: no sidecar exists and the file is not the writer's current
//     file. internal/ledger in this tree does not yet write sidecars (SPEC-01
//     §3.7a's writer half is unimplemented), so refusing every generation without
//     one would make the archival tier permanently dead — a refusal that proves
//     nothing. The form used is reported per generation, so an operator can see
//     which evidence licensed the export instead of assuming the stronger one.
func ScanGenerations(ledgerRoot, liveFile string) ([]GenerationFile, error) {
	if ledgerRoot == "" {
		return nil, errf(types.CodeHub009, ReasonArchiveCfg, "the ledger root is not configured")
	}
	entries, err := os.ReadDir(ledgerRoot)
	if err != nil {
		return nil, errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot list the ledger dir", err)
	}
	var out []GenerationFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !genRe.MatchString(name) {
			continue
		}
		if name == liveFile {
			continue
		}
		g, err := describeGeneration(filepath.Join(ledgerRoot, name), name)
		if err != nil {
			return nil, err
		}
		if idx, ok := readSidecar(filepath.Join(ledgerRoot, name+".idx")); ok {
			if idx.Bytes != 0 && idx.Bytes != g.Bytes {
				return nil, errf(types.CodeHub011, ReasonUnclosed,
					"%s: sidecar reports %d bytes, file has %d", name, idx.Bytes, g.Bytes)
			}
			if idx.Sha256 != "" && idx.Sha256 != g.SHA256 {
				return nil, errf(types.CodeHub011, ReasonUnclosed,
					"%s: sidecar sha256 does not match the file", name)
			}
			g.Closure = "idx"
		} else {
			g.Closure = "not_live"
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// sidecar is the minimal SPEC-01 §3.7a sidecar shape the hub reads. Unknown
// fields are ignored (the sidecar is the ledger's, not the hub's).
type sidecar struct {
	Bytes  int64  `json:"Bytes"`
	Sha256 string `json:"sha256"`
}

func readSidecar(path string) (sidecar, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return sidecar{}, false
	}
	var s sidecar
	if err := json.Unmarshal(b, &s); err != nil {
		return sidecar{}, false
	}
	return s, true
}

// describeGeneration stats a file and derives its content identity and seq/ts
// range with the tolerant reader (a malformed line is skipped, never guessed).
func describeGeneration(path, name string) (GenerationFile, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return GenerationFile{}, errWrap(types.CodeHub011, ReasonUnclosed, "cannot stat "+name, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return GenerationFile{}, errWrap(types.CodeHub011, ReasonUnclosed, "cannot read "+name, err)
	}
	g := GenerationFile{
		File:    name,
		Path:    path,
		Bytes:   fi.Size(),
		ModTime: types.FormatUTC(fi.ModTime()),
		SHA256:  Sha256Hex(b),
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Seq uint64 `json:"seq"`
			TS  string `json:"ts"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		g.Records++
		if g.FirstSeq == 0 || rec.Seq < g.FirstSeq {
			g.FirstSeq = rec.Seq
		}
		if rec.Seq > g.LastSeq {
			g.LastSeq = rec.Seq
		}
		if rec.TS != "" {
			if g.MinTS == "" || rec.TS < g.MinTS {
				g.MinTS = rec.TS
			}
			if rec.TS > g.MaxTS {
				g.MaxTS = rec.TS
			}
		}
	}
	return g, nil
}

// PlanArchive is SPEC-13 §2.3's `PlanArchive`: every closed generation with no
// `exported` marker, with its content-derived marker id and object key.
//
// It refuses (011, class permanent) rather than guesses when it cannot tell a
// closed generation from the live one — an unknown live file with candidates
// present is exactly that case.
func PlanArchive(stateRoot string, cfg ArchiveConfig) ([]ArchivePlan, error) {
	cfg = cfg.WithDefaults()
	gens, err := ScanGenerations(cfg.LedgerRoot, cfg.LiveFile)
	if err != nil {
		return nil, err
	}
	if len(gens) > 0 && cfg.LiveFile == "" {
		return nil, errf(types.CodeHub011, ReasonUnclosed,
			"the live ledger file is unknown and %d ledger file(s) exist: refusing to treat any as closed", len(gens))
	}
	store := NewMarkerStore(stateRoot)
	marks, _, merr := store.Load()
	if merr != nil {
		return nil, merr
	}
	exported := map[string]string{} // marker id → state
	for _, mk := range marks {
		exported[mk.MarkerID] = mk.State
	}
	var out []ArchivePlan
	for _, g := range gens {
		mid := g.SHA256
		if len(mid) > 16 {
			mid = mid[:16]
		}
		switch exported[mid] {
		case "exported", "verified", "dropped":
			// Already accounted for: the queue is not re-planned (SPEC-13 §5,
			// TROUBLE-HUB-010: "no re-planning is needed (marker ids are
			// content-derived)").
			continue
		}
		gz := int64(0)
		if cfg.Gzip {
			gz = gzipSize(g.Path)
		}
		out = append(out, ArchivePlan{
			File:      g.File,
			Path:      g.Path,
			Bytes:     g.Bytes,
			GzipBytes: gz,
			Records:   g.Records,
			FirstSeq:  g.FirstSeq,
			LastSeq:   g.LastSeq,
			MinTS:     g.MinTS,
			MaxTS:     g.MaxTS,
			MarkerID:  mid,
			SHA256:    g.SHA256,
			Namespace: cfg.Namespace,
			ObjectKey: cfg.ObjectKey(g.File),
			Closure:   g.Closure,
		})
	}
	return out, nil
}

func gzipSize(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return 0
	}
	if _, err := io.Copy(zw, f); err != nil {
		return 0
	}
	if err := zw.Close(); err != nil {
		return 0
	}
	return int64(buf.Len())
}

// Archiver runs the export/verify/mark half of SPEC-13 §3.5 for one generation.
type Archiver struct {
	cfg     ArchiveConfig
	target  Target
	markers *MarkerStore
	w       Appender // optional: writes the lifecycle{op:archive_exported} record
	now     func() time.Time

	Exported atomic.Int64
	Verified atomic.Int64
	Failed   atomic.Int64
	Paused   atomic.Int64
}

// NewArchiver builds the archival job runner. A nil target is legal: it is the
// state the loader reports as TROUBLE-HUB-009 (an unusable archival target), and
// the queue simply keeps every pending generation (SPEC-13 §5).
func NewArchiver(cfg ArchiveConfig, t Target, w Appender) *Archiver {
	return &Archiver{cfg: cfg.WithDefaults(), target: t, markers: NewMarkerStore(cfg.StateRoot), w: w, now: time.Now}
}

// Archive is SPEC-13 §2.3's `Archive`: apply one plan.
func Archive(ctx context.Context, cfg ArchiveConfig, plan ArchivePlan) (types.LedgerArchiveMarker, error) {
	a := NewArchiver(cfg, nil, nil)
	t, err := NewTarget(cfg)
	if err != nil {
		return types.LedgerArchiveMarker{}, err
	}
	a.target = t
	return a.Archive(ctx, plan)
}

// Archive exports, verifies and marks one planned generation.
func (a *Archiver) Archive(ctx context.Context, plan ArchivePlan) (types.LedgerArchiveMarker, error) {
	if a.cfg.StateRoot == "" {
		// Refused before any write: the marker file and the queue live under the
		// state root, and an empty one would resolve to the working directory.
		return types.LedgerArchiveMarker{}, errf(types.CodeHub009, ReasonArchiveCfg,
			"the hub state root is empty: refusing to write archival state")
	}
	if a.target == nil {
		a.Paused.Add(1)
		return types.LedgerArchiveMarker{}, errf(types.CodeHub009, ReasonArchiveCfg,
			"no archival target: configure server.duckbrain.namespace and server.duckbrain.endpoint (SPEC-13 §3.5 step 2)")
	}
	// Step 1b: the file must still be the file that was planned. A generation
	// whose bytes changed under the plan (a re-compaction) is refused, never
	// exported under the old marker id (§5, TROUBLE-HUB-011).
	raw, err := os.ReadFile(plan.Path)
	if err != nil {
		return types.LedgerArchiveMarker{}, errWrap(types.CodeHub011, ReasonUnclosed,
			"generation "+plan.File+" is not readable", err)
	}
	sum := Sha256Hex(raw)
	if int64(len(raw)) != plan.Bytes || sum != plan.SHA256 {
		return types.LedgerArchiveMarker{}, errf(types.CodeHub011, ReasonUnclosed,
			"generation %s changed between plan and export (bytes %d→%d)", plan.File, plan.Bytes, len(raw))
	}

	marker := types.LedgerArchiveMarker{
		MarkerID:  plan.MarkerID,
		File:      plan.File,
		Namespace: a.cfg.Namespace,
		ObjectKey: plan.ObjectKey,
		Bytes:     int64(len(raw)),
		Sha256:    sum,
		Records:   plan.Records,
		FirstSeq:  plan.FirstSeq,
		LastSeq:   plan.LastSeq,
		MinTS:     plan.MinTS,
		MaxTS:     plan.MaxTS,
		State:     "pending",
		TS:        types.FormatUTC(a.now()),
	}

	exported := raw
	if a.cfg.Gzip {
		gz, gerr := gzipBytes(raw)
		if gerr != nil {
			marker.State = "failed"
			marker.ErrorCode = string(types.CodeHub012)
			_ = a.markers.Append(marker)
			return marker, errWrap(types.CodeHub012, ReasonVerify, "cannot gzip "+plan.File, gerr)
		}
		exported = gz
	}
	marker.GzipBytes = int64(len(exported))

	// Step 2: export.
	if err := a.target.Put(ctx, plan.ObjectKey, exported); err != nil {
		marker.State = "failed"
		marker.ErrorCode = string(types.CodeHub010)
		_ = a.markers.Append(marker)
		_ = appendJob(a.cfg.StateRoot, jobFromMarker(marker, "failed"))
		a.Failed.Add(1)
		return marker, wrapTargetErr(err, "export "+plan.ObjectKey)
	}
	// The sidecar object is written only when the ledger produced one: the hub
	// never invents a sidecar it does not have (SPEC-01 §3.7a's writer half).
	if idx := filepath.Join(filepath.Dir(plan.Path), plan.File+".idx"); fileExists(idx) {
		if b, rerr := os.ReadFile(idx); rerr == nil {
			if err := a.target.Put(ctx, a.cfg.SidecarKey(plan.File), b); err != nil {
				a.Failed.Add(1)
				marker.State = "failed"
				marker.ErrorCode = string(types.CodeHub010)
				_ = a.markers.Append(marker)
				return marker, wrapTargetErr(err, "export sidecar")
			}
		}
	}
	marker.State = "exported"

	// Step 3: verify.
	if a.cfg.VerifyAfterWriting {
		back, err := a.target.Get(ctx, plan.ObjectKey)
		if err != nil {
			marker.ErrorCode = string(types.CodeHub010)
			_ = a.markers.Append(marker)
			_ = appendJob(a.cfg.StateRoot, jobFromMarker(marker, "pending"))
			a.Failed.Add(1)
			return marker, wrapTargetErr(err, "verify "+plan.ObjectKey)
		}
		backRaw := back
		if a.cfg.Gzip {
			if un, uerr := gunzipBytes(back); uerr == nil {
				backRaw = un
			} else {
				backRaw = nil
			}
		}
		if int64(len(back)) != marker.GzipBytes || Sha256Hex(backRaw) != marker.Sha256 {
			marker.ErrorCode = string(types.CodeHub012)
			_ = a.markers.Append(marker) // state stays pending/exported, never verified
			_ = appendJob(a.cfg.StateRoot, jobFromMarker(marker, "pending"))
			a.Failed.Add(1)
			return marker, errf(types.CodeHub012, ReasonVerify,
				"read-back verification failed for %s: object does not match its marker", plan.ObjectKey)
		}
		marker.State = "verified"
		marker.VerifiedTS = types.FormatUTC(a.now())
		a.Verified.Add(1)
	} else {
		a.Exported.Add(1)
	}

	// Step 4: mark. The marker file is the durable claim; the lifecycle record
	// goes to the ledger through the appender when one is attached. A repeat of
	// the same (marker_id, state) transition appends nothing: a re-export of the
	// same content is the SAME statement (content-derived ids make that true).
	fresh, err := a.markers.AppendTransition(marker)
	if err != nil {
		return marker, err
	}
	if fresh {
		if err := appendJob(a.cfg.StateRoot, jobFromMarker(marker, marker.State)); err != nil {
			return marker, err
		}
	}
	if a.w != nil {
		_, _ = a.w.Append(ctx, types.RecordDraft{
			Kind:   types.KLifecycle,
			Origin: types.Origin{HostID: a.cfg.Namespace, Source: "hub"},
			Actor:  types.Actor{Kind: types.ActorDaemon, ID: "hub-archiver"},
			Payload: map[string]any{
				"op":         "archive_exported",
				"file":       marker.File,
				"marker_id":  marker.MarkerID,
				"namespace":  marker.Namespace,
				"object_key": marker.ObjectKey,
				"bytes":      marker.Bytes,
				"gzip_bytes": marker.GzipBytes,
				"sha256":     marker.Sha256,
				"state":      marker.State,
			},
		})
	}
	return marker, nil
}

// DroppableGenerations is SPEC-13 §3.5 step 5's gate: the generations the
// retention sweep MAY delete.
//
// A generation is droppable only when it has a marker whose state is `verified`
// — "history outlives the hot host or it does not leave". `keep` newest
// generations are retained locally regardless.
//
// Two different non-verified states are deliberately NOT the same thing:
//
//   - NO marker: the generation has never been planned, so retention simply
//     retains it — that is what "archival is on" means, and reporting a refusal
//     for it would emit a TROUBLE-HUB-014 record per pass for the normal backlog.
//   - A marker that is not `verified` (pending/exported/failed, or `dropped` for
//     an already-dropped generation's re-scan): an export was attempted and is
//     NOT trustworthy, so the drop is refused with TROUBLE-HUB-014 and the
//     refused files are named. The caller must record the refusal (one
//     `lifecycle{op:"retention_deferred"}` record naming the file and the marker
//     state) and must drop exactly the returned set, never the blocked files.
func DroppableGenerations(stateRoot string, keep int) ([]GenerationFile, error) {
	ledgerRoot := LedgerRootFromState(stateRoot)
	gens, err := ScanGenerations(ledgerRoot, "")
	if err != nil {
		return nil, err
	}
	return droppable(gens, stateRoot, keep)
}

// DroppableGenerationsIn is DroppableGenerations with an explicit ledger root
// (the state root does not always hold the ledger: the composition root resolves
// both, and this keeps that resolution out of the gate).
func DroppableGenerationsIn(ledgerRoot, stateRoot, liveFile string, keep int) ([]GenerationFile, error) {
	gens, err := ScanGenerations(ledgerRoot, liveFile)
	if err != nil {
		return nil, err
	}
	return droppable(gens, stateRoot, keep)
}

// LedgerRootFromState is the conventional location (`<state_root>/ledger`),
// used when a caller only has the state root (SPEC-12 §3.2).
func LedgerRootFromState(stateRoot string) string { return filepath.Join(stateRoot, "ledger") }

func droppable(gens []GenerationFile, stateRoot string, keep int) ([]GenerationFile, error) {
	if keep < 0 {
		keep = 0
	}
	// Newest `keep` generations stay on the hot host, regardless of verification.
	sorted := append([]GenerationFile(nil), gens...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].File > sorted[j].File })
	retained := map[string]bool{}
	for i := 0; i < len(sorted) && i < keep; i++ {
		retained[sorted[i].File] = true
	}
	store := NewMarkerStore(stateRoot)
	var out []GenerationFile
	var blocked []string
	for _, g := range gens {
		if retained[g.File] {
			continue
		}
		mk, ok := store.Latest(g.File)
		if !ok {
			// Never planned: retained, not refused (see the doc comment).
			continue
		}
		if mk.State == "dropped" {
			// Already accounted for by a drop marker; nothing to drop again.
			continue
		}
		if mk.State != "verified" {
			blocked = append(blocked, g.File+" (marker "+mk.State+")")
			continue
		}
		g.Droppable = true
		out = append(out, g)
	}
	if len(blocked) > 0 {
		return out, errf(types.CodeHub014, ReasonDrop,
			"retention refused a drop for %s: only a verified export licenses it", strings.Join(blocked, ", "))
	}
	return out, nil
}

// MarkDropped appends the `dropped` marker a deleted generation leaves behind
// (SPEC-13 §3.5 step 5: "which appends a `dropped` marker"). A marker is never
// rewritten and never deleted, so the drop is a new line, not an edit.
func MarkDropped(stateRoot string, g GenerationFile, now time.Time) error {
	store := NewMarkerStore(stateRoot)
	mk, ok := store.Latest(g.File)
	if !ok || mk.State != "verified" {
		return errf(types.CodeHub014, ReasonDrop,
			"%s has no verified marker: the drop is refused", g.File)
	}
	dropped := mk
	dropped.State = "dropped"
	dropped.TS = types.FormatUTC(now)
	dropped.ErrorCode = ""
	fresh, err := store.AppendTransition(dropped)
	if err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	return appendJob(stateRoot, jobFromMarker(dropped, "dropped"))
}

func jobFromMarker(mk types.LedgerArchiveMarker, state string) archiveJob {
	return archiveJob{
		MarkerID:  mk.MarkerID,
		File:      mk.File,
		Bytes:     mk.Bytes,
		GzipBytes: mk.GzipBytes,
		Records:   mk.Records,
		State:     state,
		Namespace: mk.Namespace,
		ObjectKey: mk.ObjectKey,
		ErrorCode: mk.ErrorCode,
		TS:        mk.TS,
	}
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func gzipSizeFromBytes(raw []byte) int64 {
	b, err := gzipBytes(raw)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// wrapTargetErr classifies a target failure: a config/namespace problem is 009
// (permanent, "archival does not start"), an unreachable or erroring tier is 010
// (transient, the job stays pending and is retried with the same marker id).
func wrapTargetErr(err error, what string) *Error {
	if err == nil {
		return nil
	}
	var he *Error
	if errors.As(err, &he) {
		return he
	}
	return errWrap(types.CodeHub010, ReasonDuckBrain, what, err)
}
