package snapshot

import (
	"encoding"
	"encoding/json"
	"errors"
	"testing"
)

func TestEnumsUnmarshalText(t *testing.T) {
	tests := []struct {
		name  string
		new   func() encoding.TextUnmarshaler
		known []string
	}{
		{"ProvenanceState", func() encoding.TextUnmarshaler { return new(ProvenanceState) }, []string{"read", "missing", "unsupported", "no-steps"}},
		{"OpType", func() encoding.TextUnmarshaler { return new(OpType) }, []string{"exec", "file", "source", "build", "merge", "diff", "none", "unknown"}},
		{"VertexKind", func() encoding.TextUnmarshaler { return new(VertexKind) }, []string{"step", "internal", "cache-import", "cache-export"}},
		{"CacheType", func() encoding.TextUnmarshaler { return new(CacheType) }, []string{"local", "registry", "gha", "s3", "azblob", "unknown"}},
		{"SourceKind", func() encoding.TextUnmarshaler { return new(SourceKind) }, []string{"local", "git", "http", "image", "other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, v := range tt.known {
				u := tt.new()
				if err := u.UnmarshalText([]byte(v)); err != nil {
					t.Errorf("UnmarshalText(%q) error = %v", v, err)
				}
			}
			for _, v := range []string{"", "EXEC", "Read", "cache_import", "exec ", "x"} {
				if err := tt.new().UnmarshalText([]byte(v)); !errors.Is(err, ErrUnknownValue) {
					t.Errorf("UnmarshalText(%q) error = %v, want ErrUnknownValue", v, err)
				}
			}
		})
	}
	var op OpType
	if err := op.UnmarshalText([]byte("exec")); err != nil || op != OpExec {
		t.Errorf("UnmarshalText(exec) = %q, %v, want %q", op, err, OpExec)
	}
}

func TestSnapshotDecodingRejectsUnknownValues(t *testing.T) {
	for name, doc := range map[string]string{
		"provenance": `{"provenance":"maybe"}`,
		"op":         `{"platforms":[{"steps":[{"op":"teleport"}]}]}`,
		"vertex":     `{"vertices":[{"kind":"step-ish"}]}`,
		"cache type": `{"cache":{"imports":[{"type":"ftp"}]}}`,
		"source":     `{"platforms":[{"steps":[{"op":"source","sourceKind":"ftp"}]}]}`,
	} {
		var s Snapshot
		if err := json.Unmarshal([]byte(doc), &s); !errors.Is(err, ErrUnknownValue) {
			t.Errorf("Unmarshal() of an unknown %s error = %v, want ErrUnknownValue", name, err)
		}
	}
	var s Snapshot
	if err := json.Unmarshal([]byte(`{"provenance":"read","platforms":[{"steps":[{"op":"exec"},{"op":"source","sourceKind":"image"}]}],"vertices":[{"kind":"cache-import"}],"cache":{"imports":[{"type":"gha"}]}}`), &s); err != nil {
		t.Errorf("Unmarshal() of known values error = %v", err)
	}
}

// TestIndexesAreAlwaysWritten checks that a step's vertex and environment
// are written even when they are 0, so that a reader can tell them from
// absent ones.
func TestIndexesAreAlwaysWritten(t *testing.T) {
	for _, st := range []Step{{}, {Vertex: -1, Environment: -1}} {
		data, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(data, &v); err != nil {
			t.Fatal(err)
		}
		if _, ok := v["vertex"]; !ok {
			t.Errorf("Marshal(%+v) = %s, which has no vertex", st, data)
		}
		if _, ok := v["environment"]; !ok {
			t.Errorf("Marshal(%+v) = %s, which has no environment", st, data)
		}
	}
}
