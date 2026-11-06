package collect

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json/jsontext"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestReducedRecord checks a record that lab/gha/reduce-record.sh wrote: the
// collector reads it exactly as it reads the record it was reduced from, and
// its archive names no user or group. lab/gha/reduce-record-test.sh runs it
// with both paths set, and reads the number of builds from its log.
func TestReducedRecord(t *testing.T) {
	full, reduced := os.Getenv("ICHNOS_FULL_RECORD"), os.Getenv("ICHNOS_REDUCED_RECORD")
	if full == "" || reduced == "" {
		t.Skip("lab/gha/reduce-record-test.sh runs this test with ICHNOS_FULL_RECORD and ICHNOS_REDUCED_RECORD")
	}
	want := collectCanonical(t, full)
	got := collectCanonical(t, reduced)
	if len(want.vertices) == 0 {
		t.Fatalf("Collect(%q) has no vertices, want a record to compare", full)
	}
	if !reflect.DeepEqual(got.recordData, want.recordData) {
		t.Errorf("Collect(%q) redacted = %s, want %s", reduced, dumpRedacted(mustRedact(t, got)), dumpRedacted(mustRedact(t, want)))
	}
	t.Logf("%s has %d builds", full, len(want.builds))

	f, err := os.OpenInRoot(filepath.Dir(reduced), filepath.Base(reduced))
	if err != nil {
		t.Fatalf("opening %s: %v", reduced, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("reading %s: %v", reduced, err)
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading %s: %v", reduced, err)
		}
		if hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("%s: entry %s has user %q and group %q, want neither", reduced, hdr.Name, hdr.Uname, hdr.Gname)
		}
	}
}

// collectCanonical collects the record at path, with each step definition
// in canonical form, because the reduction may write a provenance again in
// another JSON format.
func collectCanonical(t *testing.T, path string) *Record {
	t.Helper()
	rec, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
	if err != nil {
		t.Fatalf("Collect(%q) error = %v", path, err)
	}
	for i := range rec.builds {
		for j := range rec.builds[i].steps {
			op := &rec.builds[i].steps[j].op
			if *op == nil {
				continue
			}
			if err := op.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
				t.Fatalf("Collect(%q): canonicalizing a step definition: %v", path, err)
			}
		}
	}
	return rec
}
