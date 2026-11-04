package collect

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
)

// TestCollectLargeLogs checks that logs, which BuildKit keeps in the status
// stream, cost time but not memory, and count against no limit but the
// one on decompressed bytes.
func TestCollectLargeLogs(t *testing.T) {
	path := fixturePath(t, "control", "base")
	want, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	b := loadBundle(t, path)
	line := []byte("#12 0.512 compiling module 42 of 9000\n")
	logs := frame(pbBytes(nil, 3, concat(pbString(nil, 1, digestOf('7')), pbBytes(nil, 3, bytes.Repeat(line, (1<<20)/len(line))))))
	var status []byte
	for range 64 {
		status = append(status, logs...)
	}
	b.appendStatus(status)
	large := filepath.Join(t.TempDir(), "logs.dockerbuild")
	b.write(t, large)

	c := &bundleCollector{limits: testLimits()}
	var got *Record
	grew := allocated(func() { got, err = c.Collect(t.Context(), Source{Path: large}) })
	if err != nil {
		t.Fatalf("Collect() of a record with %d MiB of logs error = %v", len(status)>>20, err)
	}
	if !reflect.DeepEqual(got.recordData, want.recordData) {
		t.Errorf("Collect() of a record with more logs = %s, want %s", dumpRecord(got), dumpRecord(want))
	}
	if grew > 16<<20 {
		t.Errorf("Collect() of a record with %d MiB of logs allocated %d MiB, want at most 16 MiB", len(status)>>20, grew>>20)
	}
}

// TestJSONBytesCountOnlyBlobsThatHoldFields checks that the limit on bytes
// of JSON counts index.json and the blobs that hold fields Ichnos reads,
// and not the trace, which Ichnos never uses.
func TestJSONBytesCountOnlyBlobsThatHoldFields(t *testing.T) {
	path := fixturePath(t, "many-steps", "base")
	b := loadBundle(t, path)
	trace := int64(len(b.blob(b.layerHex("application/vnd.buildkit.otlp.json.v0"))))
	if used := jsonBytesUsed(b); trace <= used {
		t.Fatalf("the trace has %d bytes, want more than the %d bytes of the other JSON", trace, used)
	}
	records := map[string]int64{path: jsonBytesUsed(b)}
	// The same record with index.json first, so that the blobs come after
	// it.
	for i, name := range b.order {
		if name == "index.json" {
			b.order = append([]string{name}, slices.Delete(b.order, i, i+1)...)
			break
		}
	}
	indexFirst := filepath.Join(t.TempDir(), "index-first.dockerbuild")
	b.write(t, indexFirst)
	records[indexFirst] = jsonBytesUsed(b)
	for p, used := range records {
		for _, tt := range []struct {
			limit   int64
			wantErr bool
		}{{used, false}, {used - 1, true}} {
			c := &bundleCollector{limits: testLimits()}
			c.limits.jsonBytes = tt.limit
			_, err := c.Collect(t.Context(), Source{Path: p})
			if gotErr := isLimit("bytes of JSON")(err); gotErr != tt.wantErr || (!gotErr && err != nil) {
				t.Errorf("Collect(%s) with a limit of %d bytes of JSON error = %v, want LimitError %v", filepath.Base(p), tt.limit, err, tt.wantErr)
			}
		}
	}
}

// jsonBytesUsed returns the bytes of JSON that b counts against the limit:
// index.json and every blob with a top-level field that Ichnos reads.
func jsonBytesUsed(b *bundle) int64 {
	used := int64(len(b.files["index.json"]))
	for name, data := range b.files {
		var fields map[string]json.RawMessage
		if !strings.HasPrefix(name, blobDir) || json.Unmarshal(data, &fields) != nil {
			continue
		}
		for _, f := range []string{"buildDefinition", "runDetails", "config", "layers", "FrontendAttrs"} {
			if _, ok := fields[f]; ok {
				used += int64(len(data))
				break
			}
		}
	}
	return used
}

func TestReadJSONKeepsKindsApart(t *testing.T) {
	prov := inConfig(`{"llbDefinition":[{"id":"a","op":{"x":1}}]}`)
	prov = strings.TrimSuffix(prov, "}")
	tests := []struct {
		name         string
		data         string
		wantProv     bool
		wantManifest bool
		wantAttrs    bool
	}{
		{"provenance alone", prov + `}`, true, true, true},
		{"manifest fields of the wrong type", prov + `,"layers":"x","config":5}`, true, false, true},
		{"before the provenance", `{"layers":"x",` + prov[1:] + `}`, true, false, true},
		{"failure deep in a field", `{"config":{"annotations":{"in-toto.io/predicate-type":[1,{"a":[2]}]},"x":[3]},` + prov[1:] + `}`, true, false, true},
		{"history fields of the wrong type", prov + `,"FrontendAttrs":{"a":1,"b":{"c":[]}}}`, true, true, false},
		{"provenance fields of the wrong type", `{"FrontendAttrs":{"a":"b"},"buildDefinition":{"internalParameters":{"targetPlatform":1,"buildConfig":{}}}}`, false, true, true},
		{"invalid UTF-8 in an attribute value", prov + ",\"FrontendAttrs\":{\"a\":\"\xff\"}}", true, true, false},
		{"invalid UTF-8 in an attribute name", prov + ",\"FrontendAttrs\":{\"\xff\":\"x\"}}", true, true, false},
		{"invalid UTF-8 in a layer", prov + ",\"layers\":[{\"mediaType\":\"\xff\"}]}", true, false, true},
		{"manifest field twice", prov + `,"layers":[],"layers":[]}`, true, false, true},
		{"history field twice", prov + `,"FrontendAttrs":{},"FrontendAttrs":{}}`, true, true, false},
		{"provenance field twice", `{"FrontendAttrs":{"a":"b"},"runDetails":{},"runDetails":{}}`, false, true, true},
		{"invalid UTF-8 in a kept provenance string", "{\"FrontendAttrs\":{\"a\":\"b\"},\"buildDefinition\":{\"internalParameters\":{\"targetPlatform\":\"\xff\"}}}", false, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := readJSON(testLimits(), []byte(tt.data))
			if err != nil {
				t.Fatalf("readJSON() error = %v", err)
			}
			if got := p.provErr == nil; got != tt.wantProv {
				t.Errorf("provenance error = %v, want error %v", p.provErr, !tt.wantProv)
			}
			if got := p.manifestErr == nil; got != tt.wantManifest {
				t.Errorf("manifest error = %v, want error %v", p.manifestErr, !tt.wantManifest)
			}
			if got := p.attrsErr == nil; got != tt.wantAttrs {
				t.Errorf("attributes error = %v, want error %v", p.attrsErr, !tt.wantAttrs)
			}
			if tt.wantProv && (len(p.prov.steps) != 1 || string(p.prov.steps[0].op) != `{"x":1}`) {
				t.Errorf("provenance steps = %+v, want one step", p.prov.steps)
			}
		})
	}
	for _, data := range []string{prov, prov + `,"layers":[}`, `[` + prov + `}]`, prov + `} {}`} {
		if _, err := readJSON(testLimits(), []byte(data)); !errors.Is(err, errNotValid) {
			t.Errorf("readJSON(%.40q) error = %v, want errNotValid", data, err)
		}
	}
}

// TestUnlistedBlobsAreIgnored checks that a blob the manifest does not list
// changes nothing, even when it is a valid status stream or provenance.
func TestUnlistedBlobsAreIgnored(t *testing.T) {
	path := fixturePath(t, "control", "base")
	want, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	b := loadBundle(t, path)
	for _, data := range [][]byte{
		vertices(update{digest: digestOf('7'), name: "[internal] decoy", cached: true}),
		simpleProvenance("linux/arm64").json(t),
		b.blob(b.layerHex(mediaTypeInToto))[:len(b.blob(b.layerHex(mediaTypeInToto)))-1],
		[]byte(`{"FrontendAttrs":{"build-arg:DECOY":"1"}}`),
	} {
		name := blobDir + fmt.Sprintf("%x", sha256.Sum256(data))
		b.order = append(b.order, name)
		b.files[name] = data
	}
	decoyed := filepath.Join(t.TempDir(), "decoy.dockerbuild")
	b.write(t, decoyed)
	got, err := NewBundleCollector().Collect(t.Context(), Source{Path: decoyed})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if !reflect.DeepEqual(got.recordData, want.recordData) {
		t.Errorf("Collect() with unlisted blobs = %s, want %s", dumpRecord(got), dumpRecord(want))
	}
}

// TestSkipRestAfterFailure checks that skipRest leaves the decoder after
// the member, wherever in the member's value its reader stopped.
func TestSkipRestAfterFailure(t *testing.T) {
	for _, data := range []string{
		`{"a":"x","b":1}`,
		`{"a":{"c":[1,{"d":[]}],"e":2},"b":1}`,
		`{"a":[[[]]],"b":1}`,
	} {
		for tokens := range 5 {
			j := newJSONReader([]byte(data))
			var got []int
			err := j.fields([]string{"a", "b"}, func(field int) error {
				got = append(got, field)
				if field == 1 {
					_, err := j.integer()
					return err
				}
				read := j.read()
				for range tokens {
					if j.dec.StackDepth() == 1 && j.read() > read {
						break
					}
					if err := j.next(); err != nil {
						return err
					}
				}
				return j.skipRest(read)
			})
			if err == nil {
				err = j.end()
			}
			if err != nil || !reflect.DeepEqual(got, []int{0, 1}) {
				t.Errorf("fields(%s) after reading %d tokens and skipRest = %v, %v, want both fields", data, tokens, got, err)
			}
		}
	}
}

// TestDecodeIndependentOfBlobOrder checks that the order of the blobs in the
// archive does not change the record, since each blob is read before
// index.json says what it is.
func TestDecodeIndependentOfBlobOrder(t *testing.T) {
	for _, path := range fixturePaths(t) {
		b := loadBundle(t, path)
		a := &rawArchive{index: b.files["index.json"], blobs: map[string][]byte{}}
		for name, data := range b.files {
			if hex, ok := strings.CutPrefix(name, blobDir); ok && hex != "" {
				a.blobs[hex] = data
			}
		}
		want, err := a.decode(t.Context(), testLimits())
		if err != nil {
			t.Fatalf("%s: decode() error = %v", path, err)
		}
		reversed := &layout{index: a.index, blobs: map[string]*blob{}}
		r := newBlobReader(testLimits())
		hexes := slices.Sorted(maps.Keys(a.blobs))
		slices.Reverse(hexes)
		for _, hex := range hexes {
			got, readErr := r.readBlob(t.Context(), bytes.NewReader(a.blobs[hex]), int64(len(a.blobs[hex])))
			if readErr != nil {
				t.Fatalf("%s: readBlob() error = %v", path, readErr)
			}
			reversed.blobs[hex] = got
		}
		got, err := decodeRecord(t.Context(), reversed, testLimits())
		if err != nil || !reflect.DeepEqual(got.recordData, want.recordData) {
			t.Errorf("%s: decodeRecord() with the blobs in reverse = %v, want the same record", path, err)
		}
	}
}

func TestReadBlobErrors(t *testing.T) {
	path := fixturePath(t, "control", "base")
	c := &bundleCollector{limits: testLimits()}
	c.limits.vertices = 1
	if _, err := c.Collect(t.Context(), Source{Path: path}); !isLimit("vertices")(err) {
		t.Errorf("Collect() with a limit of 1 vertex error = %v, want LimitError", err)
	}

	b := loadBundle(t, path)
	prov := int64(len(b.blob(b.layerHex(mediaTypeInToto))))
	c = &bundleCollector{limits: testLimits()}
	c.limits.jsonBytes = prov - 1
	if _, err := c.Collect(t.Context(), Source{Path: path}); !isLimit("bytes of JSON")(err) {
		t.Errorf("Collect() with a provenance over the limit on bytes of JSON error = %v, want LimitError", err)
	}

	b.manifest["layers"] = "x"
	assertCollectError(t, b, "manifest is not valid")

	b = loadBundle(t, path)
	b.addLayer(mediaTypeStatus, nil, vertices(update{digest: digestOf('7')}))
	assertCollectError(t, b, "manifest has more than one status stream")
}

// assertCollectError writes b and checks that collecting it fails with an
// ErrMalformedRecord that says what.
func assertCollectError(t *testing.T, b *bundle, what string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "record.dockerbuild")
	b.write(t, path)
	_, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
	if !errors.Is(err, ErrMalformedRecord) || !strings.HasSuffix(err.Error(), what) {
		t.Errorf("Collect() error = %v, want %q", err, what)
	}
}

// TestReadArchiveCancelled checks that a cancelled context stops the read
// within one read wherever it is, and gives the context's error.
func TestReadArchiveCancelled(t *testing.T) {
	blob := blobDir + hex64('a')
	large := 64 << 20
	tests := []struct {
		name    string
		entries []tarEntry
		after   []byte
	}{
		{"inside one status frame", []tarEntry{regular(blob, frame(bytes.Repeat([]byte{0x10, 0x00}, large/2)))}, nil},
		{"inside a blob of no known kind", []tarEntry{regular(blob, bytes.Repeat([]byte{0xff}, large))}, nil},
		{"inside a JSON blob", []tarEntry{regular(blob, append([]byte(`{"x":[`), bytes.Repeat([]byte("0,"), large/2)...))}, nil},
		{"inside oci-layout", []tarEntry{regular("oci-layout", bytes.Repeat([]byte(" "), large))}, nil},
		{"after the end of the tar", []tarEntry{regular("index.json", []byte("{}"))}, make([]byte, large)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var z bytes.Buffer
			zw, err := gzip.NewWriterLevel(&z, gzip.NoCompression)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = zw.Write(append(tarBytes(t, tt.entries...), tt.after...)); err != nil {
				t.Fatal(err)
			}
			if err = zw.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			const cancelAt = 1 << 20
			read := &cancelAfter{n: cancelAt, cancel: cancel}
			_, err = readArchive(ctx, io.TeeReader(bytes.NewReader(z.Bytes()), read), testLimits())
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("readArchive() error = %v, want context.Canceled", err)
			}
			if read.total > 2*cancelAt {
				t.Errorf("readArchive() read %d of %d bytes, cancelled after %d, want at most a few more reads", read.total, z.Len(), cancelAt)
			}
		})
	}
}

// cancelAfter cancels once n bytes have been written to it.
type cancelAfter struct {
	n      int
	total  int
	cancel context.CancelFunc
}

func (c *cancelAfter) Write(p []byte) (int, error) {
	if c.total += len(p); c.total >= c.n {
		c.cancel()
	}
	return len(p), nil
}

// TestFailedStatusStreamGivesBackVertices checks that a blob that only
// looked like a status stream does not use up the vertex limit of the real
// one.
func TestFailedStatusStreamGivesBackVertices(t *testing.T) {
	path := fixturePath(t, "control", "base")
	want, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	b := loadBundle(t, path)
	var updates []update
	for i := range len(want.vertices) {
		updates = append(updates, update{digest: fmt.Sprintf("sha256:decoy%d", i)})
	}
	decoy := append(vertices(updates...), 0xff)
	name := blobDir + strings.Repeat("0", 64)
	b.order = append([]string{name}, b.order...)
	b.files[name] = decoy
	decoyed := filepath.Join(t.TempDir(), "decoy.dockerbuild")
	b.write(t, decoyed)
	c := &bundleCollector{limits: testLimits()}
	c.limits.vertices = len(want.vertices)
	if _, err := c.Collect(t.Context(), Source{Path: decoyed}); err != nil {
		t.Errorf("Collect() with a failed stream of %d vertices before the real one error = %v", len(want.vertices), err)
	}
	b.order = append(b.order[1:], name)
	b.write(t, decoyed)
	if _, err := c.Collect(t.Context(), Source{Path: decoyed}); err != nil {
		t.Errorf("Collect() with a failed stream of %d vertices after the real one error = %v", len(want.vertices), err)
	}
}

// TestUnheldJSONStaysInBudget checks that a JSON blob that holds no field
// Ichnos reads is not buffered when it does not fit in what is left of the
// budget, so that kept and buffered JSON stay within it together.
func TestUnheldJSONStaysInBudget(t *testing.T) {
	lim := testLimits()
	lim.jsonBytes = 1 << 20
	b := newBlobReader(lim)
	held := []byte(`{"FrontendAttrs":{"a":"` + strings.Repeat("x", 600<<10) + `"}}`)
	if _, err := b.readBlob(t.Context(), bytes.NewReader(held), int64(len(held))); err != nil {
		t.Fatalf("readBlob() error = %v", err)
	}
	unheld := []byte(`{"x":"` + strings.Repeat("y", 900<<10) + `"}`)
	var got *blob
	var err error
	grew := allocated(func() { got, err = b.readBlob(t.Context(), bytes.NewReader(unheld), int64(len(unheld))) })
	if err != nil || got.json != nil {
		t.Fatalf("readBlob() = %+v, %v, want a blob over the budget", got, err)
	}
	if grew > 256<<10 {
		t.Errorf("readBlob() of a %d-byte blob over what is left of the budget allocated %d bytes", len(unheld), grew)
	}
}

// TestUnusedJSONIsNotHeld checks that a JSON blob that holds no field Ichnos
// reads, such as the trace, streams through without being held.
func TestUnusedJSONIsNotHeld(t *testing.T) {
	const size = 16 << 20
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"one object", []byte(`{"x":[` + strings.Repeat(`{"a":"bbbbbbbb"},`, size/17) + `{}]}`)},
		{"many objects", bytes.Repeat([]byte(`{"a":"bbbbbbbb"}`+"\n"), size/17)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := newBlobReader(testLimits())
			var err error
			grew := allocated(func() { _, err = b.readBlob(t.Context(), bytes.NewReader(tt.data), int64(len(tt.data))) })
			if err != nil || b.json.jsonBytes != 0 {
				t.Fatalf("readBlob() error = %v, counted %d bytes of JSON, want nil and none", err, b.json.jsonBytes)
			}
			if grew > 1<<20 {
				t.Errorf("readBlob() of a %d-byte JSON blob allocated %d bytes, want at most 1 MiB", len(tt.data), grew)
			}
		})
	}
}

func TestJSONFollowedByNULIsNotJSON(t *testing.T) {
	for _, pad := range []int{0, 1 << 20} {
		data := []byte(`{"FrontendAttrs":{"a":"` + strings.Repeat("b", pad) + `"}}` + "\x00")
		got, err := newBlobReader(testLimits()).readBlob(t.Context(), bytes.NewReader(data), int64(len(data)))
		if err != nil || got.json != nil || !errors.Is(got.jsonErr, errNotValid) {
			t.Errorf("readBlob() of %d bytes of JSON and a NUL byte = %+v, %v, want errNotValid", len(data)-1, got, err)
		}
	}
}

// TestReadArchiveLeavesNoGoroutine checks that the JSON reader of every blob
// has stopped by the time readArchive returns, whether the read succeeded or
// stopped while the reader was in the middle of a blob. synctest.Test fails
// when a goroutine it started is still blocked at the end.
func TestReadArchiveLeavesNoGoroutine(t *testing.T) {
	var z bytes.Buffer
	zw, err := gzip.NewWriterLevel(&z, gzip.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = zw.Write(tarBytes(t, regular(blobDir+hex64('a'), append([]byte(`{"x":[`), bytes.Repeat([]byte("0,"), 1<<20)...)))); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := z.Bytes()
	for _, tt := range []struct {
		name    string
		data    []byte
		cancel  bool
		wantErr func(error) bool
	}{
		{"whole", data, false, func(err error) bool { return err == nil }},
		{"cut short", data[:len(data)/2], false, isMalformed},
		{"cancelled", data, true, func(err error) bool { return errors.Is(err, context.Canceled) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var r io.Reader = bytes.NewReader(tt.data)
				if tt.cancel {
					r = io.TeeReader(r, &cancelAfter{n: len(tt.data) / 2, cancel: cancel})
				}
				if _, err := readArchive(ctx, r, testLimits()); !tt.wantErr(err) {
					t.Fatalf("readArchive() error = %v, want a different error", err)
				}
			})
		})
	}
}

// TestUnlistedBlobOverALimitFails checks that a blob the manifest does not
// list still counts against the limits that every blob of a record shares,
// so that where it sits in the archive cannot change the result.
func TestUnlistedBlobOverALimitFails(t *testing.T) {
	b := loadBundle(t, fixturePath(t, "control", "base"))
	name := blobDir + hex64('f')
	b.order = append(b.order, name)
	b.files[name] = []byte(`{"FrontendAttrs":` + object(maxFrontendAttrs, `""`) + `}`)
	path := filepath.Join(t.TempDir(), "record.dockerbuild")
	b.write(t, path)
	if _, err := NewBundleCollector().Collect(t.Context(), Source{Path: path}); !isLimit("frontend attributes")(err) {
		t.Errorf("Collect() with an unlisted blob over the limit on frontend attributes error = %v, want LimitError", err)
	}
}

func TestRedactKeepsProvenanceState(t *testing.T) {
	for _, state := range []provenanceState{provenanceRead, provenanceMissing, provenanceUnsupported, provenanceNoSteps} {
		got := mustRedact(t, &Record{&recordData{provenance: state}})
		if got.provenance != state {
			t.Errorf("redact() of a record with provenance %v = %v", state, got.provenance)
		}
	}
}
