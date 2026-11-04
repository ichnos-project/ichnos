package collect

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// list returns a JSON array of n copies of elem.
func list(n int, elem string) string {
	if n == 0 {
		return "[]"
	}
	return "[" + strings.Repeat(elem+",", n-1) + elem + "]"
}

// object returns a JSON object of n members with distinct names and the
// value value.
func object(n int, value string) string {
	var b strings.Builder
	b.WriteString("{")
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%d":%s`, i, value)
	}
	b.WriteString("}")
	return b.String()
}

func inConfig(config string) string {
	return `{"buildDefinition":{"internalParameters":{"buildConfig":` + config + `}}}`
}

func inSource(source string) string {
	return `{"runDetails":{"metadata":{"buildkit_metadata":{"source":` + source + `}}}}`
}

// listLimit is a list that a record can hold, with the limit that caps it.
// input returns a record part with n elements of the list, and read reads
// it.
type listLimit struct {
	what  string
	set   func(*limits, int)
	input func(n int) []byte
	read  func(lim limits, data []byte) error
}

// readProvenance reads data as one provenance blob, as decodeRecord does.
func readProvenance(lim limits, data []byte) (rawProvenance, error) {
	p, err := readPart(lim, data, "provenance is not valid", func(p *jsonParts) error { return p.provErr })
	if err != nil {
		return rawProvenance{}, err
	}
	return p.prov, nil
}

// readPart reads data as one JSON blob, and returns the error that the
// part of one kind gave, as decodeRecord does.
func readPart(lim limits, data []byte, what string, partErr func(*jsonParts) error) (*jsonParts, error) {
	p, err := readJSON(lim, data)
	var le *LimitError
	switch {
	case errors.As(err, &le):
		return nil, err
	case err != nil:
		return nil, malformed(what)
	case partErr(p) != nil:
		return nil, partError(partErr(p), what)
	}
	return p, nil
}

func listLimits() []listLimit {
	readProv := func(lim limits, data []byte) error {
		_, err := readProvenance(lim, data)
		return err
	}
	readHistory := func(lim limits, data []byte) error {
		_, err := readPart(lim, data, "history record is not valid", func(p *jsonParts) error { return p.attrsErr })
		return err
	}
	readManifest := func(lim limits, data []byte) error {
		_, err := readPart(lim, data, "manifest is not valid", func(p *jsonParts) error { return p.manifestErr })
		return err
	}
	return []listLimit{
		{"steps", func(l *limits, n int) { l.steps = n }, func(n int) []byte {
			return []byte(inConfig(`{"llbDefinition":` + list(n, `{"id":"s"}`) + `}`))
		}, readProv},
		{"steps", func(l *limits, n int) { l.steps = n }, func(n int) []byte {
			return []byte(inConfig(`{"digestMapping":` + object(n, `"s"`) + `}`))
		}, readProv},
		{"step inputs", func(l *limits, n int) { l.inputs = n }, func(n int) []byte {
			return []byte(inConfig(`{"llbDefinition":[{"inputs":` + list(n, `""`) + `}]}`))
		}, readProv},
		{"source ranges", func(l *limits, n int) { l.ranges = n }, func(n int) []byte {
			return []byte(inSource(`{"locations":{"s":{"locations":[{"ranges":` + list(n, `{}`) + `}]}}}`))
		}, readProv},
		{"source files", func(l *limits, n int) { l.sources = n }, func(n int) []byte {
			return []byte(inSource(`{"infos":` + list(n, `{}`) + `}`))
		}, readProv},
		{"frontend attributes", func(l *limits, n int) { l.attrs = n }, func(n int) []byte {
			return []byte(`{"FrontendAttrs":` + object(n, `""`) + `}`)
		}, readHistory},
		{"manifest layers", func(l *limits, n int) { l.layers = n }, func(n int) []byte {
			return []byte(`{"layers":` + list(n, `{}`) + `}`)
		}, readManifest},
	}
}

func TestListLimits(t *testing.T) {
	for _, tt := range listLimits() {
		t.Run(tt.what, func(t *testing.T) {
			for _, n := range []int{100, 101} {
				lim := testLimits()
				tt.set(&lim, 100)
				err := tt.read(lim, tt.input(n))
				var le *LimitError
				if got, want := errors.As(err, &le) && le.What == tt.what, n > 100; got != want {
					t.Errorf("reading %d %s with a limit of 100 error = %v, want LimitError %v", n, tt.what, err, want)
				}
				if n == 100 && err != nil {
					t.Errorf("reading 100 %s with a limit of 100 error = %v", tt.what, err)
				}
			}
		})
	}
}

// TestListLimitsBeforeKeeping checks that each limit applies before an
// element is kept, so that a list far over its limit costs memory only up
// to the limit.
func TestListLimitsBeforeKeeping(t *testing.T) {
	for _, tt := range listLimits() {
		t.Run(tt.what, func(t *testing.T) {
			lim := testLimits()
			tt.set(&lim, 1000)
			data := tt.input(1_000_000)
			grew := allocated(func() {
				var le *LimitError
				if err := tt.read(lim, data); !errors.As(err, &le) {
					t.Errorf("reading 1,000,000 %s with a limit of 1,000 error = %v, want LimitError", tt.what, err)
				}
			})
			if grew > 1<<20 {
				t.Errorf("reading 1,000,000 %s with a limit of 1,000 allocated %d bytes, want at most 1 MiB", tt.what, grew)
			}
		})
	}
	t.Run("cache imports", func(t *testing.T) {
		attrs := map[string]string{"cache-imports": list(1_000_000, `{}`)}
		grew := allocated(func() {
			var le *LimitError
			if _, err := cacheImports(attrs); !errors.As(err, &le) || le.What != "cache imports" {
				t.Errorf("cacheImports() of 1,000,000 imports error = %v, want LimitError", err)
			}
		})
		if grew > 1<<20 {
			t.Errorf("cacheImports() of 1,000,000 imports allocated %d bytes, want at most 1 MiB", grew)
		}
	})
}

func TestReadProvenanceSkipsUnusedFields(t *testing.T) {
	prov := []byte(`{"buildDefinition":{"externalParameters":` + list(1_000_000, `{}`) + `},"predicate":` + object(100_000, `[]`) + `}`)
	grew := allocated(func() {
		if _, err := readProvenance(testLimits(), prov); err != nil {
			t.Errorf("readProvenance() error = %v", err)
		}
	})
	if grew > 1<<20 {
		t.Errorf("read() of %d bytes of unused fields allocated %d bytes, want at most 1 MiB", len(prov), grew)
	}
}

func TestSourceLinesIndexesOnlyNeededLines(t *testing.T) {
	data := []byte("RUN true\n" + strings.Repeat("\n", 4_000_000))
	grew := allocated(func() {
		l := newSourceLines(data, []int{1, 2})
		if l.count() != 4_000_001 || l.text(lineRange{1, 1}) != "RUN true" {
			t.Errorf("count, text = %d, %q, want 4000001, %q", l.count(), l.text(lineRange{1, 1}), "RUN true")
		}
	})
	if grew > 2*uint64(len(data)) {
		t.Errorf("newSourceLines() of %d bytes allocated %d bytes, want at most twice the data", len(data), grew)
	}
}

func TestReadProvenanceShape(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prov    string
		wantErr bool
	}{
		{"null", `null`, false},
		{"empty", `{}`, false},
		{"not an object", `[]`, true},
		{"data after the value", `{} {}`, true},
		{"known field twice", `{"buildDefinition":{},"buildDefinition":{}}`, true},
		{"unknown field twice", `{"x":1,"x":2}`, false},
		{"wrong type for a list", inConfig(`{"llbDefinition":{}}`), true},
		{"wrong type for a string", `{"buildDefinition":{"internalParameters":{"targetPlatform":1}}}`, true},
		{"fraction for a line", inSource(`{"locations":{"s":{"locations":[{"ranges":[{"start":{"line":1.5}}]}]}}}`), true},
		{"invalid UTF-8 in a kept string", "{\"buildDefinition\":{\"internalParameters\":{\"targetPlatform\":\"\xff\"}}}", true},
		{"invalid UTF-8 in a skipped field", "{\"x\":\"\xff\"}", false},
		{"source that is not base64", inSource(`{"infos":[{"data":"!"}]}`), true},
		{"one digest twice", inConfig(`{"digestMapping":{"d":"a","d":"b"}}`), true},
		{"null fields", inConfig(`{"llbDefinition":null,"digestMapping":null}`), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readProvenance(testLimits(), []byte(tt.prov))
			if (err != nil) != tt.wantErr || (err != nil && !errors.Is(err, ErrMalformedRecord)) {
				t.Errorf("read(%s) error = %v, want an ErrMalformedRecord %v", tt.prov, err, tt.wantErr)
			}
		})
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("FROM scratch\n"))
	p, err := readProvenance(testLimits(), []byte(inSource(`{"infos":[{"data":"`+encoded+`"}]}`)))
	if err != nil || len(p.sources) != 1 || string(p.sources[0]) != "FROM scratch\n" {
		t.Errorf("read() sources = %q, %v, want one Dockerfile", p.sources, err)
	}
}

// allocated returns the bytes that f allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestManifestReadsAsStream(t *testing.T) {
	lim := testLimits()
	l := &layout{index: []byte(`{"manifests":` + list(1_000_000, `{}`) + `}`)}
	grew := allocated(func() {
		if _, err := l.manifest(); !errors.Is(err, ErrMultipleBuilds) {
			t.Errorf("manifest() of an index with many manifests error = %v, want ErrMultipleBuilds", err)
		}
	})
	if grew > 1<<20 {
		t.Errorf("manifest() of an index with many manifests allocated %d bytes, want at most 1 MiB", grew)
	}
	config := []byte(`{"config":{"annotations":` + object(100_000, `""`) + `}}`)
	grew = allocated(func() {
		if _, err := readPart(lim, config, "manifest is not valid", func(p *jsonParts) error { return p.manifestErr }); err != nil {
			t.Errorf("readJSON() of a config with many annotations error = %v", err)
		}
	})
	if grew > 1<<20 {
		t.Errorf("readJSON() of a config with many annotations allocated %d bytes, want at most 1 MiB", grew)
	}
}

func TestRedactionMemoryOnLongStrings(t *testing.T) {
	long := "https://h/?" + strings.Repeat("abc=&", 1_000_000) + "a=b"
	grew := allocated(func() {
		if got, hit := redactWith(markURLs, long); !hit || len(got) != len(long)+2 {
			t.Errorf("markURLs() hit = %v, len %d", hit, len(got))
		}
	})
	if grew > 2*uint64(len(long)) {
		t.Errorf("markURLs() of a %d-byte query allocated %d bytes, want at most twice its size", len(long), grew)
	}
	k := knownSetOf("abcd")
	text := strings.Repeat("abcdX", 1_000_000)
	grew = allocated(func() {
		if got, hit := redactWith(k.mark, text); !hit || got != strings.Repeat("***X", 1_000_000) {
			t.Errorf("mark() hit = %v, got %d bytes", hit, len(got))
		}
	})
	if grew > 2*uint64(len(text)) {
		t.Errorf("mark() of %d bytes with a match every 5 bytes allocated %d bytes, want at most twice its size", len(text), grew)
	}
}

func TestNewScrubberCountsValuesAsItFindsThem(t *testing.T) {
	recordOf := func(env string) *Record {
		op := jsontext.Value(`{"Op":{"exec":{"meta":{"env":` + env + `}}}}`)
		return &Record{&recordData{builds: []build{{steps: []step{{op: op}}}}}}
	}
	repeated := recordOf(list(1_000_000, `"TOKEN=abcd"`))
	grew := allocated(func() {
		s, err := newScrubber(repeated)
		if err != nil || !s.known.contains("abcd") {
			t.Errorf("newScrubber() = %v, %v, want the one value", s, err)
		}
	})
	if grew > 64<<20 {
		t.Errorf("newScrubber() of one value repeated 1,000,000 times allocated %d bytes, want at most 64 MiB", grew)
	}
	var b strings.Builder
	b.WriteString("[")
	for i := range 1_000_000 {
		fmt.Fprintf(&b, `"TOKEN=v%07d",`, i)
	}
	b.WriteString(`"A=b"]`)
	distinct := recordOf(b.String())
	grew = allocated(func() {
		if _, err := newScrubber(distinct); !isLimit("bytes of credential values")(err) {
			t.Errorf("newScrubber() of 8 MB of distinct values error = %v, want LimitError", err)
		}
	})
	if grew > 64<<20 {
		t.Errorf("newScrubber() of 8 MB of distinct values allocated %d bytes, want at most 64 MiB", grew)
	}
}

func TestReadKeepsStepDefinitionsApart(t *testing.T) {
	data := []byte(inConfig(`{"llbDefinition":[{"id":"a","op":{"x":1}},{"id":"b","op":{"y":2}}]}`))
	p, err := readProvenance(testLimits(), data)
	if err != nil {
		t.Fatal(err)
	}
	clear(data)
	if string(p.steps[0].op) != `{"x":1}` || string(p.steps[1].op) != `{"y":2}` {
		t.Errorf("step definitions after the provenance changed = %s, %s, want copies", p.steps[0].op, p.steps[1].op)
	}
	if cap(p.steps[0].op) > 16 {
		t.Errorf("step definition has capacity %d, so it may hold the rest of the provenance", cap(p.steps[0].op))
	}
}
