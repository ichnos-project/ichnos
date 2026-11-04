package collect

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestRecordFormatPrintsPlaceholder(t *testing.T) {
	rec := &Record{&recordData{
		attrs:    map[string]string{"build-arg:MODE": "ichnos-fake-format-value"},
		vertices: []vertex{{digest: "sha256:ichnos-fake-format-digest"}},
		builds: []build{{
			platform: "linux/amd64",
			steps: []step{{
				id:   "step0",
				op:   jsontext.Value(`{"Op":"ichnos-fake-format-value"}`),
				text: "RUN echo ichnos-fake-format-value",
			}},
		}},
	}}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		for _, arg := range []any{rec, *rec} {
			got := fmt.Sprintf(verb, arg)
			if got != recordPlaceholder {
				t.Errorf("Sprintf(%q, %T) = %q, want %q", verb, arg, got, recordPlaceholder)
			}
		}
	}
	for name, newHandler := range map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		slog.New(newHandler(&buf)).Info("collected", "record", rec, "value", *rec, "holder", holder{rec: *rec})
		if bytes.Contains(buf.Bytes(), []byte("ichnos-fake-format")) {
			t.Errorf("%s handler logged a record value: %s", name, buf.String())
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if got := fmt.Sprintf(verb, holder{rec: *rec}); strings.Contains(got, "ichnos-fake-format") {
			t.Errorf("Sprintf(%q, holder) = %q, which holds a record value", verb, got)
		}
	}
}

// holder keeps a Record in an unexported field, which fmt prints by
// reflection.
type holder struct {
	rec Record
}

func TestLimitError(t *testing.T) {
	var err error = &LimitError{What: "archive entries", Limit: 1024}
	if got, want := err.Error(), "build record exceeds the limit of 1024 archive entries"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	var le *LimitError
	if !errors.As(fmt.Errorf("reading: %w", err), &le) || le.Limit != 1024 {
		t.Errorf("errors.As(wrapped LimitError) = %v, want the LimitError", le)
	}
}

func TestNewBundleCollector(t *testing.T) {
	c := NewBundleCollector()
	if got := c.Name(); got != "bundle" {
		t.Errorf("Name() = %q, want %q", got, "bundle")
	}
}
