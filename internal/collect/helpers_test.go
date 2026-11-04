package collect

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixturePaths returns every recorded build record in testdata. It fails
// the test when there is none, so a broken glob cannot pass silently.
func fixturePaths(tb testing.TB) []string {
	tb.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "buildkit-*", "*", "*.dockerbuild"))
	if err != nil {
		tb.Fatalf("Glob() error = %v", err)
	}
	if len(paths) == 0 {
		tb.Fatal("found no build records in testdata")
	}
	return paths
}

// fixturePath returns one scenario's record, for tests that need a known
// structure.
func fixturePath(tb testing.TB, scenario, name string) string {
	tb.Helper()
	for _, p := range fixturePaths(tb) {
		if filepath.Base(filepath.Dir(p)) == scenario && filepath.Base(p) == name+".dockerbuild" {
			return p
		}
	}
	tb.Fatalf("no %s/%s.dockerbuild in testdata", scenario, name)
	return ""
}

func readFile(tb testing.TB, path string) []byte {
	tb.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		tb.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return b
}

func gunzip(tb testing.TB, data []byte) []byte {
	tb.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		tb.Fatalf("gzip.NewReader() error = %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		tb.Fatalf("reading gzip error = %v", err)
	}
	return out
}

func gzipBytes(tb testing.TB, data []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		tb.Fatalf("gzip Write() error = %v", err)
	}
	if err := zw.Close(); err != nil {
		tb.Fatalf("gzip Close() error = %v", err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	name     string
	typeflag byte
	data     []byte
	linkname string
}

func regular(name string, data []byte) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeReg, data: data}
}

func dir(name string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeDir}
}

func tarBytes(tb testing.TB, entries ...tarEntry) []byte {
	tb.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0o644, Size: int64(len(e.data)), Linkname: e.linkname}
		if e.typeflag == tar.TypeXGlobalHeader {
			hdr = &tar.Header{Name: e.name, Typeflag: e.typeflag, PAXRecords: map[string]string{"comment": "x"}}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			tb.Fatalf("WriteHeader(%q) error = %v", e.name, err)
		}
		if _, err := tw.Write(e.data); err != nil {
			tb.Fatalf("Write(%q) error = %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		tb.Fatalf("tar Close() error = %v", err)
	}
	return buf.Bytes()
}

// rawHeader returns one ustar header block, for entries that tar.Writer
// refuses to write.
func rawHeader(name string, typeflag byte, size int64) []byte {
	b := make([]byte, 512)
	copy(b[0:100], name)
	copy(b[100:108], "0000644\x00")
	copy(b[108:116], "0000000\x00")
	copy(b[116:124], "0000000\x00")
	copy(b[124:136], fmt.Sprintf("%011o\x00", size))
	copy(b[136:148], fmt.Sprintf("%011o\x00", 0))
	b[156] = typeflag
	copy(b[257:265], "ustar\x0000")
	copy(b[148:156], "        ")
	var sum int64
	for _, c := range b {
		sum += int64(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

// paxHeader returns a PAX extended header block with its records, padded to
// a whole block.
func paxHeader(records ...string) []byte {
	var body strings.Builder
	for i := 0; i+1 < len(records); i += 2 {
		rec := " " + records[i] + "=" + records[i+1] + "\n"
		n := len(rec) + 1
		for len(fmt.Sprint(n))+len(rec) != n {
			n++
		}
		body.WriteString(fmt.Sprint(n) + rec)
	}
	out := rawHeader("PaxHeaders/x", tar.TypeXHeader, int64(body.Len()))
	return append(out, pad(body.String())...)
}

func pad(s string) []byte {
	b := []byte(s)
	if r := len(b) % 512; r != 0 {
		b = append(b, make([]byte, 512-r)...)
	}
	return b
}

func tarEnd() []byte { return make([]byte, 1024) }

func hex64(c byte) string { return strings.Repeat(string(c), 64) }

func testLimits() limits { return defaultLimits() }

// isTyped reports whether err is one of the errors that Collect documents,
// or a context error.
func isTyped(err error) bool {
	var le *LimitError
	return errors.Is(err, ErrMalformedRecord) || errors.Is(err, ErrUnsupportedRecord) ||
		errors.Is(err, ErrMultipleBuilds) || errors.Is(err, ErrNotRegularFile) || errors.As(err, &le)
}

// bundle is a build record loaded into memory, for tests that change it.
// When a blob changes, it fixes the sizes in the descriptors and keeps the
// blob's name, because the collector checks sizes and not digests.
type bundle struct {
	t        testing.TB
	order    []string
	dirs     map[string]bool
	files    map[string][]byte
	manifest map[string]any
}

func loadBundle(tb testing.TB, path string) *bundle {
	tb.Helper()
	tr := tar.NewReader(bytes.NewReader(gunzip(tb, readFile(tb, path))))
	b := &bundle{t: tb, dirs: map[string]bool{}, files: map[string][]byte{}}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			tb.Fatalf("reading %s: %v", path, err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			tb.Fatalf("reading %s: %v", path, err)
		}
		b.order = append(b.order, hdr.Name)
		b.dirs[hdr.Name] = hdr.Typeflag == tar.TypeDir
		b.files[hdr.Name] = data
	}
	b.manifest = b.decode(b.blob(b.manifestHex()))
	return b
}

func (b *bundle) decode(data []byte) map[string]any {
	b.t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		b.t.Fatalf("decoding JSON: %v", err)
	}
	return v
}

func (b *bundle) encode(v any) []byte {
	b.t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		b.t.Fatalf("encoding JSON: %v", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func digestHex(d any) string {
	s, _ := d.(string)
	return strings.TrimPrefix(s, "sha256:")
}

func (b *bundle) manifestHex() string {
	index := b.decode(b.files["index.json"])
	manifests, _ := index["manifests"].([]any)
	first, _ := manifests[0].(map[string]any)
	return digestHex(first["digest"])
}

func (b *bundle) blob(hex string) []byte { return b.files[blobDir+hex] }

func (b *bundle) layers() []map[string]any {
	ls, _ := b.manifest["layers"].([]any)
	out := make([]map[string]any, 0, len(ls))
	for _, l := range ls {
		m, _ := l.(map[string]any)
		out = append(out, m)
	}
	return out
}

func (b *bundle) configHex() string {
	config, _ := b.manifest["config"].(map[string]any)
	return digestHex(config["digest"])
}

// layerHex returns the first layer of mediaType, or "" when there is none.
func (b *bundle) layerHex(mediaType string) string {
	for _, l := range b.layers() {
		if l["mediaType"] == mediaType {
			return digestHex(l["digest"])
		}
	}
	return ""
}

func (b *bundle) setBlob(hex string, data []byte) {
	b.files[blobDir+hex] = data
	config, _ := b.manifest["config"].(map[string]any)
	for _, d := range append(b.layers(), config) {
		if digestHex(d["digest"]) == hex {
			d["size"] = len(data)
		}
	}
}

func (b *bundle) editJSON(hex string, edit func(map[string]any)) {
	v := b.decode(b.blob(hex))
	edit(v)
	b.setBlob(hex, b.encode(v))
}

func (b *bundle) addLayer(mediaType string, annotations map[string]string, data []byte) {
	sum := sha256.Sum256(data)
	hex := fmt.Sprintf("%x", sum)
	name := blobDir + hex
	if _, ok := b.files[name]; !ok {
		b.order = append(b.order, name)
	}
	b.files[name] = data
	layer := map[string]any{"mediaType": mediaType, "digest": "sha256:" + hex, "size": len(data)}
	if annotations != nil {
		layer["annotations"] = annotations
	}
	ls, _ := b.manifest["layers"].([]any)
	b.manifest["layers"] = append(ls, layer)
}

func (b *bundle) tar() []byte {
	b.t.Helper()
	manifestHex := b.manifestHex()
	manifest := b.encode(b.manifest)
	b.files[blobDir+manifestHex] = manifest
	index := b.decode(b.files["index.json"])
	manifests, _ := index["manifests"].([]any)
	first, _ := manifests[0].(map[string]any)
	first["size"] = len(manifest)
	b.files["index.json"] = b.encode(index)

	entries := make([]tarEntry, 0, len(b.order))
	for _, name := range b.order {
		if b.dirs[name] {
			entries = append(entries, dir(name))
		} else {
			entries = append(entries, regular(name, b.files[name]))
		}
	}
	return tarBytes(b.t, entries...)
}

func (b *bundle) write(tb testing.TB, path string) {
	tb.Helper()
	if err := os.WriteFile(path, gzipBytes(tb, b.tar()), 0o600); err != nil {
		tb.Fatal(err)
	}
}

// rawArchive is an OCI layout held in memory, for tests that build or edit
// a record blob by blob.
type rawArchive struct {
	index []byte
	blobs map[string][]byte
}

// layout reads the blobs of a in name order, the order that buildx writes.
func (a *rawArchive) layout(ctx context.Context, lim limits) (*layout, error) {
	b := newBlobReader(lim)
	l := &layout{index: a.index, blobs: map[string]*blob{}}
	for _, hex := range slices.Sorted(maps.Keys(a.blobs)) {
		data := a.blobs[hex]
		got, err := b.readBlob(ctx, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		l.blobs[hex] = got
	}
	return l, nil
}

// readJSON reads data as one JSON blob, with nothing read before it.
func readJSON(lim limits, data []byte) (*jsonParts, error) {
	return (&jsonBlobReader{lim: lim}).readJSON(bytes.NewReader(data), int64(len(data)))
}

func (a *rawArchive) decode(ctx context.Context, lim limits) (*Record, error) {
	l, err := a.layout(ctx, lim)
	if err != nil {
		return nil, err
	}
	return decodeRecord(ctx, l, lim)
}
