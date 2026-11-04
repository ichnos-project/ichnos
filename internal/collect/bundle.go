package collect

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// maxDecompressedBytes bounds the time a record takes to read, not its
	// memory: blobs stream through, and logs are skipped as they arrive.
	// BuildKit keeps at most 2 MiB of each step's stdout and of its stderr by
	// default, so this holds 2,048 steps that all reach that cap. Reading
	// that much takes about 20 s for logs and about a minute for the slowest
	// crafted input, and a cancelled context stops it within one read or one
	// JSON blob.
	maxDecompressedBytes = 8 << 30
	maxCompressedBytes   = maxDecompressedBytes
	// maxJSONBytes caps the JSON that Ichnos keeps fields from, which is
	// what its memory grows with: index.json, and every blob with a
	// top-level field of a manifest, a history record or a provenance,
	// including the manifests of the images the build made. A provenance of
	// maxSteps steps measured about 150 MiB. The JSON reader holds the value
	// it is reading whole, so a crafted record with one value of nearly
	// this size peaks at about 1 GB.
	maxJSONBytes      = 256 << 20
	maxArchiveEntries = 1024
	maxSteps          = 100_000
	maxVertices       = 200_000
	maxTextBytes      = 16 << 20
	maxStepInputs     = 10 * maxSteps
	maxSourceRanges   = 10 * maxSteps
	maxSourceFiles    = 1000
	maxFrontendAttrs  = 10_000
	// maxManifestLayers caps the layers of every manifest in a record
	// together: its own, and those of the images it built, which come from
	// its steps and its base images.
	maxManifestLayers = maxSteps
)

// limits caps what one build record may hold. A multi-platform build has a
// provenance for each platform, and they share each limit. Tests lower them
// to reach each limit with small inputs.
type limits struct {
	compressedBytes   int64
	decompressedBytes int64
	jsonBytes         int64
	entries           int
	steps             int
	vertices          int
	textBytes         int
	inputs            int
	ranges            int
	sources           int
	attrs             int
	layers            int
}

func defaultLimits() limits {
	return limits{
		compressedBytes:   maxCompressedBytes,
		decompressedBytes: maxDecompressedBytes,
		jsonBytes:         maxJSONBytes,
		entries:           maxArchiveEntries,
		steps:             maxSteps,
		vertices:          maxVertices,
		textBytes:         maxTextBytes,
		inputs:            maxStepInputs,
		ranges:            maxSourceRanges,
		sources:           maxSourceFiles,
		attrs:             maxFrontendAttrs,
		layers:            maxManifestLayers,
	}
}

type bundleCollector struct {
	limits limits
}

var _ Collector = (*bundleCollector)(nil)

// NewBundleCollector returns a Collector that reads a .dockerbuild file, as
// docker buildx history export writes it.
func NewBundleCollector() Collector {
	return &bundleCollector{limits: defaultLimits()}
}

func (*bundleCollector) Name() string { return "bundle" }

func (c *bundleCollector) Collect(ctx context.Context, src Source) (*Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading build record: %w", err)
	}
	f, err := openRegular(src.Path, c.limits)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	a, err := readArchive(ctx, f, c.limits)
	if err != nil {
		return nil, err
	}
	return decodeRecord(ctx, a, c.limits)
}

// openRegular opens path for reading, and returns ErrNotRegularFile for
// anything but a regular file. It opens without blocking and checks the
// opened file, so a pipe with no writer cannot hang the open and the file
// checked is the file read. The open goes through the file's directory as an
// os.Root, so a symbolic link cannot lead outside that directory.
func openRegular(path string, lim limits) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("opening build record: %w", err)
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.Base(path), openFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("opening build record: %w", err)
	}
	info, err := f.Stat()
	switch {
	case err != nil:
		err = fmt.Errorf("opening build record: %w", err)
	case !info.Mode().IsRegular():
		err = ErrNotRegularFile
	case info.Size() > lim.compressedBytes:
		err = &LimitError{What: "compressed bytes", Limit: lim.compressedBytes}
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// readArchive reads a gzipped OCI layout. Every gzip member shares one
// budget, and the stream is read to its end so that the checksum and any
// trailing data are checked.
func readArchive(ctx context.Context, r io.Reader, lim limits) (*layout, error) {
	compressed := &io.LimitedReader{R: r, N: lim.compressedBytes + 1}
	decompressed := &io.LimitedReader{N: lim.decompressedBytes + 1}
	l, err := func() (*layout, error) {
		zr, err := gzip.NewReader(compressed)
		if err != nil {
			return nil, archiveError(err)
		}
		decompressed.R = zr
		// Every read of the archive goes through stopWriter, so a cancelled
		// context stops the read within one chunk, wherever it is: in a
		// blob, in an entry the tar reader skips, or after the tar's end.
		src := io.TeeReader(decompressed, stopWriter(ctx.Done()))
		l, err := readTar(ctx, src, lim)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(io.Discard, src); err != nil {
			return nil, archiveError(err)
		}
		return l, nil
	}()
	switch {
	case compressed.N <= 0:
		return nil, &LimitError{What: "compressed bytes", Limit: lim.compressedBytes}
	case decompressed.N <= 0:
		return nil, &LimitError{What: "decompressed bytes", Limit: lim.decompressedBytes}
	case ctx.Err() != nil:
		return nil, fmt.Errorf("reading build record: %w", ctx.Err())
	}
	return l, err
}

// readTar reads the entries of an OCI layout. It accepts only the names and
// types that buildx writes, and reads each blob as it arrives, so that no
// blob is held whole.
func readTar(ctx context.Context, r io.Reader, lim limits) (*layout, error) {
	tr := tar.NewReader(r)
	l := &layout{blobs: map[string]*blob{}}
	b := newBlobReader(lim)
	seen := map[string]bool{}
	var used int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("reading build record: %w", err)
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return l, nil
		}
		if err != nil {
			return nil, archiveError(err)
		}
		if len(seen) == lim.entries {
			return nil, &LimitError{What: "archive entries", Limit: int64(lim.entries)}
		}
		if seen[hdr.Name] {
			return nil, malformed("archive has a duplicate entry")
		}
		seen[hdr.Name] = true
		if !allowedEntry(hdr) {
			return nil, malformed("archive has an entry that is not part of a build record")
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Size > lim.decompressedBytes-used {
			return nil, &LimitError{What: "decompressed bytes", Limit: lim.decompressedBytes}
		}
		used += hdr.Size
		switch hdr.Name {
		case "index.json":
			if err = b.json.hold(hdr.Size); err != nil {
				return nil, err
			}
			if l.index, err = io.ReadAll(tr); err != nil {
				return nil, archiveError(err)
			}
		case "oci-layout":
		default:
			blob, err := b.readBlob(ctx, tr, hdr.Size)
			if err != nil {
				return nil, err
			}
			l.blobs[strings.TrimPrefix(hdr.Name, blobDir)] = blob
		}
	}
}

const blobDir = "blobs/sha256/"

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func isSHA256Hex(s string) bool { return sha256Hex.MatchString(s) }

// allowedEntry reports whether hdr is one of the entries of an OCI layout
// that buildx writes. Links, devices, sparse files and global headers are
// never part of one.
func allowedEntry(hdr *tar.Header) bool {
	for key := range hdr.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return false
		}
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return hdr.Name == "blobs/" || hdr.Name == blobDir
	case tar.TypeReg:
		hex, isBlob := strings.CutPrefix(hdr.Name, blobDir)
		return hdr.Name == "index.json" || hdr.Name == "oci-layout" || (isBlob && isSHA256Hex(hex))
	default:
		return false
	}
}

// archiveError wraps an error from the gzip or tar reader. Their messages
// are fixed strings and offsets, never bytes of the input.
func archiveError(err error) error {
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("%w: reading archive: %w", ErrMalformedRecord, err)
}
