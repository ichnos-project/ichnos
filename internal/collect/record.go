package collect

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	mediaTypeIndex          = "application/vnd.oci.image.index.v1+json"
	mediaTypeManifest       = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeHistory        = "application/vnd.buildkit.historyrecord.v0"
	mediaTypeStatus         = "application/vnd.buildkit.status.v0"
	statusMediaTypePrefix   = "application/vnd.buildkit.status."
	mediaTypeInToto         = "application/vnd.in-toto+json"
	annotationPredicateType = "in-toto.io/predicate-type"
	predicateSLSA1          = "https://slsa.dev/provenance/v1"
	predicateSLSAPrefix     = "https://slsa.dev/provenance/"
)

// build is one provenance: the steps BuildKit ran for one platform.
type build struct {
	platform string
	steps    []step
}

// step is one entry of the provenance's llbDefinition.
type step struct {
	id     string
	inputs []stepInput
	// op is the step's definition as BuildKit wrote it: a JSON object, or nil
	// when the step has none. It stays raw, because a JSON value decoded into
	// maps and slices can take about 70 times its size in memory.
	op    jsontext.Value
	text  string
	lines lineRange
	// vertex is the index of the step's vertex in Record.vertices, or -1
	// when the status stream has none, as for the final step.
	vertex int
}

// stepInput is one input of a step: the index of the step it reads, in its
// build's steps, and which of that step's outputs.
type stepInput struct {
	step, output int
}

// lineRange is a range of Dockerfile lines, counted from 1. The zero value
// means the step has no location.
type lineRange struct {
	start, end int
}

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// digestSyntax is the digest grammar of the OCI image specification.
var digestSyntax = regexp.MustCompile(`^[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-zA-Z0-9=_-]+$`)

// provenanceState says whether a record's builds came from its provenance,
// and why it has none when they did not, so that a report can say what to
// change.
type provenanceState int

const (
	provenanceRead provenanceState = iota
	// provenanceMissing means the record has no SLSA provenance. BuildKit
	// v0.34.0 writes one only for a build that produced a result, so a build
	// that failed has none.
	provenanceMissing
	// provenanceUnsupported means the record has SLSA provenance only in a
	// form that Ichnos does not read: of another version, such as v0.2, or
	// with no buildDefinition, as when it is wrapped in an in-toto
	// Statement.
	provenanceUnsupported
	// provenanceNoSteps means the record's SLSA v1 provenance has no step
	// definitions.
	provenanceNoSteps
)

func (s provenanceState) String() string {
	switch s {
	case provenanceRead:
		return "read"
	case provenanceMissing:
		return "missing"
	case provenanceUnsupported:
		return "unsupported"
	case provenanceNoSteps:
		return "no steps"
	}
	return fmt.Sprintf("provenanceState(%d)", int(s))
}

// decodeRecord puts together the blobs of a build record, once index.json
// and the manifest have said which blob is which. It uses the history
// record's attributes, the status stream's vertices, and the step
// definitions, locations and Dockerfile text of each SLSA v1 provenance. A
// record with no provenance that Ichnos reads has no builds, and its
// provenance state says why.
func decodeRecord(ctx context.Context, l *layout, lim limits) (*Record, error) {
	m, err := l.manifest()
	if err != nil {
		return nil, err
	}
	if m.Config.MediaType != mediaTypeHistory {
		return nil, unsupported("history record media type")
	}
	attrs, err := l.history(m.Config)
	if err != nil {
		return nil, err
	}
	roles, err := classifyLayers(m.Layers)
	if err != nil {
		return nil, err
	}
	vertices, err := l.status(roles.status)
	if err != nil {
		return nil, err
	}
	d := provenanceDecoder{vertices: vertices, lim: lim}
	builds := make([]build, 0, len(roles.provenances))
	defined := false
	for _, desc := range roles.provenances {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("reading provenance: %w", err)
		}
		p, err := l.provenance(desc)
		if err != nil {
			return nil, err
		}
		defined = defined || p.defined
		b, ok, err := d.decode(p)
		if err != nil {
			return nil, err
		}
		if ok {
			builds = append(builds, b)
		}
	}
	slices.SortFunc(builds, func(x, y build) int { return cmp.Compare(x.platform, y.platform) })
	for i := 1; i < len(builds); i++ {
		if builds[i].platform == builds[i-1].platform {
			return nil, malformed("record has two provenances for one platform")
		}
	}
	state := provenanceRead
	switch {
	case len(builds) > 0:
	case defined:
		state = provenanceNoSteps
	case len(roles.provenances) > 0 || roles.otherProvenance:
		state = provenanceUnsupported
	default:
		state = provenanceMissing
	}
	return &Record{&recordData{attrs: attrs, vertices: vertices, builds: builds, provenance: state}}, nil
}

func unsupported(what string) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedRecord, what)
}

type manifest struct {
	Config descriptor
	Layers []descriptor
}

// layout is what reading an archive found: index.json, and what each blob
// held, keyed by the hex part of its SHA-256 digest.
type layout struct {
	index []byte
	blobs map[string]*blob
}

// manifest reads index.json and returns the one manifest it lists. It stops
// at a second manifest, so a list of many costs nothing.
func (l *layout) manifest() (manifest, error) {
	if l.index == nil {
		return manifest{}, malformed("archive has no index.json")
	}
	var manifests []descriptor
	j := newJSONReader(l.index, jsontext.AllowInvalidUTF8(true))
	err := j.fields([]string{"manifests"}, func(int) error {
		return j.elements(func() error {
			if len(manifests) == 1 {
				return ErrMultipleBuilds
			}
			desc, descErr := readDescriptor(j)
			manifests = append(manifests, desc)
			return descErr
		})
	})
	if err == nil {
		err = j.end()
	}
	switch {
	case errors.Is(err, ErrMultipleBuilds):
		return manifest{}, err
	case err != nil:
		return manifest{}, malformed("index.json is not valid")
	case len(manifests) == 0:
		return manifest{}, malformed("index.json lists no manifest")
	}
	d := manifests[0]
	switch d.MediaType {
	case mediaTypeManifest:
	case mediaTypeIndex:
		return manifest{}, ErrMultipleBuilds
	default:
		return manifest{}, unsupported("manifest media type")
	}
	p, err := l.json(d, "manifest is not valid")
	if err != nil {
		return manifest{}, err
	}
	if p.manifestErr != nil {
		return manifest{}, partError(p.manifestErr, "manifest is not valid")
	}
	return p.manifest, nil
}

// history returns the frontend attributes of the history record that d
// names.
func (l *layout) history(d descriptor) (map[string]string, error) {
	const what = "history record is not valid, or has an attribute that is not a string"
	p, err := l.json(d, what)
	if err != nil {
		return nil, err
	}
	if p.attrsErr != nil {
		return nil, partError(p.attrsErr, what)
	}
	return p.attrs, nil
}

func (l *layout) provenance(d descriptor) (rawProvenance, error) {
	const what = "provenance is not valid"
	p, err := l.json(d, what)
	if err != nil {
		return rawProvenance{}, err
	}
	if p.provErr != nil {
		return rawProvenance{}, partError(p.provErr, what)
	}
	return p.prov, nil
}

func (l *layout) status(d descriptor) ([]vertex, error) {
	b, err := l.find(d)
	if err != nil {
		return nil, err
	}
	return b.vertices, b.statusErr
}

// json returns the fields of the JSON blob that d names. what describes the
// blob's kind in the error for a blob that is not valid JSON.
func (l *layout) json(d descriptor, what string) (*jsonParts, error) {
	b, err := l.find(d)
	if err != nil {
		return nil, err
	}
	var le *LimitError
	switch {
	case b.json != nil:
		return b.json, nil
	case errors.As(b.jsonErr, &le):
		return nil, b.jsonErr
	default:
		return nil, malformed(what)
	}
}

// partError returns the error of one kind's fields: its own description
// when it has one, and what otherwise, because errors from the JSON reader
// can quote bytes of the record.
func partError(err error, what string) error {
	if errors.Is(err, ErrMalformedRecord) {
		return err
	}
	return malformed(what)
}

// find returns the blob that d names, after checking its size. Its digest
// is not checked: the size check catches truncation, and nothing trusts a
// record more because its digests match.
func (l *layout) find(d descriptor) (*blob, error) {
	if !digestSyntax.MatchString(d.Digest) {
		return nil, malformed("descriptor has a malformed digest")
	}
	hex, ok := strings.CutPrefix(d.Digest, "sha256:")
	if !ok {
		return nil, unsupported("digest algorithm")
	}
	if !isSHA256Hex(hex) {
		return nil, malformed("descriptor has a malformed digest")
	}
	b, ok := l.blobs[hex]
	if !ok {
		return nil, malformed("archive is missing a blob")
	}
	if b.size != d.Size {
		return nil, malformed("blob size does not match its descriptor")
	}
	return b, nil
}

// layerRoles is what each layer of a manifest is. otherProvenance is true
// when a layer holds SLSA provenance of a version Ichnos does not read.
type layerRoles struct {
	status          descriptor
	provenances     []descriptor
	otherProvenance bool
}

// classifyLayers finds the status stream and the SLSA v1 provenances. It
// ignores the trace, the result's manifest and unknown layers. A blob listed
// twice is rejected, because decoding one provenance many times would
// multiply the work its size allows.
func classifyLayers(layers []descriptor) (layerRoles, error) {
	var roles layerRoles
	var status []descriptor
	laterStatus := false
	seen := make(map[string]bool, len(layers))
	for _, l := range layers {
		if seen[l.Digest] {
			return layerRoles{}, malformed("manifest lists one blob twice")
		}
		seen[l.Digest] = true
		predicate := l.Annotations[annotationPredicateType]
		switch {
		case l.MediaType == mediaTypeStatus:
			status = append(status, l)
		case strings.HasPrefix(l.MediaType, statusMediaTypePrefix):
			laterStatus = true
		case l.MediaType == mediaTypeInToto && predicate == predicateSLSA1:
			roles.provenances = append(roles.provenances, l)
		case l.MediaType == mediaTypeInToto && strings.HasPrefix(predicate, predicateSLSAPrefix):
			roles.otherProvenance = true
		}
	}
	switch {
	case len(status) > 1:
		return layerRoles{}, malformed("manifest has more than one status stream")
	case len(status) == 1:
		roles.status = status[0]
		return roles, nil
	case laterStatus:
		return layerRoles{}, unsupported("status stream media type")
	default:
		return layerRoles{}, malformed("manifest has no status stream")
	}
}

// provenanceDecoder puts together the builds of one record from its
// provenances. Its Dockerfile text budget is shared by all of them, so that
// a record cannot multiply what it keeps by repeating a large range.
type provenanceDecoder struct {
	vertices []vertex
	lim      limits
	text     int
}

// decode returns the build that one provenance describes, or false when the
// provenance has no step definitions.
func (d *provenanceDecoder) decode(p rawProvenance) (build, bool, error) {
	if !p.config || len(p.steps) == 0 {
		return build{}, false, nil
	}

	steps := make([]step, len(p.steps))
	byID := make(map[string]int, len(p.steps))
	for i, def := range p.steps {
		if def.id == "" {
			return build{}, false, malformed("provenance has a step with no ID")
		}
		if _, dup := byID[def.id]; dup {
			return build{}, false, malformed("provenance has two steps with one ID")
		}
		byID[def.id] = i
		steps[i] = step{id: def.id, op: def.op, vertex: -1}
	}
	for i, def := range p.steps {
		if def.inputs == nil {
			continue
		}
		steps[i].inputs = make([]stepInput, len(def.inputs))
		for k, in := range def.inputs {
			var err error
			if steps[i].inputs[k], err = resolveInput(in, byID); err != nil {
				return build{}, false, err
			}
		}
	}

	mapped := make([]bool, len(steps))
	for _, digest := range slices.Sorted(maps.Keys(p.digests)) {
		i, ok := byID[p.digests[digest]]
		if !ok {
			return build{}, false, malformed("provenance maps a digest to an unknown step")
		}
		if mapped[i] {
			return build{}, false, malformed("provenance maps two digests to one step")
		}
		mapped[i] = true
		if v, found := slices.BinarySearchFunc(d.vertices, digest, func(v vertex, digest string) int {
			return cmp.Compare(v.digest, digest)
		}); found {
			steps[i].vertex = v
		}
	}

	// Index only the lines that steps refer to: the first line of each range,
	// and the line after it, where the range ends.
	needed := make([][]int, len(p.sources))
	for i := range steps {
		for _, loc := range p.locations[steps[i].id] {
			if loc.source < 0 || loc.source >= len(p.sources) {
				return build{}, false, malformed("provenance has a location in an unknown source")
			}
			if loc.lines.start < 1 || loc.lines.end < loc.lines.start {
				return build{}, false, malformed("provenance has a location outside its source")
			}
			needed[loc.source] = append(needed[loc.source], loc.lines.start, loc.lines.end+1)
		}
	}
	sources := make([]*sourceLines, len(p.sources))
	for i, lines := range needed {
		if lines != nil {
			sources[i] = newSourceLines(p.sources[i], lines)
		}
	}
	for i := range steps {
		var first location
		for _, loc := range p.locations[steps[i].id] {
			if loc.lines.end > sources[loc.source].count() {
				return build{}, false, malformed("provenance has a location outside its source")
			}
			if first.lines.start == 0 || loc.compare(first) < 0 {
				first = loc
			}
		}
		if first.lines.start == 0 {
			continue
		}
		size := sources[first.source].size(first.lines)
		if size > d.lim.textBytes-d.text {
			return build{}, false, &LimitError{What: "bytes of Dockerfile text", Limit: int64(d.lim.textBytes)}
		}
		d.text += size
		steps[i].text = sources[first.source].text(first.lines)
		steps[i].lines = first.lines
	}
	return build{platform: p.platform, steps: steps}, true, nil
}

// resolveInput reads an input as BuildKit writes it, "<step ID>:<output>"
// (solver/llbsolver/provenance/buildconfig.go at v0.34.0), and returns the
// index of the step it names. It resolves the raw ID, before redaction,
// because redaction can turn two different IDs into the same text.
func resolveInput(in string, byID map[string]int) (stepInput, error) {
	id, output, ok := strings.CutLast(in, ":")
	if !ok || output == "" || !isDigits(output) {
		return stepInput{}, malformed("provenance has a step input that is not a step ID and an output")
	}
	n, err := strconv.Atoi(output)
	if err != nil {
		return stepInput{}, malformed("provenance has a step input that is not a step ID and an output")
	}
	i, found := byID[id]
	if !found {
		return stepInput{}, malformed("provenance has a step input that names no step")
	}
	return stepInput{step: i, output: n}, nil
}

func isDigits(s string) bool {
	for i := range len(s) {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// stepDefinition returns the definition of one step, which must be a JSON
// object, or nil when the step has none. Unlike the rest of the provenance,
// it is checked strictly: invalid UTF-8 or escapes are an error, so that
// redaction reads exactly the text that BuildKit wrote. Duplicate names are
// accepted, and redaction handles each copy.
func stepDefinition(v jsontext.Value) (jsontext.Value, error) {
	switch {
	case len(v) == 0 || v.Kind() == jsontext.KindNull:
		return nil, nil
	case v.Kind() != jsontext.KindBeginObject || !v.IsValid(jsontext.AllowDuplicateNames(true)):
		return nil, malformed("provenance has a step definition that is not a valid JSON object")
	}
	return v, nil
}

type location struct {
	source int
	lines  lineRange
}

// compare orders locations by source and then by line. A step that BuildKit
// merged from identical instructions in two stages has a location in each,
// and its text comes from the first.
func (l location) compare(m location) int {
	return cmp.Or(cmp.Compare(l.source, m.source), cmp.Compare(l.lines.start, m.lines.start), cmp.Compare(l.lines.end, m.lines.end))
}

// sourceLines is a Dockerfile, without a byte order mark and with CRLF line
// endings turned into LF. It knows its number of lines and where some of
// them start, so its memory grows with the lines that steps refer to, not
// with the Dockerfile.
type sourceLines struct {
	data  []byte
	lines int
	// starts[i] is the offset where line needed[i] starts, or the length of
	// data when the Dockerfile has fewer lines.
	needed []int
	starts []int
}

// newSourceLines indexes data. needed lists the line numbers, counted from
// 1, whose start offsets bounds may ask for.
func newSourceLines(data []byte, needed []int) *sourceLines {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if bytes.Contains(data, []byte("\r\n")) {
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	}
	slices.Sort(needed)
	needed = slices.Compact(needed)
	l := &sourceLines{data: data, needed: needed, starts: make([]int, len(needed))}
	for k := range l.starts {
		l.starts[k] = len(data)
	}
	k := 0
	for i := 0; i < len(data); {
		l.lines++
		for k < len(needed) && needed[k] <= l.lines {
			if needed[k] == l.lines {
				l.starts[k] = i
			}
			k++
		}
		n := bytes.IndexByte(data[i:], '\n')
		if n < 0 {
			break
		}
		i += n + 1
	}
	return l
}

func (l *sourceLines) count() int { return l.lines }

// start returns the offset where line n starts, which must be a needed line.
func (l *sourceLines) start(n int) int {
	k, _ := slices.BinarySearch(l.needed, n)
	return l.starts[k]
}

// bounds returns the byte offsets of lines r, without the final newline.
func (l *sourceLines) bounds(r lineRange) (int, int) {
	end := l.start(r.end + 1)
	if end > 0 && l.data[end-1] == '\n' {
		end--
	}
	return l.start(r.start), end
}

func (l *sourceLines) size(r lineRange) int {
	from, to := l.bounds(r)
	return to - from
}

func (l *sourceLines) text(r lineRange) string {
	from, to := l.bounds(r)
	return string(l.data[from:to])
}
