package registry

// modules_file_test.go — per-module units for file.read and file.patch
// (SPEC-06 §7: check-diff shape, apply semantics, verify, and every error in the
// module's own row). Fixtures live in t.TempDir() only: no sleeps, no network,
// no root, and every write goes to a temp backup dir.

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

const filePatchFixture = `# listen config
listen = 8080
workers = 4
timeout = 30

# tuning
retries = 3
buffer = 64
`

const filePatchTwoHunks = `--- a/app.conf
+++ b/app.conf
@@ -2,3 +2,3 @@
 listen = 8080
-workers = 4
+workers = 8
 timeout = 30
@@ -7,2 +7,2 @@
-retries = 3
-buffer = 64
+retries = 5
+buffer = 128
`

const filePatchApplied = `# listen config
listen = 8080
workers = 8
timeout = 30

# tuning
retries = 5
buffer = 128
`

// fileTestEnv builds a module environment whose backup dir is a temp dir.
func fileTestEnv(t *testing.T, keepBackups int) *moduleEnv {
	t.Helper()
	cfg := defaultConfig()
	cfg.FileBackupDir = filepath.Join(t.TempDir(), "backups")
	cfg.FileKeepBackups = keepBackups
	return &moduleEnv{cfg: cfg}
}

// fileTestWrite writes a fixture and returns its path.
func fileTestWrite(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	return path
}

// fileTestBackups lists the backup files of one target path.
func fileTestBackups(t *testing.T, env *moduleEnv, target string) []string {
	t.Helper()
	entries, err := os.ReadDir(env.cfg.FileBackupDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read backup dir: %v", err)
	}
	suffix := "-" + filepath.Base(target)
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func fileTestPermanent(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a permanent error, got nil")
	}
	if !errors.Is(err, types.ErrPermanent) {
		t.Fatalf("want errors.Is(err, ErrPermanent), got %v", err)
	}
}

func fileTestRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestFileReadCheckShapeApplyAndVerify(t *testing.T) {
	dir := t.TempDir()
	path := fileTestWrite(t, dir, "app.conf", "hello\nworld\n")
	m := fileReadModule{env: fileTestEnv(t, 5)}
	args := cfgTestArgs(t, m, map[string]any{"path": path})

	diff, err := m.Check(context.Background(), args)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !diff.Empty || len(diff.Entries) != 0 {
		t.Fatalf("file.read Check must be an empty diff: %+v", diff)
	}
	if !strings.Contains(diff.Summary, "12 bytes") || !strings.Contains(diff.Summary, "utf-8") {
		t.Fatalf("summary = %q", diff.Summary)
	}

	res, err := m.Apply(context.Background(), args)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Changed || len(res.Applied) != 0 {
		t.Fatalf("file.read Apply is a read: %+v", res)
	}
	if res.Output["content"] != "hello\nworld\n" {
		t.Fatalf("content = %q", res.Output["content"])
	}
	if res.Output["bytes"] != 12 || res.Output["total_bytes"] != int64(12) || res.Output["truncated"] != false {
		t.Fatalf("output = %+v", res.Output)
	}
	if res.Output["sha256"] != fileSHA256([]byte("hello\nworld\n")) {
		t.Fatalf("sha256 = %v", res.Output["sha256"])
	}

	vr, err := m.Verify(context.Background(), args)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vr.OK || vr.Method != "recheck" {
		t.Fatalf("verify = %+v", vr)
	}
	if len(vr.Evidence) != 1 || vr.Evidence[0].After != fileSHA256([]byte("hello\nworld\n")) {
		t.Fatalf("verify evidence = %+v", vr.Evidence)
	}
}

func TestFileReadOffsetTruncationAndRaw(t *testing.T) {
	dir := t.TempDir()
	path := fileTestWrite(t, dir, "app.conf", "hello\nworld\n")
	m := fileReadModule{env: fileTestEnv(t, 5)}

	// max_bytes truncates and reports it.
	res, err := m.Apply(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "max_bytes": 5}))
	if err != nil {
		t.Fatalf("apply truncated: %v", err)
	}
	if res.Output["content"] != "hello" || res.Output["bytes"] != 5 || res.Output["truncated"] != true {
		t.Fatalf("truncated output = %+v", res.Output)
	}
	if res.Output["sha256"] != fileSHA256([]byte("hello")) {
		t.Fatalf("truncated sha256 = %v", res.Output["sha256"])
	}

	// offset seeks into the file.
	res, err = m.Apply(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "offset": 6}))
	if err != nil {
		t.Fatalf("apply offset: %v", err)
	}
	if res.Output["content"] != "world\n" || res.Output["offset"] != 6 {
		t.Fatalf("offset output = %+v", res.Output)
	}

	// offset + max_bytes at the truncation boundary.
	res, err = m.Apply(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "offset": 6, "max_bytes": 5}))
	if err != nil {
		t.Fatalf("apply offset+max: %v", err)
	}
	if res.Output["content"] != "world" || res.Output["truncated"] != true {
		t.Fatalf("offset+max output = %+v", res.Output)
	}

	// encoding=raw returns base64 (SPEC-06 §6.12).
	binPath := filepath.Join(t.TempDir(), "blob.bin")
	blob := []byte{0x00, 0xff, 0x10, 'a'}
	if err := os.WriteFile(binPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = m.Apply(context.Background(), cfgTestArgs(t, m, map[string]any{"path": binPath, "encoding": "raw"}))
	if err != nil {
		t.Fatalf("apply raw: %v", err)
	}
	if res.Output["bytes_base64"] != base64.StdEncoding.EncodeToString(blob) {
		t.Fatalf("bytes_base64 = %v", res.Output["bytes_base64"])
	}
	if res.Output["sha256"] != fileSHA256(blob) {
		t.Fatalf("raw sha256 = %v", res.Output["sha256"])
	}
	if _, ok := res.Output["content"]; ok {
		t.Fatalf("raw output must not carry content: %+v", res.Output)
	}

	// utf-8 on the same bytes is permanent.
	_, err = m.Apply(context.Background(), cfgTestArgs(t, m, map[string]any{"path": binPath}))
	fileTestPermanent(t, err)
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("error should mention UTF-8: %v", err)
	}
}

func TestFileReadErrors(t *testing.T) {
	dir := t.TempDir()
	m := fileReadModule{env: fileTestEnv(t, 5)}

	_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": filepath.Join(dir, "absent.conf")}))
	fileTestPermanent(t, err)

	_, err = m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": dir}))
	fileTestPermanent(t, err)
	if !strings.Contains(err.Error(), "directory") {
		t.Fatalf("error should say directory: %v", err)
	}

	_ = cfgTestArgs(t, m, map[string]any{"path": dir, "encoding": "utf-16"})
	_, err = m.Check(context.Background(), map[string]any{"path": dir, "encoding": "utf-16"})
	fileTestPermanent(t, err)

	// Verify after the file disappears is a failure, not a panic.
	path := fileTestWrite(t, dir, "gone.conf", "x\n")
	args := cfgTestArgs(t, m, map[string]any{"path": path})
	if _, err := m.Apply(context.Background(), args); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(context.Background(), args); err == nil {
		t.Fatalf("verify of a deleted file must fail")
	}
}

func TestFilePatchCheckIsADryRun(t *testing.T) {
	dir := t.TempDir()
	env := fileTestEnv(t, 5)
	path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
	m := filePatchModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "patch": filePatchTwoHunks})

	before := fileTestRead(t, path)
	diff, err := m.Check(context.Background(), args)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if diff.Empty || len(diff.Entries) != 2 {
		t.Fatalf("check diff = %+v, want two hunk entries", diff)
	}
	want := []types.DiffEntry{
		{Path: path + ":2", Before: "workers = 4", After: "workers = 8"},
		{Path: path + ":7", Before: "retries = 3\nbuffer = 64", After: "retries = 5\nbuffer = 128"},
	}
	for i, w := range want {
		if diff.Entries[i] != w {
			t.Fatalf("entry %d = %+v, want %+v", i, diff.Entries[i], w)
		}
	}
	if after := fileTestRead(t, path); after != before {
		t.Fatalf("Check must not write:\n%q\n%q", before, after)
	}
	if backups := fileTestBackups(t, env, path); len(backups) != 0 {
		t.Fatalf("Check must not write a backup: %v", backups)
	}
}

func TestFilePatchApplyBackupAndRollback(t *testing.T) {
	dir := t.TempDir()
	env := fileTestEnv(t, 5)
	path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
	m := filePatchModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "patch": filePatchTwoHunks})

	res, err := m.Apply(context.Background(), args)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Changed || len(res.Applied) != 2 {
		t.Fatalf("apply = %+v", res)
	}
	got := fileTestRead(t, path)
	if got != filePatchApplied {
		t.Fatalf("patched content:\n--- got ---\n%s\n--- want ---\n%s", got, filePatchApplied)
	}
	if res.Output["sha256"] != fileSHA256([]byte(got)) {
		t.Fatalf("output sha256 = %v", res.Output["sha256"])
	}
	if res.Output["hunks"] != 2 || res.Output["bytes"] != len(got) {
		t.Fatalf("output = %+v", res.Output)
	}

	// The backup holds the pre-change bytes and the hint names it.
	id, _ := res.Output["backup_id"].(string)
	if id == "" {
		t.Fatalf("no backup id in %+v", res.Output)
	}
	b, err := os.ReadFile(filepath.Join(env.cfg.FileBackupDir, id+"-app.conf"))
	if err != nil {
		t.Fatalf("backup file: %v", err)
	}
	if string(b) != filePatchFixture {
		t.Fatalf("backup content = %q", string(b))
	}
	if res.Rollback == nil || !res.Rollback.Supported || res.Rollback.Module != "file.patch" {
		t.Fatalf("rollback hint = %+v", res.Rollback)
	}
	if res.Rollback.Args["path"] != path || res.Rollback.Args["restore_from"] != id {
		t.Fatalf("rollback args = %+v", res.Rollback.Args)
	}

	// Running the hint (restore_from) brings the original bytes back.
	restoreArgs := cfgTestArgs(t, m, map[string]any{"path": path, "restore_from": id})
	rdiff, err := m.Check(context.Background(), restoreArgs)
	if err != nil {
		t.Fatalf("restore check: %v", err)
	}
	if rdiff.Empty || len(rdiff.Entries) != 1 {
		t.Fatalf("restore diff = %+v", rdiff)
	}
	if rdiff.Entries[0].Before != fileSHA256([]byte(filePatchApplied)) || rdiff.Entries[0].After != fileSHA256([]byte(filePatchFixture)) {
		t.Fatalf("restore entry = %+v", rdiff.Entries[0])
	}
	rres, err := m.Apply(context.Background(), restoreArgs)
	if err != nil {
		t.Fatalf("restore apply: %v", err)
	}
	if !rres.Changed {
		t.Fatalf("restore did not change the file")
	}
	if got := fileTestRead(t, path); got != filePatchFixture {
		t.Fatalf("restored content = %q", got)
	}
	vr, err := m.Verify(context.Background(), restoreArgs)
	if err != nil || !vr.OK {
		t.Fatalf("restore verify = %+v, %v", vr, err)
	}
}

func TestFilePatchIsConvergent(t *testing.T) {
	dir := t.TempDir()
	env := fileTestEnv(t, 5)
	path := fileTestWrite(t, dir, "app.conf", filePatchApplied)
	m := filePatchModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "patch": filePatchTwoHunks})

	// Every hunk's after-side is already there: the idempotency detector.
	diff, err := m.Check(context.Background(), args)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !diff.Empty || len(diff.Entries) != 0 {
		t.Fatalf("an already-applied patch must be an empty diff: %+v", diff)
	}
	res, err := m.Apply(context.Background(), args)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Changed || len(res.Applied) != 0 {
		t.Fatalf("a second Apply must be a no-op: %+v", res)
	}
	if got := fileTestRead(t, path); got != filePatchApplied {
		t.Fatalf("no-op apply rewrote the file: %q", got)
	}
	if backups := fileTestBackups(t, env, path); len(backups) != 0 {
		t.Fatalf("a no-op apply must not write a backup: %v", backups)
	}

	// Verify proves the after-side is present.
	vr, err := m.Verify(context.Background(), args)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vr.OK || vr.Method != "recheck" || vr.Detail["hunks_applied"] != 2 {
		t.Fatalf("verify = %+v", vr)
	}
}

func TestFilePatchPartiallyAppliedIsPermanent(t *testing.T) {
	dir := t.TempDir()
	env := fileTestEnv(t, 5)
	// The file carries hunk 1's after-side but not hunk 2's: never a guess.
	partial := `# listen config
listen = 8080
workers = 8
timeout = 30

# tuning
retries = 3
buffer = 64
`
	path := fileTestWrite(t, dir, "app.conf", partial)
	m := filePatchModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "patch": filePatchTwoHunks})

	_, err := m.Check(context.Background(), args)
	fileTestPermanent(t, err)
	if !strings.Contains(err.Error(), "partially") {
		t.Fatalf("error should explain the partial state: %v", err)
	}
	if got := fileTestRead(t, path); got != partial {
		t.Fatalf("a refused patch must not write: %q", got)
	}
}

func TestFilePatchPermanentErrors(t *testing.T) {
	dir := t.TempDir()
	env := fileTestEnv(t, 5)
	m := filePatchModule{env: env}

	t.Run("hunk-does-not-match", func(t *testing.T) {
		path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
		patch := "--- a/app.conf\n+++ b/app.conf\n@@ -2,1 +2,1 @@\n-listen = 9999\n+listen = 1111\n"
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "patch": patch}))
		fileTestPermanent(t, err)
		if !strings.Contains(err.Error(), "matches neither") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("malformed-counts", func(t *testing.T) {
		path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
		patch := "--- a/app.conf\n+++ b/app.conf\n@@ -2,4 +2,3 @@\n listen = 8080\n-workers = 4\n+workers = 8\n timeout = 30\n"
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "patch": patch}))
		fileTestPermanent(t, err)
		if !strings.Contains(err.Error(), "old") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("no-hunks", func(t *testing.T) {
		path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "patch": "--- a/app.conf\n+++ b/app.conf\n"}))
		fileTestPermanent(t, err)
	})

	t.Run("neither-patch-nor-restore", func(t *testing.T) {
		path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path}))
		fileTestPermanent(t, err)
	})

	t.Run("both-patch-and-restore", func(t *testing.T) {
		path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
		_, err := m.Check(context.Background(), map[string]any{"path": path, "patch": filePatchTwoHunks, "restore_from": "bk_1-x"})
		fileTestPermanent(t, err)
	})

	t.Run("missing-file", func(t *testing.T) {
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{
			"path": filepath.Join(dir, "absent.conf"), "patch": filePatchTwoHunks,
		}))
		fileTestPermanent(t, err)
	})

	t.Run("restore-with-a-traversing-id", func(t *testing.T) {
		path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "restore_from": "../../etc/passwd"}))
		fileTestPermanent(t, err)
		if !strings.Contains(err.Error(), "backup id") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("restore-with-an-unknown-id", func(t *testing.T) {
		path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "restore_from": "bk_unknown"}))
		fileTestPermanent(t, err)
	})

	t.Run("non-utf8-target", func(t *testing.T) {
		path := filepath.Join(dir, "blob.conf")
		if err := os.WriteFile(path, []byte{0x00, 0xff, 0xfe}, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "patch": filePatchTwoHunks}))
		fileTestPermanent(t, err)
	})

	t.Run("hunks-out-of-order", func(t *testing.T) {
		path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
		// Hunks must be ordered and non-overlapping; anything else is refused
		// rather than reordered into a guess.
		patch := "--- a/app.conf\n+++ b/app.conf\n@@ -7,1 +7,1 @@\n-retries = 3\n+retries = 5\n@@ -3,1 +3,1 @@\n-workers = 4\n+workers = 8\n"
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "patch": patch}))
		fileTestPermanent(t, err)
		if !strings.Contains(err.Error(), "order") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestFilePatchContextDriftStillApplies(t *testing.T) {
	dir := t.TempDir()
	env := fileTestEnv(t, 5)
	path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
	m := filePatchModule{env: env}
	// The declared line number is wrong; the hunk text is unambiguous, so the
	// unique-context search finds it.
	patch := "--- a/app.conf\n+++ b/app.conf\n@@ -40,1 +40,1 @@\n-workers = 4\n+workers = 9\n"
	args := cfgTestArgs(t, m, map[string]any{"path": path, "patch": patch})

	diff, err := m.Check(context.Background(), args)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(diff.Entries) != 1 || diff.Entries[0].Path != path+":40" {
		t.Fatalf("diff = %+v", diff)
	}
	if _, err := m.Apply(context.Background(), args); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(fileTestRead(t, path), "workers = 9") {
		t.Fatalf("the hunk did not land: %q", fileTestRead(t, path))
	}
}

func TestFilePatchKeepsAtMostKeepBackups(t *testing.T) {
	dir := t.TempDir()
	env := fileTestEnv(t, 2)
	path := fileTestWrite(t, dir, "app.conf", "a = 1\nb = 2\nc = 3\n")
	m := filePatchModule{env: env}

	for i, line := range []string{"a", "b", "c"} {
		patch := "--- a/app.conf\n+++ b/app.conf\n@@ -1,1 +1,1 @@\n-" + line + " = " + string(rune('1'+i)) + "\n+" + line + " = " + string(rune('1'+i)) + "0\n"
		args := cfgTestArgs(t, m, map[string]any{"path": path, "patch": patch})
		res, err := m.Apply(context.Background(), args)
		if err != nil {
			t.Fatalf("apply %s: %v", line, err)
		}
		if !res.Changed {
			t.Fatalf("apply %s was a no-op", line)
		}
	}
	backups := fileTestBackups(t, env, path)
	if len(backups) != 2 {
		t.Fatalf("backups = %v, want 2 kept", backups)
	}
}

func TestFilePatchVerifyDetectsExternalChange(t *testing.T) {
	dir := t.TempDir()
	env := fileTestEnv(t, 5)
	path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
	m := filePatchModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "patch": filePatchTwoHunks})

	if _, err := m.Apply(context.Background(), args); err != nil {
		t.Fatalf("apply: %v", err)
	}
	vr, err := m.Verify(context.Background(), args)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vr.OK || vr.Detail["sha256"] != fileSHA256([]byte(filePatchApplied)) {
		t.Fatalf("verify = %+v", vr)
	}
	// Someone reverts the file: Verify must not claim success.
	if err := os.WriteFile(path, []byte(filePatchFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	vr, err = m.Verify(context.Background(), args)
	if err != nil {
		t.Fatalf("verify after revert: %v", err)
	}
	if vr.OK {
		t.Fatalf("verify must fail once the after-side is gone: %+v", vr)
	}
	if vr.Detail["hunks_applied"] != 0 {
		t.Fatalf("verify detail = %+v", vr.Detail)
	}
}

func TestFilePatchBackupFalseHasNoRollbackHint(t *testing.T) {
	dir := t.TempDir()
	env := fileTestEnv(t, 5)
	path := fileTestWrite(t, dir, "app.conf", filePatchFixture)
	m := filePatchModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "patch": filePatchTwoHunks, "backup": false})

	res, err := m.Apply(context.Background(), args)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Changed {
		t.Fatalf("apply did not change the file")
	}
	if backups := fileTestBackups(t, env, path); len(backups) != 0 {
		t.Fatalf("backup=false must not write a backup: %v", backups)
	}
	if res.Rollback == nil || res.Rollback.Supported {
		t.Fatalf("no backup means no rollback hint: %+v", res.Rollback)
	}
	if !strings.Contains(fileTestRead(t, path), "workers = 8") {
		t.Fatalf("the patch did not land")
	}
}
