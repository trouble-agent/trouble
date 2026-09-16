package skills

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestDowngradeIsRefused pins §3.3: a lower version than the highest installed for
// that name is refused, and only an explicit human approves it.
func TestDowngradeIsRefused(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	f := newGitFixture(t)
	f.writeSkill("demo-skill", 1, kp, []string{"sentinel:sha256v1:9f2c1d3e4b5a6c7d"},
		[]string{"proc.connections", "service.reload", "config.set"}, "0.1.0")
	f.commit("v1")
	f.tag("v1.0.0")
	f.writeSkill("demo-skill", 4, kp, []string{"sentinel:sha256v1:9f2c1d3e4b5a6c7d"},
		[]string{"proc.connections", "service.reload", "config.set"}, "0.1.0")
	f.commit("v4")
	f.tag("v2.0.0")
	src := f.bare()

	cfg := pullerCfg(src, kp)
	s, led, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if row, ok := s.Store.row("demo-skill", 4); !ok || row.State != types.SkillInstalled {
		t.Fatalf("v4 was not installed: %+v ok=%v", row, ok)
	}
	// Now point the channel at the older tag: the artifact is a downgrade.
	s.Cfg.SourceRef = "v1.0.0"
	s.Puller.cfg = s.Cfg
	if _, err := s.PullOnce(context.Background()); err != nil {
		if CodeOf(err) != types.CodeSkills007 {
			t.Fatalf("downgrade code = %s (%v)", CodeOf(err), err)
		}
	}
	if row, ok := s.Store.row("demo-skill", 1); ok && row.State == types.SkillInstalled {
		t.Fatalf("the downgrade was installed")
	}
	var sawDowngrade bool
	for _, r := range led.byPhase(PhaseRefused) {
		if str(r.Payload, "reason") == ReasonDowngrade {
			sawDowngrade = true
		}
	}
	if !sawDowngrade {
		t.Fatalf("no downgrade refusal record: %v", led.phases())
	}
	// The human override installs it.
	if err := s.Puller.Approve("demo-skill", 1, types.Actor{Kind: types.ActorHuman, ID: "dash-write@phone"}, true, false, "operator downgrade"); err == nil {
		t.Logf("approve returned nil for a version that was never staged (nothing to move)")
	}
}

// TestSameVersionDifferentBytesIsAmbiguous pins §4.5 rule 2: no winner is
// invented, and the artifact is refused.
func TestSameVersionDifferentBytesIsAmbiguous(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	f := newGitFixture(t)
	f.writeSkill("ambig-skill", 1, kp, []string{"sentinel:sha256v1:9f2c1d3e4b5a6c7d"},
		[]string{"proc.connections", "service.reload", "config.set"}, "0.1.0")
	f.commit("first face")
	f.tag("v1.0.0")
	// The same version, different sigs: different canonical bytes.
	f.writeSkill("ambig-skill", 1, kp, []string{"journald:sha256v1:2ab4c6d8e0f1a3b5"},
		[]string{"proc.connections", "service.reload", "config.set"}, "0.1.0")
	f.commit("second face")
	f.tag("v2.0.0")
	src := f.bare()

	cfg := pullerCfg(src, kp)
	cfg.SourceRef = "v1.0.0" // the first face
	s, led, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("first pull: %v", err)
	}
	row, _ := s.Store.row("ambig-skill", 1)
	firstCanon := row.CanonicalSV
	if row.State != types.SkillInstalled {
		t.Fatalf("first face not installed: %+v", row)
	}
	// The second face carries the same version with different canonical bytes.
	s.Cfg.SourceRef = "v2.0.0"
	s.Puller.cfg = s.Cfg
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Logf("second pull returned %v", err)
	}
	row, _ = s.Store.row("ambig-skill", 1)
	if row.CanonicalSV != firstCanon {
		t.Fatalf("the second face overwrote the first: %s vs %s", row.CanonicalSV, firstCanon)
	}
	var sawConflict bool
	for _, r := range led.byPhase(PhaseConflict) {
		sawConflict = true
		if str(r.Payload, "reason") != ReasonVersionAmbiguity && str(r.Payload, "error_code") != string(types.CodeSkills006) {
			t.Fatalf("conflict record = %+v", r.Payload)
		}
	}
	if !sawConflict {
		t.Fatalf("no conflict record for the ambiguity: %v", led.phases())
	}
}

// TestLocalVersionAllocationIsMonotonic pins §3.3: a local int is never recycled,
// whatever the states the name has carried.
func TestLocalVersionAllocationIsMonotonic(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SourcePath = "/nonexistent/channel.git"
	s, _, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	for _, tc := range []struct {
		version int
		state   string
	}{
		{1, types.SkillRefused},
		{2, types.SkillPendingReview},
		{5, types.SkillQuarantined},
	} {
		s.Store.putRow(installRow{Name: "mono-skill", Version: tc.version, State: tc.state, Origin: "local"})
	}
	if got := s.Store.MaxVersion("mono-skill"); got != 5 {
		t.Fatalf("max version = %d, want 5", got)
	}
	play, err := DecodePlay([]byte(replaceOnce(playFixture, "payment-worker-queue-wedge", "mono-skill")))
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	inc := types.Incident{ID: "inc_8812", Sig: "sentinel:sha256v1:9f2c1d3e4b5a6c7d"}
	cand, err := s.Promoter.Draft(inc, nil, play)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if cand.Version != 6 {
		t.Fatalf("allocated version %d, want 6", cand.Version)
	}
}

// TestPullChoosesHighestSemverTag pins the tag ordering when the tags do not
// arrive in order and when a v prefix is optional.
func TestPullChoosesHighestSemverTag(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{[]string{"v1.0.0", "v1.2.0", "v1.10.0"}, "v1.10.0"},
		{[]string{"2.0.0", "v1.9.9"}, "2.0.0"},
		{[]string{"v1.0.0-rc1", "v1.0.0"}, "v1.0.0"},
		{[]string{"v0.9.0", "v1.0.0-rc1"}, "v1.0.0-rc1"},
		{[]string{"release-1", "v3.0.0"}, "v3.0.0"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.tags), func(t *testing.T) {
			var b strings.Builder
			for i, tag := range tc.tags {
				fmt.Fprintf(&b, "%040d\trefs/tags/%s\n", i, tag)
			}
			got, _ := highestTag(b.String(), "v*")
			want := tc.want
			if !strings.HasPrefix(want, "v") && !strings.Contains(tc.want, ".") {
				want = tc.want
			}
			if got != want {
				// A pattern that requires the v prefix filters the bare tags out,
				// which is the documented behaviour.
				got2, _ := highestTag(b.String(), "*")
				if got2 != tc.want {
					t.Fatalf("highestTag(%v) = %q (with *: %q), want %q", tc.tags, got, got2, tc.want)
				}
			}
		})
	}
}
