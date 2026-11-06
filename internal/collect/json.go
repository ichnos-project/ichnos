package collect

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// errNotValid reports JSON that does not have the shape Ichnos reads.
// Callers turn it, and every other jsonReader error, into
// ErrMalformedRecord, because jsontext errors can quote bytes of the record.
var errNotValid = errors.New("JSON does not have the expected shape")

// jsonReader reads the tokens of one JSON value and reports where each
// ends. Duplicate names pass through.
type jsonReader struct {
	dec *jsontext.Decoder
	// buf holds the value being read, so that reset needs no new buffer.
	buf     bytes.Buffer
	scratch []byte
}

// newJSONReader reads v in place: the decoder parses a bytes.Buffer without
// copying it, and nothing writes to the buffer.
func newJSONReader(v []byte, opts ...jsontext.Options) *jsonReader {
	opts = append([]jsontext.Options{jsontext.AllowDuplicateNames(true)}, opts...)
	j := &jsonReader{buf: *bytes.NewBuffer(v)}
	j.dec = jsontext.NewDecoder(&j.buf, opts...)
	return j
}

// reset starts reading v, as newJSONReader does, with the same decoder.
func (j *jsonReader) reset(v []byte) {
	j.buf = *bytes.NewBuffer(v)
	j.dec.Reset(&j.buf, jsontext.AllowDuplicateNames(true))
}

// readStringList reads s, a JSON list of strings in a string, as BuildKit
// encodes the paths and patterns of a source attribute
// (client/llb/source.go at v0.34.0), and calls each with every string in
// order. It returns a *LimitError that each returns, and errNotValid when s
// is not such a list or each returns any other error.
func readStringList(s string, each func(string) error) error {
	j := newJSONReader([]byte(s))
	err := j.elements(func() error {
		if j.peek() != jsontext.KindString {
			return errNotValid
		}
		v, err := j.str()
		if err != nil {
			return err
		}
		return each(v)
	})
	if err == nil {
		err = j.end()
	}
	var le *LimitError
	switch {
	case errors.As(err, &le):
		return err
	case err != nil:
		return errNotValid
	}
	return nil
}

// newJSONStreamReader reads from r as it goes, and holds only the value it
// is reading.
func newJSONStreamReader(r io.Reader, opts ...jsontext.Options) *jsonReader {
	opts = append([]jsontext.Options{jsontext.AllowDuplicateNames(true)}, opts...)
	return &jsonReader{dec: jsontext.NewDecoder(r, opts...)}
}

func (j *jsonReader) peek() jsontext.Kind { return j.dec.PeekKind() }

// offset is the position just after the last token read.
func (j *jsonReader) offset() int { return int(j.dec.InputOffset()) }

// next reads one token, such as the start or end of an object.
func (j *jsonReader) next() error {
	if _, err := j.dec.ReadToken(); err != nil {
		return fmt.Errorf("reading JSON: %w", err)
	}
	return nil
}

// readValue reads the next value. It stays valid only until the next read.
func (j *jsonReader) readValue() (jsontext.Value, error) {
	v, err := j.dec.ReadValue()
	if err != nil {
		return nil, fmt.Errorf("reading JSON: %w", err)
	}
	return v, nil
}

// readString reads the next string, and returns it both raw, valid only
// until the next read, and unescaped. Invalid UTF-8 is an error.
func (j *jsonReader) readString() (jsontext.Value, string, error) {
	raw, unquoted, err := j.readBytes()
	if err != nil {
		return nil, "", err
	}
	return raw, string(unquoted), nil
}

// readBytes reads the next string, and returns it both raw and unescaped,
// each valid only until the next read. Invalid UTF-8 is an error.
func (j *jsonReader) readBytes() (jsontext.Value, []byte, error) {
	raw, err := j.readValue()
	if err != nil {
		return nil, nil, err
	}
	unquoted, err := j.unquoteBytes(raw)
	return raw, unquoted, err
}

// unquote returns the text of the string raw. Invalid UTF-8 is an error.
func (j *jsonReader) unquote(raw jsontext.Value) (string, error) {
	b, err := j.unquoteBytes(raw)
	return string(b), err
}

// unquoteBytes returns the text of the string raw, in a buffer that stays
// valid only until the next read. Invalid UTF-8 is an error.
func (j *jsonReader) unquoteBytes(raw jsontext.Value) ([]byte, error) {
	if raw.Kind() != jsontext.KindString {
		return nil, errNotValid
	}
	// The decoder has already read the whole string, so a string it cannot
	// unquote, which holds invalid UTF-8, leaves it at a token boundary.
	var err error
	if j.scratch, err = jsontext.AppendUnquote(j.scratch[:0], raw); err != nil {
		return nil, errNotValid
	}
	return j.scratch, nil
}

func (j *jsonReader) skip() error {
	if err := j.dec.SkipValue(); err != nil {
		return fmt.Errorf("reading JSON: %w", err)
	}
	return nil
}

// read returns how many names and values of the outermost object have been
// read.
func (j *jsonReader) read() int64 {
	_, n := j.dec.StackIndex(1)
	return n
}

// skipRest skips what is left of a member of the outermost object after a
// reader of its value failed. read is what j.read returned after the
// member's name. The decoder stays at a token boundary after any error that
// is not a syntax error, so the rest can still be read.
func (j *jsonReader) skipRest(read int64) error {
	for j.dec.StackDepth() > 1 {
		if k := j.peek(); k == jsontext.KindEndObject || k == jsontext.KindEndArray {
			if err := j.next(); err != nil {
				return err
			}
			continue
		}
		if err := j.skip(); err != nil {
			return err
		}
	}
	if j.read() == read {
		return j.skip()
	}
	return nil
}

// end checks that nothing but white space follows the value.
func (j *jsonReader) end() error {
	if _, err := j.dec.ReadToken(); !errors.Is(err, io.EOF) {
		return errNotValid
	}
	return nil
}

// fields reads an object whose members Ichnos knows by name. It calls read
// with the index in names of each known member, which read must consume, and
// skips the others. A known member that appears twice is an error, so that
// every reader of the record sees the same value. A null counts as an empty
// object.
func (j *jsonReader) fields(names []string, read func(field int) error) error {
	var seen uint64
	return j.entries(func(name []byte) error {
		for i, n := range names {
			if n != string(name) {
				continue
			}
			if seen&(1<<i) != 0 {
				return errNotValid
			}
			seen |= 1 << i
			return read(i)
		}
		return j.skip()
	})
}

// entries reads an object used as a map, calling read with each member's
// name, which stays valid only until the next read; read must consume the
// member's value. A null counts as an empty object.
func (j *jsonReader) entries(read func(name []byte) error) error {
	switch j.peek() {
	case jsontext.KindNull:
		return j.skip()
	case jsontext.KindBeginObject:
	case jsontext.KindInvalid, jsontext.KindFalse, jsontext.KindTrue, jsontext.KindString, jsontext.KindNumber,
		jsontext.KindEndObject, jsontext.KindBeginArray, jsontext.KindEndArray:
		fallthrough
	default:
		return errNotValid
	}
	if err := j.next(); err != nil {
		return err
	}
	for j.peek() != jsontext.KindEndObject {
		_, name, err := j.readBytes()
		if err != nil {
			return err
		}
		if err := read(name); err != nil {
			return err
		}
	}
	return j.next()
}

// elements reads an array, calling read for each element, which read must
// consume. A null counts as an empty array.
func (j *jsonReader) elements(read func() error) error {
	switch j.peek() {
	case jsontext.KindNull:
		return j.skip()
	case jsontext.KindBeginArray:
	case jsontext.KindInvalid, jsontext.KindFalse, jsontext.KindTrue, jsontext.KindString, jsontext.KindNumber,
		jsontext.KindBeginObject, jsontext.KindEndObject, jsontext.KindEndArray:
		fallthrough
	default:
		return errNotValid
	}
	if err := j.next(); err != nil {
		return err
	}
	for j.peek() != jsontext.KindEndArray {
		if err := read(); err != nil {
			return err
		}
	}
	return j.next()
}

// str reads a string. A null counts as an empty string.
func (j *jsonReader) str() (string, error) {
	if j.peek() == jsontext.KindNull {
		return "", j.skip()
	}
	_, s, err := j.readString()
	return s, err
}

// integer reads a number that is a whole number in the range of an int. A
// null counts as zero.
func (j *jsonReader) integer() (int, error) {
	if j.peek() == jsontext.KindNull {
		return 0, j.skip()
	}
	raw, err := j.readValue()
	if err != nil {
		return 0, err
	}
	if raw.Kind() != jsontext.KindNumber {
		return 0, errNotValid
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil {
		return 0, errNotValid
	}
	return n, nil
}
