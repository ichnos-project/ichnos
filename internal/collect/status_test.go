package collect

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// readStatus reads a whole status stream held in memory.
func readStatus(ctx context.Context, data []byte, lim limits) ([]vertex, error) {
	b := newBlobReader(lim)
	b.br.Reset(bytes.NewReader(data))
	return b.readStatus(ctx, b.br, int64(len(data)))
}

const (
	wtVarint  = 0
	wtFixed64 = 1
	wtBytes   = 2
	wtStart   = 3
	wtEnd     = 4
	wtFixed32 = 5
)

func pbTag(b []byte, num, wire uint64) []byte {
	return binary.AppendUvarint(b, num<<3|wire)
}

// twos returns n in two's complement, as protobuf encodes a signed varint.
func twos(n int64) uint64 {
	if n >= 0 {
		return uint64(n)
	}
	return math.MaxUint64 - twos(-(n + 1))
}

func pbVarint(b []byte, num, v uint64) []byte {
	return binary.AppendUvarint(pbTag(b, num, wtVarint), v)
}

func pbBytes(b []byte, num uint64, data []byte) []byte {
	b = binary.AppendUvarint(pbTag(b, num, wtBytes), uint64(len(data)))
	return append(b, data...)
}

func pbString(b []byte, num uint64, s string) []byte { return pbBytes(b, num, []byte(s)) }

type update struct {
	digest    string
	name      string
	err       string
	cached    bool
	started   timestamp
	completed timestamp
}

func encodeTimestamp(ts timestamp) []byte {
	var b []byte
	if ts.seconds != 0 {
		b = pbVarint(b, 1, twos(ts.seconds))
	}
	if ts.nanos != 0 {
		b = pbVarint(b, 2, twos(int64(ts.nanos)))
	}
	return b
}

func encodeVertex(u update) []byte {
	var b []byte
	if u.digest != "" {
		b = pbString(b, 1, u.digest)
	}
	b = pbString(b, 2, "sha256:input")
	if u.name != "" {
		b = pbString(b, 3, u.name)
	}
	if u.cached {
		b = pbVarint(b, 4, 1)
	}
	if u.started.valid {
		b = pbBytes(b, 5, encodeTimestamp(u.started))
	}
	if u.completed.valid {
		b = pbBytes(b, 6, encodeTimestamp(u.completed))
	}
	if u.err != "" {
		b = pbString(b, 7, u.err)
	}
	return pbBytes(b, 8, []byte{0x0a, 0x01, 'g'})
}

// frame returns one length-prefixed StatusResponse holding msg.
func frame(msg []byte) []byte {
	n := min(len(msg), math.MaxUint32)
	return append(binary.LittleEndian.AppendUint32(nil, uint32(n)), msg...)
}

// vertices returns one frame with a vertex field for each update.
func vertices(us ...update) []byte {
	var msg []byte
	for _, u := range us {
		msg = pbBytes(msg, 1, encodeVertex(u))
	}
	return frame(msg)
}

func ts(seconds int64, nanos int32) timestamp {
	return timestamp{seconds: seconds, nanos: nanos, valid: true}
}

func concat(parts ...[]byte) []byte { return slices.Concat(parts...) }

func TestReadStatusFrames(t *testing.T) {
	a := update{digest: "sha256:a", name: "[1/2] FROM alpine", started: ts(10, 0), completed: ts(11, 0)}
	b := update{digest: "sha256:b", name: "[2/2] RUN true", cached: true}
	logsOnly := frame(pbBytes(nil, 3, pbString(nil, 1, "sha256:a")))
	tests := []struct {
		name    string
		data    []byte
		want    []string
		wantErr bool
	}{
		{"one frame", vertices(a, b), []string{"sha256:a", "sha256:b"}, false},
		{"many frames", concat(vertices(b), vertices(a), vertices(a)), []string{"sha256:a", "sha256:b"}, false},
		{"zero-length frame", concat(frame(nil), vertices(a)), []string{"sha256:a"}, false},
		{"only a zero-length frame", frame(nil), nil, false},
		{"logs-only frame", concat(vertices(a), logsOnly), []string{"sha256:a"}, false},
		{"statuses and warnings", frame(concat(pbBytes(nil, 2, []byte{1, 2}), pbBytes(nil, 4, nil), pbBytes(nil, 1, encodeVertex(a)))), []string{"sha256:a"}, false},
		{"empty", nil, nil, true},
		{"one trailing byte", concat(vertices(a), []byte{1}), nil, true},
		{"three trailing bytes", concat(vertices(a), []byte{1, 0, 0}), nil, true},
		{"frame longer than the stream", vertices(a)[:len(vertices(a))-1], nil, true},
		{"frame length overflow", concat([]byte{0xff, 0xff, 0xff, 0xff}, vertices(a)), nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readStatus(t.Context(), tt.data, testLimits())
			if tt.wantErr {
				if !errors.Is(err, ErrMalformedRecord) || got != nil {
					t.Fatalf("readStatus() = %v, %v, want nil, ErrMalformedRecord", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readStatus() error = %v", err)
			}
			if digests := vertexDigests(got); !slices.Equal(digests, tt.want) {
				t.Errorf("readStatus() digests = %q, want %q", digests, tt.want)
			}
		})
	}
}

func vertexDigests(vs []vertex) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.digest)
	}
	return out
}

func TestReadStatusWireFormat(t *testing.T) {
	vertexWith := func(fields []byte) []byte {
		return frame(pbBytes(nil, 1, concat(pbString(nil, 1, "sha256:a"), fields)))
	}
	tests := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{"unknown varint field", vertexWith(pbVarint(nil, 99, 1)), false},
		{"unknown fixed64 field", vertexWith(append(pbTag(nil, 99, wtFixed64), make([]byte, 8)...)), false},
		{"unknown fixed32 field", vertexWith(append(pbTag(nil, 99, wtFixed32), make([]byte, 4)...)), false},
		{"unknown bytes field", vertexWith(pbString(nil, 99, "x")), false},
		{"largest field number", vertexWith(pbVarint(nil, 1<<29-1, 1)), false},
		{"unknown top-level field", frame(pbVarint(nil, 7, 1)), false},
		{"overlong varint", vertexWith(append(pbTag(nil, 99, wtVarint), 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01)), true},
		{"truncated varint", vertexWith(append(pbTag(nil, 99, wtVarint), 0x80)), true},
		{"varint over 64 bits", vertexWith(append(pbTag(nil, 99, wtVarint), 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02)), true},
		{"largest varint", vertexWith(append(pbTag(nil, 99, wtVarint), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01)), false},
		{"truncated tag", frame([]byte{0x80}), true},
		{"field number 0", vertexWith(pbVarint(nil, 0, 1)), true},
		{"field number over 2^29-1", vertexWith(pbVarint(nil, 1<<29, 1)), true},
		{"huge tag", vertexWith(binary.AppendUvarint(nil, 1<<63)), true},
		{"wire type 3", vertexWith(pbTag(nil, 99, wtStart)), true},
		{"wire type 4", vertexWith(pbTag(nil, 99, wtEnd)), true},
		{"wire type 6", vertexWith(pbTag(nil, 99, 6)), true},
		{"wire type 7", vertexWith(pbTag(nil, 99, 7)), true},
		{"truncated fixed64", vertexWith(append(pbTag(nil, 99, wtFixed64), make([]byte, 7)...)), true},
		{"truncated fixed32", vertexWith(append(pbTag(nil, 99, wtFixed32), make([]byte, 3)...)), true},
		{"bytes longer than the message", vertexWith(append(pbTag(nil, 99, wtBytes), 5, 'x')), true},
		{"top-level bytes longer than the frame", frame(append(pbTag(nil, 9, wtBytes), 5, 'x')), true},
		{"vertex longer than the frame", frame(append(pbTag(nil, 1, wtBytes), 9, 'x')), true},
		{"digest longer than the vertex", frame(pbBytes(nil, 1, append(pbTag(nil, 1, wtBytes), 9, 'x'))), true},
		{"name longer than the vertex", vertexWith(append(pbTag(nil, 3, wtBytes), 9, 'x')), true},
		{"timestamp longer than the vertex", vertexWith(append(pbTag(nil, 5, wtBytes), 9, 'x')), true},
		{"timestamp with a truncated varint", vertexWith(pbBytes(nil, 5, []byte{0x08, 0x80})), true},
		{"timestamp with wire type 3", vertexWith(pbBytes(nil, 5, pbTag(nil, 1, wtStart))), true},
		{"timestamp with a truncated unknown field", vertexWith(pbBytes(nil, 5, append(pbTag(nil, 9, wtBytes), 5))), true},
		{"varint at the end of the stream", append(vertices(update{digest: "sha256:a"}), 1, 0, 0, 0, 0x80), true},
		{"bytes length overflow", vertexWith(append(pbTag(nil, 99, wtBytes), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01)), true},
		{"vertex list as a varint", frame(pbVarint(nil, 1, 1)), true},
		{"digest as a varint", frame(pbBytes(nil, 1, pbVarint(nil, 1, 1))), true},
		{"name as a varint", vertexWith(pbVarint(nil, 3, 1)), true},
		{"cached as bytes", vertexWith(pbString(nil, 4, "x")), true},
		{"started as a varint", vertexWith(pbVarint(nil, 5, 1)), true},
		{"completed as a varint", vertexWith(pbVarint(nil, 6, 1)), true},
		{"error as a varint", vertexWith(pbVarint(nil, 7, 1)), true},
		{"seconds as bytes", vertexWith(pbBytes(nil, 5, pbString(nil, 1, "x"))), true},
		{"nanos as bytes", vertexWith(pbBytes(nil, 5, pbString(nil, 2, "x"))), true},
		{"vertex with no digest", frame(pbBytes(nil, 1, pbString(nil, 3, "name"))), true},
		{"vertex with an empty digest", frame(pbBytes(nil, 1, pbString(nil, 1, ""))), true},
		{"bad timestamp in an unknown field", vertexWith(pbBytes(nil, 99, []byte{0xff})), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readStatus(t.Context(), tt.data, testLimits())
			if tt.wantErr {
				if !errors.Is(err, ErrMalformedRecord) || got != nil {
					t.Fatalf("readStatus() = %v, %v, want nil, ErrMalformedRecord", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readStatus() error = %v", err)
			}
		})
	}
}

func TestReadStatusLastValueWins(t *testing.T) {
	msg := concat(
		pbString(nil, 1, "sha256:first"), pbString(nil, 1, "sha256:last"),
		pbString(nil, 3, "RUN first"), pbString(nil, 3, "[internal] last"),
		pbVarint(nil, 4, 1), pbVarint(nil, 4, 0),
		pbBytes(nil, 5, concat(pbVarint(nil, 1, 5), pbVarint(nil, 1, 7))),
	)
	got, err := readStatus(t.Context(), frame(pbBytes(nil, 1, msg)), testLimits())
	if err != nil {
		t.Fatalf("readStatus() error = %v", err)
	}
	want := []vertex{{digest: "sha256:last", kind: kindInternal, started: ts(7, 0)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readStatus() = %+v, want %+v", got, want)
	}
}

func TestClassifyVertex(t *testing.T) {
	long := strings.Repeat("x", 2*maxNamePrefix)
	names := map[string]vertexKind{
		"[internal] load build definition from Dockerfile":             kindInternal,
		"[internal] load metadata for docker.io/library/alpine:3.22":   kindInternal,
		"importing cache manifest from local:8534806393898525440":      kindImport,
		"importing cache manifest from https://u:p@registry.example/x": kindImport,
		"exporting cache to client directory":                          kindExport,
		"exporting cache to registry":                                  kindExport,
		"exporting inline cache":                                       kindExport,
		"exporting to GitHub Actions Cache":                            kindExport,
		"exporting cache to Amazon S3":                                 kindExport,
		"exporting cache to Azure Blob Storage":                        kindExport,
		"exporting to oci image format":                                kindInternal,
		"exporting to image":                                           kindInternal,
		"[linux/amd64 internal] load metadata for docker.io/library/x": kindInternal,
		"[context assets] load .dockerignore":                          kindInternal,
		"[context assets] load from client":                            kindInternal,
		"[context 2/6] RUN true":                                       kindStep,
		"[context  1/12] RUN true":                                     kindStep,
		"[internal 2/6] RUN true":                                      kindStep,
		"[linux/amd64 2/6] RUN true":                                   kindStep,
		"[stage-0 2/6] COPY . .":                                       kindStep,
		"[2/6] WORKDIR /app":                                           kindStep,
		"[internal]load":                                               kindStep,
		"":                                                             kindStep,
		"[internal] " + long:                                           kindInternal,
		"exporting inline cache" + long:                                kindStep,
		"[" + long + " internal] load":                                 kindStep,
	}
	for name, want := range names {
		got, err := readStatus(t.Context(), vertices(update{digest: "sha256:a", name: name}), testLimits())
		if err != nil {
			t.Fatalf("readStatus() error = %v", err)
		}
		if got[0].kind != want {
			t.Errorf("kind of vertex %.60q = %v, want %v", name, got[0].kind, want)
		}
	}
}

func TestReadStatusKeepsKindOfLastName(t *testing.T) {
	const d = "sha256:d"
	got, err := readStatus(t.Context(), concat(
		vertices(update{digest: d, name: "[internal] load"}),
		vertices(update{digest: d}),
	), testLimits())
	if err != nil {
		t.Fatalf("readStatus() error = %v", err)
	}
	if got[0].kind != kindInternal {
		t.Errorf("kind after an update with no name = %v, want %v", got[0].kind, kindInternal)
	}
}

func TestReadStatusDigestLength(t *testing.T) {
	for _, tt := range []struct {
		size    int
		wantErr bool
	}{{maxDigestBytes, false}, {maxDigestBytes + 1, true}} {
		digest := "sha256:" + strings.Repeat("a", tt.size-len("sha256:"))
		_, err := readStatus(t.Context(), vertices(update{digest: digest}), testLimits())
		if gotErr := errors.Is(err, ErrMalformedRecord); gotErr != tt.wantErr {
			t.Errorf("readStatus() with a digest of %d bytes error = %v, want error %v", tt.size, err, tt.wantErr)
		}
	}
}

// TestReadStatusSkipsLogs checks that logs, which can make up nearly all of
// a status stream, are read without being kept.
func TestReadStatusSkipsLogs(t *testing.T) {
	logs := frame(pbBytes(nil, 3, concat(pbString(nil, 1, "sha256:a"), pbBytes(nil, 3, make([]byte, 1<<20)))))
	data := vertices(update{digest: "sha256:a", name: "[1/1] RUN make"})
	for range 64 {
		data = append(data, logs...)
	}
	var got []vertex
	var err error
	grew := allocated(func() { got, err = readStatus(t.Context(), data, testLimits()) })
	if err != nil || len(got) != 1 {
		t.Fatalf("readStatus() = %v, %v, want one vertex", got, err)
	}
	if grew > 1<<20 {
		t.Errorf("readStatus() allocated %d bytes for %d bytes of logs, want under 1 MiB", grew, len(data))
	}
}

func TestReadStatusTimestamps(t *testing.T) {
	const maxSeconds = 253402300799
	var minSeconds int64 = -62135596800
	vertexStarted := func(stamp []byte) []byte {
		return frame(pbBytes(nil, 1, concat(pbString(nil, 1, "sha256:a"), pbBytes(nil, 5, stamp))))
	}
	tests := []struct {
		name    string
		stamp   []byte
		want    timestamp
		wantErr bool
	}{
		{"empty is the epoch", nil, ts(0, 0), false},
		{"unknown field", concat(pbVarint(nil, 1, 5), pbString(nil, 3, "x")), ts(5, 0), false},
		{"earliest", pbVarint(nil, 1, twos(minSeconds)), ts(minSeconds, 0), false},
		{"latest", concat(pbVarint(nil, 1, maxSeconds), pbVarint(nil, 2, 999_999_999)), ts(maxSeconds, 999_999_999), false},
		{"before year 1", pbVarint(nil, 1, twos(minSeconds-1)), timestamp{}, true},
		{"after year 9999", pbVarint(nil, 1, maxSeconds+1), timestamp{}, true},
		{"negative nanos", pbVarint(nil, 2, ^uint64(0)), timestamp{}, true},
		{"nanos of a whole second", pbVarint(nil, 2, 1_000_000_000), timestamp{}, true},
		{"nanos past int32", pbVarint(nil, 2, 1<<32), timestamp{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readStatus(t.Context(), vertexStarted(tt.stamp), testLimits())
			if tt.wantErr {
				if !errors.Is(err, ErrMalformedRecord) {
					t.Fatalf("readStatus() error = %v, want ErrMalformedRecord", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readStatus() error = %v", err)
			}
			if got[0].started != tt.want {
				t.Errorf("started = %+v, want %+v", got[0].started, tt.want)
			}
		})
	}
}

func TestReadStatusFold(t *testing.T) {
	const d = "sha256:d"
	tests := []struct {
		name        string
		updates     []update
		wantHit     bool
		wantLostHit bool
	}{
		{"cached only", []update{{digest: d, cached: true}}, true, false},
		{"cached then error", []update{{digest: d, cached: true}, {digest: d, err: "blob not found"}}, false, true},
		{"error then cached", []update{{digest: d, err: "blob not found"}, {digest: d, cached: true}}, false, true},
		{"error only", []update{{digest: d, err: "failed"}}, false, false},
		{"neither", []update{{digest: d}}, false, false},
		{"later update leaves out cached", []update{{digest: d, cached: true}, {digest: d}}, true, false},
		{"gha lost blob", []update{
			{digest: d, name: "[2/3] COPY app.txt ."},
			{digest: d, name: "[2/3] COPY app.txt .", cached: true, started: ts(100, 0), completed: ts(100, 5)},
			{digest: d, name: "[2/3] COPY app.txt .", cached: true, started: ts(100, 0), completed: ts(101, 0), err: "failed to compute cache key: blob not found"},
			{digest: d, name: "[2/3] COPY app.txt .", started: ts(102, 0)},
			{digest: d, name: "[2/3] COPY app.txt .", started: ts(102, 0), completed: ts(103, 0)},
		}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readStatus(t.Context(), vertices(tt.updates...), testLimits())
			if err != nil {
				t.Fatalf("readStatus() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("readStatus() returned %d vertices, want 1", len(got))
			}
			if got[0].hit() != tt.wantHit || got[0].lostHit() != tt.wantLostHit {
				t.Errorf("hit() = %v, lostHit() = %v, want %v, %v", got[0].hit(), got[0].lostHit(), tt.wantHit, tt.wantLostHit)
			}
		})
	}
}

func TestReadStatusTimes(t *testing.T) {
	const d = "sha256:d"
	got, err := readStatus(t.Context(), concat(
		vertices(update{digest: d, started: ts(20, 5), completed: ts(30, 0)}),
		vertices(update{digest: d, started: ts(20, 4), completed: ts(25, 0)}, update{digest: d}),
		vertices(update{digest: d, started: ts(21, 0), completed: ts(30, 1)}),
	), testLimits())
	if err != nil {
		t.Fatalf("readStatus() error = %v", err)
	}
	if got[0].started != ts(20, 4) || got[0].completed != ts(30, 1) {
		t.Errorf("started, completed = %+v, %+v, want the earliest start and the latest completion", got[0].started, got[0].completed)
	}

	backwards, err := readStatus(t.Context(), vertices(update{digest: d, started: ts(50, 0), completed: ts(40, 0)}), testLimits())
	if err != nil {
		t.Fatalf("readStatus() with completion before start error = %v", err)
	}
	if backwards[0].started != ts(50, 0) || backwards[0].completed != ts(40, 0) {
		t.Errorf("completion before start = %+v, want it kept", backwards[0])
	}
}

func TestReadStatusVertexLimit(t *testing.T) {
	data := vertices(update{digest: "sha256:a"}, update{digest: "sha256:b"}, update{digest: "sha256:a"})
	for _, tt := range []struct {
		limit   int
		wantErr bool
	}{{2, false}, {1, true}} {
		lim := testLimits()
		lim.vertices = tt.limit
		_, err := readStatus(t.Context(), data, lim)
		var le *LimitError
		if got := errors.As(err, &le) && le.What == "vertices"; got != tt.wantErr {
			t.Errorf("readStatus() with a limit of %d error = %v, want LimitError %v", tt.limit, err, tt.wantErr)
		}
	}
}

func TestReadStatusCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := readStatus(ctx, vertices(update{digest: "sha256:a"}), testLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readStatus() error = %v, want context.Canceled", err)
	}
}

func FuzzReadStatus(f *testing.F) {
	// statusErrors lists every message the status decoder may return, so that
	// FuzzReadStatus fails on any new message until someone checks that it holds
	// no input bytes.
	statusErrors := []string{
		"malformed build record: status stream is empty",
		"malformed build record: status stream ends inside a frame length",
		"malformed build record: status frame is longer than the stream",
		"malformed build record: status stream has a malformed varint",
		"malformed build record: status stream has a field number out of range",
		"malformed build record: status stream has an unsupported wire type",
		"malformed build record: status stream has a truncated field",
		"malformed build record: status stream has a field with the wrong wire type",
		"malformed build record: status vertex has no digest",
		"malformed build record: status vertex has a digest that is too long",
		"malformed build record: status stream has a timestamp out of range",
	}
	for _, p := range fixturePaths(f) {
		f.Add(fixtureStatus(f, p))
	}
	f.Add(vertices(update{digest: "sha256:a", cached: true, err: "x", started: ts(1, 2), completed: ts(3, 4)}))
	f.Add(concat(frame(nil), vertices(update{digest: "sha256:b", name: "n"})))
	lim := testLimits()
	lim.vertices = 64
	kindNames := map[vertexKind]string{
		kindStep:     "[2/2] RUN true",
		kindInternal: "[internal] load build definition from Dockerfile",
		kindImport:   "importing cache manifest from local:1",
		kindExport:   "exporting inline cache",
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := readStatus(t.Context(), data, lim)
		if err != nil {
			var le *LimitError
			if !errors.As(err, &le) && !slices.Contains(statusErrors, err.Error()) {
				t.Fatalf("readStatus() error = %q, want one of the known messages", err)
			}
			if got != nil {
				t.Fatalf("readStatus() returned %d vertices with error %v", len(got), err)
			}
			return
		}
		for i, v := range got {
			if i > 0 && strings.Compare(got[i-1].digest, v.digest) >= 0 {
				t.Fatalf("digests %q, %q are not unique and sorted", got[i-1].digest, v.digest)
			}
		}
		// Each vertex is cached or errored when any update about it says so,
		// and has the kind of its last name.
		want := map[string]vertex{}
		for rest := data; len(rest) > 0; {
			n := binary.LittleEndian.Uint32(rest)
			frame := rest[4 : 4+n]
			rest = rest[4+n:]
			for len(frame) > 0 {
				fl, next, fieldErr := refField(frame)
				if fieldErr != nil {
					t.Fatalf("refField() of a stream that readStatus() accepted: %v", fieldErr)
				}
				frame = next
				if fl.num != fieldStatusVertexes {
					continue
				}
				u, vertexErr := refVertex(fl.bytes)
				if vertexErr != nil {
					t.Fatalf("refVertex() of a stream that readStatus() accepted: %v", vertexErr)
				}
				w := want[u.digest]
				w.cached, w.errored = w.cached || u.cached, w.errored || u.err != ""
				if u.name != "" {
					w.kind = classifyVertex(u.name[:min(len(u.name), maxNamePrefix)])
				}
				want[u.digest] = w
			}
		}
		for _, v := range got {
			if w := want[v.digest]; v.cached != w.cached || v.errored != w.errored || v.kind != w.kind {
				t.Fatalf("vertex %q cached, errored, kind = %v, %v, %v, want %v, %v, %v", v.digest, v.cached, v.errored, v.kind, w.cached, w.errored, w.kind)
			}
		}
		again, err := readStatus(t.Context(), data, lim)
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("second readStatus() = %+v, %v, want %+v", again, err, got)
		}
		updates := make([]update, 0, len(got))
		for _, v := range got {
			u := update{digest: v.digest, name: kindNames[v.kind], cached: v.cached, started: v.started, completed: v.completed}
			if v.errored {
				u.err = "error"
			}
			updates = append(updates, u)
		}
		if len(updates) == 0 {
			return
		}
		round, err := readStatus(t.Context(), vertices(updates...), lim)
		if err != nil || !reflect.DeepEqual(round, got) {
			t.Fatalf("readStatus(encode(%+v)) = %+v, %v, want the same vertices", got, round, err)
		}
	})
}

type refFieldValue struct {
	num    uint64
	wire   uint64
	varint uint64
	bytes  []byte
}

// refField reads one field of a protobuf message held in memory, as a
// reference for the streaming reader.
func refField(b []byte) (refFieldValue, []byte, error) {
	key, n := binary.Uvarint(b)
	if n <= 0 {
		return refFieldValue{}, nil, errors.New("malformed varint")
	}
	b = b[n:]
	f := refFieldValue{num: key >> 3, wire: key & 7}
	if f.num < 1 || f.num > maxFieldNumber {
		return refFieldValue{}, nil, errors.New("field number out of range")
	}
	switch f.wire {
	case wireVarint:
		f.varint, n = binary.Uvarint(b)
		if n <= 0 {
			return refFieldValue{}, nil, errors.New("malformed varint")
		}
		return f, b[n:], nil
	case wireFixed64, wireFixed32:
		size := map[uint64]int{wireFixed64: 8, wireFixed32: 4}[f.wire]
		if len(b) < size {
			return refFieldValue{}, nil, errors.New("truncated field")
		}
		return f, b[size:], nil
	case wireBytes:
		size, n := binary.Uvarint(b)
		if n <= 0 || size > math.MaxInt32 || int(size) > len(b[n:]) {
			return refFieldValue{}, nil, errors.New("truncated field")
		}
		f.bytes = b[n : n+int(size)]
		return f, b[n+int(size):], nil
	default:
		return refFieldValue{}, nil, errors.New("unsupported wire type")
	}
}

// refVertex reads the digest, last name, cached flag and error of one
// Vertex message held in memory, as a reference for the streaming reader.
func refVertex(msg []byte) (update, error) {
	var u update
	for len(msg) > 0 {
		fl, rest, err := refField(msg)
		if err != nil {
			return update{}, err
		}
		msg = rest
		switch fl.num {
		case fieldVertexDigest:
			u.digest = string(fl.bytes)
		case fieldVertexName:
			u.name = string(fl.bytes)
		case fieldVertexCached:
			u.cached = fl.varint != 0
		case fieldVertexError:
			u.err = string(fl.bytes)
		}
	}
	return u, nil
}
