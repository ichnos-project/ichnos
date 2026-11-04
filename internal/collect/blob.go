package collect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"slices"
)

// blobReader reads the blobs of one record as they arrive. index.json, which
// says what each blob is, comes after them, so it reads each blob as every
// kind it can be at once: as a status stream, and, while it can still be
// JSON, as a manifest, a history record and a provenance. Neither reader
// holds the blob: each keeps what its kinds need, and the JSON reader also
// holds the one value it is reading, which the JSON budget bounds. The
// limits are shared by all blobs, so that a record cannot multiply what it
// keeps by adding blobs.
type blobReader struct {
	lim limits
	// vertices is the number of vertices that the status streams read so
	// far keep.
	vertices int
	br       *bufio.Reader
	// scratch holds the start of one vertex name or one digest.
	scratch []byte
	// json reads each blob in a goroutine of its own, alongside the status
	// reader, so it shares no state with the rest of blobReader.
	json *jsonBlobReader
}

func newBlobReader(lim limits) *blobReader {
	return &blobReader{lim: lim, br: bufio.NewReaderSize(nil, 64<<10), json: &jsonBlobReader{lim: lim}}
}

// jsonBlobReader reads blobs as JSON, and counts what the blobs read so far
// keep, against the limits.
type jsonBlobReader struct {
	lim       limits
	steps     int
	inputs    int
	mappings  int
	ranges    int
	sources   int
	layers    int
	attrs     int
	jsonBytes int64
}

// fits reports whether n more bytes of JSON fit in the limit.
func (b *jsonBlobReader) fits(n int64) bool { return n <= b.lim.jsonBytes-b.jsonBytes }

// hold counts n more bytes of JSON against the limit.
func (b *jsonBlobReader) hold(n int64) error {
	if !b.fits(n) {
		return &LimitError{What: "bytes of JSON", Limit: b.lim.jsonBytes}
	}
	b.jsonBytes += n
	return nil
}

// blob is what one blob held, before index.json says which kind it is.
type blob struct {
	size int64
	// json is nil when the blob is not JSON that Ichnos reads, and jsonErr
	// says why.
	json    *jsonParts
	jsonErr error
	// statusErr is nil exactly when the blob is a status stream, and then
	// vertices holds its vertices, if it has any.
	vertices  []vertex
	statusErr error
}

// jsonParts holds the fields that a manifest, a history record and a
// provenance read from one JSON blob, and the first error that each kind's
// fields gave. The kinds use different top-level names, and a kind's error
// leaves the others' fields intact, so a field that only another kind reads
// cannot spoil a blob for its own kind.
type jsonParts struct {
	manifest    manifest
	manifestErr error
	attrs       map[string]string
	attrsErr    error
	prov        rawProvenance
	provErr     error
}

// readBlob reads one blob of size bytes from r as it arrives, through the
// status stream reader and, at the same time, through the JSON reader. It
// returns an error only when the archive cannot be read or a limit is
// exceeded; that the blob is not of some kind is recorded in the blob.
func (b *blobReader) readBlob(ctx context.Context, r io.Reader, size int64) (*blob, error) {
	out := &blob{size: size}
	// A blob that does not fit in what is left of the JSON budget is not
	// read as JSON: if it holds fields, it would exceed the budget anyway,
	// and if it does not, nothing needs it. So what is kept and the value
	// the reader holds stay within the budget together. A blob of at most
	// pipeChunk bytes is gathered and read in place once it has arrived,
	// because for one that small a goroutine costs more time than it saves
	// memory.
	var pipe *jsonPipe
	var small *bytes.Buffer
	switch {
	case !b.json.fits(size):
		out.jsonErr = &LimitError{What: "bytes of JSON", Limit: b.lim.jsonBytes}
	case size <= pipeChunk:
		small = bytes.NewBuffer(make([]byte, 0, size))
		r = io.TeeReader(r, small)
	default:
		pipe = b.json.start(size)
		r = io.TeeReader(r, pipe)
	}
	b.br.Reset(r)
	out.vertices, out.statusErr = b.readStatus(ctx, b.br, size)
	// The status reader may stop early, and may have met a read error,
	// which it reports as a malformed stream. The tar and gzip readers and
	// stopWriter fail again, so draining tells a damaged archive or a
	// cancelled read from a blob of another kind.
	err := drain(ctx, b.br)
	b.br.Reset(nil)
	var jsonErr error
	switch {
	case pipe != nil:
		out.json, jsonErr = pipe.wait(err)
		out.jsonErr = jsonErr
	case small != nil && err == nil:
		out.json, jsonErr = b.json.readJSON(small, size)
		out.jsonErr = jsonErr
	}
	switch {
	case err != nil:
		return nil, err
	case out.statusErr != nil && !errors.Is(out.statusErr, ErrMalformedRecord):
		return nil, out.statusErr
	}
	if _, ok := errors.AsType[*LimitError](jsonErr); ok {
		return nil, jsonErr
	}
	return out, nil
}

// jsonPipe passes the bytes of one blob to a JSON reader in another
// goroutine. Once the reader stops, as it does at the first byte that is
// not part of one JSON value, the pipe drops the rest, so it never fails the
// read it is teed into.
type jsonPipe struct {
	w *io.PipeWriter
	// buf gathers small writes, so that the goroutines hand over fewer,
	// larger chunks.
	buf    *bufio.Writer
	closed bool
	done   <-chan jsonResult
}

type jsonResult struct {
	parts *jsonParts
	err   error
}

// start reads a blob of size bytes as JSON, in a new goroutine, from what
// is written to the returned pipe.
func (b *jsonBlobReader) start(size int64) *jsonPipe {
	chunk := int(min(size, pipeChunk))
	r, w := io.Pipe()
	done := make(chan jsonResult, 1)
	go func() {
		parts, err := b.readJSON(bufio.NewReaderSize(r, chunk), size)
		// A write that the reader no longer wants returns at once.
		_ = r.CloseWithError(errNotValid)
		done <- jsonResult{parts, err}
	}()
	return &jsonPipe{w: w, buf: bufio.NewWriterSize(w, chunk), done: done}
}

// pipeChunk is the size of the buffers on each side of a jsonPipe.
const pipeChunk = 64 << 10

func (p *jsonPipe) Write(b []byte) (int, error) {
	if !p.closed {
		if _, err := p.buf.Write(b); err != nil {
			p.closed = true
		}
	}
	return len(b), nil
}

// wait ends the blob for the reader with err, or with io.EOF when err is
// nil, and returns what the reader made of it.
func (p *jsonPipe) wait(err error) (*jsonParts, error) {
	if !p.closed && err == nil {
		// A failed flush means that the reader has stopped reading.
		_ = p.buf.Flush()
	}
	p.w.CloseWithError(err)
	res := <-p.done
	return res.parts, res.err
}

// readJSON reads one JSON blob of size bytes from r as a manifest, a
// history record and a provenance at once. A blob that holds any of their
// fields counts against the limit on bytes of JSON, since what it keeps
// grows with its size.
func (b *jsonBlobReader) readJSON(r io.Reader, size int64) (*jsonParts, error) {
	j := newJSONStreamReader(r, jsontext.AllowInvalidUTF8(true))
	p := &jsonParts{}
	held := false
	names := []string{"buildDefinition", "runDetails", "config", "layers", "FrontendAttrs"}
	kinds := []*error{&p.provErr, &p.provErr, &p.manifestErr, &p.manifestErr, &p.attrsErr}
	var seen uint64
	err := j.entries(func(name []byte) error {
		field := slices.Index(names, string(name))
		if field < 0 {
			return j.skip()
		}
		held = true
		kind := kinds[field]
		if seen&(1<<field) != 0 {
			// A field that appears twice could give two readers different
			// values, so its kind is not valid.
			if *kind == nil {
				*kind = errNotValid
			}
			return j.skip()
		}
		seen |= 1 << field
		read := j.read()
		var err error
		switch field {
		case 0:
			err = b.readBuildDefinition(j, &p.prov)
		case 1:
			err = b.readRunDetails(j, &p.prov)
		case 2:
			p.manifest.Config, err = readDescriptor(j)
		case 3:
			err = b.readLayers(j, &p.manifest)
		default:
			err = b.readAttrs(j, p)
		}
		var le *LimitError
		var se *jsontext.SyntacticError
		if err == nil || errors.As(err, &le) || errors.As(err, &se) {
			return err
		}
		if *kind == nil {
			*kind = err
		}
		return j.skipRest(read)
	})
	if err == nil {
		err = j.end()
	}
	var le *LimitError
	switch {
	case errors.As(err, &le):
		return nil, err
	case err != nil:
		return nil, errNotValid
	case held:
		if err := b.hold(size); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// readLayers reads a manifest's layers, and checks their number against the
// limit before it keeps another.
func (b *jsonBlobReader) readLayers(j *jsonReader, m *manifest) error {
	return j.elements(func() error {
		if b.layers == b.lim.layers {
			return &LimitError{What: "manifest layers", Limit: int64(b.lim.layers)}
		}
		b.layers++
		layer, err := readDescriptor(j)
		m.Layers = append(m.Layers, layer)
		return err
	})
}

// readAttrs reads the frontend attributes of a history record, and checks
// their number against the limit before it keeps another.
func (b *jsonBlobReader) readAttrs(j *jsonReader, p *jsonParts) error {
	return j.entries(func(rawName []byte) error {
		if b.attrs == b.lim.attrs {
			return &LimitError{What: "frontend attributes", Limit: int64(b.lim.attrs)}
		}
		b.attrs++
		name := string(rawName)
		value, err := j.str()
		if err != nil {
			return err
		}
		if p.attrs == nil {
			p.attrs = map[string]string{}
		}
		p.attrs[name] = value
		return nil
	})
}

// readDescriptor reads an OCI descriptor. Of its annotations it keeps only
// the predicate type, so an object of many annotations costs nothing.
func readDescriptor(j *jsonReader) (descriptor, error) {
	var d descriptor
	err := j.fields([]string{"mediaType", "digest", "size", "annotations"}, func(field int) error {
		var err error
		switch field {
		case 0:
			d.MediaType, err = j.str()
		case 1:
			d.Digest, err = j.str()
		case 2:
			var size int
			size, err = j.integer()
			d.Size = int64(size)
		default:
			err = j.entries(func(name []byte) error {
				if string(name) != annotationPredicateType {
					return j.skip()
				}
				value, valueErr := j.str()
				d.Annotations = map[string]string{annotationPredicateType: value}
				return valueErr
			})
		}
		return err
	})
	return d, err
}

// errStopped is what stopWriter returns once its context is done.
// readArchive then reports the context's own error.
var errStopped = errors.New("reading stopped")

// stopWriter fails once done is closed. Teed into the reads of an archive,
// it stops them within one read of a cancelled context.
type stopWriter <-chan struct{}

func (s stopWriter) Write(p []byte) (int, error) {
	select {
	case <-s:
		return 0, errStopped
	default:
		return len(p), nil
	}
}

// drain reads r to its end, and checks ctx after each MiB.
func drain(ctx context.Context, r *bufio.Reader) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("reading build record: %w", err)
		}
		_, err := r.Discard(1 << 20)
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return archiveError(err)
		}
	}
}
