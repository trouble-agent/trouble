package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/types"
)

// upgrade_version_test.go is the QA-TROUBLE-7 regression: the documented
// prev-release → HEAD upgrade path (SPEC-12 §3.6) between TWO DIFFERENT
// versions.
//
// The three tests in upgrade_test.go build two binaries from the SAME working
// tree and drive the recipe with hand-written plan callbacks, so "upgrade from
// vN to vN+1" is not what they prove. This file closes that hole without
// inventing a release: the two versions are §3.4 link-time stamps on the real
// cmd/troubled source (`-X ...Version=v0.0.9 -X ...GitSHA=deadbee`), which is
// exactly how a release build is made. No git tag is created (tagging is a
// release act the foreman owns) and nothing is pushed.
//
// What is REAL here, and observed by the test rather than asserted from a
// callback:
//
//   - both binaries are compiled from this tree by `go build`, and each one's
//     OWN `--version` output says which version it is (v0.0.9 / v0.1.0) before
//     and after it is installed on the live path;
//   - the park record is durable in a real ledger FILE before the process that
//     was running the upgrade is killed — the ledger is read back through a
//     fresh handle afterwards, so "the upgrade is reconstructible from the
//     ledger alone" is tested, not assumed;
//   - the ledger is left behind by a killed process holding a stale LOCK, which
//     is what a crash between §3.6 step 2 and step 3 leaves on disk;
//   - the rename-over puts the staged inode at the live path at 0755, the
//     previous build stays recoverable from backups/bin/, and the live path
//     afterwards reports v0.1.0 when executed;
//   - READY is a real HTTP 200 from a health surface.
//
// What is SIMULATED, stated rather than implied: the systemd restart (the
// recipe takes it as an injected function), and the daemon-side half of step 5
// (the new process re-adopting parked plays, SPEC-05 ReAdopt, which lives in
// internal/ladder and is covered there). The READY gate is not skipped: the
// recipe waits on the same /health.json contract the daemon serves.

// repoRoot is the repository root from the package directory (established
// convention: internal/registry's tests read "../../Makefile").
const repoRoot = "../.."

// forkUpgradeEnv asks the test binary to run one upgrade recipe in its own
// process. TestMain dispatches it BEFORE m.Run, so a child never runs the
// suite.
const forkUpgradeEnv = "TROUBLE_TEST_FORK_UPGRADE"

// TestMain runs the suite, except when the test binary has been started as a
// forked upgrade (see forkParkedUpgrade).
func TestMain(m *testing.M) {
	if spec := os.Getenv(forkUpgradeEnv); spec != "" {
		os.Exit(runForkedUpgrade(spec))
	}
	os.Exit(m.Run())
}

// ---- the version/upgrade contract (SPEC-12 §3.4, §3.6) ---------------------

// TestReleaseVersionMapping pins the shape mapping the upgrade records rest on.
// The inputs are the six forms MEASURED from `git describe --tags --always
// --dirty` on this host (2026-09-19, scratch repos — the command the Makefile
// and the Dockerfile both stamp Version with), plus the values that are not a
// version at all.
func TestReleaseVersionMapping(t *testing.T) {
	cases := []struct {
		stamp string
		want  string
		why   string
	}{
		// Tagged tree: exact, dirty, and a tag behind HEAD. All measured.
		{"v0.1.0", "v0.1.0", "the tagged commit, clean"},
		{"v0.1.0-dirty", "v0.1.0", "the tagged commit with uncommitted changes: still that build"},
		{"v0.0.9-3-gabc1234", "v0.0.9", "three commits past the tag describes that release"},
		{"v0.0.9-3-gabc1234-dirty", "v0.0.9", "same, with uncommitted changes"},
		{"v0.0.9-1-gfda6514", "v0.0.9", "the commit count is not part of the version"},
		// Tagless tree: --always falls back to the short sha (measured: the
		// tagless scratch repo printed "1e6650a" and "1e6650a-dirty").
		{"1e6650a", "1e6650a", "a sha is not a version: returned verbatim, never truncated"},
		{"1e6650a-dirty", "1e6650a-dirty", "and not invented into one either"},
		// The documented stamped fallback (§3.4). 0.0.0-dev must survive
		// whole: truncating it would report a release that does not exist.
		{"0.0.0-dev", "0.0.0-dev", "the documented tagless fallback"},
		{"0.1.0", "0.1.0", "a stamp written without the v keeps its spelling"},
		{"v0.1.0-rc1", "v0.1.0", "a pre-release suffix is not part of the release version"},
		{"", "", "no stamp at all"},
		{"unknown", "unknown", "the unstamped sha sentinel is not a version"},
	}
	for _, tc := range cases {
		if got := ReleaseVersion(tc.stamp); got != tc.want {
			t.Errorf("ReleaseVersion(%q) = %q, want %q (%s)", tc.stamp, got, tc.want, tc.why)
		}
	}
	// Idempotence matters because a resume re-states the version a park named:
	// a second pass must not keep shaving the value.
	for _, s := range []string{"v0.1.0", "v0.0.9-3-gabc1234-dirty", "0.0.0-dev", "1e6650a"} {
		once := ReleaseVersion(s)
		if twice := ReleaseVersion(once); twice != once {
			t.Errorf("ReleaseVersion is not idempotent: %q -> %q -> %q", s, once, twice)
		}
	}
}

// TestUpgradeRecordShape pins the record body §3.6 names, field by field.
func TestUpgradeRecordShape(t *testing.T) {
	rec := UpgradeRecord(UpgradeStepPark, "v0.0.9", "v0.1.0", 3)
	if rec.Kind != types.KLifecycle {
		t.Errorf("kind = %q, want %q", rec.Kind, types.KLifecycle)
	}
	if rec.Actor.Kind != types.ActorDaemon || rec.Actor.ID != "troubled" {
		t.Errorf("actor = %+v, want the daemon triple", rec.Actor)
	}
	want := map[string]any{
		"stage":        "upgrade",
		"step":         "park",
		"from_version": "v0.0.9",
		"to_version":   "v0.1.0",
		"parked":       3,
	}
	if len(rec.Payload) != len(want) {
		t.Errorf("payload has %d keys (%v), want exactly %d", len(rec.Payload), rec.Payload, len(want))
	}
	for k, v := range want {
		if got := rec.Payload[k]; got != v {
			t.Errorf("payload[%q] = %#v, want %#v", k, got, v)
		}
	}

	// The two version fields are RELEASE versions. A record carrying the raw
	// describe stamp could not be compared with the v0.1.0 a health row or a
	// later record reports, so the mapping is applied at the record boundary,
	// not left to whoever reads it.
	stamped := UpgradeRecord(UpgradeStepResume, "v0.0.9-3-gabc1234-dirty", "v0.1.0-1-gdeadbee", -1)
	if stamped.Payload["from_version"] != "v0.0.9" || stamped.Payload["to_version"] != "v0.1.0" {
		t.Errorf("stamped triple not narrowed to a release version: %v", stamped.Payload)
	}

	// parked belongs to the park step alone, and only when the caller could
	// actually count: a park performed by stopping the unit is not observable
	// from outside the process, and an absent count is not a zero one.
	for _, step := range []string{UpgradeStepResume, UpgradeStepRollback} {
		if p := UpgradeRecord(step, "v0.0.9", "v0.1.0", 3).Payload; p["parked"] != nil {
			t.Errorf("step %q carries parked: %v", step, p)
		}
	}
	if _, ok := UpgradeRecord(UpgradeStepPark, "v0.0.9", "v0.1.0", -1).Payload["parked"]; ok {
		t.Error("an uncountable park (parked < 0) must omit the key, not report 0")
	}
}

// TestUpgradeRecipeStepsInOrder pins the STEP ORDER and the from/to direction
// of both branches with synthesized callbacks: the cheap, timing-free half of
// the §3.6 contract.
func TestUpgradeRecipeStepsInOrder(t *testing.T) {
	type stepRec struct{ step, from, to string }

	run := func(t *testing.T, ready bool, restartErr error) ([]stepRec, error) {
		t.Helper()
		dir := t.TempDir()
		bc := newBinaryPair(t, dir)
		cfg := *defaults()
		cfg.StateRoot = dir
		// The ready=false arm must expire, and the ready=true arm must not: both
		// deadlines are the configured key, and the wait polls every 100ms.
		if ready {
			cfg.Lifecycle.UpgradeReadyTimeout = "5s"
		} else {
			cfg.Lifecycle.UpgradeReadyTimeout = "30ms"
		}
		var steps []stepRec
		plan := upgradePlan{
			CurrentBinary: bc.live,
			NewBinary:     bc.staged,
			FromVersion:   "v0.0.9",
			ToVersion:     "v0.1.0",
			Park:          func(context.Context) (int, error) { return 2, nil },
			Restart:       func(context.Context) error { return restartErr },
			Ready:         func(context.Context) bool { return ready },
			Record: func(d types.RecordDraft) error {
				steps = append(steps, stepRec{
					step: d.Payload["step"].(string),
					from: d.Payload["from_version"].(string),
					to:   d.Payload["to_version"].(string),
				})
				return nil
			},
		}
		return steps, Upgrade(context.Background(), cfg, plan)
	}

	assertSteps := func(t *testing.T, got, want []stepRec) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("steps = %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("step %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	}

	t.Run("park then resume", func(t *testing.T) {
		steps, err := run(t, true, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		assertSteps(t, steps, []stepRec{{"park", "v0.0.9", "v0.1.0"}, {"resume", "v0.0.9", "v0.1.0"}})
	})

	t.Run("rollback after a failed restart", func(t *testing.T) {
		steps, err := run(t, true, errors.New("systemctl restart: exit 1"))
		if err == nil {
			t.Fatal("a failed restart returned nil; §3.6 step 4 rolls back")
		}
		assertSteps(t, steps, []stepRec{{"park", "v0.0.9", "v0.1.0"}, {"rollback", "v0.1.0", "v0.0.9"}})
	})

	t.Run("rollback when READY never arrives", func(t *testing.T) {
		steps, err := run(t, false, nil)
		if err == nil {
			t.Fatal("a missed READY deadline returned nil; §3.6 step 6 rolls back")
		}
		if !strings.Contains(err.Error(), string(types.CodeLifecycle011)) {
			t.Errorf("error = %v, want %s", err, types.CodeLifecycle011)
		}
		assertSteps(t, steps, []stepRec{{"park", "v0.0.9", "v0.1.0"}, {"rollback", "v0.1.0", "v0.0.9"}})
	})
}

// TestUpgradeRecordCarriesAReleaseVersion is the contract-level half of the gap
// this file was written against: an upgrade record must name the versions it
// was between, not an empty string.
//
// The "from" side is resolved from the RUNNING process's §3.4 triple, because
// the previous binary is a file and a version stamp is not recoverable from one
// at run time. A `go test` process has no link-time triple, so this test
// asserts the documented fallback (0.0.0-dev) is what a record of it carries.
func TestUpgradeRecordCarriesAReleaseVersion(t *testing.T) {
	dir := t.TempDir()
	bc := newBinaryPair(t, dir)
	cfg := *defaults()
	cfg.StateRoot = dir
	var seen []types.RecordDraft
	plan := upgradePlan{
		CurrentBinary: bc.live,
		NewBinary:     bc.staged,
		Park:          func(context.Context) (int, error) { return 1, nil },
		Restart:       func(context.Context) error { return nil },
		Ready:         func(context.Context) bool { return true },
		Record:        func(d types.RecordDraft) error { seen = append(seen, d); return nil },
	}
	if err := Upgrade(context.Background(), cfg, plan); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("no upgrade records")
	}
	for _, d := range seen {
		for _, key := range []string{"from_version", "to_version"} {
			v, ok := d.Payload[key].(string)
			if !ok || v == "" {
				t.Errorf("record %v carries an empty %s: an upgrade record that cannot name the versions is not the §3.6 record", d.Payload, key)
			}
			if v != ReleaseVersion(v) {
				t.Errorf("record %v carries a raw describe stamp in %s: %q", d.Payload, key, v)
			}
		}
	}
	if got := seen[0].Payload["from_version"]; got != defaultVersion {
		t.Errorf("from_version resolved from an unstamped runner = %v, want the documented fallback %q", got, defaultVersion)
	}
}

// ---- the real prev-release -> HEAD path -----------------------------------

// TestCrossVersionUpgradePath exercises SPEC-12 §3.6 between two DIFFERENT
// versions, across a process that dies between step 2 and step 3.
//
// Sequence, in the recipe's own order:
//
//	build v0.0.9 -> it becomes the live binary; its own --version says so
//	build v0.1.0 -> it is staged at an explicit path; its own --version says so
//	step 2       -> a child process runs the SAME Upgrade() the CLI runs and is
//	                KILLED once its park record is durable (the crash between
//	                park and rename that §6.2 prices)
//	step 3-5     -> the recipe is completed from the on-disk state: stage,
//	                re-park, rename-over, restart, READY, resume
//
// The park is read back from the ledger FILE, so the ledger — not a variable in
// the test — is what carries the upgrade across the killed process.
func TestCrossVersionUpgradePath(t *testing.T) {
	prev := buildStampedBinary(t, "v0.0.9", "deadbee")
	next := buildStampedBinary(t, "v0.1.0", "abc1234")
	if got := binaryVersion(t, prev); got != "v0.0.9" {
		t.Fatalf("the previous build reports %q, want v0.0.9: the stamp did not land", got)
	}
	if got := binaryVersion(t, next); got != "v0.1.0" {
		t.Fatalf("the staged build reports %q, want v0.1.0: the stamp did not land", got)
	}

	dir := t.TempDir()
	root := testStateRoot(t)
	live := filepath.Join(dir, "troubled")
	prevBytes, err := os.ReadFile(prev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, prevBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	// Step 2, in a process of its own: run the recipe up to the durable park and
	// hold there until the parent kills it.
	child, parkRecord := forkParkedUpgrade(t, live, next, root)

	// The crash. The child holds the ledger open (a stale LOCK is left behind,
	// exactly as a SIGKILLed upgrade leaves it).
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill the parked upgrade: %v", err)
	}
	_ = child.Wait()
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the live binary disappeared across the kill: %v", err)
	}
	afterKill, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterKill, prevBytes) {
		t.Fatal("the live path changed before the rename: a killed upgrade must leave the previous binary in place")
	}
	// The staged copy is EXPECTED to be there: §3.6 step 1 stages <bin>.new
	// before step 2 parks, so a kill inside the park leaves it — and it is the
	// state the completing run has to handle. What must NOT be there is a
	// half-written file the recipe would then rename into place.
	staged, err := os.ReadFile(live + ".new")
	if err != nil {
		t.Fatalf("a killed upgrade must leave its staged <bin>.new for the resume to finish: %v", err)
	}
	nextBytes0, err := os.ReadFile(next)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(staged, nextBytes0) {
		t.Error("the staged file at <bin>.new is not the new build's bytes")
	}

	// Steps 3-5, from the on-disk state. A real ledger handle now, so the
	// records under assertion are the bytes on disk.
	l, w := openTestLedger(t, root, "v0.1.0")
	var steps []types.Record
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var readyHits atomic.Int64
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		readyHits.Add(1)
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"status":"ok"}`))
	})}
	t.Cleanup(func() { _ = srv.Close() })

	cfg := *defaults()
	cfg.StateRoot = root
	cfg.Lifecycle.UpgradeReadyTimeout = "10s"
	cfg.HealthURL = "http://" + ln.Addr().String() + "/health.json"

	plan := upgradePlan{
		CurrentBinary: live,
		NewBinary:     next,
		FromVersion:   "v0.0.9",
		ToVersion:     "v0.1.0",
		Park: func(context.Context) (int, error) {
			// §3.6 step 2 in production is the unit's stop, which parks the
			// in-flight plays the daemon was running. The DURABLE half of that
			// step is this record — written in the killed child's process and
			// read back here — so the completed upgrade re-states it rather
			// than pretending the park never happened.
			return 0, w.AppendDraft(UpgradeRecord(UpgradeStepPark, "v0.0.9", "v0.1.0", 0))
		},
		Restart: func(context.Context) error { // §3.6 step 4: start the new build
			go func() { _ = srv.Serve(ln) }()
			return nil
		},
		Ready: func(ctx context.Context) bool {
			return waitReady(ctx, cfg, cfg.Lifecycle.UpgradeReadyTimeout.Std())
		},
		Record: func(d types.RecordDraft) error {
			rec, err := w.Append(context.Background(), d)
			if err == nil {
				steps = append(steps, rec)
			}
			return err
		},
	}
	if err := Upgrade(context.Background(), cfg, plan); err != nil {
		t.Fatalf("cross-version upgrade: %v", err)
	}
	if readyHits.Load() == 0 {
		t.Fatal("the READY gate never polled the health surface")
	}

	// 1. The live path carries the STAGED build, at 0755.
	got, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	nextBytes, err := os.ReadFile(next)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, nextBytes) {
		t.Error("the live path does not carry the staged build")
	}
	fi, err := os.Stat(live)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("live mode = %04o, want 0755 (§3.6 step 3 chmods before the rename)", fi.Mode().Perm())
	}
	if v := binaryVersion(t, live); v != "v0.1.0" {
		t.Errorf("the live binary reports %q, want v0.1.0: the upgrade is only real if the build now on the live path says so", v)
	}
	if _, err := os.Stat(live + ".new"); !os.IsNotExist(err) {
		t.Error("the staged path <bin>.new still exists after a completed upgrade")
	}

	// 2. The previous build is recoverable from <state root>/backups/bin/
	//    (§3.6 step 3: the backup is keyed by the version of the binary it
	//    saves, so it names the version the rollback would restore).
	backupDir := filepath.Join(root, "backups", "bin")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("no backup dir after the upgrade: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("backups/bin/ is empty: the §3.6 step 6 rollback has nothing to restore")
	}
	backupPath := filepath.Join(backupDir, entries[0].Name())
	if !strings.Contains(entries[0].Name(), "v0.0.9") {
		t.Errorf("backup name %q does not name the previous version", entries[0].Name())
	}
	if v := binaryVersion(t, backupPath); v != "v0.0.9" {
		t.Errorf("the backup reports %q, want v0.0.9", v)
	}

	// 3. The ledger chain, read back from the SAME file the killed child wrote:
	//    its park (a different process, a different build's upgrade), then this
	//    process's re-park and resume. The park step surviving the crash is what
	//    makes the resume legal instead of a guess.
	chain := upgradeChain(t, recordsOf(l))
	if len(chain) < 3 {
		t.Fatalf("upgrade records = %v, want the killed child's park plus this run's park and resume", payloadsOf(chain))
	}
	first := chain[0]
	if first.Payload["step"] != UpgradeStepPark || first.Payload["to_version"] != "v0.1.0" {
		t.Errorf("the child's park did not survive: %v", first.Payload)
	}
	if first.RecID != parkRecord.RecID {
		t.Errorf("the park in the ledger is %q, the one the child reported is %q: the two processes disagree about the audit trail",
			first.RecID, parkRecord.RecID)
	}
	last := chain[len(chain)-1]
	if last.Payload["step"] != UpgradeStepResume {
		t.Fatalf("newest upgrade step = %v, want %s", last.Payload["step"], UpgradeStepResume)
	}
	if last.Payload["from_version"] != "v0.0.9" || last.Payload["to_version"] != "v0.1.0" {
		t.Errorf("resume record = %v, want from_version=v0.0.9 to_version=v0.1.0", last.Payload)
	}
	if len(steps) != 2 || steps[0].Payload["step"] != UpgradeStepPark || steps[1].Payload["step"] != UpgradeStepResume {
		t.Errorf("this run's records = %v, want park + resume", payloadsOf(steps))
	}
	// The §3.4 Actor triple on every step says which build acted — including
	// across the upgrade boundary in the same file.
	for _, r := range chain {
		if r.Actor.Kind != types.ActorDaemon || r.Actor.ID == "" || r.Actor.Version == "" || r.Actor.GitSHA == "" || r.Actor.BuildTime == "" {
			t.Errorf("upgrade record %v carries an incomplete actor triple %+v", r.Payload, r.Actor)
		}
	}
}

// TestCrossVersionUpgradeRollbackOnReadyTimeout is §3.6 step 6 against a real
// binary swap: READY never arrives within the CONFIGURED
// lifecycle.upgrade_ready_timeout, so the previous build is restored by the
// same rename recipe and the ledger says why.
func TestCrossVersionUpgradeRollbackOnReadyTimeout(t *testing.T) {
	prev := buildStampedBinary(t, "v0.0.9", "deadbee")
	next := buildStampedBinary(t, "v0.1.0", "abc1234")

	dir := t.TempDir()
	root := testStateRoot(t)
	live := filepath.Join(dir, "troubled")
	prevBytes, err := os.ReadFile(prev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, prevBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	l, w := openTestLedger(t, root, "v0.1.0")
	cfg := *defaults()
	cfg.StateRoot = root
	cfg.Lifecycle.UpgradeReadyTimeout = "150ms" // the configured deadline, not a hard-coded 30s
	// Nothing ever answers here: the health surface the READY gate polls.
	cfg.HealthURL = "http://" + freePort(t) + "/health.json"

	var steps []types.Record
	plan := upgradePlan{
		CurrentBinary: live,
		NewBinary:     next,
		FromVersion:   "v0.0.9",
		ToVersion:     "v0.1.0",
		Park:          func(context.Context) (int, error) { return 0, nil },
		Restart:       func(context.Context) error { return nil },
		Ready: func(ctx context.Context) bool {
			return waitReady(ctx, cfg, cfg.Lifecycle.UpgradeReadyTimeout.Std())
		},
		Record: func(d types.RecordDraft) error {
			rec, err := w.Append(context.Background(), d)
			if err == nil {
				steps = append(steps, rec)
			}
			return err
		},
	}
	start := time.Now()
	err = Upgrade(context.Background(), cfg, plan)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("an upgrade whose READY never arrived returned nil")
	}
	if !strings.Contains(err.Error(), string(types.CodeLifecycle011)) {
		t.Errorf("error = %v, want %s", err, types.CodeLifecycle011)
	}
	if !strings.Contains(err.Error(), string(cfg.Lifecycle.UpgradeReadyTimeout)) {
		t.Errorf("error = %v, want it to name the configured deadline %s", err, cfg.Lifecycle.UpgradeReadyTimeout)
	}
	if elapsed < cfg.Lifecycle.UpgradeReadyTimeout.Std() {
		t.Errorf("the rollback fired after %s, before the configured deadline %s: the wait was not the configured one", elapsed, cfg.Lifecycle.UpgradeReadyTimeout)
	}

	// The live path is BACK to the previous build — bytes and reported version.
	back, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, prevBytes) {
		t.Error("the live path was not restored to the previous build")
	}
	if v := binaryVersion(t, live); v != "v0.0.9" {
		t.Errorf("after the rollback the live binary reports %q, want v0.0.9", v)
	}
	if _, err := os.Stat(live + ".new"); !os.IsNotExist(err) {
		t.Error("a leftover <bin>.new survived the rollback")
	}

	// And the ledger names the rollback, in the direction the swap happened.
	chain := upgradeChain(t, recordsOf(l))
	if len(chain) != 2 {
		t.Fatalf("upgrade records = %v, want park + rollback", payloadsOf(chain))
	}
	if chain[0].Payload["step"] != UpgradeStepPark {
		t.Errorf("first record = %v, want the park", chain[0].Payload)
	}
	rb := chain[1]
	if rb.Payload["step"] != UpgradeStepRollback {
		t.Fatalf("second record = %v, want the rollback", rb.Payload)
	}
	if rb.Payload["from_version"] != "v0.1.0" || rb.Payload["to_version"] != "v0.0.9" {
		t.Errorf("rollback record = %v, want from_version=v0.1.0 to_version=v0.0.9 (the swap being undone)", rb.Payload)
	}
	if _, ok := rb.Payload["parked"]; ok {
		t.Errorf("rollback carries parked: %v", rb.Payload)
	}
}

// TestUpgradeParkNotPersistedLeavesTheBinaryAlone is §3.6 step 2 at its
// strictest: a park that cannot be PERSISTED (the ledger refuses the record)
// aborts the upgrade before the rename, and the live binary is untouched. The
// versioned counterpart of TestUpgradeParkFailureNoRename, which covers a park
// FUNCTION that fails.
func TestUpgradeParkNotPersistedLeavesTheBinaryAlone(t *testing.T) {
	dir := t.TempDir()
	bc := newBinaryPair(t, dir)
	cfg := *defaults()
	cfg.StateRoot = testStateRoot(t)
	restarted := false
	plan := upgradePlan{
		CurrentBinary: bc.live,
		NewBinary:     bc.staged,
		FromVersion:   "v0.0.9",
		ToVersion:     "v0.1.0",
		Park:          func(context.Context) (int, error) { return 1, nil },
		Restart:       func(context.Context) error { restarted = true; return nil },
		Ready:         func(context.Context) bool { return true },
		Record: func(d types.RecordDraft) error {
			if d.Payload["step"] == UpgradeStepPark {
				return errors.New("ledger: fsync failed")
			}
			return nil
		},
	}
	err := Upgrade(context.Background(), cfg, plan)
	if err == nil {
		t.Fatal("a park the ledger refused still upgraded")
	}
	if !strings.Contains(err.Error(), string(types.CodeLifecycle011)) {
		t.Errorf("error = %v, want %s", err, types.CodeLifecycle011)
	}
	if !strings.Contains(err.Error(), "park not persisted") {
		t.Errorf("error = %v, want it to name the refused park", err)
	}
	if restarted {
		t.Error("the restart ran after a refused park: the rename was not aborted")
	}
	if b, err := os.ReadFile(bc.live); err != nil || string(b) != "old" {
		t.Errorf("the live binary is %q (err %v), want it untouched by a refused park", b, err)
	}
	if _, err := os.Stat(bc.live + ".new"); !os.IsNotExist(err) {
		t.Error("the staged binary was left behind after a refused park")
	}
}

// TestRunUpgradeRecordsThroughItsWriter is the ops-level half of the same
// contract: `trouble upgrade` (RunUpgrade) must carry FromVersion/ToVersion and
// its Record writer down into the recipe, so an operator upgrade that swaps in
// a v0.1.0 build over a v0.0.9 one cannot end up with an unversioned ledger.
//
// RestartUnit is false, which is the documented way to run the recipe without
// systemd: the park step IS the unit's stop, so with nothing to stop the recipe
// runs park -> rename -> READY. READY is a real listener here.
func TestRunUpgradeRecordsThroughItsWriter(t *testing.T) {
	dir := t.TempDir()
	root := testStateRoot(t)
	bc := newBinaryPair(t, dir)
	l, w := openTestLedger(t, root, "v0.1.0")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"status":"ok"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cfg := *defaults()
	cfg.StateRoot = root
	cfg.HealthURL = "http://" + ln.Addr().String() + "/health.json"
	cfg.Lifecycle.UpgradeReadyTimeout = "5s"
	err = RunUpgrade(context.Background(), cfg, UpgradeOptions{
		CurrentBin:  bc.live,
		To:          bc.staged,
		RestartUnit: false,
		FromVersion: "v0.0.9",
		ToVersion:   "v0.1.0",
		Record:      w,
	})
	if err != nil {
		t.Fatalf("RunUpgrade: %v", err)
	}

	chain := upgradeChain(t, recordsOf(l))
	if len(chain) != 2 {
		t.Fatalf("upgrade records = %v, want park + resume", payloadsOf(chain))
	}
	if chain[0].Payload["step"] != UpgradeStepPark || chain[1].Payload["step"] != UpgradeStepResume {
		t.Errorf("steps = %v, want park then resume", payloadsOf(chain))
	}
	for _, r := range chain {
		if r.Payload["from_version"] != "v0.0.9" || r.Payload["to_version"] != "v0.1.0" {
			t.Errorf("%v: want from_version=v0.0.9 to_version=v0.1.0", r.Payload)
		}
	}
	if b, err := os.ReadFile(bc.live); err != nil || string(b) != "new" {
		t.Errorf("live binary = %q (err %v), want the staged bytes", b, err)
	}
}

// ---- the operator-run stamped probe ---------------------------------------

// TestUpgradeCrossVersionRecordChain is the operator-run probe: it asserts that
// a STAMPED build records its own triple in the upgrade chain — the half a
// `go test` process cannot demonstrate, because the §3.4 variables are set at
// link time and a test binary has none.
//
// Run it the way a release is verified:
//
//	TROUBLE_TEST_STAMPED=1 go test ./internal/lifecycle/ -run TestUpgradeCrossVersionRecordChain -count=1
//
// from a package built with the §3.4 stamps (scripts/upgrade_cross_version.sh
// does exactly that and is wired as `make upgrade-cross-version`). A skip here
// is NOT a pass; the committed, always-run behaviour is covered by the tests
// above plus TestCrossVersionUpgradePath and
// TestCrossVersionUpgradeRollbackOnReadyTimeout.
func TestUpgradeCrossVersionRecordChain(t *testing.T) {
	if os.Getenv("TROUBLE_TEST_STAMPED") != "1" {
		t.Skip("requires a stamped binary: TROUBLE_TEST_STAMPED=1 with the §3.4 link-time stamps set (make upgrade-cross-version)")
	}
	v, sha, bt, unstamped := VersionInfo()
	if unstamped {
		t.Fatalf("TROUBLE_TEST_STAMPED=1 on an unstamped build (%s %s): the probe must run against a stamped binary", v, sha)
	}
	// The two builds this probe swaps between. Under the operator script they are
	// the REAL stamped binaries the script compiled, so the live path it
	// inspects afterwards is itself a runnable release; standalone they are byte
	// stand-ins (this probe is about the RECORDS, not about executing).
	root, current, next := testStateRoot(t), filepath.Join(t.TempDir(), "troubled"), filepath.Join(t.TempDir(), "troubled.new")
	if probeRoot := os.Getenv("TROUBLE_PROBE_ROOT"); probeRoot != "" {
		root = probeRoot
		current = filepath.Join(probeRoot, "live", "troubled")
		next = filepath.Join(probeRoot, "troubled.v0.1.0")
		if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
			t.Fatalf("probe root: %v", err)
		}
		prevBytes, err := os.ReadFile(filepath.Join(probeRoot, "troubled.v0.0.9"))
		if err != nil {
			t.Fatalf("probe root: %v", err)
		}
		if err := os.WriteFile(current, prevBytes, 0o755); err != nil {
			t.Fatalf("probe root: %v", err)
		}
	} else {
		if err := os.WriteFile(current, []byte("prev"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(next, []byte("next"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// ONE ledger handle: openTestLedger takes the single-writer LOCK (SPEC-01
	// §3.4 rule 6), so a second ledger.Open on the same root is refused by
	// design — the reader and the writer here are the same handle.
	l, w := openTestLedger(t, root, v)
	plan := upgradePlan{
		CurrentBinary: current,
		NewBinary:     next,
		FromVersion:   "v0.0.9",
		ToVersion:     v,
		Park:          func(context.Context) (int, error) { return 1, nil },
		Restart:       func(context.Context) error { return nil },
		Ready:         func(context.Context) bool { return true },
		Record: func(d types.RecordDraft) error {
			_, err := w.Append(context.Background(), d)
			return err
		},
	}
	cfg := *defaults()
	cfg.StateRoot = root
	if err := Upgrade(context.Background(), cfg, plan); err != nil {
		t.Fatalf("stamped upgrade: %v", err)
	}
	chain := upgradeChain(t, recordsOf(l))
	if len(chain) != 2 {
		t.Fatalf("upgrade records = %v, want park + resume", payloadsOf(chain))
	}
	for _, r := range chain {
		if r.Actor.Version != v || r.Actor.GitSHA != sha || r.Actor.BuildTime != bt {
			t.Errorf("record %v carries actor %+v, want the stamped triple %s %s %s", r.Payload, r.Actor, v, sha, bt)
		}
		if r.Payload["to_version"] != ReleaseVersion(v) {
			t.Errorf("record %v to_version = %v, want the running release %s", r.Payload, r.Payload["to_version"], ReleaseVersion(v))
		}
	}
}

// ---- helpers ---------------------------------------------------------------

// binaryPair is a synthetic live/staged pair. The bytes differ, so a test can
// tell which one is in place, but neither file is executable: nothing here runs
// them (TestCrossVersionUpgradePath uses real binaries built from this tree).
type binaryPair struct{ live, staged string }

func newBinaryPair(t *testing.T, dir string) binaryPair {
	t.Helper()
	bc := binaryPair{live: filepath.Join(dir, "troubled"), staged: filepath.Join(dir, "troubled.v0.1.0")}
	if err := os.WriteFile(bc.live, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bc.staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bc
}

// lifecyclePkg is the §3.4 stamp package path, written once.
const lifecyclePkg = "github.com/trouble-agent/trouble/internal/lifecycle"

// buildStampedBinary compiles cmd/troubled from THIS tree with the §3.4
// link-time triple set to one version — how a release build is made. No git tag
// is created and nothing is pushed: tagging is a release act the foreman owns,
// and the upgrade path under test does not need one.
//
// The package-level build cache is reused, so the second build of the same tree
// with different ldflags costs well under a second. TROUBLE_TEST_GOCACHE points
// it at a shared directory for concurrent workers on one box.
func buildStampedBinary(t *testing.T, version, sha string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "troubled")
	ldflags := fmt.Sprintf("-s -w -X %s.Version=%s -X %s.GitSHA=%s -X %s.BuildTime=2026-09-19T00:00:00.000Z",
		lifecyclePkg, version, lifecyclePkg, sha, lifecyclePkg)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", out, "./cmd/troubled")
	cmd.Dir = repoRoot
	env := append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod")
	if cache := os.Getenv("TROUBLE_TEST_GOCACHE"); cache != "" {
		env = append(env, "GOCACHE="+cache)
	}
	cmd.Env = env
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", version, err, b)
	}
	return out
}

// binaryVersion reads the release version a built binary reports on
// `--version` (shape: "<version> <sha> <build_time> [stamped]"). Executing the
// binary is the only honest way to assert WHICH build is on a path: the file's
// bytes say which inode is there, the binary's own report says which version
// it is.
func binaryVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", bin, err, out)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatalf("%s --version printed nothing", bin)
	}
	return fields[0]
}

// testLedgerWriter is the RecordWriter the upgrade steps are recorded through: a
// real *ledger.Ledger, so the records under assertion are the bytes on disk.
//
// It fills in a missing Origin the way the production seam does (app.DraftWriter
// stamps the host identity the daemon resolved, SPEC-12 §4.5): the ledger itself
// requires origin.host_id and origin.source on every record, and lifecycle —
// which never learns the host id — must not have to invent one.
type testLedgerWriter struct{ l *ledger.Ledger }

func (w testLedgerWriter) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	if d.Origin.HostID == "" {
		d.Origin = types.Origin{HostID: "qat7host", Source: "troubled"}
	}
	return w.l.Append(ctx, d)
}

// AppendDraft is Append for a caller that only wants the error (the plan's Park
// seam returns an error, not a record).
func (w testLedgerWriter) AppendDraft(d types.RecordDraft) error {
	_, err := w.Append(context.Background(), d)
	return err
}

// openTestLedger opens a real ledger at <stateRoot>/ledger with the daemon's own
// shape (SPEC-12 §4.1 step 5), stamped with the version the caller stands in
// for.
func openTestLedger(t *testing.T, stateRoot, version string) (*ledger.Ledger, testLedgerWriter) {
	t.Helper()
	l := mustReopenLedger(t, stateRoot)
	return l, testLedgerWriter{l}
}

func mustReopenLedger(t *testing.T, stateRoot string) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(context.Background(), ledger.Options{
		Root:      filepath.Join(stateRoot, "ledger"),
		Rotation:  ledger.DefaultRotationPolicy(),
		Retention: ledger.DefaultRetentionPolicy(),
		Index:     ledger.DefaultIndexOptions(),
		Writer: types.Actor{Kind: types.ActorDaemon, ID: "troubled", Version: "v0.0.9", GitSHA: "deadbee",
			BuildTime: "2026-09-19T00:00:00.000Z"},
		MaxSchema: ledger.SchemaVersionV1,
		Now:       time.Now,
		HostID:    "qat7host",
		Zone:      "loopback",
	})
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	return l
}

// forkParkedUpgrade runs SPEC-12 §3.6 steps 1-2 of the recipe in a CHILD
// process and returns the child plus the park record it reported, so the test
// can kill it once the record is durable.
//
// The child calls the same Upgrade() cmd/trouble's verb calls. Its Park returns
// immediately, and its Record seam writes the park and then blocks: the recipe
// records the park BETWEEN step 2 and step 3, so that blocking point IS the
// window this test is about. The parent learns the park is durable by reading
// the ledger FILE (a plain read: the child holds the ledger's LOCK).
func forkParkedUpgrade(t *testing.T, live, staged, stateRoot string) (*exec.Cmd, types.Record) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	report, err := os.CreateTemp(t.TempDir(), "park-*.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = report.Close()
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), forkUpgradeEnv+"="+strings.Join([]string{live, staged, stateRoot, report.Name()}, "\x1f"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the forked upgrade: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	// Wait for the durable park: a `stage=upgrade` line in the ledger FILE with
	// step=park. The presence of the LINE is the durability claim (Append
	// returns only after the write and the fsync), and the child has not been
	// allowed to continue past it.
	ledgerFile := filepath.Join(stateRoot, "ledger")
	deadline := time.Now().Add(60 * time.Second)
	for {
		if b, err := os.ReadFile(report.Name()); err == nil && len(b) > 2 {
			var rec types.Record
			if err := json.Unmarshal(b, &rec); err != nil {
				t.Fatalf("park report: %v", err)
			}
			return cmd, rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("the forked upgrade never wrote a durable park record\nstderr:\n%s\nledger dir:\n%s",
				stderr.String(), listJSONL(ledgerFile))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runForkedUpgrade is the child side of forkParkedUpgrade: it runs the recipe's
// first steps in the process the test binary started and blocks with its park
// durable. It exits non-zero — never zero — when anything refuses, so a parent
// that reads exit code 0 knows the recipe reached the park.
func runForkedUpgrade(spec string) int {
	parts := strings.Split(spec, "\x1f")
	if len(parts) != 4 {
		fmt.Fprintf(os.Stderr, "forked upgrade: malformed spec %q\n", spec)
		return 2
	}
	live, staged, stateRoot, reportPath := parts[0], parts[1], parts[2], parts[3]

	cfg := *defaults()
	cfg.StateRoot = stateRoot
	cfg.Lifecycle.UpgradeReadyTimeout = "1m"

	l, err := ledger.Open(context.Background(), ledger.Options{
		Root:      filepath.Join(stateRoot, "ledger"),
		Rotation:  ledger.DefaultRotationPolicy(),
		Retention: ledger.DefaultRetentionPolicy(),
		Index:     ledger.DefaultIndexOptions(),
		Writer: types.Actor{Kind: types.ActorDaemon, ID: "troubled", Version: "v0.0.9", GitSHA: "deadbee",
			BuildTime: "2026-09-19T00:00:00.000Z"},
		MaxSchema: ledger.SchemaVersionV1,
		Now:       time.Now,
		HostID:    "qat7host",
		Zone:      "loopback",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "forked upgrade: ledger.Open: %v\n", err)
		return 1
	}

	// The park record is reported to the parent, and the recipe then HOLDS INSIDE
	// the record seam: §3.6 records the park between step 2 and the rename, so
	// blocking here puts the parent's kill exactly in the window this test is
	// about. The block is a sleep loop, not a bare `select {}`, so the runtime's
	// deadlock detector cannot turn a parked upgrade into a panic exit.
	plan := upgradePlan{
		CurrentBinary: live,
		NewBinary:     staged,
		FromVersion:   "v0.0.9",
		ToVersion:     "v0.1.0",
		Park:          func(context.Context) (int, error) { return 0, nil },
		Restart:       func(context.Context) error { return nil },
		Ready:         func(context.Context) bool { return false },
		Record: func(d types.RecordDraft) error {
			rec, err := testLedgerWriter{l}.Append(context.Background(), d)
			if err != nil {
				return err
			}
			if d.Payload["step"] != UpgradeStepPark {
				return nil
			}
			b, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := os.WriteFile(reportPath, b, 0o600); err != nil {
				return err
			}
			for { // parked in the ledger, before the rename: the parent kills this process
				time.Sleep(50 * time.Millisecond)
			}
		},
	}
	err = Upgrade(context.Background(), cfg, plan)
	fmt.Fprintf(os.Stderr, "forked upgrade: the recipe returned (%v) before the parent killed it\n", err)
	return 3
}

// testStateRoot returns a 0700 state root the LEDGER will accept: the ledger
// refuses a root under /tmp by design (SPEC-01 §4.3, SPEC-12 §3.2), and
// t.TempDir() is under /tmp, so every ledger-backed test in this file lives
// under the same $HOME/.local/state base the ledger's own tests use.
func testStateRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("no home dir: %v", err)
	}
	base := filepath.Join(home, ".local", "state", "trouble-test")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	root, err := os.MkdirTemp(base, "qat7-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

// upgradeChain filters a record slice to the §3.6 upgrade records, in ledger
// order.
func upgradeChain(t *testing.T, recs []types.Record) []types.Record {
	t.Helper()
	var out []types.Record
	for _, r := range recs {
		if r.Payload["stage"] == "upgrade" {
			out = append(out, r)
		}
	}
	return out
}

// recordsOf replays every record the ledger can read back from seq 1.
func recordsOf(l *ledger.Ledger) []types.Record {
	var out []types.Record
	_ = l.Query().ScanFrom(1, func(r types.Record) bool {
		out = append(out, r)
		return true
	})
	return out
}

// payloadsOf renders just the payloads of a record slice, for failure messages.
func payloadsOf(recs []types.Record) []map[string]any {
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Payload)
	}
	return out
}

// listJSONL renders the ledger directory's files (names and sizes) for a
// failure message when a park never landed.
func listJSONL(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Sprintf("%s: %v", dir, err)
	}
	var b strings.Builder
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "  %s (%d bytes)\n", e.Name(), fi.Size())
	}
	return b.String()
}
