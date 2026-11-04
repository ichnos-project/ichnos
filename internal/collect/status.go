package collect

import (
	"bufio"
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
)

// vertex is one BuildKit vertex, folded from every update about it in the
// status stream. Its name is not kept, because BuildKit expands build
// arguments into names; only the kind that the name shows is.
type vertex struct {
	digest string
	kind   vertexKind
	vertexStatus
}

// vertexStatus is what the updates about a vertex say. BuildKit repeats a
// vertex in later updates and leaves out cached there, so cached is true if
// any update says so, and errored is true if any update has an error.
type vertexStatus struct {
	cached    bool
	errored   bool
	started   timestamp
	completed timestamp
}

// hit reports whether the vertex used the cache. A vertex that reported a
// hit and then an error lost the hit: BuildKit could not load the cached
// result, so it ran the step again.
func (s vertexStatus) hit() bool { return s.cached && !s.errored }

// lostHit reports whether the vertex reported a hit and then an error.
func (s vertexStatus) lostHit() bool { return s.cached && s.errored }

// timestamp is a protobuf Timestamp. It is not a time.Time, so that values
// that hold it share no pointers.
type timestamp struct {
	seconds int64
	nanos   int32
	valid   bool
}

func (t timestamp) compare(u timestamp) int {
	return cmp.Or(cmp.Compare(t.seconds, u.seconds), cmp.Compare(t.nanos, u.nanos))
}

type vertexKind int

const (
	kindStep vertexKind = iota
	kindInternal
	kindImport
	kindExport
)

func (k vertexKind) String() string {
	switch k {
	case kindStep:
		return "step"
	case kindInternal:
		return "internal"
	case kindImport:
		return "import"
	case kindExport:
		return "export"
	}
	return fmt.Sprintf("vertexKind(%d)", int(k))
}

const (
	// The range of a protobuf Timestamp: 0001-01-01T00:00:00Z to
	// 9999-12-31T23:59:59.999999999Z.
	minTimestampSeconds = -62135596800
	maxTimestampSeconds = 253402300799
	maxTimestampNanos   = 999_999_999
	maxFieldNumber      = 1<<29 - 1

	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5

	// maxDigestBytes is the longest vertex digest. The longest that BuildKit
	// can write, SHA-512, takes 135 bytes.
	maxDigestBytes = 256
	// maxNamePrefix is how much of a vertex name is classified. The labels
	// that classifyVertex looks for end long before it, and a longer name
	// is a build step's own text.
	maxNamePrefix = 4 << 10
)

// Field numbers in BuildKit's control.proto at v0.34.0.
const (
	fieldStatusVertexes = 1

	fieldVertexDigest    = 1
	fieldVertexName      = 3
	fieldVertexCached    = 4
	fieldVertexStarted   = 5
	fieldVertexCompleted = 6
	fieldVertexError     = 7

	fieldTimestampSeconds = 1
	fieldTimestampNanos   = 2
)

// readStatus decodes a status stream of size bytes from r: frames of a
// 4-byte little-endian length and a StatusResponse message. It reads the
// stream as it arrives and skips logs without keeping them, so its memory
// grows with the vertices, never with the logs. It returns the vertices
// sorted by digest. Any damage fails the whole stream, because a stream cut
// short would turn hits into misses.
func (b *blobReader) readStatus(ctx context.Context, r *bufio.Reader, size int64) ([]vertex, error) {
	if size == 0 {
		return nil, malformed("status stream is empty")
	}
	f := folder{index: map[string]int{}, b: b}
	s := pbReader{r: r, left: size}
	for s.left > 0 {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("reading status stream: %w", err)
		}
		if err := f.frame(&s); err != nil {
			return nil, err
		}
	}
	if len(f.vertices) > b.lim.vertices-b.vertices {
		return nil, &LimitError{What: "vertices", Limit: int64(b.lim.vertices)}
	}
	b.vertices += len(f.vertices)
	slices.SortFunc(f.vertices, func(a, b vertex) int { return cmp.Compare(a.digest, b.digest) })
	return f.vertices, nil
}

// folder folds the updates of one status stream into vertices. A stream
// may hold at most the limit on its own, and counts against the limit that
// every status stream of the record shares only once it has been read
// whole, so a stream that fails costs nothing and the order of the blobs
// does not matter.
type folder struct {
	index    map[string]int
	vertices []vertex
	b        *blobReader
}

func (f *folder) frame(s *pbReader) error {
	if s.left < 4 {
		return malformed("status stream ends inside a frame length")
	}
	var hdr [4]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		return malformed("status stream ends inside a frame length")
	}
	s.left -= 4
	n := int64(binary.LittleEndian.Uint32(hdr[:]))
	if n > s.left {
		return malformed("status frame is longer than the stream")
	}
	msg := s.sub(n)
	for msg.left > 0 {
		num, wire, err := msg.field()
		if err != nil {
			return err
		}
		if num != fieldStatusVertexes {
			if skipErr := msg.skip(wire); skipErr != nil {
				return skipErr
			}
			continue
		}
		if wire != wireBytes {
			return malformed("status stream has a field with the wrong wire type")
		}
		size, err := msg.length()
		if err != nil {
			return err
		}
		v := msg.sub(size)
		u, err := f.b.readVertex(&v)
		if err != nil {
			return err
		}
		if err := f.fold(u); err != nil {
			return err
		}
	}
	return nil
}

// vertexUpdate is one update of a vertex. named is false when it has no
// name, and then its kind says nothing.
type vertexUpdate struct {
	vertex
	named bool
}

func (f *folder) fold(u vertexUpdate) error {
	i, ok := f.index[u.digest]
	if !ok {
		if len(f.vertices) == f.b.lim.vertices {
			return &LimitError{What: "vertices", Limit: int64(f.b.lim.vertices)}
		}
		i = len(f.vertices)
		f.index[u.digest] = i
		f.vertices = append(f.vertices, vertex{digest: u.digest})
	}
	v := &f.vertices[i]
	if u.named {
		v.kind = u.kind
	}
	v.cached = v.cached || u.cached
	v.errored = v.errored || u.errored
	if u.started.valid && (!v.started.valid || u.started.compare(v.started) < 0) {
		v.started = u.started
	}
	if u.completed.valid && (!v.completed.valid || u.completed.compare(v.completed) > 0) {
		v.completed = u.completed
	}
	return nil
}

// readVertex decodes one update of a Vertex message. A field that appears
// twice keeps its last value, as protobuf specifies. Only the start of the
// name is kept, and only to classify it.
func (b *blobReader) readVertex(m *pbReader) (vertexUpdate, error) {
	var u vertexUpdate
	for m.left > 0 {
		num, wire, err := m.field()
		if err != nil {
			return vertexUpdate{}, err
		}
		switch num {
		case fieldVertexDigest, fieldVertexName, fieldVertexStarted, fieldVertexCompleted, fieldVertexError:
			if wire != wireBytes {
				return vertexUpdate{}, malformed("status stream has a field with the wrong wire type")
			}
		case fieldVertexCached:
			if wire != wireVarint {
				return vertexUpdate{}, malformed("status stream has a field with the wrong wire type")
			}
		}
		switch num {
		case fieldVertexDigest:
			u.digest, err = b.readDigest(m)
		case fieldVertexName:
			u.kind, u.named, err = b.readName(m)
		case fieldVertexCached:
			var cached uint64
			cached, err = m.varint()
			u.cached = cached != 0
		case fieldVertexStarted:
			u.started, err = readTimestamp(m)
		case fieldVertexCompleted:
			u.completed, err = readTimestamp(m)
		case fieldVertexError:
			var size int64
			if size, err = m.length(); err == nil {
				u.errored = size > 0
				err = m.discard(size)
			}
		default:
			err = m.skip(wire)
		}
		if err != nil {
			return vertexUpdate{}, err
		}
	}
	if u.digest == "" {
		return vertexUpdate{}, malformed("status vertex has no digest")
	}
	return u, nil
}

func (b *blobReader) readDigest(m *pbReader) (string, error) {
	n, err := m.length()
	if err != nil {
		return "", err
	}
	if n > maxDigestBytes {
		return "", malformed("status vertex has a digest that is too long")
	}
	digest, err := m.prefix(b.scratch, n, maxDigestBytes)
	b.scratch = digest
	return string(digest), err
}

// readName returns the kind of vertex that a name shows, and whether there
// is a name.
func (b *blobReader) readName(m *pbReader) (vertexKind, bool, error) {
	n, err := m.length()
	if err != nil {
		return kindStep, false, err
	}
	name, err := m.prefix(b.scratch, n, maxNamePrefix)
	b.scratch = name
	return classifyVertex(string(name)), n > 0, err
}

// readTimestamp decodes the Timestamp message that follows in m. Its seconds
// and nanos are signed, in two's complement, so a negative value arrives as
// a varint above math.MaxInt64.
func readTimestamp(m *pbReader) (timestamp, error) {
	size, err := m.length()
	if err != nil {
		return timestamp{}, err
	}
	msg := m.sub(size)
	var seconds, nanos uint64
	for msg.left > 0 {
		num, wire, err := msg.field()
		if err != nil {
			return timestamp{}, err
		}
		if num != fieldTimestampSeconds && num != fieldTimestampNanos {
			if skipErr := msg.skip(wire); skipErr != nil {
				return timestamp{}, skipErr
			}
			continue
		}
		if wire != wireVarint {
			return timestamp{}, malformed("status stream has a field with the wrong wire type")
		}
		x, err := msg.varint()
		if err != nil {
			return timestamp{}, err
		}
		if num == fieldTimestampSeconds {
			seconds = x
		} else {
			nanos = x
		}
	}
	t := timestamp{valid: true}
	switch {
	case seconds <= maxTimestampSeconds:
		t.seconds = int64(seconds)
	case -seconds <= -minTimestampSeconds:
		t.seconds = -int64(-seconds)
	default:
		return timestamp{}, malformed("status stream has a timestamp out of range")
	}
	if nanos > maxTimestampNanos {
		return timestamp{}, malformed("status stream has a timestamp out of range")
	}
	t.nanos = int32(nanos)
	return t, nil
}

// pbReader reads the fields of a protobuf message from r as they arrive.
// left is the number of bytes of the message not yet read, and every length
// is checked against it. Errors are ErrMalformedRecord, including a read
// that fails, so the caller checks the source for a read error afterwards.
type pbReader struct {
	r    *bufio.Reader
	left int64
}

// sub returns a reader of the n bytes that follow, a nested message, and
// counts them as read in m. n must be at most m.left.
func (m *pbReader) sub(n int64) pbReader {
	m.left -= n
	return pbReader{r: m.r, left: n}
}

// ReadByte reads one byte of the message, so that binary.ReadUvarint can
// read a varint that must end within it.
func (m *pbReader) ReadByte() (byte, error) {
	if m.left == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	c, err := m.r.ReadByte()
	if err != nil {
		return 0, io.ErrUnexpectedEOF
	}
	m.left--
	return c, nil
}

func (m *pbReader) varint() (uint64, error) {
	x, err := binary.ReadUvarint(m)
	if err != nil {
		return 0, malformed("status stream has a malformed varint")
	}
	return x, nil
}

// field reads the key of the next field, and rejects groups and unknown
// wire types, which have no length to skip by.
func (m *pbReader) field() (num, wire uint64, err error) {
	key, err := m.varint()
	if err != nil {
		return 0, 0, err
	}
	num, wire = key>>3, key&7
	switch {
	case num < 1 || num > maxFieldNumber:
		return 0, 0, malformed("status stream has a field number out of range")
	case wire != wireVarint && wire != wireFixed64 && wire != wireBytes && wire != wireFixed32:
		return 0, 0, malformed("status stream has an unsupported wire type")
	}
	return num, wire, nil
}

// length reads the length of a bytes field, which must fit in the message.
func (m *pbReader) length() (int64, error) {
	n, err := m.varint()
	if err != nil {
		return 0, err
	}
	if n > math.MaxInt64 || int64(n) > m.left {
		return 0, malformed("status stream has a truncated field")
	}
	return int64(n), nil
}

func (m *pbReader) discard(n int64) error {
	if n > m.left {
		return malformed("status stream has a truncated field")
	}
	m.left -= n
	for n > 0 {
		skipped, err := m.r.Discard(int(min(n, 1<<20)))
		n -= int64(skipped)
		if err != nil {
			return malformed("status stream has a truncated field")
		}
	}
	return nil
}

// skip skips the value of a field of the given wire type.
func (m *pbReader) skip(wire uint64) error {
	switch wire {
	case wireVarint:
		_, err := m.varint()
		return err
	case wireFixed64:
		return m.discard(8)
	case wireFixed32:
		return m.discard(4)
	default:
		n, err := m.length()
		if err != nil {
			return err
		}
		return m.discard(n)
	}
}

// prefix reads the n bytes of a bytes field, whose length m has read, and
// returns the first keep of them in buf.
func (m *pbReader) prefix(buf []byte, n int64, keep int) ([]byte, error) {
	k := min(n, int64(keep))
	buf = slices.Grow(buf[:0], int(k))[:k]
	if _, err := io.ReadFull(m.r, buf); err != nil {
		return nil, malformed("status stream has a truncated field")
	}
	m.left -= k
	return buf, m.discard(n - k)
}

// classifyVertex tells cache imports, cache exports and BuildKit's own work
// from build steps by their names at BuildKit v0.34.0. The cache export names
// are the Name methods of the exporters in cache/remotecache. Other names
// that start with "exporting to" belong to the image and local exporters.
func classifyVertex(name string) vertexKind {
	switch {
	case strings.HasPrefix(name, "importing cache manifest from "):
		return kindImport
	case isInternalName(name):
		return kindInternal
	}
	switch name {
	case "exporting cache to client directory",
		"exporting cache to registry",
		"exporting inline cache",
		"exporting to GitHub Actions Cache",
		"exporting cache to Amazon S3",
		"exporting cache to Azure Blob Storage":
		return kindExport
	}
	if strings.HasPrefix(name, "exporting to ") {
		return kindInternal
	}
	return kindStep
}

// isInternalName reports whether a vertex name starts with the label of
// BuildKit's own steps: [internal], [<platform> internal] in a build for
// several platforms, or [context <name>] for a named build context.
func isInternalName(name string) bool {
	label, ok := strings.CutPrefix(name, "[")
	if !ok {
		return false
	}
	label, _, ok = strings.Cut(label, "] ")
	if !ok {
		return false
	}
	name, isContext := strings.CutPrefix(label, "context ")
	return label == "internal" || strings.HasSuffix(label, " internal") || (isContext && !stepCounter.MatchString(name))
}

// stepCounter matches the position in a build step's name, such as 2/6 in
// [context 2/6], where context is the name of a stage.
var stepCounter = regexp.MustCompile(`^ *[0-9]+/[0-9]+$`)
