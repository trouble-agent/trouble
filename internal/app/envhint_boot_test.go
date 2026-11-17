package app

// envhint_boot_test.go — DOC-27 at the seam that was actually broken.
//
// lifecycle.Resolved.UnknownEnv already carried the near-miss; no boot path read
// it, so the documented WARN never reached an operator's log. A unit test on
// EnvHints cannot catch that regression coming back — deleting one call in
// RunDaemon would leave every lifecycle test green. So this test boots the real
// daemon, with a state root it then refuses (004 under /tmp), and asserts the
// hint on the log of THAT boot. The refusal is not the subject: it is the proof
// that the boot ran the step that emits the hint and then failed LATER, at
// step 2, exactly as an operator would see it.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// strayEnvBoot boots RunDaemon with exactly one environment row and a state root
// under /tmp, and returns the captured log and the boot's error. Testing short
// timeouts keep a hypothetical successful boot from serving forever.
func strayEnvBoot(t *testing.T, row string) (string, error) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "trbl-doc27-hint-")
	if err != nil {
		t.Skipf("cannot create a /tmp state root to refuse: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, bootErr := RunDaemon(ctx, BootOptions{
		Args: []string{"--state_root", root},
		Env:  []string{row},
		Log:  log,
	})
	return buf.String(), bootErr
}

func TestBootWarnsOnANearMissEnvName(t *testing.T) {
	const secret = "hunter2-not-a-real-secret"
	logText, bootErr := strayEnvBoot(t, "TROUBLE_INGEST_BINDS="+secret)

	// Premise: the boot reached step 2 and was refused there. Without this the
	// assertions below could be reading a log from a boot that never started.
	if bootErr == nil || !strings.Contains(bootErr.Error(), "TROUBLE-LIFECYCLE-004") {
		t.Fatalf("boot error = %v, want the state-root refusal (TROUBLE-LIFECYCLE-004) after the hint step", bootErr)
	}

	if !strings.Contains(logText, `level=WARN msg="unknown env key"`) {
		t.Fatalf("the boot log carries no unknown-env WARN:\n%s", logText)
	}
	if !strings.Contains(logText, "name=TROUBLE_INGEST_BINDS") {
		t.Errorf("the WARN does not name the variable the operator set:\n%s", logText)
	}
	if !strings.Contains(logText, "did_you_mean=TROUBLE_INGEST_BIND") {
		t.Errorf("the WARN does not name the real variable it resembles:\n%s", logText)
	}
	// The value is the thing most likely to be a secret; it must not be logged.
	if strings.Contains(logText, secret) {
		t.Errorf("the boot log carries the stray VALUE, which is exactly the plaintext secret this diagnostic exists to catch:\n%s", logText)
	}
}

func TestBootStaysSilentOnAnUnrelatedStrayEnvName(t *testing.T) {
	logText, bootErr := strayEnvBoot(t, "TROUBLE_NONSENSE=hunter2")

	if bootErr == nil || !strings.Contains(bootErr.Error(), "TROUBLE-LIFECYCLE-004") {
		t.Fatalf("boot error = %v, want the same state-root refusal, so this boot ran the same steps as the near-miss case", bootErr)
	}
	if strings.Contains(logText, "unknown env key") {
		t.Errorf("an unrelated stray name produced a hint: TROUBLE_NONSENSE is not a typo of anything and must stay silent:\n%s", logText)
	}
}
