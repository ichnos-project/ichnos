package collect

import (
	"errors"
	"fmt"
)

var (
	// ErrNotRegularFile means the path names a directory, a pipe, a device
	// or another file that is not a regular file.
	ErrNotRegularFile = errors.New("build record is not a regular file")

	// ErrMalformedRecord means the file is damaged, or is not a build record
	// that BuildKit wrote.
	ErrMalformedRecord = errors.New("malformed build record")

	// ErrUnsupportedRecord means the build record uses a format or a version
	// of a format that Ichnos does not read.
	ErrUnsupportedRecord = errors.New("unsupported build record format")

	// ErrMultipleBuilds means the file holds more than one build record.
	ErrMultipleBuilds = errors.New("file holds more than one build record; export one build")
)

// LimitError reports a build record that exceeds one of the collector's
// limits, such as its size or its number of steps.
type LimitError struct {
	// What names the unit of the limit, such as "archive entries".
	What string
	// Limit is the largest accepted value.
	Limit int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("build record exceeds the limit of %d %s", e.Limit, e.What)
}

// malformed returns an ErrMalformedRecord with a fixed description. The
// description must never hold bytes from the record.
func malformed(what string) error {
	return fmt.Errorf("%w: %s", ErrMalformedRecord, what)
}
