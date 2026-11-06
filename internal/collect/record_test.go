package collect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// recordParts are the blobs of a synthetic build record. layout builds an
// OCI layout from them with correct digests and sizes, unless a test edits
// the descriptors.
type recordParts struct {
	history     []byte
	status      []byte
	provenances [][]byte
	extra       []descriptor
	extraBlobs  [][]byte
	editLayers  func([]descriptor) []descriptor
	editConfig  func(*descriptor)
	editIndex   func(*descriptor) []descriptor
}

func blobDescriptor(mediaType string, data []byte) descriptor {
	sum := sha256.Sum256(data)
	return descriptor{MediaType: mediaType, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(data))}
}

func mustJSON(tb testing.TB, v any) []byte {
	tb.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		tb.Fatalf("Marshal() error = %v", err)
	}
	return b
}

func (p recordParts) archive(tb testing.TB) *rawArchive {
	tb.Helper()
	a := &rawArchive{blobs: map[string][]byte{}}
	add := func(mediaType string, data []byte) descriptor {
		d := blobDescriptor(mediaType, data)
		a.blobs[strings.TrimPrefix(d.Digest, "sha256:")] = data
		return d
	}
	config := add(mediaTypeHistory, p.history)
	if p.editConfig != nil {
		p.editConfig(&config)
	}
	var layers []descriptor
	layers = append(layers, add("application/vnd.buildkit.otlp.json.v0", []byte(`{"trace":true}`)))
	if p.status != nil {
		layers = append(layers, add(mediaTypeStatus, p.status))
	}
	for _, prov := range p.provenances {
		d := add(mediaTypeInToto, prov)
		d.Annotations = map[string]string{annotationPredicateType: predicateSLSA1}
		layers = append(layers, d)
	}
	for i, d := range p.extra {
		if i < len(p.extraBlobs) {
			add(d.MediaType, p.extraBlobs[i])
		}
		layers = append(layers, d)
	}
	if p.editLayers != nil {
		layers = p.editLayers(layers)
	}
	manifest := add(mediaTypeManifest, mustJSON(tb, map[string]any{
		"schemaVersion": 2, "mediaType": mediaTypeManifest, "config": config, "layers": layers,
	}))
	manifests := []descriptor{manifest}
	if p.editIndex != nil {
		manifests = p.editIndex(&manifest)
	}
	a.index = mustJSON(tb, map[string]any{"schemaVersion": 2, "manifests": manifests})
	return a
}

func historyJSON(tb testing.TB, attrs map[string]string) []byte {
	tb.Helper()
	return mustJSON(tb, map[string]any{"Ref": "ref", "FrontendAttrs": attrs, "numTotalSteps": 1})
}

// testStep is one llbDefinition entry of a synthetic provenance.
type testStep struct {
	id     string
	op     any
	inputs []string
	digest string
	ranges [][3]int
}

type testProvenance struct {
	platform   string
	steps      []testStep
	dockerfile string
	noConfig   bool
	// internal holds more internalParameters, which replace the generated
	// ones.
	internal map[string]any
}

func (p testProvenance) json(tb testing.TB) []byte {
	tb.Helper()
	defs := []map[string]any{}
	mapping := map[string]string{}
	locations := map[string]any{}
	for _, s := range p.steps {
		def := map[string]any{"id": s.id, "op": s.op}
		if s.inputs != nil {
			def["inputs"] = s.inputs
		}
		defs = append(defs, def)
		if s.digest != "" {
			mapping[s.digest] = s.id
		}
		if s.ranges != nil {
			var locs []map[string]any
			for _, r := range s.ranges {
				locs = append(locs, map[string]any{
					"sourceIndex": r[0],
					"ranges":      []any{map[string]any{"start": map[string]int{"line": r[1]}, "end": map[string]int{"line": r[2]}}},
				})
			}
			locations[s.id] = map[string]any{"locations": locs}
		}
	}
	internal := map[string]any{"builderPlatform": "linux/arm64", "targetPlatform": p.platform}
	if !p.noConfig {
		internal["buildConfig"] = map[string]any{"llbDefinition": defs, "digestMapping": mapping}
	}
	maps.Copy(internal, p.internal)
	return mustJSON(tb, map[string]any{
		"buildDefinition": map[string]any{"buildType": "test", "internalParameters": internal},
		"runDetails": map[string]any{"metadata": map[string]any{"buildkit_metadata": map[string]any{
			"source": map[string]any{
				"locations": locations,
				"infos":     []any{map[string]any{"filename": "Dockerfile", "data": []byte(p.dockerfile)}},
			},
		}}},
	})
}

func execOp(args ...string) map[string]any {
	return map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": args}}}}
}

func digestOf(c byte) string { return "sha256:" + hex64(c) }

// simpleProvenance is a two-line Dockerfile with one vertex for each line,
// and a terminal step with no vertex.
func simpleProvenance(platform string) testProvenance {
	return testProvenance{
		platform:   platform,
		dockerfile: "FROM alpine\nRUN true\n",
		steps: []testStep{
			{id: "step0", op: map[string]any{"Op": map[string]any{"source": map[string]any{"identifier": "docker-image://docker.io/library/alpine:latest"}}}, digest: digestOf('a'), ranges: [][3]int{{0, 1, 1}}},
			{id: "step1", op: execOp("/bin/sh", "-c", "true"), inputs: []string{"step0:0"}, digest: digestOf('b'), ranges: [][3]int{{0, 2, 2}}},
			{id: "step2", op: map[string]any{"Op": map[string]any{}}, inputs: []string{"step1:0"}, digest: digestOf('c')},
		},
	}
}

func simpleStatus() []byte {
	return vertices(
		update{digest: digestOf('a'), name: "[1/2] FROM docker.io/library/alpine:latest"},
		update{digest: digestOf('b'), name: "[2/2] RUN true", cached: true, started: ts(1, 0), completed: ts(2, 0)},
		update{digest: "sha256:internal", name: "[internal] load build definition from Dockerfile"},
	)
}

func simpleParts(tb testing.TB) recordParts {
	tb.Helper()
	return recordParts{
		history:     historyJSON(tb, map[string]string{"platform": "linux/amd64"}),
		status:      simpleStatus(),
		provenances: [][]byte{simpleProvenance("linux/amd64").json(tb)},
	}
}

func TestDecodeRecordSimple(t *testing.T) {
	rec, err := simpleParts(t).archive(t).decode(t.Context(), testLimits())
	if err != nil {
		t.Fatalf("decodeRecord() error = %v", err)
	}
	if got := rec.attrs["platform"]; got != "linux/amd64" {
		t.Errorf("attrs[platform] = %q, want %q", got, "linux/amd64")
	}
	if len(rec.vertices) != 3 || len(rec.builds) != 1 {
		t.Fatalf("decodeRecord() = %d vertices, %d builds, want 3, 1", len(rec.vertices), len(rec.builds))
	}
	b := rec.builds[0]
	if b.platform != "linux/amd64" || len(b.steps) != 3 {
		t.Fatalf("build = %q with %d steps, want linux/amd64 with 3", b.platform, len(b.steps))
	}
	want := []struct {
		text  string
		lines lineRange
		hit   bool
		has   bool
	}{
		{"FROM alpine", lineRange{1, 1}, false, true},
		{"RUN true", lineRange{2, 2}, true, true},
		{"", lineRange{}, false, false},
	}
	for i, w := range want {
		s := b.steps[i]
		if s.text != w.text || s.lines != w.lines {
			t.Errorf("step %d text, lines = %q, %v, want %q, %v", i, s.text, s.lines, w.text, w.lines)
		}
		if has := s.vertex >= 0; has != w.has || (has && rec.vertices[s.vertex].hit() != w.hit) {
			t.Errorf("step %d vertex = %d, want present %v with hit %v", i, s.vertex, w.has, w.hit)
		}
	}
	if want := []stepInput{{step: 0, output: 0}}; !slices.Equal(b.steps[1].inputs, want) {
		t.Errorf("step 1 inputs = %v, want %v", b.steps[1].inputs, want)
	}
}

func TestDecodeRecordLayout(t *testing.T) {
	sha512 := "sha512:" + strings.Repeat("ab", 64)
	tests := []struct {
		name    string
		parts   func(*recordParts)
		archive func(*rawArchive)
		wantErr error
	}{
		{"missing index", nil, func(a *rawArchive) { a.index = nil }, ErrMalformedRecord},
		{"index not JSON", nil, func(a *rawArchive) { a.index = []byte("{") }, ErrMalformedRecord},
		{"empty index", nil, func(a *rawArchive) { a.index = []byte(`{"manifests":[]}`) }, ErrMalformedRecord},
		{"several manifests", func(p *recordParts) {
			p.editIndex = func(d *descriptor) []descriptor { return []descriptor{*d, *d} }
		}, nil, ErrMultipleBuilds},
		{"nested index", func(p *recordParts) {
			p.editIndex = func(d *descriptor) []descriptor { d.MediaType = mediaTypeIndex; return []descriptor{*d} }
		}, nil, ErrMultipleBuilds},
		{"manifest of another type", func(p *recordParts) {
			p.editIndex = func(d *descriptor) []descriptor {
				d.MediaType = "application/vnd.docker.distribution.manifest.v2+json"
				return []descriptor{*d}
			}
		}, nil, ErrUnsupportedRecord},
		{"manifest blob missing", func(p *recordParts) {
			p.editIndex = func(d *descriptor) []descriptor { d.Digest = digestOf('e'); return []descriptor{*d} }
		}, nil, ErrMalformedRecord},
		{"provenance size mismatch", func(p *recordParts) {
			p.editLayers = func(ls []descriptor) []descriptor {
				for i := range ls {
					if ls[i].MediaType == mediaTypeInToto {
						ls[i].Size++
					}
				}
				return ls
			}
		}, nil, ErrMalformedRecord},
		{"status size mismatch", func(p *recordParts) {
			p.editLayers = func(ls []descriptor) []descriptor {
				for i := range ls {
					if ls[i].MediaType == mediaTypeStatus {
						ls[i].Size++
					}
				}
				return ls
			}
		}, nil, ErrMalformedRecord},
		{"manifest not JSON", func(p *recordParts) {
			p.editIndex = func(d *descriptor) []descriptor {
				*d = blobDescriptor(mediaTypeManifest, []byte("["))
				return []descriptor{*d}
			}
		}, nil, ErrMalformedRecord},
		{"wrong config media type", func(p *recordParts) {
			p.editConfig = func(d *descriptor) { d.MediaType = "application/vnd.buildkit.historyrecord.v1" }
		}, nil, ErrUnsupportedRecord},
		{"wrong status media type", func(p *recordParts) {
			p.editLayers = func(ls []descriptor) []descriptor {
				for i := range ls {
					if ls[i].MediaType == mediaTypeStatus {
						ls[i].MediaType = "application/vnd.buildkit.status.v1"
					}
				}
				return ls
			}
		}, nil, ErrUnsupportedRecord},
		{"bad digest syntax", func(p *recordParts) {
			p.editConfig = func(d *descriptor) { d.Digest = "sha256:" + hex64('z') }
		}, nil, ErrMalformedRecord},
		{"digest with no algorithm", func(p *recordParts) {
			p.editConfig = func(d *descriptor) { d.Digest = hex64('a') }
		}, nil, ErrMalformedRecord},
		{"short digest", func(p *recordParts) {
			p.editConfig = func(d *descriptor) { d.Digest = "sha256:abc" }
		}, nil, ErrMalformedRecord},
		{"non-sha256 digest", func(p *recordParts) {
			p.editConfig = func(d *descriptor) { d.Digest = sha512 }
		}, nil, ErrUnsupportedRecord},
		{"missing blob", func(p *recordParts) {
			p.editConfig = func(d *descriptor) { d.Digest = digestOf('e') }
		}, nil, ErrMalformedRecord},
		{"size larger than the blob", func(p *recordParts) {
			p.editConfig = func(d *descriptor) { d.Size++ }
		}, nil, ErrMalformedRecord},
		{"size smaller than the blob", func(p *recordParts) {
			p.editConfig = func(d *descriptor) { d.Size-- }
		}, nil, ErrMalformedRecord},
		{"missing status layer", func(p *recordParts) { p.status = nil }, nil, ErrMalformedRecord},
		{"provenance listed twice", func(p *recordParts) {
			prov := simpleProvenance("linux/amd64")
			prov.noConfig = true
			p.provenances = [][]byte{prov.json(t)}
			p.editLayers = func(ls []descriptor) []descriptor {
				for _, l := range ls {
					if l.MediaType == mediaTypeInToto {
						return append(ls, l)
					}
				}
				return ls
			}
		}, nil, ErrMalformedRecord},
		{"duplicate status layer", func(p *recordParts) {
			p.editLayers = func(ls []descriptor) []descriptor {
				for _, l := range ls {
					if l.MediaType == mediaTypeStatus {
						return append(ls, l)
					}
				}
				return ls
			}
		}, nil, ErrMalformedRecord},
		{"missing trace", func(p *recordParts) {
			p.editLayers = func(ls []descriptor) []descriptor {
				return slices.DeleteFunc(ls, func(d descriptor) bool { return strings.Contains(d.MediaType, "otlp") })
			}
		}, nil, nil},
		{"unknown layer types", func(p *recordParts) {
			p.extra = []descriptor{{MediaType: "application/x-unknown", Digest: "not a digest", Size: -1}}
		}, nil, nil},
		{"status v0 next to a later version", func(p *recordParts) {
			p.extra = []descriptor{{MediaType: "application/vnd.buildkit.status.v1", Digest: digestOf('f'), Size: 1}}
		}, nil, nil},
		{"history not JSON", func(p *recordParts) { p.history = []byte("{") }, nil, ErrMalformedRecord},
		{"non-string attribute", func(p *recordParts) {
			p.history = []byte(`{"FrontendAttrs":{"platform":"linux/amd64","build-arg:N":5}}`)
		}, nil, ErrMalformedRecord},
		{"malformed status", func(p *recordParts) { p.status = []byte{1} }, nil, ErrMalformedRecord},
		{"empty status", func(p *recordParts) { p.status = []byte{} }, nil, ErrMalformedRecord},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := simpleParts(t)
			if tt.parts != nil {
				tt.parts(&p)
			}
			a := p.archive(t)
			if tt.archive != nil {
				tt.archive(a)
			}
			_, err := a.decode(t.Context(), testLimits())
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("decodeRecord() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("decodeRecord() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeRecordProvenance(t *testing.T) {
	withSteps := func(edit func([]testStep) []testStep) func(*recordParts) {
		return func(p *recordParts) {
			prov := simpleProvenance("linux/amd64")
			prov.steps = edit(prov.steps)
			p.provenances = [][]byte{prov.json(t)}
		}
	}
	otherPredicate := func(predicate string) func(*recordParts) {
		return func(p *recordParts) {
			p.provenances = nil
			d := blobDescriptor(mediaTypeInToto, simpleProvenance("linux/amd64").json(t))
			d.Annotations = map[string]string{annotationPredicateType: predicate}
			p.extra = []descriptor{d}
			p.extraBlobs = [][]byte{simpleProvenance("linux/amd64").json(t)}
		}
	}
	deep := strings.Repeat("[", 10_001) + strings.Repeat("]", 10_001)
	tests := []struct {
		name       string
		edit       func(*recordParts)
		wantBuilds []string
		wantErr    error
		wantState  provenanceState
	}{
		{"no provenance", func(p *recordParts) { p.provenances = nil }, nil, nil, provenanceMissing},
		{"no buildConfig", func(p *recordParts) {
			prov := simpleProvenance("linux/amd64")
			prov.noConfig = true
			p.provenances = [][]byte{prov.json(t)}
		}, nil, nil, provenanceNoSteps},
		{"null buildConfig", func(p *recordParts) {
			prov := simpleProvenance("linux/amd64")
			prov.internal = map[string]any{"buildConfig": nil}
			p.provenances = [][]byte{prov.json(t)}
		}, nil, nil, provenanceNoSteps},
		{"buildConfig with no steps", func(p *recordParts) {
			p.provenances = [][]byte{[]byte(inConfig(`{}`))}
		}, nil, nil, provenanceNoSteps},
		{"provenance in an in-toto Statement", func(p *recordParts) {
			p.provenances = [][]byte{[]byte(`{"_type":"https://in-toto.io/Statement/v1","predicateType":"https://slsa.dev/provenance/v1","predicate":` +
				string(simpleProvenance("linux/amd64").json(t)) + `}`)}
		}, nil, nil, provenanceUnsupported},
		{"no buildDefinition", func(p *recordParts) {
			p.provenances = [][]byte{[]byte(`{"runDetails":{}}`)}
		}, nil, nil, provenanceUnsupported},
		{"no steps, then no buildDefinition", func(p *recordParts) {
			prov := simpleProvenance("linux/amd64")
			prov.noConfig = true
			p.provenances = [][]byte{prov.json(t), []byte(`{"runDetails":{}}`)}
		}, nil, nil, provenanceNoSteps},
		{"no buildDefinition, then no steps", func(p *recordParts) {
			prov := simpleProvenance("linux/amd64")
			prov.noConfig = true
			p.provenances = [][]byte{[]byte(`{"runDetails":{}}`), prov.json(t)}
		}, nil, nil, provenanceNoSteps},
		{"null buildDefinition", func(p *recordParts) {
			p.provenances = [][]byte{[]byte(`{"buildDefinition":null}`)}
		}, nil, nil, provenanceNoSteps},
		{"another predicate type", otherPredicate("https://spdx.dev/Document"), nil, nil, provenanceMissing},
		{"SLSA v0.2", otherPredicate("https://slsa.dev/provenance/v0.2"), nil, nil, provenanceUnsupported},
		{"SLSA v0.2 next to v1", func(p *recordParts) {
			d := blobDescriptor(mediaTypeInToto, simpleProvenance("linux/arm64").json(t))
			d.Annotations = map[string]string{annotationPredicateType: "https://slsa.dev/provenance/v0.2"}
			p.extra = []descriptor{d}
			p.extraBlobs = [][]byte{simpleProvenance("linux/arm64").json(t)}
		}, []string{"linux/amd64"}, nil, provenanceRead},
		{"source that is not an object", func(p *recordParts) {
			p.provenances = [][]byte{[]byte(`{"buildDefinition":{"internalParameters":{"buildConfig":{}}},"runDetails":{"metadata":{"buildkit_metadata":{"source":[]}}}}`)}
		}, nil, ErrMalformedRecord, provenanceRead},
		{"two platforms", func(p *recordParts) {
			p.provenances = [][]byte{simpleProvenance("linux/arm64").json(t), simpleProvenance("linux/amd64").json(t)}
		}, []string{"linux/amd64", "linux/arm64"}, nil, provenanceRead},
		{"one platform twice", func(p *recordParts) {
			second := simpleProvenance("linux/amd64")
			second.dockerfile += "# a different blob\n"
			p.provenances = [][]byte{simpleProvenance("linux/amd64").json(t), second.json(t)}
		}, nil, ErrMalformedRecord, provenanceRead},
		{"invalid JSON", func(p *recordParts) { p.provenances = [][]byte{[]byte("{")} }, nil, ErrMalformedRecord, provenanceRead},
		{"data after the JSON", func(p *recordParts) {
			p.provenances = [][]byte{append(simpleProvenance("linux/amd64").json(t), " {}"...)}
		}, nil, ErrMalformedRecord, provenanceRead},
		{"depth over 10,000", func(p *recordParts) {
			p.provenances = [][]byte{[]byte(`{"buildDefinition":` + deep + `}`)}
		}, nil, ErrMalformedRecord, provenanceRead},
		{"empty step ID", withSteps(func(s []testStep) []testStep { s[1].id = ""; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"duplicate step ID", withSteps(func(s []testStep) []testStep { s[1].id = "step0"; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"op that is not an object", withSteps(func(s []testStep) []testStep { s[1].op = "exec"; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"null op", withSteps(func(s []testStep) []testStep { s[2].op = nil; return s }), []string{"linux/amd64"}, nil, provenanceRead},
		{"op that is a list", withSteps(func(s []testStep) []testStep { s[1].op = []any{}; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"op with invalid UTF-8", withSteps(func(s []testStep) []testStep { s[1].op = json.RawMessage("{\"a\":\"\xff\"}"); return s }), nil, ErrMalformedRecord, provenanceRead},
		{"op with a lone surrogate", withSteps(func(s []testStep) []testStep { s[1].op = json.RawMessage(`{"a":"\ud800"}`); return s }), nil, ErrMalformedRecord, provenanceRead},
		{"op with duplicate names", withSteps(func(s []testStep) []testStep { s[1].op = json.RawMessage(`{"a":1,"a":2}`); return s }), []string{"linux/amd64"}, nil, provenanceRead},
		{"invalid UTF-8 in a field Ichnos does not read", func(p *recordParts) {
			prov := simpleProvenance("linux/amd64")
			prov.internal = map[string]any{"github_event_payload": json.RawMessage("\"\xff\"")}
			p.provenances = [][]byte{prov.json(t)}
		}, []string{"linux/amd64"}, nil, provenanceRead},
		{"step with no vertex", withSteps(func(s []testStep) []testStep { s[1].digest = digestOf('9'); return s }), []string{"linux/amd64"}, nil, provenanceRead},
		{"input that names no step", withSteps(func(s []testStep) []testStep { s[1].inputs = []string{"step9:0"}; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"input with no output", withSteps(func(s []testStep) []testStep { s[1].inputs = []string{"step0"}; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"input with an empty output", withSteps(func(s []testStep) []testStep { s[1].inputs = []string{"step0:"}; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"input with a negative output", withSteps(func(s []testStep) []testStep { s[1].inputs = []string{"step0:-1"}; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"input with a signed output", withSteps(func(s []testStep) []testStep { s[1].inputs = []string{"step0:+1"}; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"input with an output that is not a number", withSteps(func(s []testStep) []testStep { s[1].inputs = []string{"step0:x"}; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"input with an output too large for an int", withSteps(func(s []testStep) []testStep { s[1].inputs = []string{"step0:99999999999999999999"}; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"empty input", withSteps(func(s []testStep) []testStep { s[1].inputs = []string{""}; return s }), nil, ErrMalformedRecord, provenanceRead},
		{"input to a step listed later", withSteps(func(s []testStep) []testStep { s[0].inputs = []string{"step1:0"}; return s }), []string{"linux/amd64"}, nil, provenanceRead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := simpleParts(t)
			tt.edit(&p)
			rec, err := p.archive(t).decode(t.Context(), testLimits())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("decodeRecord() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeRecord() error = %v", err)
			}
			if rec.provenance != tt.wantState {
				t.Errorf("provenance = %v, want %v", rec.provenance, tt.wantState)
			}
			var platforms []string
			for _, b := range rec.builds {
				platforms = append(platforms, b.platform)
			}
			if !slices.Equal(platforms, tt.wantBuilds) {
				t.Errorf("builds = %q, want %q", platforms, tt.wantBuilds)
			}
		})
	}
}

func TestDecodeRecordCancelled(t *testing.T) {
	l, err := simpleParts(t).archive(t).layout(t.Context(), testLimits())
	if err != nil {
		t.Fatalf("layout() error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := decodeRecord(ctx, l, testLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("decodeRecord() error = %v, want context.Canceled", err)
	}
	if _, err := simpleParts(t).archive(t).decode(ctx, testLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("decode() error = %v, want context.Canceled", err)
	}
}

func TestDecodeRecordDigestMapping(t *testing.T) {
	prov := simpleProvenance("linux/amd64")
	raw := strings.Replace(string(prov.json(t)), `"digestMapping":{`, `"digestMapping":{"`+digestOf('d')+`":"missing",`, 1)
	p := simpleParts(t)
	p.provenances = [][]byte{[]byte(raw)}
	_, err := p.archive(t).decode(t.Context(), testLimits())
	if !errors.Is(err, ErrMalformedRecord) {
		t.Fatalf("decodeRecord() with a digest mapped to a missing step error = %v, want ErrMalformedRecord", err)
	}

	raw = strings.Replace(string(prov.json(t)), `"digestMapping":{`, `"digestMapping":{"`+digestOf('d')+`":"step1",`, 1)
	p.provenances = [][]byte{[]byte(raw)}
	_, err = p.archive(t).decode(t.Context(), testLimits())
	if !errors.Is(err, ErrMalformedRecord) {
		t.Fatalf("decodeRecord() with two digests mapped to one step error = %v, want ErrMalformedRecord", err)
	}

	prov.steps[1].digest = digestOf('9')
	p.provenances = [][]byte{prov.json(t)}
	rec, err := p.archive(t).decode(t.Context(), testLimits())
	if err != nil {
		t.Fatalf("decodeRecord() error = %v", err)
	}
	if got := rec.builds[0].steps[1].vertex; got != -1 {
		t.Errorf("step with no vertex has vertex %d, want -1", got)
	}
	if got := rec.builds[0].steps[2].vertex; got != -1 {
		t.Errorf("terminal step has vertex %d, want -1", got)
	}
}

func TestDecodeRecordSteps(t *testing.T) {
	prov := simpleProvenance("linux/amd64")
	for _, tt := range []struct {
		limit   int
		wantErr bool
	}{{len(prov.steps), false}, {len(prov.steps) - 1, true}} {
		lim := testLimits()
		lim.steps = tt.limit
		_, err := simpleParts(t).archive(t).decode(t.Context(), lim)
		var le *LimitError
		if got := errors.As(err, &le) && le.What == "steps"; got != tt.wantErr {
			t.Errorf("decodeRecord() with a limit of %d steps error = %v, want LimitError %v", tt.limit, err, tt.wantErr)
		}
	}
	p := simpleParts(t)
	p.provenances = [][]byte{simpleProvenance("linux/arm64").json(t), simpleProvenance("linux/amd64").json(t)}
	lim := testLimits()
	lim.steps = 2*len(prov.steps) - 1
	var le *LimitError
	if _, err := p.archive(t).decode(t.Context(), lim); !errors.As(err, &le) {
		t.Errorf("decodeRecord() with two builds over the shared step limit error = %v, want LimitError", err)
	}
}

func TestDecodeRecordLocations(t *testing.T) {
	tests := []struct {
		name       string
		dockerfile string
		ranges     [][3]int
		want       string
		wantLines  lineRange
		wantErr    bool
	}{
		{"no location", "FROM a\nRUN b\n", nil, "", lineRange{}, false},
		{"one line", "FROM a\nRUN b\n", [][3]int{{0, 2, 2}}, "RUN b", lineRange{2, 2}, false},
		{"multi-line instruction", "FROM a\nRUN b \\\n  && c\nRUN d\n", [][3]int{{0, 2, 3}}, "RUN b \\\n  && c", lineRange{2, 3}, false},
		{"CRLF", "FROM a\r\nRUN b \\\r\n  c\r\n", [][3]int{{0, 2, 3}}, "RUN b \\\n  c", lineRange{2, 3}, false},
		{"BOM", "\xef\xbb\xbfFROM a\nRUN b\n", [][3]int{{0, 1, 1}}, "FROM a", lineRange{1, 1}, false},
		{"no final newline", "FROM a\nRUN b", [][3]int{{0, 2, 2}}, "RUN b", lineRange{2, 2}, false},
		{"empty line", "FROM a\n\n", [][3]int{{0, 2, 2}}, "", lineRange{2, 2}, false},
		{"invalid UTF-8", "FROM a\nRUN \xff\xfe\n", [][3]int{{0, 2, 2}}, "RUN \xff\xfe", lineRange{2, 2}, false},
		{"earliest of two stages", "FROM a\nRUN b\nFROM c\nRUN b\n", [][3]int{{0, 4, 4}, {0, 2, 2}}, "RUN b", lineRange{2, 2}, false},
		{"earliest of two stages listed first", "FROM a\nRUN b\nFROM c\nRUN b\n", [][3]int{{0, 2, 2}, {0, 4, 4}}, "RUN b", lineRange{2, 2}, false},
		{"line 0", "FROM a\n", [][3]int{{0, 0, 1}}, "", lineRange{}, true},
		{"negative line", "FROM a\n", [][3]int{{0, -1, 1}}, "", lineRange{}, true},
		{"end before start", "FROM a\nRUN b\n", [][3]int{{0, 2, 1}}, "", lineRange{}, true},
		{"past EOF", "FROM a\nRUN b\n", [][3]int{{0, 2, 3}}, "", lineRange{}, true},
		{"past EOF in a later range", "FROM a\nRUN b\n", [][3]int{{0, 1, 1}, {0, 9, 9}}, "", lineRange{}, true},
		{"empty Dockerfile", "", [][3]int{{0, 1, 1}}, "", lineRange{}, true},
		{"source index out of range", "FROM a\n", [][3]int{{1, 1, 1}}, "", lineRange{}, true},
		{"negative source index", "FROM a\n", [][3]int{{-1, 1, 1}}, "", lineRange{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov := testProvenance{platform: "linux/amd64", dockerfile: tt.dockerfile, steps: []testStep{
				{id: "step0", op: execOp("true"), digest: digestOf('a'), ranges: tt.ranges},
			}}
			p := simpleParts(t)
			p.provenances = [][]byte{prov.json(t)}
			rec, err := p.archive(t).decode(t.Context(), testLimits())
			if tt.wantErr {
				if !errors.Is(err, ErrMalformedRecord) {
					t.Fatalf("decodeRecord() error = %v, want ErrMalformedRecord", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeRecord() error = %v", err)
			}
			s := rec.builds[0].steps[0]
			if s.text != tt.want || s.lines != tt.wantLines {
				t.Errorf("text, lines = %q, %v, want %q, %v", s.text, s.lines, tt.want, tt.wantLines)
			}
		})
	}
}

func TestDecodeRecordTextBudget(t *testing.T) {
	line := strings.Repeat("x", 100)
	dockerfile := "RUN " + line + "\n"
	var steps []testStep
	for i := range 10 {
		steps = append(steps, testStep{id: fmt.Sprint("step", i), op: execOp("true"), ranges: [][3]int{{0, 1, 1}}})
	}
	prov := testProvenance{platform: "linux/amd64", dockerfile: dockerfile, steps: steps}
	textSize := 10 * len("RUN "+line)
	for _, tt := range []struct {
		limit   int
		wantErr bool
	}{{textSize, false}, {textSize - 1, true}} {
		p := simpleParts(t)
		p.provenances = [][]byte{prov.json(t)}
		lim := testLimits()
		lim.textBytes = tt.limit
		_, err := p.archive(t).decode(t.Context(), lim)
		var le *LimitError
		if got := errors.As(err, &le) && le.What == "bytes of Dockerfile text"; got != tt.wantErr {
			t.Errorf("decodeRecord() with a text limit of %d error = %v, want LimitError %v", tt.limit, err, tt.wantErr)
		}
	}
}

func TestDecodeRecordKeepsStepBytes(t *testing.T) {
	const op = "{ \"a\" :\n [ 1 , \"\\u0026 x\" , 12345678901234567890123 ] }"
	prov := simpleProvenance("linux/amd64")
	prov.steps[1].op = json.RawMessage(op)
	p := simpleParts(t)
	p.provenances = [][]byte{bytes.Replace(prov.json(t), []byte(`{"a":[1,"\u0026 x",12345678901234567890123]}`), []byte(op), 1)}
	rec, err := p.archive(t).decode(t.Context(), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(rec.builds[0].steps[1].op); got != op {
		t.Errorf("step definition = %s, want %s", got, op)
	}
}

// historyCounts are the step counts that BuildKit stores in the history
// record. An absent count is zero.
type historyCounts struct {
	Total     int `json:"numTotalSteps"`
	Cached    int `json:"numCachedSteps"`
	Completed int `json:"numCompletedSteps"`
}

func fixtureHistoryCounts(tb testing.TB, path string) historyCounts {
	tb.Helper()
	b := loadBundle(tb, path)
	var c historyCounts
	if err := json.Unmarshal(b.blob(b.configHex()), &c); err != nil {
		tb.Fatalf("history record of %s: %v", path, err)
	}
	return c
}

func fixtureStatus(tb testing.TB, path string) []byte {
	tb.Helper()
	b := loadBundle(tb, path)
	return b.blob(b.layerHex(mediaTypeStatus))
}

// prHitLines are the lines that hit in each scenario's pull request build,
// from the table in lab/README.md, by platform. A step lists the first line
// of its text, so in multi-stage line 7 counts as line 2, whose step BuildKit
// shares with it, and in copy-link both halves of the COPY --link on line 3
// count.
func prHitLines() map[string]map[string][]int {
	one := func(lines ...int) map[string][]int { return map[string][]int{"linux/amd64": lines} }
	return map[string]map[string][]int{
		"control":                  one(2, 3, 4, 5, 6),
		"changed-instruction":      one(2, 3),
		"changed-copy-file":        one(2, 3, 4),
		"changed-base-image":       one(),
		"no-cache-import":          one(),
		"added-step":               one(2),
		"added-copy":               one(2, 3, 4),
		"changed-dockerignore":     one(2, 3, 4, 5, 6),
		"multi-stage":              one(2, 8, 9),
		"removed-step":             one(2),
		"reordered-steps":          one(2, 3),
		"many-steps":               one(2, 3, 4, 5, 6, 7, 8, 9, 10),
		"added-env":                one(2, 4),
		"added-user":               one(2, 4),
		"changed-build-arg":        one(3, 4),
		"rotating-build-arg":       one(3, 4),
		"min-mode-needed":          one(6),
		"overwritten-cache":        one(2, 3, 4),
		"identical-lines":          one(2, 3),
		"changed-next-to-added":    one(2, 3),
		"two-platforms":            {"linux/amd64": {3, 4, 5, 6, 7}, "linux/arm64": {3, 4, 5}},
		"subdirectory-context":     one(2, 3, 4),
		"wildcard-copy":            one(2, 3, 4),
		"two-cache-imports":        one(2, 3, 4, 5, 6),
		"copy-link":                one(2, 3, 3, 5),
		"remote-add":               one(2),
		"named-context":            one(2, 3, 4),
		"no-cache-filter":          one(2, 6, 7, 8, 9),
		"registry-cache":           one(2, 3, 4),
		"failed-cache-import":      one(),
		"one-failed-cache-import":  one(2, 3, 4),
		"git-context-dockerignore": one(2, 3, 4, 5, 6),
		"bind-mount":               one(2, 3),
		"cache-mount-from":         one(2, 3),
	}
}

func TestCollectFixtures(t *testing.T) {
	checked := map[string]bool{}
	for _, path := range fixturePaths(t) {
		scenario, name := filepath.Base(filepath.Dir(path)), strings.TrimSuffix(filepath.Base(path), ".dockerbuild")
		t.Run(scenario+"/"+name, func(t *testing.T) {
			rec, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			counts := fixtureHistoryCounts(t, path)
			var cached, completed int
			for _, v := range rec.vertices {
				if v.cached {
					cached++
				}
				if v.completed.valid {
					completed++
				}
			}
			if len(rec.vertices) != counts.Total || cached != counts.Cached || completed != counts.Completed {
				t.Errorf("vertices, cached, completed = %d, %d, %d, want numTotalSteps %d, numCachedSteps %d, numCompletedSteps %d",
					len(rec.vertices), cached, completed, counts.Total, counts.Cached, counts.Completed)
			}
			if len(rec.builds) == 0 {
				t.Fatal("Collect() found no build")
			}
			hits := map[string][]int{}
			for _, b := range rec.builds {
				lines := []int{}
				for _, s := range b.steps {
					if s.vertex >= len(rec.vertices) {
						t.Fatalf("step %q has vertex %d of %d", s.id, s.vertex, len(rec.vertices))
					}
					if s.vertex >= 0 && rec.vertices[s.vertex].hit() {
						lines = append(lines, s.lines.start)
					}
				}
				slices.Sort(lines)
				hits[b.platform] = lines
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
	for scenario := range prHitLines() {
		if !checked[scenario] {
			t.Errorf("scenario %q from lab/README.md has no pull request record", scenario)
		}
	}
}

func FuzzDecodeRecord(f *testing.F) {
	for _, p := range fixturePaths(f) {
		b := loadBundle(f, p)
		var prov []byte
		if hex := b.layerHex(mediaTypeInToto); hex != "" {
			prov = b.blob(hex)
		}
		f.Add(b.blob(b.configHex()), b.blob(b.layerHex(mediaTypeStatus)), prov)
	}
	simple := simpleParts(f)
	f.Add(simple.history, simple.status, simple.provenances[0])
	lim := testLimits()
	lim.steps = 256
	lim.vertices = 256
	lim.textBytes = 1 << 16
	f.Fuzz(func(t *testing.T, history, status, provenance []byte) {
		p := recordParts{history: history, status: status}
		if provenance != nil {
			p.provenances = [][]byte{provenance}
		}
		rec, err := p.archive(t).decode(t.Context(), lim)
		if err != nil {
			if !isTyped(err) {
				t.Fatalf("decodeRecord() error = %v, want a typed error", err)
			}
			return
		}
		if (len(rec.builds) > 0) != (rec.provenance == provenanceRead) {
			t.Fatalf("decodeRecord() = %d builds with provenance %v", len(rec.builds), rec.provenance)
		}
		text := 0
		for _, b := range rec.builds {
			for _, s := range b.steps {
				if s.vertex < -1 || s.vertex >= len(rec.vertices) {
					t.Fatalf("step %q has vertex %d of %d", s.id, s.vertex, len(rec.vertices))
				}
				if (s.lines == lineRange{} && s.text != "") || s.lines.start < 0 || s.lines.end < s.lines.start {
					t.Fatalf("step %q has text %q and lines %v", s.id, s.text, s.lines)
				}
				text += len(s.text)
			}
		}
		if text > lim.textBytes {
			t.Fatalf("decodeRecord() kept %d bytes of text, want at most %d", text, lim.textBytes)
		}
		once, err := redact(t.Context(), rec)
		if err != nil {
			if !isTyped(err) {
				t.Fatalf("redact() error = %v, want a typed error", err)
			}
			return
		}
		if twice, err := redact(t.Context(), rec); err != nil || !redactedEqual(once, twice) {
			t.Fatalf("redact() is not deterministic")
		}
	})
}

func TestBundleRoundTrip(t *testing.T) {
	path := fixturePaths(t)[0]
	b := loadBundle(t, path)
	out := filepath.Join(t.TempDir(), "copy.dockerbuild")
	b.write(t, out)
	want, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewBundleCollector().Collect(t.Context(), Source{Path: out})
	if err != nil {
		t.Fatalf("Collect() of a rebuilt fixture error = %v", err)
	}
	if !bytes.Equal([]byte(dumpRecord(got)), []byte(dumpRecord(want))) {
		t.Errorf("rebuilt fixture decodes differently")
	}
}
