// Package collect reads BuildKit build records. A Record holds the values
// as BuildKit wrote them, which can include credentials, so only this
// package reads it, and it redacts them before any other step uses them.
package collect

import (
	"context"
	"fmt"
	"io"
)

// Collector reads one build record.
type Collector interface {
	// Name returns the collector's name, such as "bundle".
	Name() string
	// Collect reads the build record that src names. It returns
	// ErrNotRegularFile, ErrMalformedRecord, ErrUnsupportedRecord,
	// ErrMultipleBuilds or a *LimitError when the record cannot be used, an
	// error that wraps the file system's error, such as fs.ErrNotExist, when
	// the file cannot be opened, and an error that wraps ctx.Err() when ctx
	// ends first.
	Collect(ctx context.Context, src Source) (*Record, error)
}

// Source names a build record.
type Source struct {
	// Path is the path of the record file. A symbolic link may point to
	// another file in the same directory, but not outside it.
	Path string
}

// Record is one build record as BuildKit wrote it, before redaction. Its
// values can hold credentials, so it prints as a fixed placeholder.
type Record struct {
	*recordData
}

// recordData holds a Record's values behind a pointer, so that printing a
// struct that holds a Record in an unexported field, which fmt does by
// reflection without calling Format, shows only an address.
type recordData struct {
	attrs      map[string]string
	vertices   []vertex
	builds     []build
	provenance provenanceState
}

const recordPlaceholder = "collect.Record{...}"

// Format prints a fixed placeholder for every verb, so that no record value
// reaches a log or an error message.
func (Record) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, recordPlaceholder)
}
