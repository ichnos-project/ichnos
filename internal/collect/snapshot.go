package collect

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ichnos-project/ichnos/pkg/snapshot"
)

const (
	// maxEnvVars caps the environment variables that a snapshot keeps: each
	// distinct environment of a platform is kept once, however many steps
	// share it. A kept variable costs about 80 bytes, against 4 bytes of
	// JSON for the shortest entry; at the cap, a snapshot held 77 MiB.
	maxEnvVars = 10 * maxSteps
	// maxStepEnvVars caps the entries of one RUN step's environment, which
	// the hasher holds while it reads them, about 100 bytes each, before it
	// knows whether the environment is new. A step of 1,000,000 distinct
	// variables, a definition of 12 MB, stops at the cap after allocating
	// 3.5 MB.
	maxStepEnvVars = 10_000
	// maxReadSources caps the paths that the copy actions and mounts
	// of all steps read from another step.
	maxReadSources = 10 * maxSteps
	// maxIgnorePatterns caps the .dockerignore patterns of all build
	// contexts.
	maxIgnorePatterns = 100_000
	// maxNoCacheStages caps the stages that --no-cache-filter names.
	maxNoCacheStages = 10_000
	// maxPathBytes is the longest build context directory or source path
	// that a snapshot keeps.
	maxPathBytes = 4096
)

// snapshotLimits caps what a snapshot keeps from one record, on top of what
// the collector keeps. Tests lower them to reach each limit with small
// inputs.
type snapshotLimits struct {
	heldMembers int
	envVars     int
	stepEnvVars int
	sources     int
	patterns    int
	stages      int
}

func defaultSnapshotLimits() snapshotLimits {
	return snapshotLimits{
		heldMembers: maxHeldMembers,
		envVars:     maxEnvVars,
		stepEnvVars: maxStepEnvVars,
		sources:     maxReadSources,
		patterns:    maxIgnorePatterns,
		stages:      maxNoCacheStages,
	}
}

var errNoRecord = errors.New("no build record to take a snapshot of")

// Snapshot returns the snapshot of the build that r records. It redacts r
// first, and builds the snapshot only from what redaction keeps, so that
// every value it hashes or keeps has had its credentials removed.
//
// It returns ErrMalformedRecord when a step's inputs form a cycle, or a step
// definition does not have the shape that BuildKit writes, such as an object
// with a duplicate name; a *LimitError when the record holds more than a
// snapshot keeps; an error that wraps ctx.Err() when ctx ends first, which
// it checks between steps; and an error for a nil or zero Record.
func Snapshot(ctx context.Context, r *Record) (*snapshot.Snapshot, error) {
	if r == nil || r.recordData == nil {
		return nil, errNoRecord
	}
	red, err := redact(ctx, r)
	if err != nil {
		return nil, err
	}
	return newSnapshot(ctx, red, defaultSnapshotLimits())
}

func newSnapshot(ctx context.Context, red redacted, lim snapshotLimits) (*snapshot.Snapshot, error) {
	h := newHasher(lim)
	s := &snapshot.Snapshot{
		SchemaVersion:      snapshot.SchemaVersion,
		FingerprintVersion: snapshot.FingerprintVersion,
		Provenance:         provenanceStateOf(red.provenance),
		Platforms:          make([]snapshot.Platform, 0, len(red.builds)),
		Vertices:           make([]snapshot.Vertex, len(red.vertices)),
		Cache:              snapshot.Cache{Imports: make([]snapshot.CacheImport, len(red.imports))},
		BuildArgs:          make([]snapshot.BuildArg, len(red.buildArgs)),
	}
	for i, v := range red.vertices {
		s.Vertices[i] = vertexOf(v)
	}
	for i, t := range red.imports {
		s.Cache.Imports[i] = snapshot.CacheImport{Type: cacheTypeOf(t)}
	}
	if red.noCache.set {
		stages, err := noCacheStages(red.noCache.stages, lim.stages)
		if err != nil {
			return nil, err
		}
		s.Cache.NoCache = &snapshot.NoCache{Stages: stages, Withheld: red.noCache.withheld}
	}
	for i, a := range red.buildArgs {
		s.BuildArgs[i] = h.buildArg(a)
		if a.name == "SOURCE_DATE_EPOCH" && !a.withheld {
			if n, err := strconv.ParseInt(a.value, 10, 64); err == nil {
				s.SourceDateEpoch = &n
			}
		}
	}
	slices.SortStableFunc(s.BuildArgs, func(x, y snapshot.BuildArg) int { return cmp.Compare(x.Name, y.Name) })
	s.Commit, s.Dirty = parseRevision(red.revision)

	b := platformBuilder{
		h: h, vertices: len(red.vertices), dirs: make(map[string]contextDir, len(red.contextDirs)),
		patternsLeft: lim.patterns, patternLimit: lim.patterns, envLimit: lim.envVars,
	}
	for _, d := range red.contextDirs {
		if _, dup := b.dirs[d.name]; dup {
			d = contextDir{name: d.name, withheld: true}
		}
		b.dirs[d.name] = d
	}
	for _, rb := range red.builds {
		p, err := b.platform(ctx, rb)
		if err != nil {
			return nil, err
		}
		s.Platforms = append(s.Platforms, p)
	}
	slices.SortStableFunc(s.Platforms, func(x, y snapshot.Platform) int { return cmp.Compare(x.Platform, y.Platform) })
	return s, nil
}

func provenanceStateOf(p provenanceState) snapshot.ProvenanceState {
	switch p {
	case provenanceRead:
		return snapshot.ProvenanceRead
	case provenanceMissing:
		return snapshot.ProvenanceMissing
	case provenanceUnsupported:
		return snapshot.ProvenanceUnsupported
	case provenanceNoSteps:
		return snapshot.ProvenanceNoSteps
	}
	return snapshot.ProvenanceUnsupported
}

func vertexOf(v redactedVertex) snapshot.Vertex {
	out := snapshot.Vertex{
		Cached:    v.hit(),
		LostHit:   v.lostHit(),
		Errored:   v.errored,
		Started:   timeOf(v.started),
		Completed: timeOf(v.completed),
	}
	switch v.kind {
	case kindStep:
		out.Kind = snapshot.VertexStep
	case kindInternal:
		out.Kind = snapshot.VertexInternal
	case kindImport:
		out.Kind = snapshot.VertexCacheImport
	case kindExport:
		out.Kind = snapshot.VertexCacheExport
	}
	return out
}

func timeOf(t timestamp) time.Time {
	if !t.valid {
		return time.Time{}
	}
	return time.Unix(t.seconds, int64(t.nanos)).UTC()
}

func cacheTypeOf(c cacheType) snapshot.CacheType {
	switch c {
	case cacheLocal:
		return snapshot.CacheLocal
	case cacheRegistry:
		return snapshot.CacheRegistry
	case cacheGHA:
		return snapshot.CacheGHA
	case cacheS3:
		return snapshot.CacheS3
	case cacheAzblob:
		return snapshot.CacheAzblob
	case cacheUnknown:
	}
	return snapshot.CacheUnknown
}

// noCacheStages reads the no-cache attribute as the Dockerfile frontend
// does: empty for every stage, or stage names separated by commas
// (frontend/dockerui/config.go at v0.34.0).
func noCacheStages(stages string, limit int) ([]string, error) {
	switch {
	case stages == "":
		return []string{}, nil
	case strings.Count(stages, ",") >= limit:
		return nil, &LimitError{What: "no-cache stages", Limit: int64(limit)}
	}
	return strings.Split(stages, ","), nil
}

// buildArg returns a build argument with its value hashed, or withheld.
func (h *hasher) buildArg(a buildArg) snapshot.BuildArg {
	if a.withheld {
		return snapshot.BuildArg{Name: a.name, Withheld: true}
	}
	return snapshot.BuildArg{Name: a.name, Value: h.sumFields(tagBuildArg, []byte(a.name), []byte(a.value))}
}

// revisionSyntax is the vcs:revision that buildx writes: a full SHA-1 or
// SHA-256 commit, with -dirty when BUILDX_GIT_CHECK_DIRTY is set and the
// work tree has changes (build/git.go at buildx v0.28.0).
var revisionSyntax = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})(-dirty)?$`)

func parseRevision(rev string) (string, bool) {
	m := revisionSyntax.FindStringSubmatch(rev)
	if m == nil {
		return "", false
	}
	return m[1], m[2] != ""
}

// platformBuilder builds the platforms of one snapshot. patternsLeft is
// what is left of patternLimit, the limit on .dockerignore patterns, and
// envVars is how many environment variables the environments kept so far
// hold, against envLimit; all platforms share both limits.
type platformBuilder struct {
	h            *hasher
	vertices     int
	dirs         map[string]contextDir
	patternsLeft int
	patternLimit int
	envVars      int
	envLimit     int
}

// platform returns the snapshot of one build. It visits each step once,
// after its inputs, so it computes each ancestor fingerprint from those of
// the inputs.
func (b *platformBuilder) platform(ctx context.Context, rb redactedBuild) (snapshot.Platform, error) {
	order, err := inputsFirst(rb.steps)
	if err != nil {
		return snapshot.Platform{}, err
	}
	index := make([]int, len(rb.steps))
	for i, old := range order {
		index[old] = i
	}
	p := snapshot.Platform{
		Platform:     rb.platform,
		Withheld:     rb.withheld,
		Steps:        make([]snapshot.Step, len(order)),
		Environments: [][]snapshot.EnvVar{},
		Contexts:     []snapshot.Context{},
	}
	total := 0
	for _, st := range rb.steps {
		total += len(st.inputs)
	}
	inputs := make([]int, total)
	contextOf := map[int]int{}
	contextKeys := map[snapshot.Fingerprint]int{}
	envOf := map[snapshot.Fingerprint]int{}
	for i, old := range order {
		if err := ctx.Err(); err != nil {
			return snapshot.Platform{}, fmt.Errorf("taking a snapshot: %w", err)
		}
		st := rb.steps[old]
		if st.vertex < -1 || st.vertex >= b.vertices {
			return snapshot.Platform{}, malformed("step has a vertex that the record does not have")
		}
		facts, err := b.h.readOp(st.op, st.changedEnv)
		if err != nil {
			return snapshot.Platform{}, err
		}
		step := snapshot.Step{
			Op:          facts.kind,
			Text:        st.text,
			Vertex:      st.vertex,
			Environment: -1,
			Withheld:    st.withheld || facts.withheld,
			Fingerprints: snapshot.Fingerprints{
				Command: facts.command,
				Other:   facts.other,
			},
		}
		if st.lines != (lineRange{}) {
			step.Lines = snapshot.Lines{Start: st.lines.start, End: st.lines.end}
		}
		if len(facts.env) > 0 {
			if step.Environment, err = b.environment(&p, envOf, facts.env, facts.envKey); err != nil {
				return snapshot.Platform{}, err
			}
		}
		for _, e := range facts.env {
			step.Withheld = step.Withheld || e.Withheld
		}
		step.WithheldAncestor = step.Withheld
		step.SourceKind = facts.source
		switch facts.source {
		case snapshot.SourceLocal:
			step.FromLocalContext = true
			if contextOf[i], err = b.context(&p, contextKeys, facts, i, st.withheld); err != nil {
				return snapshot.Platform{}, err
			}
		case snapshot.SourceGit:
			step.FromGit = true
		case snapshot.SourceHTTP:
			step.FromHTTP = true
		case snapshot.SourceImage, snapshot.SourceOther:
		}
		self := b.h.self(facts)
		b.h.scrap = append(b.h.scrap[:0], self[:]...)
		if n := len(st.inputs); n > 0 {
			step.Inputs, inputs = inputs[:n:n], inputs[n:]
		}
		for k, in := range st.inputs {
			j := index[in.step]
			step.Inputs[k] = j
			parent := &p.Steps[j]
			step.FromLocalContext = step.FromLocalContext || parent.FromLocalContext
			step.FromGit = step.FromGit || parent.FromGit
			step.FromHTTP = step.FromHTTP || parent.FromHTTP
			step.WithheldAncestor = step.WithheldAncestor || parent.WithheldAncestor
			b.h.scrap = append(b.h.scrap, parent.Fingerprints.Ancestor[:]...)
			b.h.scrap = binary.AppendVarint(b.h.scrap, int64(in.output))
		}
		step.Fingerprints.Ancestor = b.h.sum(tagAncestor, b.h.scrap)
		for _, c := range facts.reads {
			if c.input >= len(st.inputs) {
				continue
			}
			ctxIndex, ok := contextOf[index[st.inputs[c.input].step]]
			if !ok {
				continue
			}
			path, ok := sourcePath(c.src)
			if !ok {
				step.Withheld, step.WithheldAncestor = true, true
				continue
			}
			step.Sources = append(step.Sources, snapshot.Source{Context: ctxIndex, Path: path})
		}
		p.Steps[i] = step
	}
	return p, nil
}

// environment returns the index in p.Environments of env, whose key is key,
// and adds a copy of it when the platform has no equal environment yet.
// Only an added environment counts against the limit, so steps that share
// one cost one copy, in memory and in the snapshot's JSON.
func (b *platformBuilder) environment(p *snapshot.Platform, envOf map[snapshot.Fingerprint]int, env []snapshot.EnvVar, key snapshot.Fingerprint) (int, error) {
	if k, ok := envOf[key]; ok {
		return k, nil
	}
	if len(env) > b.envLimit-b.envVars {
		return 0, &LimitError{What: "environment variables", Limit: int64(b.envLimit)}
	}
	b.envVars += len(env)
	kept := make([]snapshot.EnvVar, len(env))
	copy(kept, env)
	envOf[key] = len(p.Environments)
	p.Environments = append(p.Environments, kept)
	return len(p.Environments) - 1, nil
}

// self returns the fingerprint of a step without its inputs: its command,
// with the details of a build context left out, its other fields and the
// key of its environment, which covers each variable's name and value
// fingerprint, and only the name of a withheld variable.
func (h *hasher) self(facts opFacts) snapshot.Fingerprint {
	h.scrap = append(h.scrap[:0], facts.contextCommand[:]...)
	h.scrap = append(h.scrap, facts.other[:]...)
	if len(facts.env) > 0 {
		h.scrap = append(h.scrap, facts.envKey[:]...)
	}
	return h.sum(tagSelf, h.scrap)
}

// context returns the index in p.Contexts of the local build context that
// the step at index i loads, and adds the context when the platform has no
// equal one yet. Steps that load equal contexts share one, so a directory
// that the record gives once is kept once, however many steps load it.
func (b *platformBuilder) context(p *snapshot.Platform, keys map[snapshot.Fingerprint]int, facts opFacts, i int, withheld bool) (int, error) {
	var flags [2]byte
	if withheld {
		flags[0] = 1
	}
	if facts.hasPatterns {
		flags[1] = 1
	}
	key := b.h.sumFields(tagContextKey, []byte(facts.localName), flags[:], []byte(facts.excludePatterns))
	if k, ok := keys[key]; ok {
		p.Contexts[k].Steps = append(p.Contexts[k].Steps, i)
		return k, nil
	}
	c := snapshot.Context{Name: facts.localName, Steps: []int{i}, Withheld: withheld}
	if d, ok := b.dirs[facts.localName]; ok {
		switch {
		case d.withheld:
			c.Withheld = true
		case validPath(d.dir):
			c.Dir = d.dir
		}
	}
	if facts.hasPatterns {
		patterns, err := parseStringList(facts.excludePatterns, &b.patternsLeft, b.patternLimit)
		switch {
		case errors.Is(err, errNotValid):
			c.Withheld = true
		case err != nil:
			return 0, err
		default:
			c.IgnorePatterns = patterns
		}
	}
	keys[key] = len(p.Contexts)
	p.Contexts = append(p.Contexts, c)
	return len(p.Contexts) - 1, nil
}

// sourcePath returns a COPY or ADD source path, which BuildKit records
// cleaned and from the root of the build context, as /app.txt, as a path
// relative to the context, "." for the context itself, and reports whether
// it passes validPath. A path that cleaning would change is rejected rather
// than cleaned, because cleaning could join text into a value that
// redaction had no reason to remove, as ab/./cd into ab/cd.
func sourcePath(src string) (string, bool) {
	p := strings.TrimPrefix(src, "/")
	if p == "" {
		p = "."
	}
	return p, path.Clean(p) == p && validPath(p)
}

// validPath reports whether p is a plain slash-separated relative path that
// stays where it starts: fs.ValidPath holds, there is no backslash or
// colon, every character is graphic, so none is a control, format or line
// separator character, and no element is a name that Windows reserves. A
// crafted record could use any of those to point elsewhere or to change how
// the path reads, and a snapshot can be read on another system.
//
// buildx writes a build context's directory with filepath.Rel, "." for the
// root of the work tree (build/git.go at buildx v0.28.0), so on Windows it
// writes backslashes, and such a directory fails here. It is not converted,
// because a backslash is a legal character in a Linux file name, and the
// snapshot would then name a directory that was never used.
func validPath(p string) bool {
	if len(p) > maxPathBytes || !fs.ValidPath(p) || strings.ContainsAny(p, `\:`) {
		return false
	}
	for _, c := range p {
		if !unicode.IsGraphic(c) {
			return false
		}
	}
	for elem := range strings.SplitSeq(p, "/") {
		if isReservedWindowsName(elem) {
			return false
		}
	}
	return true
}

// isReservedWindowsName reports whether a path element is a name that
// Windows reserves for a device: the part before its first dot or colon,
// without trailing spaces, is CON, PRN, AUX, NUL, COM or LPT with a digit,
// or CONIN$ or CONOUT$, with ASCII letters in any case, as isReservedName
// in internal/filepathlite reads it at Go 1.27. Windows 11 no longer
// reserves such a name with an extension, as NUL.txt, but older versions
// do, so it counts here.
func isReservedWindowsName(elem string) bool {
	base := elem
	if i := strings.IndexAny(base, ".:"); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimRight(base, " ")
	for _, name := range [...]string{"CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$"} {
		if equalFoldASCII(base, name) {
			return true
		}
	}
	if len(base) < 4 || !equalFoldASCII(base[:3], "COM") && !equalFoldASCII(base[:3], "LPT") {
		return false
	}
	switch base[3:] {
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "\u00b9", "\u00b2", "\u00b3":
		return true
	}
	return false
}

// equalFoldASCII reports whether s equals upper, which is in upper case,
// when the ASCII letters of s are folded to upper case. Windows folds no
// other letter in a device name, so a dotless i (U+0131) is not I, as
// strings.ToUpper and strings.EqualFold would make it.
func equalFoldASCII(s, upper string) bool {
	if len(s) != len(upper) {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != upper[i] {
			return false
		}
	}
	return true
}

// parseStringList reads the .dockerignore patterns of local.excludepatterns,
// and counts each against what is left of the limit before it keeps it. It
// returns errNotValid when s is not a JSON list of strings, as when
// redaction changed it.
func parseStringList(s string, left *int, limit int) ([]string, error) {
	var out []string
	err := readStringList(s, func(v string) error {
		if *left == 0 {
			return &LimitError{What: ".dockerignore patterns", Limit: int64(limit)}
		}
		*left--
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// inputsFirst returns an order of the steps in which every step comes after
// its inputs, keeping the given order where it already does, as BuildKit
// writes it. It returns ErrMalformedRecord when the inputs form a cycle. It
// walks each step's inputs depth first with a stack of its own, so a long
// chain of steps cannot overflow the call stack.
func inputsFirst(steps []redactedStep) ([]int, error) {
	const (
		unvisited = iota
		visiting
		done
	)
	type frame struct{ step, next int }
	state := make([]uint8, len(steps))
	order := make([]int, 0, len(steps))
	var stack []frame
	for root := range steps {
		if state[root] != unvisited {
			continue
		}
		state[root] = visiting
		stack = append(stack[:0], frame{step: root})
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			inputs := steps[f.step].inputs
			if f.next == len(inputs) {
				state[f.step] = done
				order = append(order, f.step)
				stack = stack[:len(stack)-1]
				continue
			}
			in := inputs[f.next].step
			f.next++
			if in < 0 || in >= len(steps) {
				return nil, malformed("provenance has a step input that names no step")
			}
			switch state[in] {
			case unvisited:
				state[in] = visiting
				stack = append(stack, frame{step: in})
			case visiting:
				return nil, malformed("provenance has steps whose inputs form a cycle")
			}
		}
	}
	return order, nil
}
