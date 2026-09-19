package lifecycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// advertised_host_test.go pins SPEC-12 §3.1/§3.1g (TRBL-017): `ingest.advertised_host`
// is the key whose documented default made the shipped boot unusable. The
// registry starts from the empty marker and the value the daemon (and the
// sentinel's DSN generation) actually uses is DERIVED from the resolved bind —
// the loopback name while the bind is loopback, nothing otherwise, because the
// §3.2 bind preflight owns the refusal of a non-loopback bind with no declared
// host. Two properties are load-bearing and are asserted here:
//
//  1. the derivation is conditioned on the bind (a wildcard bind must NOT hand
//     the sentinel "localhost": SPEC-04 §2.3a refuses it, and a silent
//     no-report is exactly the class that rule exists for);
//  2. the derived value is what the explain row and the boot `config` record
//     carry, so an operator never reads a blank where the daemon uses a host.

// TestAdvertisedHostLoopbackDerivation drives the derivation matrix.
func TestAdvertisedHostLoopbackDerivation(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "absent.toml")

	cases := []struct {
		name string
		bind string
		want string
	}{
		{name: "the shipped default bind (127.0.0.1)", bind: "", want: advertisedHostLoopbackDefault},
		{name: "loopback by address", bind: "127.0.0.1:7643", want: advertisedHostLoopbackDefault},
		{name: "loopback by name", bind: "localhost:7643", want: advertisedHostLoopbackDefault},
		{name: "loopback in 127/8", bind: "127.0.0.2:7643", want: advertisedHostLoopbackDefault},
		{name: "ipv6 loopback", bind: "[::1]:7643", want: advertisedHostLoopbackDefault},
		{name: "wildcard", bind: "0.0.0.0:7643", want: ""},
		{name: "ipv6 wildcard", bind: "[::]:7643", want: ""},
		{name: "lan address", bind: "10.0.0.5:7643", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var env []string
			if tc.bind != "" {
				env = []string{"TROUBLE_INGEST_BIND=" + tc.bind}
			}
			res, err := Resolve(nil, env, absent)
			if err != nil {
				t.Fatalf("Resolve(bind=%q) = %v", tc.bind, err)
			}
			if got := res.Config.Ingest.AdvertisedHost; got != tc.want {
				t.Fatalf("ingest.advertised_host = %q for bind %q, want %q", got, tc.bind, tc.want)
			}
			// The projection: the row must carry the derived value, never the
			// empty marker the registry started from.
			row, ok := resolvedRows(res)["ingest.advertised_host"]
			if !ok {
				t.Fatal("no resolved row for ingest.advertised_host")
			}
			if row.Value != tc.want {
				t.Errorf("explain row ingest.advertised_host = %#v, want %#v (the value the daemon will use)", row.Value, tc.want)
			}
		})
	}
}

// TestAdvertisedHostDeclaredValueWinsAndIsRecorded pins the other direction: a
// declared host is not overwritten by the derivation, keeps its provenance on
// the explain row, and reaches the boot `config` record verbatim.
func TestAdvertisedHostDeclaredValueWinsAndIsRecorded(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "config.toml")
	body := "state_root = " + quote(state) + "\n" +
		"ingest.bind = \"127.0.0.1:7643\"\n" +
		"ingest.advertised_host = \"trouble.example\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(nil, nil, cfgPath)
	if err != nil {
		t.Fatalf("Resolve(%s) = %v", cfgPath, err)
	}
	if got := res.Config.Ingest.AdvertisedHost; got != "trouble.example" {
		t.Fatalf("ingest.advertised_host = %q, want the declared name", got)
	}
	row := resolvedRows(res)["ingest.advertised_host"]
	if row.Value != "trouble.example" || row.Source != "file" || row.SourceRef != cfgPath {
		t.Errorf("explain row = (%#v, source=%q, ref=%q), want (\"trouble.example\", \"file\", %q)",
			row.Value, row.Source, row.SourceRef, cfgPath)
	}

	w := &watermarkWriter{}
	if err := WriteConfigRecord(w, res); err != nil {
		t.Fatalf("WriteConfigRecord = %v", err)
	}
	b, err := json.Marshal(w.drafts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "trouble.example") {
		t.Error("the boot config record does not carry the declared advertised host")
	}
}

// TestAdvertisedHostDerivedValueReachesBootRecord is the §3.1g observable: a
// boot that declares NO host still records the host it will use, so a reader of
// the ledger sees the DSN host instead of a blank.
func TestAdvertisedHostDerivedValueReachesBootRecord(t *testing.T) {
	dir := t.TempDir()
	res, err := Resolve(nil, nil, filepath.Join(dir, "absent.toml"))
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if res.Config.Ingest.AdvertisedHost != advertisedHostLoopbackDefault {
		t.Fatalf("premise: derived host = %q", res.Config.Ingest.AdvertisedHost)
	}
	w := &watermarkWriter{}
	if err := WriteConfigRecord(w, res); err != nil {
		t.Fatalf("WriteConfigRecord = %v", err)
	}
	var found bool
	for _, draft := range w.drafts() {
		vals, _ := draft.Payload["values"].([]types.ConfigValue)
		for _, cv := range vals {
			if cv.Key != "ingest.advertised_host" {
				continue
			}
			found = true
			if cv.Value != advertisedHostLoopbackDefault {
				t.Errorf("boot record ingest.advertised_host = %#v, want %q", cv.Value, advertisedHostLoopbackDefault)
			}
		}
	}
	if !found {
		t.Fatal("the boot config record carries no ingest.advertised_host row")
	}
}

// quote renders a TOML basic string for the fixtures above (TOML has no
// backslash-free spelling for a Windows path, and the fixtures are absolute).
func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}
