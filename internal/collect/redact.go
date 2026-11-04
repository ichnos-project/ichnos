package collect

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"iter"
	"maps"
	"math/bits"
	"slices"
	"strings"
)

const (
	// withheldValue replaces each value that redaction removes. It is
	// shorter than minScrubBytes, so a later pass never scrubs it.
	withheldValue = "***"
	// minScrubBytes is the shortest known value that is scrubbed from other
	// strings. Shorter values are still withheld where they are set, but
	// removing them everywhere would destroy unrelated text.
	minScrubBytes = 4
)

// redacted is a Record with its credentials removed. It keeps only the
// fields that Ichnos uses: the build arguments, the cache settings, each
// vertex's kind and result, each step's definition and Dockerfile text, and
// the provenance state, which says why a record has no builds. Vertex
// digests are dropped. Other values stay in plain text until a snapshot
// hashes them.
type redacted struct {
	buildArgs  []buildArg
	imports    []cacheType
	noCache    noCacheSetting
	vertices   []redactedVertex
	builds     []redactedBuild
	provenance provenanceState
}

type buildArg struct {
	name  string
	value string
	// withheld is true when the value is missing or incomplete, because its
	// name looks like a credential or part of it was redacted.
	withheld bool
}

// noCacheSetting is the no-cache attribute. stages is empty when it covers
// every stage.
type noCacheSetting struct {
	set    bool
	stages string
}

type cacheType int

const (
	cacheUnknown cacheType = iota
	cacheLocal
	cacheRegistry
	cacheGHA
	cacheS3
	cacheAzblob
)

func (c cacheType) String() string {
	switch c {
	case cacheUnknown:
		return "unknown"
	case cacheLocal:
		return "local"
	case cacheRegistry:
		return "registry"
	case cacheGHA:
		return "gha"
	case cacheS3:
		return "s3"
	case cacheAzblob:
		return "azblob"
	}
	return fmt.Sprintf("cacheType(%d)", int(c))
}

type redactedVertex struct {
	kind vertexKind
	vertexStatus
}

type redactedBuild struct {
	platform string
	steps    []redactedStep
}

type redactedStep struct {
	id     string
	inputs []string
	// op is the redacted definition as JSON, with BuildKit's key order and,
	// where nothing was redacted, BuildKit's bytes.
	op     jsontext.Value
	text   string
	lines  lineRange
	vertex int
	// withheld is true when the step has an input whose name looks like a
	// credential, or when redaction removed or replaced any of its values, so
	// a comparison cannot tell whether that input changed.
	withheld bool
}

// maxKnownBytes caps the total size of the distinct credential values that
// are removed from every other string. Their search holds about 33 bytes per
// byte of them, about 33 MiB at the cap.
const maxKnownBytes = 1 << 20

// maxStringBytes is the longest string that redaction inspects. A longer one
// is withheld whole without being read, which bounds the time and memory
// that one string takes.
const maxStringBytes = 1 << 20

// redact returns r without credentials. It never changes r, and the result
// shares no map or slice with it. A value is withheld when its name looks
// like a credential; URLs, and proxy settings with no scheme, lose their user
// information and query values; and the values of credential build
// arguments and environment variables are removed from every other string,
// because BuildKit expands them into COPY sources, ADD URLs, heredocs and
// image references. It returns a *LimitError when those values are too large
// to search for.
func redact(r *Record) (redacted, error) {
	s, err := newScrubber(r)
	if err != nil {
		return redacted{}, err
	}
	imports, err := cacheImports(r.attrs)
	if err != nil {
		return redacted{}, err
	}
	out := redacted{
		buildArgs:  s.buildArgs(r.attrs),
		imports:    imports,
		noCache:    s.noCache(r.attrs),
		vertices:   make([]redactedVertex, len(r.vertices)),
		builds:     make([]redactedBuild, len(r.builds)),
		provenance: r.provenance,
	}
	for i, v := range r.vertices {
		out.vertices[i] = redactedVertex{kind: v.kind, vertexStatus: v.vertexStatus}
	}
	for i, b := range r.builds {
		var ignored bool
		steps := make([]redactedStep, len(b.steps))
		for j, st := range b.steps {
			if steps[j], err = s.step(st); err != nil {
				return redacted{}, err
			}
		}
		out.builds[i] = redactedBuild{platform: s.str(b.platform, &ignored), steps: steps}
	}
	return out, nil
}

// cacheImports returns the type of each cache import. An attribute that
// cannot be read counts as one import of an unknown type, because the build
// did ask for a cache.
func cacheImports(attrs map[string]string) ([]cacheType, error) {
	raw, ok := attrs["cache-imports"]
	if !ok {
		return nil, nil
	}
	j := newJSONStreamReader(strings.NewReader(raw), jsontext.AllowInvalidUTF8(true))
	out := []cacheType{}
	err := j.elements(func() error {
		if len(out) == maxCacheImports {
			return &LimitError{What: "cache imports", Limit: maxCacheImports}
		}
		var typ string
		err := j.fields([]string{"Type"}, func(int) error {
			var err error
			typ, err = j.str()
			return err
		})
		out = append(out, parseCacheType(typ))
		return err
	})
	if err == nil {
		err = j.end()
	}
	var le *LimitError
	switch {
	case err == nil:
		return out, nil
	case errors.As(err, &le):
		return nil, err
	default:
		return []cacheType{cacheUnknown}, nil
	}
}

const maxCacheImports = 1000

func parseCacheType(s string) cacheType {
	switch s {
	case "local":
		return cacheLocal
	case "registry":
		return cacheRegistry
	case "gha":
		return cacheGHA
	case "s3":
		return cacheS3
	case "azblob":
		return cacheAzblob
	}
	return cacheUnknown
}

// scrubber removes the known credential values of one record from strings.
// known is nil when the record has none.
type scrubber struct {
	known *knownSet
}

// newScrubber collects the distinct credential values of r. It counts their
// size as it finds them, so a record cannot make it hold more than the limit.
func newScrubber(r *Record) (scrubber, error) {
	known := map[string]bool{}
	total := 0
	add := func(v string) error {
		if !scrubbable(v) || known[v] {
			return nil
		}
		if total += len(v); total > maxKnownBytes {
			return &LimitError{What: "bytes of credential values", Limit: maxKnownBytes}
		}
		known[v] = true
		return nil
	}
	for k, v := range r.attrs {
		if name, ok := strings.CutPrefix(k, "build-arg:"); ok && isCredentialName(name) {
			if err := add(v); err != nil {
				return scrubber{}, err
			}
		}
	}
	for _, b := range r.builds {
		for _, st := range b.steps {
			if err := forEachEnvValue(st.op, add); err != nil {
				return scrubber{}, err
			}
		}
	}
	if len(known) == 0 {
		return scrubber{}, nil
	}
	return scrubber{known: newKnownSet(slices.Sorted(maps.Keys(known)))}, nil
}

// scrubbable reports whether a credential value is removed from every other
// string. A value shorter than minScrubBytes is not, and neither is a word
// that switches a setting on or off: no credential is one, and removing it
// everywhere would hide unrelated flags, such as --strict-ssl=true when
// AUTH_ENABLED=true is set.
func scrubbable(v string) bool {
	if len(v) < minScrubBytes {
		return false
	}
	for _, flag := range [...]string{"true", "false", "enable", "disable", "enabled", "disabled"} {
		// Equal lengths keep the match to ASCII. EqualFold alone would also
		// match spellings such as fal\u017fe, which a credential could be.
		if len(v) == len(flag) && strings.EqualFold(v, flag) {
			return false
		}
	}
	return true
}

// forEachEnvValue calls f with the value of each credential environment
// variable in a step definition: the NAME=VALUE strings directly inside a
// list under the key env, at any depth.
func forEachEnvValue(op jsontext.Value, f func(string) error) error {
	if op == nil {
		return nil
	}
	w := envWalker{j: newJSONReader(op), f: f}
	err := w.value("")
	var le *LimitError
	if err != nil && !errors.As(err, &le) {
		return malformed("step definition is not valid JSON")
	}
	return err
}

type envWalker struct {
	j *jsonReader
	f func(string) error
}

// value reads the next value, which sits under key.
func (w *envWalker) value(key string) error {
	switch w.j.peek() {
	case jsontext.KindBeginObject:
		if err := w.j.next(); err != nil {
			return err
		}
		for w.j.peek() != jsontext.KindEndObject {
			_, name, err := w.j.readString()
			if err != nil {
				return err
			}
			if err := w.value(name); err != nil {
				return err
			}
		}
		return w.j.next()
	case jsontext.KindBeginArray:
		if err := w.j.next(); err != nil {
			return err
		}
		for w.j.peek() != jsontext.KindEndArray {
			if key != "env" || w.j.peek() != jsontext.KindString {
				if err := w.value(""); err != nil {
					return err
				}
				continue
			}
			_, entry, err := w.j.readString()
			if err != nil {
				return err
			}
			if name, value, ok := strings.Cut(entry, "="); ok && isCredentialName(name) {
				if err := w.f(value); err != nil {
					return err
				}
			}
		}
		return w.j.next()
	case jsontext.KindString, jsontext.KindNull, jsontext.KindFalse, jsontext.KindTrue, jsontext.KindNumber,
		jsontext.KindInvalid, jsontext.KindEndObject, jsontext.KindEndArray:
		fallthrough
	default:
		return w.j.skip()
	}
}

func (s scrubber) buildArgs(attrs map[string]string) []buildArg {
	var out []buildArg
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		name, ok := strings.CutPrefix(k, "build-arg:")
		if !ok {
			continue
		}
		var ignored, withheld bool
		a := buildArg{name: s.str(name, &ignored)}
		switch {
		case isCredentialName(name):
			withheld = true
		case isProxyName(name):
			a.value = s.proxy(attrs[k], &withheld)
		default:
			a.value = s.str(attrs[k], &withheld)
		}
		a.withheld = withheld
		out = append(out, a)
	}
	return out
}

func (s scrubber) noCache(attrs map[string]string) noCacheSetting {
	stages, ok := attrs["no-cache"]
	if !ok {
		return noCacheSetting{}
	}
	var ignored bool
	return noCacheSetting{set: true, stages: s.str(stages, &ignored)}
}

func (s scrubber) step(st step) (redactedStep, error) {
	out := redactedStep{lines: st.lines, vertex: st.vertex}
	out.id = s.str(st.id, &out.withheld)
	if st.inputs != nil {
		out.inputs = make([]string, len(st.inputs))
		for i, in := range st.inputs {
			out.inputs[i] = s.str(in, &out.withheld)
		}
	}
	op, err := s.op(st.op, &out.withheld)
	if err != nil {
		return redactedStep{}, err
	}
	out.op = op
	out.text = s.text(st.text, &out.withheld)
	return out, nil
}

// op redacts a step definition in one pass. It reads the definition as a
// stream and copies it only from the first change on, so a definition that
// needs no redaction costs one copy, and memory stays proportional to the
// definition's size. A string that no rule changes keeps its original bytes.
func (s scrubber) op(op jsontext.Value, withheld *bool) (jsontext.Value, error) {
	if op == nil {
		return nil, nil
	}
	w := opRedactor{s: s, j: newJSONReader(op), src: op, withheld: withheld}
	if err := w.value(""); err != nil {
		return nil, malformed("step definition is not valid JSON")
	}
	if w.out == nil {
		return bytes.Clone(op), nil
	}
	return append(w.out, op[w.copied:]...), nil
}

// opRedactor copies one step definition, redacting it on the way. Until a
// rule changes something, out is nil and nothing is copied.
type opRedactor struct {
	s        scrubber
	j        *jsonReader
	src      jsontext.Value
	out      []byte
	copied   int
	withheld *bool
}

// replace puts repl in place of src[start:end].
func (w *opRedactor) replace(start, end int, repl []byte) {
	if w.out == nil {
		w.out = make([]byte, 0, len(w.src))
	}
	w.out = append(w.out, w.src[w.copied:start]...)
	w.out = append(w.out, repl...)
	w.copied = end
}

// value reads the next value, which sits under key.
func (w *opRedactor) value(key string) error {
	switch w.j.peek() {
	case jsontext.KindBeginObject:
		return w.object(key)
	case jsontext.KindBeginArray:
		if err := w.j.next(); err != nil {
			return err
		}
		for w.j.peek() != jsontext.KindEndArray {
			if err := w.value(key); err != nil {
				return err
			}
		}
		return w.j.next()
	case jsontext.KindString:
		return w.string(func(x string) string { return w.s.str(x, w.withheld) })
	case jsontext.KindNull, jsontext.KindFalse, jsontext.KindTrue, jsontext.KindNumber,
		jsontext.KindInvalid, jsontext.KindEndObject, jsontext.KindEndArray:
		fallthrough
	default:
		return w.j.skip()
	}
}

// object reads the next object, which sits under parent. A key that looks
// like a credential has its whole value withheld, whatever its type, and so
// does the definition of a nested build, which holds binary LLB. The data
// of a file that a step writes, such as a heredoc, is base64, so it is
// decoded, redacted and encoded again. A member whose name a rule changes is
// left out with its comma, so that two members can never end up with one
// name.
func (w *opRedactor) object(parent string) error {
	if err := w.j.next(); err != nil {
		return err
	}
	kept := false
	for w.j.peek() != jsontext.KindEndObject {
		rawName, k, err := w.j.readString()
		if err != nil {
			return err
		}
		nameStart := w.j.offset() - len(rawName)
		var renamed bool
		if w.s.str(k, &renamed); !renamed {
			kept = true
			if err = w.member(parent, k); err != nil {
				return err
			}
			continue
		}
		*w.withheld = true
		if err = w.j.skip(); err != nil {
			return err
		}
		start, end := nameStart, w.j.offset()
		switch {
		case kept:
			start, err = w.separatorBefore(nameStart, ',')
		case w.j.peek() != jsontext.KindEndObject:
			end, err = w.separatorAfter(end, ',')
		}
		if err != nil {
			return err
		}
		w.replace(start, end, nil)
	}
	return w.j.next()
}

var errNoSeparator = errors.New("JSON separator not found")

// separatorAfter returns the index just after the separator c that follows
// src[:i] with only white space between.
func (w *opRedactor) separatorAfter(i int, c byte) (int, error) {
	for ; i < len(w.src); i++ {
		switch w.src[i] {
		case c:
			return i + 1, nil
		case ' ', '\t', '\n', '\r':
		default:
			return 0, errNoSeparator
		}
	}
	return 0, errNoSeparator
}

// separatorBefore returns the index of the separator c that precedes
// src[i:] with only white space between.
func (w *opRedactor) separatorBefore(i int, c byte) (int, error) {
	for i--; i >= w.copied; i-- {
		switch w.src[i] {
		case c:
			return i, nil
		case ' ', '\t', '\n', '\r':
		default:
			return 0, errNoSeparator
		}
	}
	return 0, errNoSeparator
}

// member reads the value of the member k of an object under parent.
func (w *opRedactor) member(parent, k string) error {
	isString := w.j.peek() == jsontext.KindString
	switch {
	case isCredentialName(k), parent == "build" && k == "def":
		*w.withheld = true
		start, err := w.separatorAfter(w.j.offset(), ':')
		if err != nil {
			return err
		}
		if err := w.j.skip(); err != nil {
			return err
		}
		w.replace(start, w.j.offset(), []byte(`"`+withheldValue+`"`))
		return nil
	case parent == "mkfile" && k == "data" && isString:
		return w.string(func(x string) string { return w.s.encoded(x, w.withheld) })
	case k == "args" && w.j.peek() == jsontext.KindBeginArray:
		return w.args()
	case k == "env" && w.j.peek() == jsontext.KindBeginArray:
		return w.env()
	case isProxyName(k) && isString:
		return w.string(func(x string) string { return w.s.proxy(x, w.withheld) })
	default:
		return w.value(k)
	}
}

// args reads the arguments of a command. An argument after an option that
// takes a credential, as in "--password", "x", is redacted as that
// credential, because the rules see one argument at a time.
func (w *opRedactor) args() error {
	if err := w.j.next(); err != nil {
		return err
	}
	prev := ""
	for w.j.peek() != jsontext.KindEndArray {
		if w.j.peek() != jsontext.KindString {
			prev = ""
			if err := w.value("args"); err != nil {
				return err
			}
			continue
		}
		option := prev
		err := w.string(func(x string) string {
			prev = x
			return w.s.arg(option, x, w.withheld)
		})
		if err != nil {
			return err
		}
	}
	return w.j.next()
}

// arg redacts one argument of a command, given the argument before it.
func (s scrubber) arg(option, x string, withheld *bool) string {
	whole, afterColon := credentialOption(option)
	switch {
	case x == "" || x[0] == '-':
	case whole:
		*withheld = true
		return withheldValue
	case afterColon:
		if user, _, ok := strings.Cut(x, ":"); ok {
			*withheld = true
			return s.str(user, withheld) + ":" + withheldValue
		}
	}
	return s.str(x, withheld)
}

// env reads a list of NAME=VALUE entries. It withholds the value of each
// entry whose name looks like a credential, and redacts proxy settings.
func (w *opRedactor) env() error {
	if err := w.j.next(); err != nil {
		return err
	}
	for w.j.peek() != jsontext.KindEndArray {
		var err error
		if w.j.peek() == jsontext.KindString {
			err = w.string(func(x string) string { return w.s.envEntry(x, w.withheld) })
		} else {
			err = w.value("env")
		}
		if err != nil {
			return err
		}
	}
	return w.j.next()
}

// string reads the next string and replaces it when redact changes it.
// A string longer than maxStringBytes is withheld whole without being
// unquoted.
func (w *opRedactor) string(redact func(string) string) error {
	raw, err := w.j.readValue()
	if err != nil {
		return err
	}
	end := w.j.offset()
	if len(raw) > maxStringBytes {
		*w.withheld = true
		w.replace(end-len(raw), end, []byte(`"`+withheldValue+`"`))
		return nil
	}
	x, err := w.j.unquote(raw)
	if err != nil {
		return err
	}
	y := redact(x)
	if y == x {
		return nil
	}
	quoted, err := jsontext.AppendQuote(nil, y)
	if err != nil {
		return fmt.Errorf("quoting a redacted string: %w", err)
	}
	w.replace(end-len(raw), end, quoted)
	return nil
}

// envEntry redacts one NAME=VALUE entry of an environment list.
func (s scrubber) envEntry(entry string, withheld *bool) string {
	return s.scrub(entry, withheld, envRules)
}

// encoded redacts base64 data as text. Data that is not valid base64 is
// withheld, and so is data whose encoded text holds a known value, before
// or after redaction, which can happen by chance. Only known values are
// checked in the encoded text, because the other rules would match random
// base64.
func (s scrubber) encoded(enc string, withheld *bool) string {
	data, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || (s.known != nil && s.known.contains(enc)) {
		*withheld = true
		return withheldValue
	}
	var hit bool
	text := s.str(string(data), &hit)
	if !hit {
		return enc
	}
	*withheld = true
	out := base64.StdEncoding.EncodeToString([]byte(text))
	if s.known != nil && s.known.contains(out) {
		return withheldValue
	}
	return out
}

// isProxyName reports whether a name is a proxy setting, such as
// http_proxy, which BuildKit copies from build arguments into proxy_env.
func isProxyName(name string) bool {
	const suffix = "_proxy"
	return len(name) >= len(suffix) && strings.EqualFold(name[len(name)-len(suffix):], suffix)
}

// proxy redacts a proxy setting.
func (s scrubber) proxy(x string, withheld *bool) string {
	return s.scrub(x, withheld, proxyRules)
}

// str redacts one string, and sets *withheld when a rule applied.
func (s scrubber) str(x string, withheld *bool) string {
	return s.scrub(x, withheld, stringRules)
}

// text redacts the Dockerfile text of one instruction, which may span
// continuation lines.
func (s scrubber) text(x string, withheld *bool) string {
	return s.scrub(x, withheld, textRules)
}

// scrub removes the known values and what the rules mark from x, and sets
// *withheld when any applied. Every rule reads x itself, and each run of
// bytes that any of them marks is replaced once, so no rule can change the
// text that another reads.
//
// When x holds a known value, the rules also read it with the known values
// replaced, because a value that BuildKit expanded into x can split what
// they look for, as a password holding a / splits a URL. Then they read the
// result while that marks more, at most maxRereads times: what one rule or
// known value removes can take a separator that another rule reads, as a
// header's value can take the = of a query parameter, so that what is left
// reads differently. A reading only adds marks, so the cap bounds the time
// and never lets a value through.
//
// A known value that holds a quote can make one reading take the rest of x
// as quoted, and so remove it. The rules cannot tell a quote in an expanded
// value from one that opens a quoted credential, and reading it as data
// would leave that credential.
func (s scrubber) scrub(x string, withheld *bool, r ruleSet) string {
	if len(x) > maxStringBytes {
		*withheld = true
		return withheldValue
	}
	m := mask{n: len(x)}
	known := s.known != nil && s.known.mark(x, &m)
	if known {
		m.markApplied(x, r)
	}
	hit := r.mark(x, &m) || known
	for range maxRereads {
		if m.bits == nil || !m.markApplied(x, r) {
			break
		}
	}
	*withheld = *withheld || hit
	return m.apply(x)
}

// maxRereads bounds how often scrub reads its result again. Fuzzing found no
// string that needed more than one reading.
const maxRereads = 3

// ruleSet names the rules for one kind of string. Every kind has the rules
// for assignments, labels, options and URLs.
type ruleSet int

const (
	stringRules ruleSet = iota
	// textRules add the legacy form of ENV.
	textRules
	// proxyRules read the whole string as a URL, which may have no scheme:
	// curl and other programs accept a proxy as user:password@host:port.
	proxyRules
	// envRules add the value of a credential or proxy environment entry.
	envRules
)

// mark marks what the rules remove from x, and reports whether any applied.
func (r ruleSet) mark(x string, m *mask) bool {
	hit := markAssignments(x, m)
	hit = markLabels(x, m) || hit
	hit = markOptions(x, m) || hit
	hit = markURLs(x, m) || hit
	switch r {
	case stringRules:
	case textRules:
		hit = markLegacyEnv(x, m) || hit
	case proxyRules:
		hit = markProxy(x, 0, m) || hit
	case envRules:
		hit = markEnvValue(x, m) || hit
	}
	return hit
}

// mask is the set of bytes of one string that redaction removes, as one bit
// for each byte. bits stays nil until a byte is marked.
type mask struct {
	n    int
	bits []uint64
}

// mark adds the bytes from start to end.
func (m *mask) mark(start, end int) {
	if start >= end {
		return
	}
	if m.bits == nil {
		m.bits = make([]uint64, (m.n+63)/64)
	}
	for start < end {
		w, lo := start>>6, start&63
		hi := min(end-w<<6, 64)
		m.bits[w] |= ^uint64(0) >> (64 - (hi - lo)) << lo
		start = (w + 1) << 6
	}
}

// next returns the first index from i on whose byte is marked, or unmarked
// when marked is false, or n when there is none.
func (m *mask) next(i int, marked bool) int {
	if m.bits == nil {
		if marked {
			return m.n
		}
		return min(i, m.n)
	}
	for i < m.n {
		w := m.bits[i>>6]
		if !marked {
			w = ^w
		}
		if w >>= i & 63; w != 0 {
			return min(i+bits.TrailingZeros64(w), m.n)
		}
		i = (i | 63) + 1
	}
	return m.n
}

// apply returns x with each run of marked bytes replaced by withheldValue.
func (m *mask) apply(x string) string {
	if m.bits == nil {
		return x
	}
	var b strings.Builder
	b.Grow(len(x))
	for i := 0; i < len(x); {
		start := m.next(i, true)
		b.WriteString(x[i:start])
		if start == len(x) {
			break
		}
		b.WriteString(withheldValue)
		i = m.next(start, false)
	}
	return b.String()
}

// markApplied marks the bytes of x behind those that the rules mark in
// m.apply(x), and reports whether it marked any that m did not.
func (m *mask) markApplied(x string, r ruleSet) bool {
	y := m.apply(x)
	my := mask{n: len(y)}
	r.mark(y, &my)
	return m.markReplaced(x, &my)
}

// markReplaced marks the bytes of x behind those that my marks in
// m.apply(x), and reports whether it marked any that m did not. It reads m
// as apply did, and marks only bytes it has passed, so it must come before
// any other mark is added to m. It finds each run of m and of my once, so
// its time is linear in the length of x.
func (m *mask) markReplaced(x string, my *mask) bool {
	added := false
	// [k, kEnd) is the next run that my marks.
	k := my.next(0, true)
	kEnd := my.next(k, false)
	for i, j := 0, 0; i < len(x) && k < my.n; {
		run := m.next(i, true)
		// x[i:run] is y[j:end], so a byte of x is offset from its byte of
		// y by i-j.
		end := j + run - i
		for k < end {
			if lo, hi := max(k, j), min(kEnd, end); lo < hi {
				m.mark(lo+i-j, hi+i-j)
				added = true
			}
			if kEnd > end {
				break
			}
			k = my.next(kEnd, true)
			kEnd = my.next(k, false)
		}
		if run == len(x) {
			return added
		}
		j = end + len(withheldValue)
		i = m.next(run, false)
	}
	return added
}

// isCredentialName reports whether a name looks like it holds a credential.
// It matches words, not substrings, so that TOKENIZERS_PARALLELISM,
// KEYBOARD, PATH and local.sharedkeyhint do not match. A few BuildKit
// attribute names match but hold only the name of a secret, a public key or
// a flag. PWD also matches, although a shell sets it to the working
// directory: withholding it costs only a precise comparison of that step.
func isCredentialName(name string) bool {
	switch name {
	case "secretOpt",
		"git.authheadersecret", "git.authtokensecret", "http.authheadersecret",
		"git.sig.pubkey", "git.sig.rejectexpired", "git.sig.requiresignedtag", "git.sig.ignoresignedtag",
		"http.sig.pubkey", "http.sig.signature":
		return false
	}
	for w := range words(name) {
		if isCredentialWord(w) {
			return true
		}
		if n := len(w); n > 1 && (w[n-1] == 's' || w[n-1] == 'S') && isCredentialWord(w[:n-1]) {
			return true
		}
	}
	return false
}

// isCredentialWord reports whether one word of a name, in any case, is a
// credential word, or ends with one that is distinctive enough to end a
// longer word, as in PGPASSWORD. A word holds only ASCII letters and digits,
// so folding ASCII case is enough, and only its last bytes can match.
func isCredentialWord(w string) bool {
	var buf [len("authorization")]byte
	tail := w[max(0, len(w)-len(buf)):]
	for i := range len(tail) {
		c := tail[i]
		if isUpper(c) {
			c += 'a' - 'A'
		}
		buf[i] = c
	}
	t := string(buf[:len(tail)])
	if len(t) == len(w) {
		switch t {
		case "key", "auth", "authorization", "cookie", "sig", "pass", "pat", "jwt", "bearer", "cred", "credential":
			return true
		}
	}
	for _, suffix := range [...]string{"token", "secret", "password", "passwd", "passphrase", "pwd", "apikey"} {
		if strings.HasSuffix(t, suffix) {
			return true
		}
	}
	return false
}

// words splits a name at every byte that is not an ASCII letter or digit,
// between a lower-case and an upper-case letter, before the last letter of
// an upper-case run that a lower-case letter follows, as in APIKey, and
// between letters and digits.
func words(name string) iter.Seq[string] {
	return func(yield func(string) bool) {
		start := -1
		for i := range len(name) {
			c := name[i]
			if !isAlnum(c) {
				if start >= 0 && !yield(name[start:i]) {
					return
				}
				start = -1
				continue
			}
			if start >= 0 && wordBreak(name, i) {
				if !yield(name[start:i]) {
					return
				}
				start = i
			}
			if start < 0 {
				start = i
			}
		}
		if start >= 0 {
			yield(name[start:])
		}
	}
}

// wordBreak reports whether a word starts at name[i], given that name[i-1]
// is a letter or digit.
func wordBreak(name string, i int) bool {
	p, c := name[i-1], name[i]
	switch {
	case isDigit(p) != isDigit(c):
		return true
	case isLower(p) && isUpper(c):
		return true
	case isUpper(p) && isUpper(c) && i+1 < len(name) && isLower(name[i+1]):
		return true
	}
	return false
}

func isLower(c byte) bool { return 'a' <= c && c <= 'z' }
func isUpper(c byte) bool { return 'A' <= c && c <= 'Z' }
func isDigit(c byte) bool { return '0' <= c && c <= '9' }
func isAlnum(c byte) bool { return isLower(c) || isUpper(c) || isDigit(c) }

// isNameByte reports whether c can be part of the name before an "=", as in
// API_TOKEN=, --password= or http.header.Authorization=.
func isNameByte(c byte) bool { return isAlnum(c) || c == '_' || c == '-' || c == '.' }

// markAssignments marks the value of each NAME=VALUE in s whose name looks
// like a credential. The value ends at unquoted, unescaped white space, or
// at the end of s when a quote is not closed.
func markAssignments(s string, m *mask) bool {
	matched := false
	for i := 0; i < len(s); i++ {
		if s[i] != '=' {
			continue
		}
		j := i
		for j > 0 && isNameByte(s[j-1]) {
			j--
		}
		if j == i || !isCredentialName(s[j:i]) {
			continue
		}
		matched = true
		end := valueEnd(s, i+1)
		m.mark(i+1, end)
		i = end - 1
	}
	return matched
}

// markLabels marks the value after a credential name that a colon labels,
// or an equals sign with white space before it, as in the header
// "Authorization: Bearer x", the YAML "password: x" or the INI
// "password = x". NAME=VALUE with nothing between is markAssignments', and a
// name right after :// is a URL's user name, which markURLs reads. A quoted
// value loses what is inside its quotes; one inside the quoted string that
// the label starts, as in -H "Authorization: x", ends with that string; one
// after white space runs to the end of the line, as YAML and INI values do;
// and any other ends at white space.
func markLabels(s string, m *mask) bool {
	matched := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != ':' && c != '=' || strings.HasPrefix(s[i:], "://") {
			continue
		}
		k := i
		for k > 0 && (s[k-1] == ' ' || s[k-1] == '\t') {
			k--
		}
		if c == '=' && k == i {
			continue
		}
		var keyQuote byte
		if k > 0 && isQuote(s[k-1]) {
			keyQuote = s[k-1]
			k--
		}
		j := k
		for j > 0 && isNameByte(s[j-1]) {
			j--
		}
		switch {
		case j == k || !isCredentialName(s[j:k]):
			continue
		case strings.HasSuffix(s[:j], "://"):
			continue
		}
		matched = true
		start := i + 1
		for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
			start++
		}
		var end int
		switch {
		case start < len(s) && isQuote(s[start]):
			end = quoteEnd(s, start)
			if end > start+1 && s[end-1] == s[start] {
				end--
			}
			start++
		case keyQuote == 0 && j > 0 && isQuote(s[j-1]):
			end = quoteEnd(s, j-1)
			if end > start && s[end-1] == s[j-1] {
				end--
			}
		case start > i+1:
			end = start
			for end < len(s) && s[end] != '\n' && s[end] != '\r' {
				end++
			}
		default:
			end = valueEnd(s, start)
		}
		m.mark(start, end)
		i = max(i, end-1)
	}
	return matched
}

// markOptions marks the value of a command-line option that takes a
// credential, as credentialOption tells, when white space separates them.
// A value that starts with - is the next option, so the option took none.
func markOptions(s string, m *mask) bool {
	matched := false
	for i := 0; i < len(s); i++ {
		if s[i] != '-' || i > 0 && !isSpace(s[i-1]) {
			continue
		}
		k := i
		for k < len(s) && isNameByte(s[k]) {
			k++
		}
		whole, afterColon := credentialOption(s[i:k])
		start := skipSpace(s, k)
		if !whole && !afterColon || start == k || start == len(s) || s[start] == '-' {
			i = k - 1
			continue
		}
		end := valueEnd(s, start)
		if afterColon {
			n := strings.IndexByte(s[start:end], ':')
			if n < 0 {
				i = k - 1
				continue
			}
			start += n + 1
		}
		m.mark(start, end)
		matched = true
		i = end - 1
	}
	return matched
}

// credentialOption reports how an option takes a credential as its value:
// whole, after an option whose name looks like a credential, as in
// --password x, or after the colon, as curl and wget take user:password
// after -u and --user.
func credentialOption(option string) (whole, afterColon bool) {
	if option == "-u" || option == "--user" {
		return false, true
	}
	name, ok := strings.CutPrefix(option, "--")
	for i := range len(name) {
		if !isNameByte(name[i]) {
			return false, false
		}
	}
	return ok && name != "" && isCredentialName(name), false
}

func isQuote(c byte) bool { return c == '"' || c == '\'' }

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func valueEnd(s string, i int) int {
	for i < len(s) {
		switch c := s[i]; c {
		case '\\':
			i += 2
		case '"', '\'':
			i = quoteEnd(s, i)
		case ' ', '\t', '\n', '\r':
			return i
		default:
			i++
		}
	}
	return len(s)
}

// quoteEnd returns the index after the quote that closes the one at s[i],
// or len(s) when it is not closed.
func quoteEnd(s string, i int) int {
	q := s[i]
	for i++; i < len(s); i++ {
		switch {
		case s[i] == '\\' && q == '"':
			i++
		case s[i] == q:
			return i + 1
		}
	}
	return len(s)
}

// markLegacyEnv marks the value of an ENV instruction in the legacy form
// ENV NAME VALUE, where the value is the rest of the instruction.
func markLegacyEnv(s string, m *mask) bool {
	word, i := nextWord(s, skipSpace(s, 0))
	if strings.EqualFold(word, "ONBUILD") {
		word, i = nextWord(s, skipSpace(s, i))
	}
	if !strings.EqualFold(word, "ENV") {
		return false
	}
	name, i := nextWord(s, skipSpace(s, i))
	if name == "" || strings.Contains(name, "=") || !isCredentialName(name) {
		return false
	}
	m.mark(skipSpace(s, i), len(s))
	return true
}

// markEnvValue marks the whole value of an environment entry NAME=VALUE
// whose name looks like a credential, and marks the value of a proxy
// setting as markProxy does.
func markEnvValue(s string, m *mask) bool {
	name, _, hasValue := strings.Cut(s, "=")
	switch {
	case isCredentialName(name):
		m.mark(len(name)+1, len(s))
		return true
	case isProxyName(name) && hasValue:
		return markProxy(s, len(name)+1, m)
	}
	return false
}

// markProxy marks the user information and query values of the proxy
// setting s[pos:], which is the rest of s. curl accepts a setting with no
// scheme, as user:password@host:port, and rejects a scheme that is not a
// proxy's, so a setting that starts with another is read as user
// information from its start: that removes it whether the setting had such
// a scheme or a password that starts with //.
func markProxy(s string, pos int, m *mask) bool {
	if n := strings.Index(s[pos:], "://"); n > 0 && isProxyScheme(s[pos:pos+n]) {
		pos += n + len("://")
	}
	_, hit := markURL(s, pos, proxyURL, lastAt(s, pos, len(s)), m)
	return hit
}

// skipSpace skips white space and line continuations from s[i].
func skipSpace(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n':
			i++
		case continuationAt(s, i) > 0:
			i += continuationAt(s, i)
		default:
			return i
		}
	}
	return i
}

func nextWord(s string, i int) (string, int) {
	j := i
	for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != '\r' && s[j] != '\n' && continuationAt(s, j) == 0 {
		j++
	}
	return s[i:j], j
}

// continuationAt returns the length of a line continuation at s[i], a
// backslash and a line break, or 0 when there is none.
func continuationAt(s string, i int) int {
	switch {
	case strings.HasPrefix(s[i:], "\\\n"):
		return 2
	case strings.HasPrefix(s[i:], "\\\r\n"):
		return 3
	}
	return 0
}

// markURLs marks the user information and every query value of each URL in
// s. It finds URLs by "://" and never parses them, so it cannot fail. A URL
// ends at white space, a quote or an angle bracket, or where the scheme of
// another URL starts. Image references keep the part before "@", because
// there it is a name followed by a digest.
func markURLs(s string, m *mask) bool {
	matched := false
	// at is the last @ before stretchEnd, the next white space or control
	// byte from pos, or -1. It is found once for each stretch, however many
	// URLs the stretch holds, so the time stays linear.
	stretchEnd, at := 0, -1
	for pos := 0; ; {
		n := strings.Index(s[pos:], "://")
		if n < 0 {
			return matched
		}
		schemeEnd := pos + n
		schemeStart := schemeEnd
		for schemeStart > pos && isSchemeByte(s[schemeStart-1]) {
			schemeStart--
		}
		kind := anyURL
		if scheme := s[schemeStart:schemeEnd]; strings.EqualFold(scheme, "docker-image") || strings.EqualFold(scheme, "oci-layout") {
			kind = imageURL
		}
		start := schemeEnd + len("://")
		if start >= stretchEnd {
			stretchEnd = start
			for stretchEnd < len(s) && s[stretchEnd] > ' ' && s[stretchEnd] != 0x7f {
				stretchEnd++
			}
			at = lastAt(s, start, stretchEnd)
		}
		var hit bool
		pos, hit = markURL(s, start, kind, at, m)
		matched = hit || matched
	}
}

// urlKind says where markURL ends the user information of a URL.
type urlKind int

const (
	// anyURL's user information is what comes before the last @ in its
	// authority, which ends at the first /, ?, # or byte that ends a URL.
	// A password or token can hold any of those raw, as curl sends it, and
	// end the authority early, so the user information runs to the last @
	// before white space unless the URL has no password and its host is a
	// name with a dot or an IP address, with no port. That is the only
	// common URL with an @ in its path, as in
	// git+https://github.com/org/repo.git@v1: among Dockerfiles in
	// Sourcegraph's public index, it held 3,899 lines with a later @, where
	// an authority with a port held 3 and one with no dot held none.
	anyURL urlKind = iota
	// imageURL has no user information: before its @ is a name, and after
	// it a digest.
	imageURL
	// proxyURL's user information runs to the last @ in the setting: a
	// proxy setting has no path, apart from the socket path that curl
	// takes for a SOCKS proxy, so an @ in it is in the user information.
	proxyURL
)

// markURL marks the user information and query values of the URL whose
// authority starts at s[pos], and returns the index where the URL ends.
// last is the index of the last @ that its user information can run to, or
// -1.
func markURL(s string, pos int, kind urlKind, last int, m *mask) (int, bool) {
	matched := false
	end := scanURL(s, pos, "/?#")
	at := strings.LastIndexByte(s[pos:end], '@')
	switch kind {
	case anyURL:
		if last > pos+at && readsOn(s, pos, end, at) {
			at = last - pos
		}
	case imageURL:
		at = -1
	case proxyURL:
		if last >= pos {
			at = last - pos
		}
	}
	if at >= 0 {
		end = scanURL(s, pos+at+1, "/?#")
	}
	if at > 0 {
		m.mark(pos, pos+at)
		matched = true
	}
	pos = scanURL(s, end, "?#")
	if pos < len(s) && s[pos] == '?' {
		end = scanURL(s, pos+1, "#")
		matched = markQuery(s[:end], pos+1, m) || matched
		pos = end
	}
	if pos < len(s) && s[pos] == '#' {
		pos = scanURL(s, pos+1, "")
	}
	return pos, matched
}

// scanURL returns the index of the first byte from s[i] that ends a URL or
// is one of stops. When another URL starts, it returns the start of that
// URL's scheme, so the next URL is redacted on its own.
func scanURL(s string, i int, stops string) int {
	from := i
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case endsURL(c):
			return i
		case (c == '/' || c == '?' || c == '#') && strings.IndexByte(stops, c) >= 0:
			return i
		}
		if s[i] == ':' && strings.HasPrefix(s[i:], "://") {
			for i > from && isSchemeByte(s[i-1]) {
				i--
			}
			return i
		}
	}
	return i
}

// readsOn reports whether the user information of the URL whose authority,
// as URL parsers read it, is s[pos:end] can run on to a later @: when the
// part before the authority's last @, at, holds a password, when the host
// after it is not one that keepsPath, as when an email address is the user
// name and a raw / ends its password, or when the authority is empty because
// another scheme starts it, as a password that starts with // makes it.
func readsOn(s string, pos, end, at int) bool {
	host := s[pos:end]
	switch {
	case at >= 0:
		if strings.IndexByte(s[pos:pos+at], ':') >= 0 {
			return true
		}
		host = s[pos+at+1 : end]
	case end == pos:
		return schemeAt(s, pos)
	}
	return !keepsPath(host)
}

// schemeAt reports whether a scheme and :// start s[i:].
func schemeAt(s string, i int) bool {
	j := i
	for j < len(s) && isSchemeByte(s[j]) {
		j++
	}
	return j > i && strings.HasPrefix(s[j:], "://")
}

// lastAt returns the index of the last @ in s[i:j], or -1.
func lastAt(s string, i, j int) int {
	if k := strings.LastIndexByte(s[i:j], '@'); k >= 0 {
		return i + k
	}
	return -1
}

// endsURL reports whether c ends a URL: white space, a control byte, a quote
// or an angle bracket.
func endsURL(c byte) bool {
	return c <= ' ' || c == 0x7f || c == '"' || c == '\'' || c == '<' || c == '>' || c == '`'
}

func isSchemeByte(c byte) bool { return isAlnum(c) || c == '+' || c == '-' || c == '.' }

// isProxyScheme reports whether s, in any ASCII case, is a scheme that curl
// or Go's net/http takes for a proxy.
func isProxyScheme(s string) bool {
	for _, scheme := range [...]string{"http", "https", "socks4", "socks4a", "socks5", "socks5h"} {
		// Equal lengths keep the match to ASCII.
		if len(s) == len(scheme) && strings.EqualFold(s, scheme) {
			return true
		}
	}
	return false
}

// keepsPath reports whether a host is a name with a dot, an IPv4 address or
// an IPv6 literal, with no port.
func keepsPath(a string) bool {
	if strings.HasPrefix(a, "[") {
		return strings.IndexByte(a, ']') == len(a)-1
	}
	dot := false
	for i := range len(a) {
		switch c := a[i]; {
		case c == '.':
			dot = true
		case isAlnum(c) || c == '-':
		default:
			return false
		}
	}
	return dot
}

// markQuery marks each value of the query s[start:], and reports whether it
// marked any. A parameter with no "=" counts as a value.
func markQuery(s string, start int, m *mask) bool {
	matched := false
	for start <= len(s) {
		end := len(s)
		if n := strings.IndexByte(s[start:], '&'); n >= 0 {
			end = start + n
		}
		value := start
		if eq := strings.IndexByte(s[start:end], '='); eq >= 0 {
			value += eq + 1
		}
		if value < end {
			m.mark(value, end)
			matched = true
		}
		start = end + 1
	}
	return matched
}
