package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// foldRow is one resolved config value, flattened to what the tests read.
type foldRow struct {
	value     string
	source    string
	sourceRef string
}

func foldWindowRow(t *testing.T, res Resolved) foldRow {
	t.Helper()
	for _, v := range res.Values {
		if v.Key == "sensors.sample_fold_window" {
			return foldRow{value: fmt.Sprint(v.Value), source: v.Source, sourceRef: v.SourceRef}
		}
	}
	t.Fatalf("sensors.sample_fold_window is not in the resolved set: %d rows", len(res.Values))
	return foldRow{}
}

// The SPEC-03 §3.8a sampled-event fold window is a registered key: without a
// registry entry a `sensors.*` file key is an unknown file key
// (TROUBLE-LIFECYCLE-001), which would leave the fold in the shipped posture
// with no way for an operator to turn it off. These tests pin the key in the
// places a key has to be true: the default, the file, the environment, the flag,
// and the OFF spelling.
func TestSensorsSampleFoldWindowKey(t *testing.T) {
	def := defaults()
	if got := string(def.Sensors.SampleFoldWindow); got != "5m" {
		t.Fatalf("compiled default = %q, want \"5m\" (internal/sensors pins the same value)", got)
	}

	absent := filepath.Join(t.TempDir(), "absent.toml")
	res, err := Resolve(nil, nil, absent)
	if err != nil {
		t.Fatalf("default resolve: %v", err)
	}
	row := foldWindowRow(t, res)
	if row.source != "default" || row.sourceRef != "builtin" || row.value != "5m" {
		t.Fatalf("default row = %+v, want value=5m source=default source_ref=builtin", row)
	}

	// The file wins over the default...
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sensors]\nsample_fold_window = \"90s\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	res, err = Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("file resolve: %v", err)
	}
	row = foldWindowRow(t, res)
	if row.source != "file" || row.value != "90s" {
		t.Fatalf("file row = %+v, want value=90s source=file", row)
	}
	if got := res.Config.Sensors.SampleFoldWindow.Std(); got.String() != "1m30s" {
		t.Fatalf("resolved file value = %s, want 1m30s", got)
	}

	// ...the environment wins over the file...
	res, err = Resolve(nil, []string{"TROUBLE_SENSORS_SAMPLE_FOLD_WINDOW=2m"}, path)
	if err != nil {
		t.Fatalf("env resolve: %v", err)
	}
	row = foldWindowRow(t, res)
	if row.source != "env" || row.value != "2m" || row.sourceRef != "TROUBLE_SENSORS_SAMPLE_FOLD_WINDOW" {
		t.Fatalf("env row = %+v, want value=2m source=env", row)
	}

	// ...and the flag wins over everything.
	res, err = Resolve([]string{"--sensors-sample_fold_window", "30s"}, []string{"TROUBLE_SENSORS_SAMPLE_FOLD_WINDOW=2m"}, path)
	if err != nil {
		t.Fatalf("flag resolve: %v", err)
	}
	row = foldWindowRow(t, res)
	if row.source != "flag" || row.value != "30s" || row.sourceRef != "--sensors-sample_fold_window" {
		t.Fatalf("flag row = %+v, want value=30s source=flag", row)
	}
	if got := res.Config.Sensors.SampleFoldWindow; got != "30s" {
		t.Fatalf("resolved value = %q, want 30s", got)
	}

	// `0` is the documented OFF. It survives resolution as a value (not as
	// "unset"), and it parses to a zero duration — which is what
	// internal/sensors reads as "fold disabled".
	res, err = Resolve([]string{"--sensors-sample_fold_window", "0"}, nil, path)
	if err != nil {
		t.Fatalf("zero resolve: %v", err)
	}
	if got := string(res.Config.Sensors.SampleFoldWindow); got != "0" {
		t.Fatalf("resolved zero = %q, want the OFF spelling to survive verbatim", got)
	}
	if got := res.Config.Sensors.SampleFoldWindow.Std(); got != 0 {
		t.Fatalf("OFF parses to %s, want 0", got)
	}
}
