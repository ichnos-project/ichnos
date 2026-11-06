package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ichnos-project/ichnos/pkg/snapshot"
)

func collectFixture(tb testing.TB, path string) *Record {
	tb.Helper()
	rec, err := NewBundleCollector().Collect(tb.Context(), Source{Path: path})
	if err != nil {
		tb.Fatalf("Collect(%s) error = %v", path, err)
	}
	return rec
}

func mustSnapshot(tb testing.TB, rec *Record) *snapshot.Snapshot {
	tb.Helper()
	s, err := Snapshot(tb.Context(), rec)
	if err != nil {
		tb.Fatalf("Snapshot() error = %v", err)
	}
	return s
}

func newSnapshotOf(tb testing.TB, red redacted) *snapshot.Snapshot {
	tb.Helper()
	s, err := newSnapshot(tb.Context(), red, defaultSnapshotLimits())
	if err != nil {
		tb.Fatalf("newSnapshot() error = %v", err)
	}
	return s
}

// checkSnapshot checks the invariants that a Snapshot promises its readers.
func checkSnapshot(tb testing.TB, s *snapshot.Snapshot) {
	tb.Helper()
	if s.SchemaVersion != snapshot.SchemaVersion || s.FingerprintVersion != snapshot.FingerprintVersion {
		tb.Fatalf("versions = %d, %d", s.SchemaVersion, s.FingerprintVersion)
	}
	if (len(s.Platforms) > 0) != (s.Provenance == snapshot.ProvenanceRead) {
		tb.Fatalf("%d platforms with provenance %q", len(s.Platforms), s.Provenance)
	}
	if s.Platforms == nil || s.Vertices == nil || s.BuildArgs == nil || s.Cache.Imports == nil {
		tb.Fatal("a list of the snapshot is nil, so it would encode as null")
	}
	if !slices.IsSortedFunc(s.Platforms, func(a, b snapshot.Platform) int { return strings.Compare(a.Platform, b.Platform) }) {
		tb.Fatal("platforms are not sorted")
	}
	for i, a := range s.BuildArgs {
		if a.Withheld != a.Value.IsZero() || (i > 0 && s.BuildArgs[i-1].Name > a.Name) {
			tb.Fatalf("build argument %+v", a)
		}
	}
	for _, p := range s.Platforms {
		if p.Contexts == nil || p.Environments == nil {
			tb.Fatalf("platform %q has nil contexts or environments", p.Platform)
		}
		used := make([]bool, len(p.Environments))
		for _, st := range p.Steps {
			if st.Environment >= 0 && st.Environment < len(used) {
				used[st.Environment] = true
			}
		}
		seen := map[string]bool{}
		for k, env := range p.Environments {
			key := fmt.Sprint(env)
			if len(env) == 0 || seen[key] || !used[k] {
				tb.Fatalf("environment %d of %q is empty, repeated or unused: %+v", k, p.Platform, env)
			}
			seen[key] = true
			for n, e := range env {
				if (n > 0 && env[n-1].Name >= e.Name) || e.Withheld != e.Value.IsZero() {
					tb.Fatalf("environment %d of %q is not sorted, or withholds wrongly: %+v", k, p.Platform, env)
				}
			}
		}
		for i, st := range p.Steps {
			if st.Vertex < -1 || st.Vertex >= len(s.Vertices) {
				tb.Fatalf("step %d has vertex %d of %d", i, st.Vertex, len(s.Vertices))
			}
			for _, in := range st.Inputs {
				if in < 0 || in >= i {
					tb.Fatalf("step %d has input %d", i, in)
				}
			}
			f := st.Fingerprints
			if f.Command.IsZero() || f.Other.IsZero() || f.Ancestor.IsZero() {
				tb.Fatalf("step %d has a zero fingerprint", i)
			}
			if st.Environment < -1 || st.Environment >= len(p.Environments) {
				tb.Fatalf("step %d has environment %d of %d", i, st.Environment, len(p.Environments))
			}
			for _, e := range stepEnv(p, st) {
				if e.Withheld && !st.Withheld {
					tb.Fatalf("step %d has the withheld variable %q, but is not withheld", i, e.Name)
				}
			}
			if st.Withheld && !st.WithheldAncestor {
				tb.Fatalf("step %d is withheld, but not its ancestors", i)
			}
			for _, in := range st.Inputs {
				if in >= 0 && in < i && p.Steps[in].WithheldAncestor && !st.WithheldAncestor {
					tb.Fatalf("step %d descends from a withheld input, but does not say so", i)
				}
			}
			if (st.Text != "" && st.Lines == snapshot.Lines{}) || st.Lines.Start < 0 || st.Lines.End < st.Lines.Start {
				tb.Fatalf("step %d has text %q and lines %+v", i, st.Text, st.Lines)
			}
			if (st.SourceKind != "") != (st.Op == snapshot.OpSource) {
				tb.Fatalf("step %d is a %q step with source kind %q", i, st.Op, st.SourceKind)
			}
			for _, src := range st.Sources {
				if src.Context < 0 || src.Context >= len(p.Contexts) || !validPath(src.Path) ||
					!slices.ContainsFunc(p.Contexts[src.Context].Steps, func(k int) bool { return slices.Contains(st.Inputs, k) }) {
					tb.Fatalf("step %d has source %+v", i, src)
				}
			}
		}
		loaded := map[int]bool{}
		for _, c := range p.Contexts {
			if len(c.Steps) == 0 || !slices.IsSorted(c.Steps) || (c.Dir != "" && !validPath(c.Dir)) {
				tb.Fatalf("context %+v has no steps, unsorted steps or a directory that is not valid", c)
			}
			for _, k := range c.Steps {
				if k < 0 || k >= len(p.Steps) || p.Steps[k].SourceKind != snapshot.SourceLocal || !p.Steps[k].FromLocalContext || loaded[k] {
					tb.Fatalf("context %+v names step %d, which is not one local source step", c, k)
				}
				loaded[k] = true
			}
		}
	}
	data, err := json.Marshal(s)
	if err != nil {
		tb.Fatalf("Marshal() error = %v", err)
	}
	var back snapshot.Snapshot
	if err := json.Unmarshal(data, &back); err != nil {
		tb.Fatalf("Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(&back, s) {
		tb.Fatalf("the snapshot changed in a JSON round trip:\n%s", data)
	}
}

// stepEnv returns the environment of a step.
func stepEnv(p snapshot.Platform, st snapshot.Step) []snapshot.EnvVar {
	if st.Environment < 0 || st.Environment >= len(p.Environments) {
		return nil
	}
	return p.Environments[st.Environment]
}

// firstContext returns the first context of the first platform of s.
func firstContext(tb testing.TB, s *snapshot.Snapshot) snapshot.Context {
	tb.Helper()
	if len(s.Platforms) == 0 || len(s.Platforms[0].Contexts) == 0 {
		tb.Fatalf("snapshot has no context: %+v", s.Platforms)
	}
	return s.Platforms[0].Contexts[0]
}

func TestSnapshotFixtures(t *testing.T) {
	checked := map[string]bool{}
	for _, path := range fixturePaths(t) {
		scenario, name := filepath.Base(filepath.Dir(path)), strings.TrimSuffix(filepath.Base(path), ".dockerbuild")
		t.Run(scenario+"/"+name, func(t *testing.T) {
			rec := collectFixture(t, path)
			s := mustSnapshot(t, rec)
			checkSnapshot(t, s)
			if len(s.Vertices) != len(rec.vertices) {
				t.Errorf("snapshot has %d vertices, want %d", len(s.Vertices), len(rec.vertices))
			}
			hits := map[string][]int{}
			for _, p := range s.Platforms {
				lines := []int{}
				for _, st := range p.Steps {
					if st.Vertex >= 0 && s.Vertices[st.Vertex].Cached {
						lines = append(lines, st.Lines.Start)
					}
				}
				slices.Sort(lines)
				hits[p.Platform] = lines
			}
			want, inTable := prHitLines()[scenario]
			switch {
			case name == "base":
				for platform, lines := range hits {
					if len(lines) > 0 {
						t.Errorf("base build for %s hit lines %v, want none", platform, lines)
					}
				}
			case name == "pr" && inTable:
				checked[scenario] = true
				if !maps.EqualFunc(hits, want, slices.Equal) {
					t.Errorf("pull request build hit lines %v, want %v", hits, want)
				}
			}
		})
	}
	if len(checked) != len(prHitLines()) {
		t.Errorf("checked the hit lines of %d scenarios, want %d", len(checked), len(prHitLines()))
	}
}

// fingerprintChanges are the pull request lines, in the lab's scenarios,
// whose fingerprints differ from the base build's step with the same
// Dockerfile text, and the environment variables that differ. Lines whose
// text is not in the base build, or appears twice, are left out. They come
// from how each scenario was built, in lab/README.md: a changed variable
// changes the environment of every RUN step after it, a USER line changes
// their other fields, and anything that changes a step changes the
// ancestors of every step after it. A variable that a line adds also
// changes the other fields, which hold the order of the names. A change to
// the content of a copied file or of a remote file shows in no fingerprint.
// A change to .dockerignore, or a COPY that a line adds, changes what the
// build context sends, which shows only in the command fingerprint of the
// step that loads the context. That step has no Dockerfile text, so it is
// on no line here.
func fingerprintChanges() map[string]struct {
	env, other, ancestor []int
	vars                 []string
} {
	type changes = struct {
		env, other, ancestor []int
		vars                 []string
	}
	return map[string]changes{
		"added-env":             {env: []int{5, 7}, other: []int{5, 7}, ancestor: []int{5, 6, 7}, vars: []string{"MODE"}},
		"added-user":            {other: []int{5, 7}, ancestor: []int{5, 6, 7}},
		"changed-build-arg":     {env: []int{5, 7}, ancestor: []int{5, 6, 7}, vars: []string{"MODE"}},
		"rotating-build-arg":    {env: []int{5, 7}, ancestor: []int{5, 6, 7}, vars: []string{"BUILD_ID"}},
		"added-step":            {ancestor: []int{4, 5, 6, 7}},
		"removed-step":          {ancestor: []int{3, 4, 5, 6}},
		"changed-instruction":   {ancestor: []int{5, 6}},
		"changed-base-image":    {ancestor: []int{2, 3, 4, 5, 6}},
		"reordered-steps":       {ancestor: []int{4, 5, 6}},
		"many-steps":            {ancestor: []int{12}},
		"copy-link":             {ancestor: []int{6}},
		"identical-lines":       {ancestor: []int{5, 7}},
		"changed-next-to-added": {ancestor: []int{6, 7}},
	}
}

// TestSnapshotFingerprintsAcrossLabBuilds compares each scenario's base and
// pull request snapshots. It checks that the fingerprints change exactly
// where the scenario changed an input, and that a step whose ancestor
// fingerprint changed never hit the cache, which would mean that the
// fingerprint covers something that BuildKit's cache key does not.
func TestSnapshotFingerprintsAcrossLabBuilds(t *testing.T) {
	pairs := map[string]map[string]string{}
	for _, p := range fixturePaths(t) {
		scenario := filepath.Base(filepath.Dir(p))
		if pairs[scenario] == nil {
			pairs[scenario] = map[string]string{}
		}
		pairs[scenario][strings.TrimSuffix(filepath.Base(p), ".dockerbuild")] = p
	}
	for scenario, paths := range pairs {
		t.Run(scenario, func(t *testing.T) {
			base, pr := mustSnapshot(t, collectFixture(t, paths["base"])), mustSnapshot(t, collectFixture(t, paths["pr"]))
			if len(base.Platforms) != len(pr.Platforms) {
				t.Fatalf("base has %d platforms, pull request %d", len(base.Platforms), len(pr.Platforms))
			}
			var env, other, ancestor []int
			var vars []string
			for i, pp := range pr.Platforms {
				bp := base.Platforms[i]
				for _, st := range pp.Steps {
					b, ok := onlyStepWithText(bp, st.Text)
					if _, once := onlyStepWithText(pp, st.Text); !ok || !once {
						continue
					}
					f, g := st.Fingerprints, b.Fingerprints
					if f.Command != g.Command {
						t.Errorf("line %d has the same text but another command fingerprint", st.Lines.Start)
					}
					if !slices.Equal(stepEnv(pp, st), stepEnv(bp, b)) {
						env = append(env, st.Lines.Start)
						vars = append(vars, changedVars(stepEnv(bp, b), stepEnv(pp, st))...)
					}
					if f.Other != g.Other {
						other = append(other, st.Lines.Start)
					}
					if f.Ancestor != g.Ancestor {
						ancestor = append(ancestor, st.Lines.Start)
						if st.Vertex >= 0 && pr.Vertices[st.Vertex].Cached {
							t.Errorf("line %d hit the cache, but its ancestor fingerprint changed", st.Lines.Start)
						}
					}
				}
			}
			slices.Sort(vars)
			want := fingerprintChanges()[scenario]
			for _, tt := range []struct {
				what      string
				got, want []int
			}{{"environment", env, want.env}, {"other fields", other, want.other}, {"ancestors", ancestor, want.ancestor}} {
				if !slices.Equal(tt.got, tt.want) {
					t.Errorf("lines whose %s changed = %v, want %v", tt.what, tt.got, tt.want)
				}
			}
			if vars = slices.Compact(vars); !slices.Equal(vars, want.vars) {
				t.Errorf("changed variables = %q, want %q", vars, want.vars)
			}
		})
	}
}

func onlyStepWithText(p snapshot.Platform, text string) (snapshot.Step, bool) {
	var found []snapshot.Step
	for _, st := range p.Steps {
		if text != "" && st.Text == text {
			found = append(found, st)
		}
	}
	if len(found) != 1 {
		return snapshot.Step{}, false
	}
	return found[0], true
}

func changedVars(a, b []snapshot.EnvVar) []string {
	values := func(env []snapshot.EnvVar) map[string]snapshot.EnvVar {
		m := map[string]snapshot.EnvVar{}
		for _, e := range env {
			m[e.Name] = e
		}
		return m
	}
	x, y := values(a), values(b)
	var out []string
	for name := range maps.Keys(x) {
		if x[name] != y[name] {
			out = append(out, name)
		}
	}
	for name := range maps.Keys(y) {
		if _, ok := x[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

func TestSnapshotLabFacts(t *testing.T) {
	snap := func(scenario, name string) *snapshot.Snapshot {
		return mustSnapshot(t, collectFixture(t, fixturePath(t, scenario, name)))
	}
	named := snap("named-context", "pr")
	if named.Commit != "f5eb747c38e59dfc56f36a66a60a0b3812b371ca" || named.Dirty {
		t.Errorf("commit = %q, %v, want f5eb747c38e59dfc56f36a66a60a0b3812b371ca", named.Commit, named.Dirty)
	}
	if len(named.Platforms) != 1 || len(named.Platforms[0].Contexts) != 2 {
		t.Fatalf("named-context platforms = %+v, want one with two contexts", named.Platforms)
	}
	p := named.Platforms[0]
	if got := []string{p.Contexts[0].Name, p.Contexts[0].Dir, p.Contexts[1].Name, p.Contexts[1].Dir}; !slices.Equal(got, []string{"context", "app", "assets", "assets"}) {
		t.Errorf("contexts = %+v, want context in app and assets in assets", p.Contexts)
	}
	if st, _ := onlyStepWithText(p, "COPY --from=assets shared.txt ."); !slices.Equal(st.Sources, []snapshot.Source{{Context: 1, Path: "shared.txt"}}) {
		t.Errorf("sources of the named context's COPY = %+v", st.Sources)
	}
	if got := firstContext(t, snap("subdirectory-context", "pr")).Dir; got != "services/api" {
		t.Errorf("subdirectory-context dir = %q, want services/api", got)
	}
	if got := snap("control", "pr"); got.Commit != "" || len(got.Platforms) != 1 || len(got.Platforms[0].Contexts) != 1 || got.Platforms[0].Contexts[0].Dir != "" {
		t.Errorf("control commit, contexts = %q, %+v, want none and one context with no directory", got.Commit, got.Platforms[0].Contexts)
	}
	for name, want := range map[string][]string{"base": {".dockerignore", "unused.txt"}, "pr": {".dockerignore", "extra.txt"}} {
		if got := firstContext(t, snap("changed-dockerignore", name)).IgnorePatterns; !slices.Equal(got, want) {
			t.Errorf("changed-dockerignore %s patterns = %q, want %q", name, got, want)
		}
	}
	if got := snap("no-cache-filter", "pr").Cache.NoCache; got == nil || !slices.Equal(got.Stages, []string{"builder"}) {
		t.Errorf("no-cache = %+v, want the builder stage", got)
	}
	if got := snap("control", "pr").Cache.NoCache; got != nil {
		t.Errorf("no-cache = %+v, want nil", got)
	}
	if got := snap("two-cache-imports", "pr").Cache.Imports; !slices.Equal(got, []snapshot.CacheImport{{Type: snapshot.CacheLocal}, {Type: snapshot.CacheLocal}}) {
		t.Errorf("imports = %+v, want two local imports", got)
	}
	if got := snap("no-cache-import", "pr").Cache.Imports; len(got) != 0 {
		t.Errorf("imports = %+v, want none", got)
	}
	if st, _ := onlyStepWithText(snap("wildcard-copy", "pr").Platforms[0], "COPY app-*.txt ."); !slices.Equal(st.Sources, []snapshot.Source{{Context: 0, Path: "app-*.txt"}}) {
		t.Errorf("wildcard sources = %+v", st.Sources)
	}
	two := snap("two-platforms", "pr")
	if len(two.Platforms) != 2 {
		t.Fatalf("two-platforms has %d platforms", len(two.Platforms))
	}
	if got := []string{two.Platforms[0].Platform, two.Platforms[1].Platform}; !slices.Equal(got, []string{"linux/amd64", "linux/arm64"}) {
		t.Errorf("platforms = %q", got)
	}
	remote := snap("remote-add", "pr").Platforms[0]
	if !slices.ContainsFunc(remote.Steps, func(st snapshot.Step) bool { return st.FromHTTP && st.Text != "" }) {
		t.Error("remote-add has no step that descends from an HTTP source")
	}
}

// redStep returns a redacted step with no vertex.
func redStep(op string, inputs ...stepInput) redactedStep {
	s := redactedStep{inputs: inputs, vertex: -1}
	if op != "" {
		s.op = jsontext.Value(op)
	}
	return s
}

func in(i int) stepInput { return stepInput{step: i} }

const (
	imageOp   = `{"Op":{"source":{"identifier":"docker-image://docker.io/library/alpine:3.22"}},"platform":{"OS":"linux","Architecture":"amd64"}}`
	contextOp = `{"Op":{"source":{"identifier":"local://context","attrs":{"local.followpaths":"[\"a\"]","local.excludepatterns":"[\"*.log\"]"}}}}`
	copyOp    = `{"Op":{"file":{"actions":[{"Action":{"copy":{"src":"/a","dest":"/"}},"input":0,"output":0,"secondaryInput":1}]}}}`
)

func runOp(cmd string) string {
	return `{"Op":{"exec":{"meta":{"args":["/bin/sh","-c",` + fmt.Sprintf("%q", cmd) + `]}}}}`
}

func oneBuild(steps ...redactedStep) redacted {
	return redacted{builds: []redactedBuild{{platform: "linux/amd64", steps: steps}}, provenance: provenanceRead}
}

func TestSnapshotGraph(t *testing.T) {
	forward := newSnapshotOf(t, oneBuild(redStep(imageOp), redStep(runOp("a"), in(0)), redStep(runOp("b"), in(1))))
	backward := newSnapshotOf(t, oneBuild(redStep(runOp("b"), in(1)), redStep(runOp("a"), in(2)), redStep(imageOp)))
	checkSnapshot(t, forward)
	checkSnapshot(t, backward)
	if !reflect.DeepEqual(forward, backward) {
		t.Errorf("a provenance in reverse order gave\n%+v\nwant\n%+v", backward.Platforms[0].Steps, forward.Platforms[0].Steps)
	}

	for name, red := range map[string]redacted{
		"self-loop":                oneBuild(redStep(runOp("a"), in(0))),
		"two-step cycle":           oneBuild(redStep(runOp("a"), in(1)), redStep(runOp("b"), in(0))),
		"cycle behind a prefix":    oneBuild(redStep(imageOp), redStep(runOp("a"), in(0), in(2)), redStep(runOp("b"), in(1))),
		"input that names no step": oneBuild(redStep(runOp("a"), in(5))),
		"negative input":           oneBuild(redStep(runOp("a"), in(-1))),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newSnapshot(t.Context(), red, defaultSnapshotLimits()); !errors.Is(err, ErrMalformedRecord) {
				t.Errorf("newSnapshot() error = %v, want ErrMalformedRecord", err)
			}
		})
	}

	ancestors := func(red redacted) map[string]snapshot.Fingerprint {
		out := map[string]snapshot.Fingerprint{}
		for i, st := range newSnapshotOf(t, red).Platforms[0].Steps {
			out[fmt.Sprintf("%s#%d", st.Op, i)] = st.Fingerprints.Ancestor
		}
		return out
	}
	diamond := func(first, top string, inputs ...stepInput) redacted {
		return oneBuild(redStep(imageOp), redStep(first, in(0)), redStep(runOp("right"), in(0)), redStep(top, inputs...))
	}
	base := ancestors(diamond(runOp("left"), runOp("top"), in(1), in(2)))
	for name, tt := range map[string]struct {
		red     redacted
		changed []string
	}{
		"nothing":             {diamond(runOp("left"), runOp("top"), in(1), in(2)), nil},
		"a parent":            {diamond(runOp("LEFT"), runOp("top"), in(1), in(2)), []string{"exec#1", "exec#3"}},
		"the order of inputs": {diamond(runOp("left"), runOp("top"), in(2), in(1)), []string{"exec#3"}},
		"an output index":     {diamond(runOp("left"), runOp("top"), in(1), stepInput{step: 2, output: 1}), []string{"exec#3"}},
		"the top":             {diamond(runOp("left"), runOp("TOP"), in(1), in(2)), []string{"exec#3"}},
	} {
		got := ancestors(tt.red)
		var changed []string
		for k := range got {
			if got[k] != base[k] {
				changed = append(changed, k)
			}
		}
		slices.Sort(changed)
		if !slices.Equal(changed, tt.changed) {
			t.Errorf("changing %s changed the ancestors of %q, want %q", name, changed, tt.changed)
		}
	}

	without := newSnapshotOf(t, oneBuild(redStep(imageOp), redStep(runOp("a"), in(0))))
	with := newSnapshotOf(t, oneBuild(redStep(imageOp), redStep(runOp("other"), in(0)), redStep(runOp("a"), in(0))))
	if without.Platforms[0].Steps[1].Fingerprints.Ancestor != with.Platforms[0].Steps[2].Fingerprints.Ancestor {
		t.Error("an unrelated step, which shifts step IDs, changed another step's ancestor fingerprint")
	}

	if _, err := newSnapshot(t.Context(), redacted{builds: []redactedBuild{{steps: []redactedStep{{vertex: 3}}}}, provenance: provenanceRead}, defaultSnapshotLimits()); !errors.Is(err, ErrMalformedRecord) {
		t.Errorf("newSnapshot() of a step with a vertex the record does not have error = %v, want ErrMalformedRecord", err)
	}
	shared := redStep(imageOp)
	shared.vertex = 0
	s := newSnapshotOf(t, redacted{
		vertices:   []redactedVertex{{kind: kindStep, cached: true}},
		builds:     []redactedBuild{{platform: "linux/arm64", steps: []redactedStep{shared}}, {platform: "linux/amd64", steps: []redactedStep{shared}}},
		provenance: provenanceRead,
	})
	checkSnapshot(t, s)
	if len(s.Platforms) != 2 || len(s.Platforms[0].Steps) != 1 || len(s.Platforms[1].Steps) != 1 {
		t.Fatalf("two platforms of one step each gave %+v", s.Platforms)
	}
	if len(s.Vertices) != 1 || s.Platforms[0].Steps[0].Vertex != 0 || s.Platforms[1].Steps[0].Vertex != 0 || s.Platforms[0].Platform != "linux/amd64" {
		t.Errorf("two platforms whose steps share a vertex gave %+v", s)
	}
}

// envStep returns a RUN step with the environment entries env.
func envStep(env ...string) redactedStep {
	quoted := make([]string, len(env))
	for i, e := range env {
		quoted[i] = fmt.Sprintf("%q", e)
	}
	return redStep(`{"Op":{"exec":{"meta":{"env":[` + strings.Join(quoted, ",") + `]}}}}`)
}

// TestSnapshotSharesEnvironments checks that each distinct environment of a
// platform is kept once, wherever the steps that share it are, and counts
// once against the limit.
func TestSnapshotSharesEnvironments(t *testing.T) {
	a, b := envStep("A=1", "B=2"), envStep("C=3")
	s := newSnapshotOf(t, oneBuild(a, b, a, envStep("B=2", "A=1"), redStep(imageOp)))
	checkSnapshot(t, s)
	p := s.Platforms[0]
	var got []int
	for _, st := range p.Steps {
		got = append(got, st.Environment)
	}
	if len(p.Environments) != 2 || !slices.Equal(got, []int{0, 1, 0, 0, -1}) {
		t.Errorf("environments = %+v, steps point to %v, want two, and 0, 1, 0, 0, -1", p.Environments, got)
	}

	lim := defaultSnapshotLimits()
	lim.envVars = 2
	if _, err := newSnapshot(t.Context(), oneBuild(a, a, envStep("B=2", "A=1")), lim); err != nil {
		t.Errorf("newSnapshot() of steps that share one environment of 2 variables with a limit of 2 error = %v", err)
	}
	var le *LimitError
	if _, err := newSnapshot(t.Context(), oneBuild(a, b), lim); !errors.As(err, &le) || le.What != "environment variables" {
		t.Errorf("newSnapshot() of two environments of 3 variables with a limit of 2 error = %v, want LimitError", err)
	}
}

// TestSnapshotSizeWithSharedEnvironment checks that steps sharing a large
// environment add only their own fields to the JSON, not a copy of the
// environment each.
func TestSnapshotSizeWithSharedEnvironment(t *testing.T) {
	vars := make([]string, 400)
	for i := range vars {
		vars[i] = fmt.Sprintf("VARIABLE_%03d=some-value-of-a-realistic-length", i)
	}
	size := func(n int) int {
		steps := make([]redactedStep, n)
		for i := range steps {
			steps[i] = envStep(vars...)
		}
		return len(mustJSON(t, newSnapshotOf(t, oneBuild(steps...))))
	}
	small, large := size(100), size(200)
	if perStep := (large - small) / 100; perStep > 1024 {
		t.Errorf("each step that shares a 400-variable environment adds %d bytes of JSON, want at most 1 KiB", perStep)
	}
}

// TestSnapshotWithheldAncestor checks that a withheld input marks every
// step that descends from it, and no other.
func TestSnapshotWithheldAncestor(t *testing.T) {
	withheld := envStep("API_TOKEN=***", "A=1")
	s := newSnapshotOf(t, oneBuild(redStep(imageOp), redStep(runOp("a"), in(0)), withheld, redStep(runOp("c"), in(2)), redStep(runOp("d"), in(1))))
	checkSnapshot(t, s)
	var got [][2]bool
	for _, st := range s.Platforms[0].Steps {
		got = append(got, [2]bool{st.Withheld, st.WithheldAncestor})
	}
	if want := [][2]bool{{}, {}, {true, true}, {false, true}, {}}; !slices.Equal(got, want) {
		t.Errorf("withheld, withheld ancestor = %v, want %v", got, want)
	}
}

func TestSourcePath(t *testing.T) {
	for src, want := range map[string]string{
		"/app.txt": "app.txt", "/": ".", "": ".", "app.txt": "app.txt", "/*.go": "*.go", "/a/b": "a/b",
		"/[ab]?.txt": "[ab]?.txt", "/a b/c": "a b/c", "/a/./b": "", "/a/../b": "", "/a/": "", "/.": ".",
		"/../x": "", "/a/../../x": "", "//a": "", "/C:/x": "", "/a\\b": "", "/nul.txt": "", "/a/COM1": "",
		"/a\x00b": "", "/a\u200bb": "", "/a\xffb": "",
	} {
		got, ok := sourcePath(src)
		if want == "" {
			if ok {
				t.Errorf("sourcePath(%q) = %q, want it rejected", src, got)
			}
			continue
		}
		if got != want || !ok {
			t.Errorf("sourcePath(%q) = %q, %v, want %q", src, got, ok, want)
		}
	}
	red := oneBuild(redStep(contextOp), redStep(`{"Op":{"file":{"actions":[{"Action":{"copy":{"src":"/../secret"}},"secondaryInput":0}]}}}`, in(0)),
		redStep(runOp("x"), in(1)))
	s := newSnapshotOf(t, red)
	checkSnapshot(t, s)
	if st := s.Platforms[0].Steps; st[1].Sources != nil || !st[1].Withheld || !st[2].WithheldAncestor {
		t.Errorf("a source outside the context gave %+v, want no source and a withheld step", st[1])
	}
}

// TestValidPathAllocations checks that checking a path allocates nothing,
// since every source path and directory of a snapshot is checked.
func TestValidPathAllocations(t *testing.T) {
	for _, p := range []string{"services/api/main.go", "a/NUL.txt", "a/Com1/b", "a/conout$"} {
		if n := testing.AllocsPerRun(100, func() { validPath(p) }); n != 0 {
			t.Errorf("validPath(%q) allocates %v times, want 0", p, n)
		}
	}
}

// TestSnapshotMountSources checks that the paths that a RUN step mounts
// from a local build context, as a bind mount or as a cache mount with
// from=, are among its sources, since BuildKit's cache key covers their
// content, and that a path that is not valid withholds the step.
func TestSnapshotMountSources(t *testing.T) {
	run := func(mounts ...string) string {
		return `{"Op":{"exec":{"meta":{"args":["go","mod","download"]},"mounts":[{"dest":"/"},` + strings.Join(mounts, ",") + `]}}}`
	}
	build := func(mounts ...string) snapshot.Step {
		s := newSnapshotOf(t, oneBuild(redStep(imageOp), redStep(contextOp), redStep(runOp("x"), in(0)), redStep(run(mounts...), in(0), in(1), in(2))))
		checkSnapshot(t, s)
		return s.Platforms[0].Steps[3]
	}
	st := build(`{"input":1,"selector":"/go.mod","dest":"/src/go.mod","output":-1,"readonly":true}`,
		`{"input":1,"dest":"/ctx","output":-1,"readonly":true}`, `{"input":2,"selector":"/x","dest":"/x","output":-1,"readonly":true}`,
		`{"input":1,"selector":"/c","dest":"/c","output":-1,"mountType":3,"cacheOpt":{"ID":"/c"}}`)
	if want := []snapshot.Source{{Context: 0, Path: "go.mod"}, {Context: 0, Path: "."}, {Context: 0, Path: "c"}}; !slices.Equal(st.Sources, want) || st.Withheld {
		t.Errorf("sources = %+v, withheld %v, want %+v", st.Sources, st.Withheld, want)
	}
	if st := build(`{"input":1,"selector":"/../x","dest":"/x","output":-1,"readonly":true}`); st.Sources != nil || !st.Withheld {
		t.Errorf("a selector outside the context gave %+v, want no source and a withheld step", st)
	}
	if st := build(`{"input":1,"selector":"/x","dest":"/x","mountType":5}`); st.Sources != nil || !st.Withheld || !st.WithheldAncestor {
		t.Errorf("a mount of an unknown type gave %+v, want no source and a withheld step", st)
	}
}

// TestSnapshotSizeWithSharedContext checks that steps loading one context
// share it, so a long directory is kept once, not once for each step.
func TestSnapshotSizeWithSharedContext(t *testing.T) {
	dir := strings.Repeat("d", maxPathBytes)
	size := func(n int) (int, *snapshot.Snapshot) {
		steps := make([]redactedStep, n)
		for i := range steps {
			steps[i] = redStep(contextOp)
		}
		red := oneBuild(steps...)
		red.contextDirs = []contextDir{{name: "context", dir: dir}}
		s := newSnapshotOf(t, red)
		return len(mustJSON(t, s)), s
	}
	small, s := size(100)
	large, _ := size(200)
	if len(s.Platforms[0].Contexts) != 1 || len(s.Platforms[0].Contexts[0].Steps) != 100 || s.Platforms[0].Contexts[0].Dir != dir {
		t.Fatalf("100 steps loading one context gave contexts %d", len(s.Platforms[0].Contexts))
	}
	if perStep := (large - small) / 100; perStep > 1024 {
		t.Errorf("each step that loads a context with a %d-byte directory adds %d bytes of JSON, want at most 1 KiB", len(dir), perStep)
	}
}

// TestSnapshotWithholdsWhatRedactionChanged checks that every value that
// redaction changed, and that a snapshot keeps or hashes, is withheld, so
// that two values that redaction turns into the same text never compare
// as equal: the snapshots for two secrets that differ in one digit must be
// the same, with the value marked as withheld.
func TestSnapshotWithholdsWhatRedactionChanged(t *testing.T) {
	const planted = "ichnos-fake-known-1"
	exec := func(env ...any) map[string]any {
		return map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []any{"true"}, "env": env}}}}
	}
	// decoy adds, under key, another exec.meta.env that redaction changes,
	// which a snapshot does not read. Keys are written sorted, so "A" comes
	// before Op and "decoy" after it.
	decoy := func(key, secret string) map[string]any {
		op := exec("A=1", "DATABASE_URL=postgres://app:"+secret+"@db/x")
		env := []any{"A=1", "B=2", "C=3", "D=4", "E=5", "F=6", "DATABASE_URL=postgres://app:" + secret + "@db/x"}
		op[key] = map[string]any{"exec": map[string]any{"meta": map[string]any{"env": env}}}
		return op
	}
	onlyURLWithheld := func(s *snapshot.Snapshot) bool {
		env := stepEnv(s.Platforms[0], s.Platforms[0].Steps[0])
		return len(env) == 2 && !env[0].Withheld && env[1].Name == "DATABASE_URL" && env[1].Withheld && s.Platforms[0].Steps[0].Withheld
	}
	tests := []struct {
		name  string
		rec   func(secret string) *Record
		check func(s *snapshot.Snapshot) bool
	}{
		{"another meta.env before Op", func(secret string) *Record { return recordWith(t, nil, decoy("A", secret)) }, onlyURLWithheld},
		{"another meta.env after Op", func(secret string) *Record { return recordWith(t, nil, decoy("decoy", secret)) }, onlyURLWithheld},
		{"a URL password in an environment value", func(secret string) *Record {
			return recordWith(t, nil, exec("PATH=/bin", "DATABASE_URL=postgres://app:"+secret+"@db/x"))
		}, func(s *snapshot.Snapshot) bool {
			env := stepEnv(s.Platforms[0], s.Platforms[0].Steps[0])
			return len(env) == 2 && env[0].Name == "DATABASE_URL" && env[0].Withheld && !env[1].Withheld && s.Platforms[0].Steps[0].Withheld
		}},
		{"a known value in an environment name", func(secret string) *Record {
			return recordWith(t, map[string]string{"build-arg:API_TOKEN": secret}, exec("PATH=/bin", secret+"_MODE=fast"))
		}, func(s *snapshot.Snapshot) bool {
			env := stepEnv(s.Platforms[0], s.Platforms[0].Steps[0])
			return len(env) == 2 && env[0].Name == "***_MODE" && env[0].Withheld && !env[1].Withheld
		}},
		{"a known value in a build argument name", func(secret string) *Record {
			return recordWith(t, map[string]string{"build-arg:API_TOKEN": secret, "build-arg:" + secret + "_MODE": "fast"})
		}, func(s *snapshot.Snapshot) bool {
			return len(s.BuildArgs) == 2 && s.BuildArgs[0].Name == "***_MODE" && s.BuildArgs[0].Withheld && s.BuildArgs[0].Value.IsZero()
		}},
		{"a known value in no-cache stages", func(secret string) *Record {
			return recordWith(t, map[string]string{"build-arg:API_TOKEN": secret, "no-cache": "build," + secret})
		}, func(s *snapshot.Snapshot) bool {
			return s.Cache.NoCache != nil && s.Cache.NoCache.Withheld && slices.Equal(s.Cache.NoCache.Stages, []string{"build", "***"})
		}},
		{"a known value in a build context's name", func(secret string) *Record {
			return recordWith(t, map[string]string{"build-arg:API_TOKEN": secret, "vcs:localdir:" + secret: "secret-dir", "vcs:localdir:***": "literal-dir"},
				map[string]any{"Op": map[string]any{"source": map[string]any{"identifier": "local://***"}}})
		}, func(s *snapshot.Snapshot) bool {
			c := s.Platforms[0].Contexts
			return len(c) == 1 && c[0].Name == "***" && c[0].Dir == "" && c[0].Withheld
		}},
		{"a part of a credential in a command", func(secret string) *Record {
			return recordWith(t, map[string]string{"build-arg:API_TOKEN": secret},
				map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []any{"sh", "-c", "echo " + secret[1:]}}}}})
		}, func(s *snapshot.Snapshot) bool { return s.Platforms[0].Steps[0].Withheld }},
		{"a part of a credential in an environment value", func(secret string) *Record {
			return recordWith(t, map[string]string{"build-arg:API_TOKEN": secret}, exec("PATH=/bin", "SUFFIX="+secret[1:]))
		}, func(s *snapshot.Snapshot) bool {
			env := stepEnv(s.Platforms[0], s.Platforms[0].Steps[0])
			return len(env) == 2 && !env[0].Withheld && env[1].Name == "SUFFIX" && env[1].Withheld
		}},
		{"a known value in a platform", func(secret string) *Record {
			r := recordWith(t, map[string]string{"build-arg:API_TOKEN": secret}, exec("PATH=/bin"))
			r.builds[0].platform = "linux/" + secret
			return r
		}, func(s *snapshot.Snapshot) bool {
			return len(s.Platforms) == 1 && s.Platforms[0].Withheld && s.Platforms[0].Platform == "linux/***"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := mustSnapshot(t, tt.rec(planted)), mustSnapshot(t, tt.rec("ichnos-fake-known-2"))
			checkSnapshot(t, a)
			if !tt.check(a) {
				t.Errorf("snapshot = %s, want the changed value withheld", mustJSON(t, a))
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("snapshots for two secrets differ:\n%s\n%s", mustJSON(t, a), mustJSON(t, b))
			}
		})
	}
}

// TestSnapshotWithholdsCredentialFragments checks that redaction removes,
// from the text that a snapshot keeps from a step definition, every 8 bytes
// of a credential, as the Dockerfile frontend leaves of one after
// ${NPM_TOKEN%?}, ${NPM_TOKEN#?} or ${NPM_TOKEN/ichnos-/}, which matching
// whole values does not find, and withholds what it changed.
func TestSnapshotWithholdsCredentialFragments(t *testing.T) {
	const planted = "ichnos-fake-token-123456"
	op := func(v map[string]any) jsontext.Value { return opJSON(t, v) }
	source := func(identifier string, attrs map[string]any) map[string]any {
		return map[string]any{"Op": map[string]any{"source": map[string]any{"identifier": identifier, "attrs": attrs}}}
	}
	copyFrom := func(srcs ...string) map[string]any {
		actions := make([]any, len(srcs))
		for i, src := range srcs {
			actions[i] = map[string]any{"Action": map[string]any{"copy": map[string]any{"src": src, "dest": "/"}}, "input": 0, "output": 0, "secondaryInput": 1}
		}
		return map[string]any{"Op": map[string]any{"file": map[string]any{"actions": actions}}}
	}
	rec := &Record{&recordData{attrs: map[string]string{"build-arg:NPM_TOKEN": planted}, provenance: provenanceRead, builds: []build{{platform: "linux/amd64", steps: []step{
		{id: "s0", op: op(source("local://fake-token", nil)), vertex: -1},
		{id: "s1", op: op(source("local://context", map[string]any{"local.excludepatterns": `["a","token-12"]`})), vertex: -1},
		{id: "s2", op: op(source("local://other", map[string]any{"local.excludepatterns": `["a","token-1"]`})), vertex: -1},
		{id: "s3", op: jsontext.Value(imageOp), vertex: -1},
		{id: "s4", inputs: []stepInput{{step: 3}}, op: op(map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{
			"args": []any{"true"}, "env": []any{"chnos-fake-token-123456=x", "PATH=/bin", "ichnos-=y"},
		}}}}), vertex: -1},
		{id: "s5", inputs: []stepInput{{step: 3}, {step: 1}}, op: op(copyFrom("/ichnos-fake-token-12345", "/token-1", "/fake-token-123456")), vertex: -1},
	}}}}}
	s := mustSnapshot(t, rec)
	checkSnapshot(t, s)
	p := s.Platforms[0]
	if c := p.Contexts[0]; c.Name != withheldValue || !c.Withheld || c.Dir != "" || !p.Steps[0].Withheld {
		t.Errorf("a context named after a credential fragment gave %+v, step withheld %v", c, p.Steps[0].Withheld)
	}
	if c := p.Contexts[1]; c.Name != "context" || !c.Withheld || c.IgnorePatterns != nil {
		t.Errorf("patterns with a credential fragment gave %+v, want them withheld", c)
	}
	if c := p.Contexts[2]; c.Withheld || !slices.Equal(c.IgnorePatterns, []string{"a", "token-1"}) {
		t.Errorf("patterns with 7 bytes of a credential gave %+v, want them kept", c)
	}
	want := []snapshot.EnvVar{{Name: withheldValue, Withheld: true}, {Name: "PATH"}, {Name: "ichnos-"}}
	env := stepEnv(p, p.Steps[4])
	if len(env) != len(want) || !p.Steps[4].Withheld {
		t.Fatalf("environment = %+v, want %+v", env, want)
	}
	for i := range want {
		if env[i].Name != want[i].Name || env[i].Withheld != want[i].Withheld {
			t.Errorf("environment = %+v, want %+v", env, want)
		}
	}
	if got, want := p.Steps[5].Sources, []snapshot.Source{{Context: 1, Path: withheldValue}, {Context: 1, Path: "token-1"}, {Context: 1, Path: withheldValue}}; !slices.Equal(got, want) || !p.Steps[5].Withheld {
		t.Errorf("sources = %+v, withheld %v, want %+v, withheld", got, p.Steps[5].Withheld, want)
	}
}

// TestSnapshotEnvironmentReuseAfterRedaction checks that a RUN step whose
// environment has the same text as the previous RUN step's still withholds
// what redaction changed: a literal X=*** and a redacted one must not share
// a list, in either order.
func TestSnapshotEnvironmentReuseAfterRedaction(t *testing.T) {
	op := `{"Op":{"exec":{"meta":{"args":["true"],"env":["PATH=/bin","X=***"]}}}}`
	literal, changed := redStep(op), redStep(op)
	changed.changedEnv, changed.withheld = []int{1}, true
	for name, steps := range map[string][]redactedStep{
		"literal first": {literal, changed, changed, literal},
		"changed first": {changed, literal, literal, changed},
	} {
		s := newSnapshotOf(t, oneBuild(steps...))
		checkSnapshot(t, s)
		for i, st := range s.Platforms[0].Steps {
			env := stepEnv(s.Platforms[0], st)
			if want := steps[i].changedEnv != nil; len(env) != 2 || env[1].Name != "X" || env[1].Withheld != want {
				t.Errorf("%s: step %d has the environment %+v, want X withheld %v", name, i, env, want)
			}
		}
	}
}

// TestSnapshotLongChain checks that a chain of steps at the step limit,
// listed with each step before its input, is put in order without
// recursion.
func TestSnapshotLongChain(t *testing.T) {
	steps := make([]redactedStep, maxSteps)
	for i := range maxSteps - 1 {
		steps[i] = redStep(`{"Op":{"exec":{}}}`, in(i+1))
	}
	steps[maxSteps-1] = redStep(imageOp)
	s := newSnapshotOf(t, oneBuild(steps...))
	if got := s.Platforms[0].Steps; got[0].Op != snapshot.OpSource || !slices.Equal(got[len(got)-1].Inputs, []int{len(got) - 2}) {
		t.Errorf("a reversed chain of %d steps was not put in order", maxSteps)
	}
}

func TestSnapshotContextsAndSources(t *testing.T) {
	ctxOp := func(attrs string) string {
		return `{"Op":{"source":{"identifier":"local://context","attrs":{` + attrs + `}}}}`
	}
	build := func(contextOp string, copyFrom stepInput) redacted {
		red := oneBuild(redStep(imageOp), redStep(contextOp), redStep(runOp("x"), in(0)), redStep(copyOp, in(0), copyFrom),
			redStep(`{"Op":{"source":{"identifier":"git://github.com/o/r#main"}}}`), redStep(`{"Op":{"source":{"identifier":"https://h.example/f"}}}`),
			redStep(runOp("y"), in(3), in(4), in(5)))
		red.contextDirs = []contextDir{{name: "assets", dir: "a"}, {name: "context", dir: "services/api"}}
		return red
	}
	s := newSnapshotOf(t, build(contextOp, in(1)))
	checkSnapshot(t, s)
	p := s.Platforms[0]
	if want := []snapshot.Context{{Name: "context", Steps: []int{1}, Dir: "services/api", IgnorePatterns: []string{"*.log"}}}; !reflect.DeepEqual(p.Contexts, want) {
		t.Errorf("contexts = %+v, want %+v", p.Contexts, want)
	}
	if want := []snapshot.Source{{Context: 0, Path: "a"}}; !slices.Equal(p.Steps[3].Sources, want) {
		t.Errorf("sources = %+v, want %+v", p.Steps[3].Sources, want)
	}
	flags := func(st snapshot.Step) [3]bool { return [3]bool{st.FromLocalContext, st.FromGit, st.FromHTTP} }
	for i, want := range [][3]bool{{}, {true, false, false}, {}, {true, false, false}, {false, true, false}, {false, false, true}, {true, true, true}} {
		if got := flags(p.Steps[i]); got != want {
			t.Errorf("step %d descends from local, Git, HTTP = %v, want %v", i, got, want)
		}
	}
	if got := newSnapshotOf(t, build(contextOp, in(2))).Platforms[0].Steps[3].Sources; got != nil {
		t.Errorf("a copy from another step's result gave sources %+v", got)
	}
	for name, tt := range map[string]struct {
		attrs    string
		patterns []string
		withheld bool
	}{
		"no patterns":          {`"local.followpaths":"[]"`, nil, false},
		"empty patterns":       {`"local.excludepatterns":"[]"`, nil, false},
		"null patterns":        {`"local.excludepatterns":"null"`, nil, false},
		"patterns not JSON":    {`"local.excludepatterns":"[\"a"`, nil, true},
		"patterns not strings": {`"local.excludepatterns":"[1]"`, nil, true},
		"patterns then data":   {`"local.excludepatterns":"[] []"`, nil, true},
		"patterns":             {`"local.excludepatterns":"[\"a\",\"!b\",\"**/*.go\"]"`, []string{"a", "!b", "**/*.go"}, false},
	} {
		c := firstContext(t, newSnapshotOf(t, build(ctxOp(tt.attrs), in(1))))
		if !slices.Equal(c.IgnorePatterns, tt.patterns) || c.Withheld != tt.withheld {
			t.Errorf("%s: context = %+v, want patterns %q, withheld %v", name, c, tt.patterns, tt.withheld)
		}
	}
	for dir, want := range map[string]string{
		".": ".", "services/api": "services/api", "a b/c-d_e.f": "a b/c-d_e.f",
		"../../../home/runner/.ssh": "", "/etc": "", "..\\..\\x": "", "a\\b": "", "C:/x": "", "a:b": "",
		"a/../b": "", "a//b": "", "a/": "", "./a": "", "": "", "a\x00b": "", "a\nb": "", "a\u202eb": "",
		"a\u200bb": "", "a\ufeffb": "", "a\u00adb": "", "a\u2060b": "", "a\u2028b": "", "a\U000e0041b": "",
		"a\xffb": "", strings.Repeat("a", maxPathBytes): strings.Repeat("a", maxPathBytes), strings.Repeat("a", maxPathBytes+1): "",
		"NUL": "", "con": "", "COM1": "", "lpt9": "", "a/nul.txt": "", "AUX ": "", "CONIN$": "", "conout$": "", "com\u00b9": "",
		"a/Prn.log/b": "", "NULL": "NULL", "console": "console", "COM0": "COM0", "COM10": "COM10", "a.nul": "a.nul",
		"lpt\u00b3": "", "Con.": "", "con\u0131n$": "con\u0131n$", "\u212aon": "\u212aon", "co\u0273": "co\u0273",
	} {
		red := build(contextOp, in(1))
		red.contextDirs = []contextDir{{name: "context", dir: dir}}
		if c := firstContext(t, newSnapshotOf(t, red)); c.Dir != want || c.Withheld {
			t.Errorf("context directory %q gave %q, withheld %v, want %q", dir, c.Dir, c.Withheld, want)
		}
	}
	withheldDir := build(contextOp, in(1))
	withheldDir.contextDirs = []contextDir{{name: "context", dir: "a/***", withheld: true}}
	if c := firstContext(t, newSnapshotOf(t, withheldDir)); c.Dir != "" || !c.Withheld {
		t.Errorf("a directory that redaction changed gave %+v, want no directory, withheld", c)
	}
	red := build(contextOp, in(1))
	red.builds[0].steps[1].withheld = true
	if c := firstContext(t, newSnapshotOf(t, red)); !c.Withheld {
		t.Errorf("context of a withheld step = %+v, want withheld", c)
	}
}

func TestSnapshotRecordFacts(t *testing.T) {
	h := newHasher(defaultSnapshotLimits())
	red := redacted{
		buildArgs: []buildArg{
			{name: "API_TOKEN", withheld: true},
			{name: "MODE", value: "fast"},
			{name: "SOURCE_DATE_EPOCH", value: "1700000000"},
		},
		imports:  []cacheType{cacheGHA, cacheUnknown, cacheRegistry, cacheLocal, cacheS3, cacheAzblob},
		noCache:  noCacheSetting{set: true, stages: "a,b"},
		revision: strings.Repeat("ab", 20) + "-dirty",
		vertices: []redactedVertex{
			{kind: kindStep, cached: true, errored: true, started: ts(1, 5), completed: ts(3, 0)},
			{kind: kindImport, errored: true},
			{kind: kindExport},
			{kind: kindInternal, cached: true},
		},
		provenance: provenanceMissing,
	}
	s := newSnapshotOf(t, red)
	checkSnapshot(t, s)
	wantArgs := []snapshot.BuildArg{
		{Name: "API_TOKEN", Withheld: true},
		{Name: "MODE", Value: refSum(tagBuildArg, []byte("\x04MODE\x04fast"))},
		{Name: "SOURCE_DATE_EPOCH", Value: h.sumFields(tagBuildArg, []byte("SOURCE_DATE_EPOCH"), []byte("1700000000"))},
	}
	if !slices.Equal(s.BuildArgs, wantArgs) {
		t.Errorf("build arguments = %+v, want %+v", s.BuildArgs, wantArgs)
	}
	if s.SourceDateEpoch == nil || *s.SourceDateEpoch != 1700000000 {
		t.Errorf("SOURCE_DATE_EPOCH = %v, want 1700000000", s.SourceDateEpoch)
	}
	wantImports := []snapshot.CacheImport{{Type: "gha"}, {Type: "unknown"}, {Type: "registry"}, {Type: "local"}, {Type: "s3"}, {Type: "azblob"}}
	if s.Cache.NoCache == nil {
		t.Fatal("no-cache = nil, want stages a, b")
	}
	if !slices.Equal(s.Cache.Imports, wantImports) || !slices.Equal(s.Cache.NoCache.Stages, []string{"a", "b"}) {
		t.Errorf("cache = %+v, want imports %+v and stages a, b", s.Cache, wantImports)
	}
	if s.Commit != strings.Repeat("ab", 20) || !s.Dirty || s.Provenance != snapshot.ProvenanceMissing || len(s.Platforms) != 0 {
		t.Errorf("commit, dirty, provenance, platforms = %q, %v, %q, %d", s.Commit, s.Dirty, s.Provenance, len(s.Platforms))
	}
	wantVertices := []snapshot.Vertex{
		{Kind: snapshot.VertexStep, LostHit: true, Errored: true, Started: time.Unix(1, 5).UTC(), Completed: time.Unix(3, 0).UTC()},
		{Kind: snapshot.VertexCacheImport, Errored: true},
		{Kind: snapshot.VertexCacheExport},
		{Kind: snapshot.VertexInternal, Cached: true},
	}
	if !reflect.DeepEqual(s.Vertices, wantVertices) {
		t.Errorf("vertices = %+v, want %+v", s.Vertices, wantVertices)
	}

	for rev, want := range map[string][2]any{
		strings.Repeat("0", 40):            {strings.Repeat("0", 40), false},
		strings.Repeat("f", 64):            {strings.Repeat("f", 64), false},
		strings.Repeat("f", 64) + "-dirty": {strings.Repeat("f", 64), true},
		strings.Repeat("F", 40):            {"", false},
		strings.Repeat("0", 39):            {"", false},
		strings.Repeat("0", 41):            {"", false},
		strings.Repeat("0", 40) + "\n":     {"", false},
		strings.Repeat("0", 40) + "-x":     {"", false},
		"":                                 {"", false},
	} {
		if commit, dirty := parseRevision(rev); commit != want[0] || dirty != want[1] {
			t.Errorf("parseRevision(%q) = %q, %v, want %v", rev, commit, dirty, want)
		}
	}
	for _, tt := range []struct {
		value string
		want  *int64
	}{{"-5", new(int64(-5))}, {"+7", new(int64(7))}, {"builder", nil}, {"99999999999999999999", nil}, {"", nil}, {" 1", nil}} {
		s := newSnapshotOf(t, redacted{buildArgs: []buildArg{{name: "SOURCE_DATE_EPOCH", value: tt.value}}})
		if !reflect.DeepEqual(s.SourceDateEpoch, tt.want) {
			t.Errorf("SOURCE_DATE_EPOCH %q gave %v, want %v", tt.value, s.SourceDateEpoch, tt.want)
		}
	}
	if s := newSnapshotOf(t, redacted{buildArgs: []buildArg{{name: "SOURCE_DATE_EPOCH", value: "5", withheld: true}}}); s.SourceDateEpoch != nil {
		t.Errorf("a withheld SOURCE_DATE_EPOCH gave %v", *s.SourceDateEpoch)
	}
	unsorted := newSnapshotOf(t, redacted{buildArgs: []buildArg{{name: "B", value: "1"}, {name: "A", value: "2"}}, provenance: provenanceMissing})
	checkSnapshot(t, unsorted)
	if len(unsorted.BuildArgs) != 2 {
		t.Fatalf("build arguments = %+v, want two", unsorted.BuildArgs)
	}
	if got := []string{unsorted.BuildArgs[0].Name, unsorted.BuildArgs[1].Name}; !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("build arguments out of order gave %q, want them sorted", got)
	}
	for stages, want := range map[string][]string{"": {}, "a": {"a"}, "a,,b": {"a", "", "b"}} {
		s := newSnapshotOf(t, redacted{noCache: noCacheSetting{set: true, stages: stages}})
		if s.Cache.NoCache == nil || s.Cache.NoCache.Stages == nil || !slices.Equal(s.Cache.NoCache.Stages, want) {
			t.Errorf("no-cache %q gave %+v, want %q", stages, s.Cache.NoCache, want)
		}
	}
	for state, want := range map[provenanceState]snapshot.ProvenanceState{
		provenanceRead: snapshot.ProvenanceRead, provenanceMissing: snapshot.ProvenanceMissing,
		provenanceUnsupported: snapshot.ProvenanceUnsupported, provenanceNoSteps: snapshot.ProvenanceNoSteps,
	} {
		if got := provenanceStateOf(state); got != want {
			t.Errorf("provenanceStateOf(%v) = %q, want %q", state, got, want)
		}
	}
}

func TestSnapshotLimits(t *testing.T) {
	ctx := func(i int) string {
		return `{"Op":{"source":{"identifier":"local://context","attrs":{"local.excludepatterns":` + fmt.Sprintf("%q", list(60, fmt.Sprintf(`"p%d"`, i))) + `}}}}`
	}
	cp := `{"Op":{"file":{"actions":` + list(60, `{"Action":{"copy":{"src":"/a"}},"secondaryInput":0}`) + `}}}`
	twoOf := func(op func(i int) string) redacted {
		red := oneBuild(redStep(op(0)), redStep(op(1)))
		red.builds = append(red.builds, redactedBuild{platform: "linux/arm64", steps: []redactedStep{redStep(op(2))}})
		return red
	}
	two := func(op string) redacted { return twoOf(func(int) string { return op }) }
	env := twoOf(func(i int) string {
		vars := make([]string, 60)
		for k := range vars {
			vars[k] = fmt.Sprintf(`"V%d=%d"`, k, i)
		}
		return `{"Op":{"exec":{"meta":{"env":[` + strings.Join(vars, ",") + `]}}}}`
	})
	tests := []struct {
		what  string
		set   func(*snapshotLimits, int)
		red   redacted
		total int
	}{
		{".dockerignore patterns", func(l *snapshotLimits, n int) { l.patterns = n }, twoOf(ctx), 180},
		{"environment variables", func(l *snapshotLimits, n int) { l.envVars = n }, env, 180},
		{"sources read from other steps", func(l *snapshotLimits, n int) { l.sources = n }, two(cp), 180},
		{"no-cache stages", func(l *snapshotLimits, n int) { l.stages = n }, redacted{noCache: noCacheSetting{set: true, stages: strings.Repeat("s,", 179) + "s"}}, 180},
	}
	for _, tt := range tests {
		t.Run(tt.what, func(t *testing.T) {
			for _, limit := range []int{tt.total, tt.total - 1} {
				lim := defaultSnapshotLimits()
				tt.set(&lim, limit)
				_, err := newSnapshot(t.Context(), tt.red, lim)
				var le *LimitError
				over := limit < tt.total
				if got := errors.As(err, &le) && le.What == tt.what && le.Limit == int64(limit); got != over || (!over && err != nil) {
					t.Errorf("newSnapshot() of %d with a limit of %d error = %v, want LimitError %v", tt.total, limit, err, over)
				}
			}
		})
	}
}

func TestSnapshotCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := newSnapshot(ctx, oneBuild(redStep(imageOp)), defaultSnapshotLimits()); !errors.Is(err, context.Canceled) {
		t.Errorf("newSnapshot() error = %v, want context.Canceled", err)
	}
	if _, err := Snapshot(ctx, collectFixture(t, fixturePath(t, "control", "pr"))); !errors.Is(err, context.Canceled) {
		t.Errorf("Snapshot() error = %v, want context.Canceled", err)
	}
	if _, err := newSnapshot(ctx, redacted{provenance: provenanceMissing}, defaultSnapshotLimits()); err != nil {
		t.Errorf("newSnapshot() of a record with no steps error = %v, want nil", err)
	}
}

// TestSnapshotErrorsHoldNoRecordBytes plants a value where it makes the
// snapshot fail, and checks that the error does not hold it.
func TestSnapshotErrorsHoldNoRecordBytes(t *testing.T) {
	const value = "ichnos-fake-secret-snapshot"
	for name, red := range map[string]redacted{
		"duplicate name":                oneBuild(redStep(`{"Op":{"exec":{"meta":{"` + value + `":1,"` + value + `":2}}}}`)),
		"two operations":                oneBuild(redStep(`{"Op":{"exec":{},"` + value + `":{}}}`)),
		"environment entry":             oneBuild(redStep(`{"Op":{"exec":{"meta":{"env":[{"` + value + `":1}]}}}}`)),
		"source":                        oneBuild(redStep(`{"Op":{"source":"` + value + `"}}`)),
		"cycle":                         oneBuild(redStep(runOp(value), in(0))),
		"limit on no-cache":             {noCache: noCacheSetting{set: true, stages: strings.Repeat(value+",", maxNoCacheStages)}},
		"definition with data after it": oneBuild(redStep(`{"Op":{}} "` + value + `"`)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newSnapshot(t.Context(), red, defaultSnapshotLimits())
			if !isTyped(err) {
				t.Fatalf("newSnapshot() error = %v, want a typed error", err)
			}
			if strings.Contains(err.Error(), value) {
				t.Errorf("newSnapshot() error %q holds the planted value", err)
			}
		})
	}
}

// TestSnapshotTextIsValidUTF8 checks that Dockerfile text that is not valid
// UTF-8, which a Dockerfile can hold, is the same text in the snapshot as
// after a JSON round trip, and that redaction reads that text: a known
// value that holds a replacement character is removed from the text that
// the snapshot keeps.
func TestSnapshotTextIsValidUTF8(t *testing.T) {
	for _, tt := range []struct{ known, text, want string }{
		{"", "RUN a\xffb\xc3", "RUN a\ufffdb\ufffd"},
		{"abcd\ufffdefgh", "RUN echo abcd\xffefgh", "RUN echo ***"},
	} {
		rec := &Record{&recordData{attrs: map[string]string{"build-arg:API_TOKEN": tt.known}, provenance: provenanceRead, builds: []build{{
			platform: "linux/amd64",
			steps:    []step{{id: "s0", op: jsontext.Value(imageOp), text: tt.text, lines: lineRange{1, 1}, vertex: -1}},
		}}}}
		s := mustSnapshot(t, rec)
		checkSnapshot(t, s)
		if got := s.Platforms[0].Steps[0].Text; got != tt.want {
			t.Errorf("text %q gave %q, want %q", tt.text, got, tt.want)
		}
	}
}

// goldenEnv names the environment variable that rewrites the golden files
// instead of comparing with them: ICHNOS_UPDATE_GOLDEN=1 go test ./... The
// linters allow no flag outside internal/cli, so -update cannot be one.
const goldenEnv = "ICHNOS_UPDATE_GOLDEN"

// checkGolden compares the indented JSON of s with the golden file name in
// testdata/snapshot, or rewrites the file when goldenEnv is set.
func checkGolden(t *testing.T, name string, s *snapshot.Snapshot) {
	t.Helper()
	got, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "snapshot", name)
	if os.Getenv(goldenEnv) == "1" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if want := readFile(t, path); !bytes.Equal(got, want) {
		t.Errorf("snapshot differs from %s. If the change is on purpose, run %s=1 go test ./internal/collect and review the diff. "+
			"Any change to the JSON shape, as a field added, removed, renamed or retyped, or a new enum value, needs a new snapshot.SchemaVersion. "+
			"Any change that makes the same build give other fingerprints or other kept text, in how fingerprints are computed or what they leave out "+
			"or in the redaction rules, needs a new snapshot.FingerprintVersion, which the release notes list.", path, goldenEnv)
	}
}

// TestSnapshotGolden pins the JSON form of schema version 1 and of
// fingerprint version 1, byte for byte, on two lab records.
func TestSnapshotGolden(t *testing.T) {
	for _, tt := range []struct{ scenario, name string }{{"control", "pr"}, {"named-context", "pr"}} {
		t.Run(tt.scenario+"/"+tt.name, func(t *testing.T) {
			checkGolden(t, tt.scenario+"-"+tt.name+".json", mustSnapshot(t, collectFixture(t, fixturePath(t, tt.scenario, tt.name))))
		})
	}
}

// TestSnapshotGoldenAllFields pins every key of the JSON form, with a
// synthetic record that sets every field of a snapshot.
func TestSnapshotGoldenAllFields(t *testing.T) {
	envOp := `{"Op":{"exec":{"meta":{"args":["make"],"env":["PATH=/bin","API_TOKEN=***","DB=postgres://***@db/x"]},` +
		`"mounts":[{"dest":"/"},{"input":1,"selector":"/go.mod","dest":"/src/go.mod","output":-1,"readonly":true}]}},` +
		`"platform":{"OS":"linux","Architecture":"amd64"}}`
	run := redStep(envOp, in(0), in(1))
	run.text, run.lines, run.vertex, run.withheld, run.changedEnv = "RUN make", lineRange{2, 3}, 2, true, []int{2}
	copyStep := redStep(copyOp, in(4), in(1))
	copyStep.text, copyStep.lines, copyStep.vertex = "COPY config.txt .", lineRange{4, 4}, 0
	from := redStep(imageOp)
	from.text, from.lines, from.vertex = "FROM alpine:3.22", lineRange{1, 1}, 0
	ctx := redStep(contextOp)
	ctx.vertex = 1
	red := redacted{
		buildArgs: []buildArg{{name: "API_TOKEN", withheld: true}, {name: "MODE", value: "fast"}, {name: "SOURCE_DATE_EPOCH", value: "1700000000"}},
		imports:   []cacheType{cacheLocal, cacheRegistry, cacheGHA, cacheS3, cacheAzblob, cacheUnknown},
		noCache:   noCacheSetting{set: true, stages: "build,***", withheld: true},
		revision:  strings.Repeat("0123456789", 4) + "-dirty",
		contextDirs: []contextDir{
			{name: "assets", dir: "app/***", withheld: true},
			{name: "context", dir: "services/api"},
		},
		vertices: []redactedVertex{
			{kind: kindStep, cached: true, started: ts(1700000000, 5), completed: ts(1700000001, 0)},
			{kind: kindInternal, started: ts(1700000000, 0), completed: ts(1700000000, 1)},
			{kind: kindStep, cached: true, errored: true, started: ts(1700000002, 0), completed: ts(1700000003, 0)},
			{kind: kindImport, errored: true},
			{kind: kindExport, started: ts(1700000004, 0)},
		},
		builds: []redactedBuild{
			{platform: "linux/amd64", steps: []redactedStep{
				from, ctx,
				redStep(`{"Op":{"source":{"identifier":"git://github.com/ichnos-project/lab#main:app","attrs":{"git.fullurl":"https://github.com/ichnos-project/lab.git"}}}}`),
				redStep(`{"Op":{"source":{"identifier":"https://files.example/tool.tgz"}}}`),
				run, copyStep,
				redStep(`{"Op":{"merge":{"inputs":[{"input":0},{"input":1}]}}}`, in(2), stepInput{step: 3, output: 1}),
				redStep(`{"Op":{"source":{"identifier":"local://assets","attrs":{"local.sharedkeyhint":"assets"}}}}`),
				redStep(`{"Op":{"source":{"identifier":"blob://sha256:0123"}}}`),
			}},
			{platform: "linux/***", withheld: true, steps: []redactedStep{redStep(imageOp)}},
		},
		provenance: provenanceRead,
	}
	s := newSnapshotOf(t, red)
	checkSnapshot(t, s)
	checkGolden(t, "all-fields.json", s)
	keys := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				keys[k] = true
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	var v any
	if err := json.Unmarshal(mustJSON(t, s), &v); err != nil {
		t.Fatal(err)
	}
	walk(v)
	for _, k := range []string{
		"schemaVersion", "fingerprintVersion", "provenance", "platforms", "vertices", "cache", "buildArgs", "commit", "dirty", "sourceDateEpoch",
		"platform", "withheld", "steps", "environments", "contexts", "op", "text", "lines", "start", "end", "vertex", "inputs", "environment",
		"fingerprints", "command", "other", "ancestor", "withheldAncestor", "fromLocalContext", "fromGit", "fromHTTP", "sources", "context", "path",
		"sourceKind",
		"name", "value", "dir", "ignorePatterns", "kind", "cached", "lostHit", "errored", "started", "completed", "imports", "type", "noCache", "stages",
	} {
		if !keys[k] {
			t.Errorf("the synthetic snapshot sets no %q, so its golden file does not pin it", k)
		}
	}
}

func TestSnapshotNoRecord(t *testing.T) {
	for name, rec := range map[string]*Record{"nil": nil, "zero": {}} {
		if s, err := Snapshot(t.Context(), rec); err == nil || s != nil {
			t.Errorf("Snapshot() of a %s Record = %v, %v, want an error", name, s, err)
		}
	}
}

// TestSnapshotIsPure checks that a snapshot leaves its record alone, gives
// the same result every time, and shares nothing that a change to the
// record could reach.
func TestSnapshotIsPure(t *testing.T) {
	rec := collectFixture(t, fixturePath(t, "named-context", "pr"))
	before := dumpRecord(rec)
	once := mustSnapshot(t, rec)
	if dumpRecord(rec) != before {
		t.Fatal("Snapshot() changed the Record")
	}
	if twice := mustSnapshot(t, rec); !reflect.DeepEqual(once, twice) {
		t.Error("Snapshot() twice gave different results")
	}
	for i := range rec.builds[0].steps {
		clear(rec.builds[0].steps[i].op)
		for k := range rec.builds[0].steps[i].inputs {
			rec.builds[0].steps[i].inputs[k].step = 0
		}
	}
	if fresh := mustSnapshot(t, collectFixture(t, fixturePath(t, "named-context", "pr"))); !reflect.DeepEqual(once, fresh) {
		t.Error("changing the Record after Snapshot() changed the snapshot")
	}
}

func TestParseStringList(t *testing.T) {
	for _, tt := range []struct {
		s    string
		want []string
	}{{`[]`, nil}, {`null`, nil}, {` ["a", "\u0062"] `, []string{"a", "b"}}} {
		left := 10
		got, err := parseStringList(tt.s, &left, 10)
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("parseStringList(%q) = %q, %v, want %q", tt.s, got, err, tt.want)
		}
	}
	for _, s := range []string{``, `[`, `["a"] x`, `{}`, `"a"`, `[null]`, `[["a"]]`, "[\"\xff\"]"} {
		left := 10
		if got, err := parseStringList(s, &left, 10); !errors.Is(err, errNotValid) || got != nil {
			t.Errorf("parseStringList(%q) = %q, %v, want errNotValid", s, got, err)
		}
	}
}

func FuzzStringList(f *testing.F) {
	for _, s := range []string{`[]`, `null`, `[".dockerignore","unused.txt"]`, `["a"`, `[1]`, `["\u00e9"]`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		const limit = 8
		left := limit
		got, err := parseStringList(s, &left, limit)
		var le *LimitError
		switch {
		case errors.Is(err, errNotValid):
			var v []string
			if got != nil || (jsontext.Value(s).IsValid() && json.Unmarshal([]byte(s), &v) == nil) {
				t.Fatalf("parseStringList(%q) = %q, %v, but it is a list of strings", s, got, err)
			}
			return
		case err != nil:
			if !errors.As(err, &le) || got != nil {
				t.Fatalf("parseStringList(%q) = %q, %v", s, got, err)
			}
			return
		}
		if len(got) > limit || left != limit-len(got) {
			t.Fatalf("parseStringList(%q) kept %d strings and left %d of %d", s, len(got), left, limit)
		}
		var v []string
		if err := json.Unmarshal([]byte(s), &v); err != nil || !slices.Equal(v, got) {
			t.Fatalf("parseStringList(%q) = %q, but encoding/json reads %q, %v", s, got, v, err)
		}
	})
}

// fuzzKnown is the credential that FuzzSnapshot's seeds plant in the
// provenance, where BuildKit would have expanded a build argument.
const fuzzKnown = "ichnos-fuzz-known"

func FuzzSnapshot(f *testing.F) {
	plant := func(s string) string {
		for _, name := range []string{"config.txt", "unused.txt", "extra.txt", "linux/"} {
			s = strings.ReplaceAll(s, name, name+fuzzKnown)
		}
		return strings.ReplaceAll(s, "app.txt", "app.txt"+fuzzKnown[1:])
	}
	for _, p := range fixturePaths(f) {
		b := loadBundle(f, p)
		hex := b.layerHex(mediaTypeInToto)
		if hex == "" {
			f.Add(b.blob(b.configHex()), b.blob(b.layerHex(mediaTypeStatus)), []byte(nil), fuzzKnown)
			continue
		}
		f.Add(b.blob(b.configHex()), b.blob(b.layerHex(mediaTypeStatus)), b.blob(hex), fuzzKnown)
		b.editDockerfile(plant)
		planted := plant(string(b.blob(b.layerHex(mediaTypeInToto))))
		f.Add(b.blob(b.configHex()), b.blob(b.layerHex(mediaTypeStatus)), []byte(planted), fuzzKnown)
	}
	lim := testLimits()
	lim.steps = 256
	lim.vertices = 256
	lim.textBytes = 1 << 16
	f.Fuzz(func(t *testing.T, history, status, provenance []byte, known string) {
		p := recordParts{history: history, status: status}
		if provenance != nil {
			p.provenances = [][]byte{provenance}
		}
		rec, err := p.archive(t).decode(t.Context(), lim)
		if err != nil {
			return
		}
		if rec.attrs == nil {
			rec.attrs = map[string]string{}
		}
		rec.attrs["build-arg:API_TOKEN"] = known
		s, err := Snapshot(t.Context(), rec)
		if err != nil {
			if !isTyped(err) {
				t.Fatalf("Snapshot() error = %v, want a typed error", err)
			}
			return
		}
		checkSnapshot(t, s)
		if again := mustSnapshot(t, rec); !reflect.DeepEqual(s, again) {
			t.Fatal("Snapshot() is not deterministic")
		}
		if scrubbable(known) && !strings.Contains(known, "*") {
			for _, text := range snapshotText(t, s) {
				if strings.Contains(text, known) {
					t.Fatalf("snapshot holds the credential %q in %q", known, text)
				}
			}
			for _, text := range snapshotText(t, s) {
				for i := 0; i+fragmentBytes <= len(known); i++ {
					if strings.Contains(text, known[i:i+fragmentBytes]) {
						t.Fatalf("snapshot holds %q of the credential %q in %q", known[i:i+fragmentBytes], known, text)
					}
				}
			}
		}
	})
}

// snapshotText returns every string of a snapshot's JSON that comes from
// the record: all but fingerprints, times and enum values, which hold no
// text of the record.
func snapshotText(tb testing.TB, s *snapshot.Snapshot) []string {
	tb.Helper()
	var v any
	if err := json.Unmarshal(mustJSON(tb, s), &v); err != nil {
		tb.Fatal(err)
	}
	var out []string
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				walk(k, child)
			}
		case []any:
			for _, child := range x {
				walk(key, child)
			}
		case string:
			switch key {
			case "command", "other", "ancestor", "value", "started", "completed", "op", "kind", "type", "provenance":
			default:
				out = append(out, x)
			}
		}
	}
	walk("", v)
	return out
}

// syntheticRecord is a build of n steps in the shape of a real one: a base
// image, a build context, then RUN steps with 30 environment variables
// alternating with COPY steps, each reading the step before it and with a
// Dockerfile line and a vertex of its own. One variable changes every ten
// RUN steps, as an ENV or ARG line between them would change it.
func syntheticRecord(n int) *Record {
	steps := make([]step, n)
	vertices := make([]vertex, n)
	env := make([]string, 30)
	for i := range env {
		env[i] = fmt.Sprintf("%q", fmt.Sprintf("VAR_%02d=value-%02d-for-a-realistic-length", i, i))
	}
	envJSON := func(i int) string {
		changed := fmt.Sprintf("%q", fmt.Sprintf("STAGE=%d", i/20))
		return "[" + strings.Join(env, ",") + "," + changed + "]"
	}
	for i := range steps {
		var op string
		switch {
		case i == 0:
			op = imageOp
		case i == 1:
			op = contextOp
		case i%2 == 0:
			op = `{"Op":{"exec":{"meta":{"args":["/bin/sh","-c","make target-` + fmt.Sprint(i) + ` && echo done"],"env":` + envJSON(i) +
				`,"cwd":"/src","removeMountStubsRecursive":true},"mounts":[{"dest":"/"}]}},"constraints":{},"platform":{"Architecture":"amd64","OS":"linux"}}`
		default:
			op = `{"Op":{"file":{"actions":[{"Action":{"copy":{"src":"/file-` + fmt.Sprint(i) + `.txt","dest":"/src/","mode":-1,"followSymlink":true,` +
				`"dirCopyContents":true,"createDestPath":true,"allowWildcard":true,"allowEmptyWildcard":true,"timestamp":-1}},"input":0,"output":0,"secondaryInput":1}]}},"constraints":{}}`
		}
		steps[i] = step{id: fmt.Sprintf("step%d", i), op: jsontext.Value(op), vertex: i, text: fmt.Sprintf("RUN line %d", i), lines: lineRange{i + 1, i + 1}}
		switch {
		case i == 0, i == 1:
		case i%2 == 0:
			steps[i].inputs = []stepInput{{step: i - 1}}
		default:
			steps[i].inputs = []stepInput{{step: i - 1}, {step: 1}}
		}
		vertices[i] = vertex{digest: fmt.Sprintf("sha256:%064x", i), kind: kindStep, cached: i%3 == 0, started: ts(int64(i), 0), completed: ts(int64(i)+1, 0)}
	}
	return &Record{&recordData{
		attrs:      map[string]string{"build-arg:MODE": "fast", "cache-imports": `[{"Type":"gha"}]`},
		vertices:   vertices,
		builds:     []build{{platform: "linux/amd64", steps: steps}},
		provenance: provenanceRead,
	}}
}

// BenchmarkSnapshot takes the snapshot of every lab record, which are the
// size of most records, and of a synthetic build of 100,000 steps, the
// collector's step limit.
func BenchmarkSnapshot(b *testing.B) {
	for _, bm := range []struct {
		name    string
		records func() []*Record
	}{
		{"lab records", func() []*Record {
			var lab []*Record
			for _, p := range fixturePaths(b) {
				lab = append(lab, collectFixture(b, p))
			}
			return lab
		}},
		{"100000 steps", func() []*Record { return []*Record{syntheticRecord(maxSteps)} }},
		{"100000 steps with a credential", func() []*Record {
			r := syntheticRecord(maxSteps)
			r.attrs["build-arg:API_TOKEN"] = "ichnos-fake-benchmark-token-qwertyuiop"
			return []*Record{r}
		}},
	} {
		b.Run(bm.name, func(b *testing.B) {
			records := bm.records()
			b.ReportAllocs()
			for b.Loop() {
				for _, r := range records {
					if _, err := Snapshot(b.Context(), r); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
