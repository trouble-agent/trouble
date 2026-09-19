package app

// config_projects_test.go is the daemon half of the `[[projects]]` surface
// (SPEC-12 §3.1a): a declaration that cannot be used refuses the boot with
// TROUBLE-LIFECYCLE-001 BEFORE the bind preflight opens a listener, and it is
// recorded like every other boot refusal. A project must never be silently
// dropped — an ingest listener that knows no project is worse than a boot that
// will not start.

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// bootConfigRefused boots RunDaemon against a config file and returns the error
// it refuses with (a boot that reaches READY fails the test).
func bootConfigRefused(t *testing.T, cfgBody, root string, ingestPort int, args ...string) error {
	t.Helper()
	cfgPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// The context bound is deliberately looser than the wait it bounds: an
	// exhausted wait budget must be reported by the wait, which knows the load
	// and may skip, never by the context, whose cancellation reaches RunDaemon
	// as an error and so looks exactly like the refusal under test.
	ctx, cancel := context.WithTimeout(context.Background(), scaledBootBudget(bootReadyBase, bootLoadAvg(), runtime.NumCPU())+bootCtxGrace)
	defer cancel()
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := RunDaemon(ctx, BootOptions{
			Args:    append([]string{"--config", cfgPath}, args...),
			Env:     []string{},
			OnReady: func(*Daemon) { ready <- struct{}{} },
		})
		done <- err
	}()
	reached, err := awaitBootReady(t, "the refusal boot", bootReadyBase, ready, done)
	if reached {
		t.Fatalf("the boot reached READY with a malformed project declaration")
	}
	if err == nil {
		t.Fatalf("the boot succeeded; a malformed project declaration must refuse it")
	}
	return err
}

func TestMalformedProjectDeclarationRefusesTheBoot(t *testing.T) {
	cases := []struct {
		name string
		row  string
		// wantRecord is false for a declaration the RESOLVER refuses: a config
		// file error happens before the state root and the ledger exist, so
		// there is no ledger to record a boot_refused record in.
		wantRecord bool
	}{
		{
			name:       "public key is not 32 hex",
			row:        "id = \"1\"\npublic_key = \"not-a-key\"\n",
			wantRecord: true,
		},
		{
			name:       "id is not numeric",
			row:        "id = \"one\"\npublic_key = \"0123456789abcdef0123456789abcdef\"\n",
			wantRecord: true,
		},
		{
			name:       "unknown key in the table",
			row:        "id = \"1\"\npublic_key = \"0123456789abcdef0123456789abcdef\"\nquota = 600\n",
			wantRecord: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := stateBase(t) // 0700 under $HOME: the ledger refuses /tmp
			dashPort, ingestPort := freePortPair(t)
			body := fmt.Sprintf(`state_root = %q

[ingest]
bind = "127.0.0.1:%d"
advertised_host = "trouble.example.net"

[dashboard]
bind = "127.0.0.1:%d"

[[projects]]
%s`, root, ingestPort, dashPort, c.row)

			err := bootConfigRefused(t, body, root, ingestPort)
			if !strings.Contains(err.Error(), string(types.CodeLifecycle001)) {
				t.Errorf("boot refusal = %v, want %s", err, types.CodeLifecycle001)
			}

			// The refusal is auditable (when the boot got as far as a ledger),
			// and the ingest port never opened: the whole point of refusing
			// before the preflight.
			if c.wantRecord && !scanLedgerForBootRefusal(t, root, types.CodeLifecycle001) {
				t.Errorf("no boot_refused record in the ledger under %s", root)
			}
			ln, lerr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ingestPort))
			if lerr != nil {
				t.Fatalf("the ingest port is still held after a refused boot: %v", lerr)
			}
			_ = ln.Close()
		})
	}
}

// scanLedgerForBootRefusal greps the ledger's JSONL for the boot_refused record
// carrying code (the ledger is closed by the refusal, so the file is complete
// and readable without the index). The code is a parameter because a refusal is
// owned by the stage that refused: TROUBLE-LIFECYCLE-001 for a malformed
// `[[projects]]` declaration, TROUBLE-HUB-001 for an unusable server profile.
func scanLedgerForBootRefusal(t *testing.T, root string, code types.ErrorCode) bool {
	t.Helper()
	dir := filepath.Join(root, "ledger")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, `"stage":"boot_refused"`) &&
				strings.Contains(line, string(code)) {
				return true
			}
		}
	}
	return false
}
