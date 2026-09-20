package sentinel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// collectorSigGoldenPath is the checked-in value-level pin of §7's collector row:
// one entry per fixture file, one sig per event the fixture produces, in order.
const collectorSigGoldenPath = "testdata/collector_sig_golden.json"

// collectorSigGolden is the golden file's shape.
type collectorSigGolden struct {
	NormVersion int `json:"norm_version"`
	Fixtures    []struct {
		Name string   `json:"name"`
		Sigs []string `json:"sigs"`
	} `json:"fixtures"`
}

// collectorFixtures lists every fixture under testdata/logs as "<parser>/<case>",
// sorted: the set §7's collector row has to cover.
func collectorFixtures(t *testing.T) []string {
	t.Helper()
	parsers, err := os.ReadDir(filepath.Join("testdata", "logs"))
	if err != nil {
		t.Fatalf("read testdata/logs: %v", err)
	}
	var out []string
	for _, p := range parsers {
		if !p.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join("testdata", "logs", p.Name()))
		if err != nil {
			t.Fatalf("read testdata/logs/%s: %v", p.Name(), err)
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".lines") {
				out = append(out, p.Name()+"/"+strings.TrimSuffix(f.Name(), ".lines"))
			}
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("no fixtures under testdata/logs: the row has nothing to pin")
	}
	return out
}

// collectFixtureSigs feeds one fixture through the shipped dispatcher on a fresh
// server and returns the sigs of the events it produced, in order.
func collectFixtureSigs(t *testing.T, parser, name string) []string {
	t.Helper()
	ts := collectorTestServer(t)
	defer ts.close()
	recs := ts.feedFixture(t, "journal:legacy-daemon", collectorFixture(t, parser, name))
	sigs := make([]string, 0, len(recs))
	for _, rec := range recs {
		sigs = append(sigs, rec.Sig)
	}
	return sigs
}

// TestCollectorFixtureSigsMatchGolden pins §7's collector row — "every fixture
// yields the expected event count, level, culprit and sig" — at value level: the
// count/level/culprit are asserted in collector_test.go, the sigs here, one per
// fixture and per event, against testdata/collector_sig_golden.json. The fixture
// set is walked from the filesystem, so a fixture added without a golden entry
// fails rather than slipping through; and because the values are the canonical
// form's own output, a mismatch means the normalizer moved, which requires the
// norm_version bump and the re-key in the same commit (§3.3).
func TestCollectorFixtureSigsMatchGolden(t *testing.T) {
	names := collectorFixtures(t)
	var got collectorSigGolden
	got.NormVersion = types.NormVersionV1
	for _, n := range names {
		parser, name, _ := strings.Cut(n, "/")
		got.Fixtures = append(got.Fixtures, struct {
			Name string   `json:"name"`
			Sigs []string `json:"sigs"`
		}{Name: n, Sigs: collectFixtureSigs(t, parser, name)})
	}

	if *updateGolden {
		// The key order mirrors the file's, so regenerating an unchanged
		// dispatcher is a no-op diff; the comment is carried as it stands.
		var existing struct {
			Comment string `json:"_comment"`
		}
		if raw, rerr := os.ReadFile(collectorSigGoldenPath); rerr == nil {
			_ = json.Unmarshal(raw, &existing)
		}
		out := struct {
			Comment  string `json:"_comment"`
			Fixtures []struct {
				Name string   `json:"name"`
				Sigs []string `json:"sigs"`
			} `json:"fixtures"`
			NormVersion int `json:"norm_version"`
		}{Comment: existing.Comment, Fixtures: got.Fixtures, NormVersion: types.NormVersionV1}
		raw, merr := json.MarshalIndent(out, "", "  ")
		if merr != nil {
			t.Fatalf("marshal golden: %v", merr)
		}
		if werr := os.WriteFile(collectorSigGoldenPath, append(raw, '\n'), 0o644); werr != nil {
			t.Fatalf("write %s: %v", collectorSigGoldenPath, werr)
		}
		t.Fatalf("rewrote %s from the shipped dispatcher: read the diff and commit it with the change that moved the sigs", collectorSigGoldenPath)
	}

	raw, err := os.ReadFile(collectorSigGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", collectorSigGoldenPath, err)
	}
	var gold collectorSigGolden
	if err := json.Unmarshal(raw, &gold); err != nil {
		t.Fatalf("%s: %v", collectorSigGoldenPath, err)
	}
	if gold.NormVersion != types.NormVersionV1 {
		t.Fatalf("the golden file is for norm_version %d, the code is at %d",
			gold.NormVersion, types.NormVersionV1)
	}
	byName := map[string][]string{}
	for _, f := range gold.Fixtures {
		byName[f.Name] = f.Sigs
	}
	for _, f := range got.Fixtures {
		want, ok := byName[f.Name]
		if !ok {
			t.Errorf("%s has no entry in %s: a new fixture is pinned by re-keying the file (-update-golden)",
				f.Name, collectorSigGoldenPath)
			continue
		}
		if !reflect.DeepEqual(f.Sigs, want) {
			t.Errorf("%s: sigs = %v, golden = %v (a moved canonical form requires the norm_version bump and the re-key)",
				f.Name, f.Sigs, want)
		}
		delete(byName, f.Name)
	}
	orphans := make([]string, 0, len(byName))
	for name := range byName {
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		t.Errorf("%s pins %s, which no longer exists under testdata/logs", collectorSigGoldenPath, name)
	}
}
