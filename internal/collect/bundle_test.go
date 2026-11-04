package collect

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpenRegular(t *testing.T) {
	dirPath := t.TempDir()
	outside := t.TempDir()
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dirPath, "record.dockerbuild"), []byte("data"))
	write(filepath.Join(outside, "record.dockerbuild"), []byte("data"))
	if err := os.Symlink("record.dockerbuild", filepath.Join(dirPath, "inside.dockerbuild")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "record.dockerbuild"), filepath.Join(dirPath, "outside.dockerbuild")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", filepath.Base(outside), "record.dockerbuild"), filepath.Join(dirPath, "relative.dockerbuild")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr func(error) bool
	}{
		{"regular file", filepath.Join(dirPath, "record.dockerbuild"), nil},
		{"symlink inside the folder", filepath.Join(dirPath, "inside.dockerbuild"), nil},
		{"missing", filepath.Join(dirPath, "missing.dockerbuild"), func(err error) bool { return errors.Is(err, fs.ErrNotExist) }},
		{"directory", dirPath, func(err error) bool { return errors.Is(err, ErrNotRegularFile) }},
		{"symlink leaving the folder", filepath.Join(dirPath, "outside.dockerbuild"), func(err error) bool { return err != nil && !errors.Is(err, ErrNotRegularFile) }},
		{"relative symlink leaving the folder", filepath.Join(dirPath, "relative.dockerbuild"), func(err error) bool { return err != nil && !errors.Is(err, ErrNotRegularFile) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := openRegular(tt.path, testLimits())
			if f != nil {
				defer closeFile(t, f)
			}
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("openRegular(%q) error = %v, want nil", tt.path, err)
				}
				return
			}
			if !tt.wantErr(err) {
				t.Fatalf("openRegular(%q) error = %v, want a different error", tt.path, err)
			}
			if f != nil {
				t.Errorf("openRegular(%q) returned a file with error %v", tt.path, err)
			}
		})
	}
}

func closeFile(tb testing.TB, f *os.File) {
	tb.Helper()
	if err := f.Close(); err != nil {
		tb.Errorf("Close() error = %v", err)
	}
}

func TestOpenRegularCompressedLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.dockerbuild")
	if err := os.WriteFile(path, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		limit   int64
		wantErr bool
	}{{100, false}, {99, true}} {
		lim := testLimits()
		lim.compressedBytes = tt.limit
		f, err := openRegular(path, lim)
		if f != nil {
			closeFile(t, f)
		}
		var le *LimitError
		if got := errors.As(err, &le); got != tt.wantErr {
			t.Errorf("openRegular() with a limit of %d error = %v, want LimitError %v", tt.limit, err, tt.wantErr)
		}
	}
}

func TestCollectFileErrors(t *testing.T) {
	dirPath := t.TempDir()
	empty := filepath.Join(dirPath, "empty.dockerbuild")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name    string
		path    string
		wantErr func(error) bool
	}{
		{"missing", filepath.Join(dirPath, "missing.dockerbuild"), func(err error) bool { return errors.Is(err, fs.ErrNotExist) }},
		{"directory", dirPath, func(err error) bool { return errors.Is(err, ErrNotRegularFile) }},
		{"empty", empty, isMalformed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := NewBundleCollector().Collect(t.Context(), Source{Path: tt.path})
			if !tt.wantErr(err) || rec != nil {
				t.Fatalf("Collect(%q) = %v, %v, want a different error", tt.path, rec, err)
			}
		})
	}
}

func TestCollectCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := NewBundleCollector().Collect(ctx, Source{Path: fixturePaths(t)[0]})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Collect() error = %v, want context.Canceled", err)
	}
}

func TestReadArchiveGzip(t *testing.T) {
	plain := tarBytes(t, regular("index.json", []byte("{}")), regular("oci-layout", []byte("{}")))
	valid := gzipBytes(t, plain)

	badCRC := bytes.Clone(valid)
	badCRC[len(badCRC)-8] ^= 0xff

	half := len(plain) / 2
	concatenated := append(gzipBytes(t, plain[:half]), gzipBytes(t, plain[half:])...)

	tests := []struct {
		name    string
		data    []byte
		lim     func(*limits)
		wantErr func(error) bool
	}{
		{"valid", valid, nil, nil},
		{"concatenated members", concatenated, nil, nil},
		{"empty", nil, nil, isMalformed},
		{"not gzip", []byte("this is not gzip data"), nil, isMalformed},
		{"truncated", valid[:len(valid)/2], nil, isMalformed},
		{"bad CRC", badCRC, nil, isMalformed},
		{"trailing garbage", append(bytes.Clone(valid), "garbage"...), nil, isMalformed},
		{"compressed at the limit", valid, func(l *limits) { l.compressedBytes = int64(len(valid)) }, nil},
		{"compressed one over the limit", valid, func(l *limits) { l.compressedBytes = int64(len(valid)) - 1 }, isLimit("compressed bytes")},
		{"decompressed at the limit", valid, func(l *limits) { l.decompressedBytes = int64(len(plain)) }, nil},
		{"decompressed one over the limit", valid, func(l *limits) { l.decompressedBytes = int64(len(plain)) - 1 }, isLimit("decompressed bytes")},
		{"concatenated members share the budget", concatenated, func(l *limits) { l.decompressedBytes = int64(len(plain)) - 1 }, isLimit("decompressed bytes")},
		{"decompression bomb", gzipBytes(t, append(bytes.Clone(plain), make([]byte, 1<<20)...)), func(l *limits) { l.decompressedBytes = 64 << 10 }, isLimit("decompressed bytes")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lim := testLimits()
			if tt.lim != nil {
				tt.lim(&lim)
			}
			a, err := readArchive(t.Context(), bytes.NewReader(tt.data), lim)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("readArchive() error = %v, want nil", err)
				}
				if string(a.index) != "{}" {
					t.Errorf("readArchive() index = %q, want %q", a.index, "{}")
				}
				return
			}
			if !tt.wantErr(err) {
				t.Fatalf("readArchive() error = %v, want a different error", err)
			}
		})
	}
}

func TestReadTarEntries(t *testing.T) {
	blob := "blobs/sha256/" + hex64('a')
	oldSparse := append(rawHeader("index.json", tar.TypeGNUSparse, 0), tarEnd()...)
	paxSparse := append(append(paxHeader(
		"GNU.sparse.major", "0", "GNU.sparse.minor", "1",
		"GNU.sparse.name", "index.json", "GNU.sparse.size", "2",
		"GNU.sparse.numblocks", "1", "GNU.sparse.map", "0,2"),
		rawHeader("index.json", tar.TypeReg, 2)...), pad("{}")...)
	paxSparse = append(paxSparse, tarEnd()...)

	tests := []struct {
		name    string
		data    []byte
		wantErr func(error) bool
	}{
		{"layout", tarBytes(t, dir("blobs/"), dir("blobs/sha256/"), regular(blob, []byte("x")), regular("index.json", []byte("{}")), regular("oci-layout", []byte("{}"))), nil},
		{"index first", tarBytes(t, regular("index.json", []byte("{}")), regular(blob, []byte("x"))), nil},
		{"symlink", tarBytes(t, tarEntry{name: "index.json", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}), isMalformed},
		{"hardlink", tarBytes(t, regular(blob, []byte("x")), tarEntry{name: "index.json", typeflag: tar.TypeLink, linkname: blob}), isMalformed},
		{"character device", tarBytes(t, tarEntry{name: "index.json", typeflag: tar.TypeChar}), isMalformed},
		{"block device", tarBytes(t, tarEntry{name: "index.json", typeflag: tar.TypeBlock}), isMalformed},
		{"FIFO", tarBytes(t, tarEntry{name: "index.json", typeflag: tar.TypeFifo}), isMalformed},
		{"global PAX header", tarBytes(t, tarEntry{name: "global", typeflag: tar.TypeXGlobalHeader}), isMalformed},
		{"old GNU sparse file", oldSparse, isMalformed},
		{"PAX sparse file", paxSparse, isMalformed},
		{"duplicate file", tarBytes(t, regular("index.json", []byte("{}")), regular("index.json", []byte("{}"))), isMalformed},
		{"duplicate directory", tarBytes(t, dir("blobs/"), dir("blobs/")), isMalformed},
		{"not tar", []byte("this is not a tar archive at all, and it is long enough to fill a header block"), isMalformed},
		{"truncated", tarBytes(t, regular("index.json", bytes.Repeat([]byte("x"), 2000)))[:1024], isMalformed},
		{"truncated inside a blob", tarBytes(t, regular(blob, bytes.Repeat([]byte("x"), 2000)))[:1024], isArchiveDamage},
		{"truncated inside a status stream", tarBytes(t, regular(blob, concat(vertices(update{digest: "sha256:a"}), frame(pbBytes(nil, 3, make([]byte, 2000))))))[:1024], isArchiveDamage},
	}
	for _, name := range []string{
		"./index.json", "/index.json", "other.txt", "blobs/sha512/" + hex64('a'),
		"blobs/sha256/" + hex64('a')[:63], "blobs/sha256/" + hex64('a') + "a", "blobs/sha256/" + hex64('A'),
		"blobs/sha256/" + hex64('g'), "blobs/sha256/../index.json",
	} {
		tests = append(tests, struct {
			name    string
			data    []byte
			wantErr func(error) bool
		}{"name " + name, tarBytes(t, regular(name, []byte("x"))), isMalformed})
	}
	for _, d := range []string{"blobs", "index.json", "blobs/sha256", "other/"} {
		tests = append(tests, struct {
			name    string
			data    []byte
			wantErr func(error) bool
		}{"directory " + d, tarBytes(t, dir(d)), isMalformed})
	}
	for _, f := range []string{"blobs/", "blobs/sha256/", "index.json/", "blobs/sha256/" + hex64('a') + "/"} {
		data := append(rawHeader(f, tar.TypeReg, 0), tarEnd()...)
		tests = append(tests, struct {
			name    string
			data    []byte
			wantErr func(error) bool
		}{"file " + f, data, isMalformed})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readTar(t.Context(), bytes.NewReader(tt.data), testLimits())
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("readTar() error = %v, want nil", err)
				}
				return
			}
			if !tt.wantErr(err) {
				t.Fatalf("readTar() error = %v, want a different error", err)
			}
		})
	}
}

func TestReadTarContents(t *testing.T) {
	l, err := readTar(t.Context(), bytes.NewReader(tarBytes(t,
		regular("blobs/sha256/"+hex64('b'), []byte("blob")),
		regular("oci-layout", []byte("layout")),
		regular("index.json", []byte("index")),
	)), testLimits())
	if err != nil {
		t.Fatalf("readTar() error = %v", err)
	}
	if string(l.index) != "index" {
		t.Errorf("index = %q, want %q", l.index, "index")
	}
	if b := l.blobs[hex64('b')]; b == nil || b.size != 4 || len(l.blobs) != 1 {
		t.Errorf("blobs = %v, want one blob of 4 bytes", l.blobs)
	}
}

func TestReadTarLimits(t *testing.T) {
	entries := []tarEntry{dir("blobs/"), dir("blobs/sha256/"), regular("index.json", []byte("{}"))}
	data := tarBytes(t, entries...)
	for _, tt := range []struct {
		name    string
		lim     func(*limits)
		wantErr func(error) bool
	}{
		{"entries at the limit", func(l *limits) { l.entries = len(entries) }, nil},
		{"entries one over the limit", func(l *limits) { l.entries = len(entries) - 1 }, isLimit("archive entries")},
		{"contents at the limit", func(l *limits) { l.decompressedBytes = 2 }, nil},
		{"contents one over the limit", func(l *limits) { l.decompressedBytes = 1 }, isLimit("decompressed bytes")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lim := testLimits()
			tt.lim(&lim)
			_, err := readTar(t.Context(), bytes.NewReader(data), lim)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("readTar() error = %v, want nil", err)
				}
				return
			}
			if !tt.wantErr(err) {
				t.Fatalf("readTar() error = %v, want a different error", err)
			}
		})
	}
}

func TestReadTarDeclaredSize(t *testing.T) {
	for _, tt := range []struct {
		name    string
		blob    bool
		size    int64
		wantErr func(error) bool
	}{
		{"larger than the budget", false, 1 << 40, isLimit("decompressed bytes")},
		{"larger than the data", false, 200 << 20, isMalformed},
		{"blob larger than the data", true, 200 << 20, isMalformed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := append(rawHeader("index.json", tar.TypeReg, tt.size), pad("{}")...)
			if tt.blob {
				data = append(rawHeader("blobs/sha256/"+hex64('a'), tar.TypeReg, tt.size), bytes.Repeat([]byte(" "), 4096)...)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := readTar(t.Context(), bytes.NewReader(data), testLimits())
			runtime.ReadMemStats(&after)
			if !tt.wantErr(err) {
				t.Fatalf("readTar() error = %v, want a different error", err)
			}
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
				t.Errorf("readTar() allocated %d bytes for a declared size of %d", grew, tt.size)
			}
		})
	}
}

// cancelWriter cancels a context when it is written to. Behind an
// io.TeeReader, it cancels on the first read from an archive.
type cancelWriter context.CancelFunc

func (c cancelWriter) Write(p []byte) (int, error) {
	c()
	return len(p), nil
}

func TestReadTarCancelledMidRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	data := tarBytes(t, dir("blobs/"), dir("blobs/sha256/"), regular("index.json", []byte("{}")))
	_, err := readTar(ctx, io.TeeReader(bytes.NewReader(data), cancelWriter(cancel)), testLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readTar() error = %v, want context.Canceled", err)
	}
}

func FuzzReadArchive(f *testing.F) {
	for _, p := range fixturePaths(f) {
		f.Add(gunzip(f, readFile(f, p)))
	}
	f.Add(tarBytes(f, dir("blobs/"), regular("index.json", []byte("{}"))))
	lim := testLimits()
	lim.entries = 16
	lim.decompressedBytes = 1 << 20
	lim.jsonBytes = 64 << 10
	f.Fuzz(func(t *testing.T, data []byte) {
		l, err := readTar(t.Context(), bytes.NewReader(data), lim)
		if err != nil {
			if !isTyped(err) {
				t.Fatalf("readTar() error = %v, want a typed error", err)
			}
			return
		}
		if int64(len(l.index)) > lim.jsonBytes {
			t.Errorf("readTar() kept an index of %d bytes, want at most %d", len(l.index), lim.jsonBytes)
		}
		vertices := 0
		var total int64
		for hex, b := range l.blobs {
			if !isSHA256Hex(hex) {
				t.Errorf("readTar() kept blob %q, want only SHA-256 names", hex)
			}
			if b.json != nil && b.size > lim.jsonBytes {
				t.Errorf("readTar() read a JSON blob of %d bytes, want at most %d", b.size, lim.jsonBytes)
			}
			if (b.json == nil) == (b.jsonErr == nil) || (b.vertices != nil && b.statusErr != nil) {
				t.Errorf("readTar() blob %q has a result and an error of one kind, or neither for JSON", hex)
			}
			vertices += len(b.vertices)
			total += b.size
		}
		if total > lim.decompressedBytes {
			t.Errorf("readTar() read %d bytes, want at most %d", total, lim.decompressedBytes)
		}
		if vertices > lim.vertices {
			t.Errorf("readTar() kept %d vertices, want at most %d", vertices, lim.vertices)
		}
		if len(l.blobs) > lim.entries {
			t.Errorf("readTar() kept %d blobs, want at most %d", len(l.blobs), lim.entries)
		}
	})
}

func isMalformed(err error) bool { return errors.Is(err, ErrMalformedRecord) }

// isArchiveDamage reports an archive that cannot be read, as opposed to a
// blob that is not of the kind its descriptor says.
func isArchiveDamage(err error) bool {
	return errors.Is(err, ErrMalformedRecord) && strings.Contains(err.Error(), "reading archive")
}

func isLimit(what string) func(error) bool {
	return func(err error) bool {
		var le *LimitError
		return errors.As(err, &le) && le.What == what
	}
}

// BenchmarkCollect reads the lab records, which are the size of most
// records, and one whose trace is repeated to 32 MiB, as the trace of a
// build of a few thousand steps would be.
func BenchmarkCollect(b *testing.B) {
	large := loadBundle(b, fixturePath(b, "added-copy", "pr"))
	hex := large.layerHex("application/vnd.buildkit.otlp.json.v0")
	large.setBlob(hex, bytes.Repeat(large.blob(hex), 32<<20/len(large.blob(hex))))
	largePath := filepath.Join(b.TempDir(), "large-trace.dockerbuild")
	large.write(b, largePath)
	for _, bm := range []struct {
		name  string
		paths []string
	}{
		{"lab records", fixturePaths(b)},
		{"large trace", []string{largePath}},
	} {
		b.Run(bm.name, func(b *testing.B) {
			c := NewBundleCollector()
			b.ReportAllocs()
			for b.Loop() {
				for _, p := range bm.paths {
					if _, err := c.Collect(b.Context(), Source{Path: p}); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
