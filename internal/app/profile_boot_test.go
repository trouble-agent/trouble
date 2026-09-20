package app

// profile_boot_test.go is the daemon half of the SPEC-13 §2.1 server profile:
// an unusable profile refuses the boot with TROUBLE-HUB-001 BEFORE the bind
// preflight opens a listener (SPEC-12 §3.7a rule 2, SPEC-13 §4.1 step 1), and
// the refusal is recorded like every other boot refusal. Same shape as the
// `[[projects]]` refusal test: the KEYS resolve (they are registered), and the
// GATE is what refuses.
//
// The profile's runtime half — the Redis stream, the dedup gate and the
// DuckBrain archival tier — lives in internal/hub and its boot/degradation/health
// wiring is exercised by hub_boot_test.go (a validated light-hub config opens the
// queue, mounts it in front of the sentinel's sink and reports the /health.json
// `hub` stanza). What THIS file asserts is the boundary a boot can decide before a
// connection exists: a light-hub config is refused when it is incomplete, and the
// refusal costs zero HTTP responses.

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestIncompleteLightHubProfileRefusesTheBoot(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		wantKey string
	}{
		{
			name: "light-hub without a Redis URL",
			profile: `[server]
profile = "light-hub"

[server.duckbrain]
namespace = "trouble/host-1"
`,
			wantKey: "server.redis.url",
		},
		{
			name: "light-hub without a DuckBrain namespace",
			profile: `[server]
profile = "light-hub"

[server.redis]
url = "redis://127.0.0.1:6379/0"
`,
			wantKey: "server.duckbrain.namespace",
		},
		{
			name: "light-hub on a satellite",
			profile: `[hub]
mode = "satellite"

[server]
profile = "light-hub"

[server.redis]
url = "redis://127.0.0.1:6379/0"

[server.duckbrain]
namespace = "trouble/host-1"
`,
			wantKey: "hub.mode",
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

%s`, root, ingestPort, dashPort, c.profile)

			err := bootConfigRefused(t, body, root, ingestPort)
			if !strings.Contains(err.Error(), string(types.CodeHub001)) {
				t.Errorf("boot refusal = %v, want %s", err, types.CodeHub001)
			}
			if !strings.Contains(err.Error(), c.wantKey) {
				t.Errorf("boot refusal %q does not name %q", err.Error(), c.wantKey)
			}
			// The refusal is auditable: the boot got as far as the ledger, so
			// exactly one boot_refused record carries the code.
			if !scanLedgerForBootRefusal(t, root, types.CodeHub001) {
				t.Errorf("no boot_refused record in the ledger under %s", root)
			}
			// And it cost zero HTTP responses: the ingest port was never held.
			ln, lerr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ingestPort))
			if lerr != nil {
				t.Fatalf("the ingest port is still held after a refused boot: %v", lerr)
			}
			_ = ln.Close()
		})
	}
}
