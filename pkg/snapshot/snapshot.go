// Package snapshot is Ichnos's normalized model of one container build: the
// steps it ran, whether each one used the cache, and fingerprints of what
// each one depends on. It shields every consumer from BuildKit's
// undocumented record formats.
//
// A Snapshot holds no credential that redaction finds: Ichnos builds it from
// a redacted build record, and keeps build argument and environment values
// only as fingerprints. Its JSON form, snapshot.json, is a versioned
// contract, described in docs/snapshot.md. The Go API is unstable until
// Ichnos v1.0.
//
// Every text field comes from the build record, which the author of a pull
// request controls: Step.Text, Platform.Platform, Source.Path,
// Context.Name, Context.Dir, Context.IgnorePatterns, NoCache.Stages, and
// the names of build arguments and environment variables. A renderer must
// escape each of them for where it writes them. Source.Path and
// Context.Dir are relative paths: check each with filepath.IsLocal, and
// open them only through an os.Root.
//
// Nothing reads a stored snapshot yet, and decoding an untrusted
// snapshot.json with encoding/json is not safe: it accepts an empty object,
// any schema version, duplicate names and names in another case, it leaves
// a missing enum as "" and a missing index as 0, and it accepts lists of
// any length. Enum values and fingerprints are checked only when present,
// and index ranges are not checked at all.
package snapshot

import (
	"errors"
	"fmt"
	"time"
)

// ErrUnknownValue means a text names no value of an enum, such as an
// operation type that this version of the package does not know.
var ErrUnknownValue = errors.New("unknown value")

// SchemaVersion is the version of the JSON form of a Snapshot that this
// package describes. It changes with any change to that form, as a field
// added, removed, renamed or retyped, or a new enum value.
const SchemaVersion = 1

// FingerprintVersion is the version of the rules that compute fingerprints
// and redact what a snapshot keeps. It changes whenever the same build
// would give other fingerprints or other kept text. Two snapshots compare
// only when their versions match.
const FingerprintVersion = 1

// Snapshot is one build.
type Snapshot struct {
	SchemaVersion      int `json:"schemaVersion"`
	FingerprintVersion int `json:"fingerprintVersion"`
	// Provenance says whether the record's provenance was read, and why the
	// snapshot has no platforms when it was not.
	Provenance ProvenanceState `json:"provenance"`
	// Platforms are the builds for each target platform, sorted by
	// platform. It is empty unless Provenance is ProvenanceRead.
	Platforms []Platform `json:"platforms"`
	// Vertices are what BuildKit reported about each vertex of the build:
	// its steps, its cache imports and exports, and BuildKit's own work.
	// Their order means nothing.
	Vertices []Vertex `json:"vertices"`
	Cache    Cache    `json:"cache"`
	// BuildArgs are the build arguments, sorted by name.
	BuildArgs []BuildArg `json:"buildArgs"`
	// Commit is the Git commit that was built, as buildx records it from the
	// work tree of a local build context, or empty when the record holds
	// none.
	Commit string `json:"commit,omitempty"`
	// Dirty is true when buildx recorded that the work tree had changes that
	// were not committed. buildx checks only when BUILDX_GIT_CHECK_DIRTY is
	// set, and never records a clean tree, so false means only that the
	// record does not say the tree was dirty.
	Dirty bool `json:"dirty,omitzero"`
	// SourceDateEpoch is the SOURCE_DATE_EPOCH build argument, in seconds,
	// or nil when the build did not set it to a number.
	SourceDateEpoch *int64 `json:"sourceDateEpoch,omitempty"`
}

// ProvenanceState says whether a build record's provenance was read.
type ProvenanceState string

// UnmarshalText sets p from its name, and returns ErrUnknownValue for any
// other text.
func (p *ProvenanceState) UnmarshalText(text []byte) error {
	switch v := ProvenanceState(text); v {
	case ProvenanceRead, ProvenanceMissing, ProvenanceUnsupported, ProvenanceNoSteps:
		*p = v
		return nil
	}
	return fmt.Errorf("provenance state: %w", ErrUnknownValue)
}

const (
	// ProvenanceRead means the record's SLSA v1 provenance was read.
	ProvenanceRead ProvenanceState = "read"
	// ProvenanceMissing means the record has no provenance. BuildKit writes
	// none for a build that failed.
	ProvenanceMissing ProvenanceState = "missing"
	// ProvenanceUnsupported means the record's provenance is in a form that
	// Ichnos does not read, such as SLSA v0.2.
	ProvenanceUnsupported ProvenanceState = "unsupported"
	// ProvenanceNoSteps means the provenance has no step definitions.
	ProvenanceNoSteps ProvenanceState = "no-steps"
)

// Platform is the build for one target platform.
type Platform struct {
	// Platform is the target platform, such as linux/amd64.
	Platform string `json:"platform"`
	// Withheld is true when redaction changed Platform.
	Withheld bool `json:"withheld,omitzero"`
	// Steps are the steps of the build, each after the steps it reads.
	Steps []Step `json:"steps"`
	// Environments are the distinct environments of the platform's RUN
	// steps, each sorted by name. Steps that share an environment point to
	// the same one, so it is stored once.
	Environments [][]EnvVar `json:"environments"`
	// Contexts are the local build contexts that the steps load.
	Contexts []Context `json:"contexts"`
}

// Step is one step of a build, one BuildKit operation. A Dockerfile line
// can have several steps, and a step can belong to several lines, of which
// it keeps the first.
type Step struct {
	Op OpType `json:"op"`
	// SourceKind is what a source step loads, and empty for every other
	// step.
	SourceKind SourceKind `json:"sourceKind,omitempty"`
	// Text is the Dockerfile instruction that the step comes from, with
	// credentials removed, or empty when the step has no location, as
	// BuildKit's own steps have none. Each run of bytes that is not valid
	// UTF-8 is replaced by one Unicode replacement character.
	Text string `json:"text,omitempty"`
	// Lines are the Dockerfile lines that Text spans, or zero when the step
	// has no location.
	Lines Lines `json:"lines,omitzero"`
	// Vertex is the index in Snapshot.Vertices of what BuildKit reported
	// about the step, or -1 when it reported nothing, as for the final step.
	Vertex int `json:"vertex"`
	// Inputs are the indexes in Platform.Steps of the steps whose results
	// the step reads, in order. Each is lower than the step's own index.
	Inputs []int `json:"inputs,omitempty"`
	// Environment is the index in Platform.Environments of the step's
	// environment variables, or -1 when it has none, as only a RUN step
	// has them.
	Environment int `json:"environment"`
	// Fingerprints identify the step's definition and everything it
	// descends from.
	Fingerprints Fingerprints `json:"fingerprints"`
	// Withheld is true when an input of the step was withheld: a variable
	// whose name looks like a credential, a value that redaction changed, or
	// a mount of a type that Ichnos does not know. A comparison cannot tell
	// whether such an input changed.
	Withheld bool `json:"withheld,omitzero"`
	// WithheldAncestor is true when the step, or a step it descends from,
	// was withheld, so its ancestor fingerprint cannot show whether that
	// input changed.
	WithheldAncestor bool `json:"withheldAncestor,omitzero"`
	// FromLocalContext, FromGit and FromHTTP report whether the step, or a
	// step it descends from, loads a local build context, a Git repository
	// or a file from an HTTP URL.
	FromLocalContext bool `json:"fromLocalContext,omitzero"`
	FromGit          bool `json:"fromGit,omitzero"`
	FromHTTP         bool `json:"fromHTTP,omitzero"`
	// Sources are the paths that the step reads from a local build
	// context, whose content BuildKit's cache key covers: those that a COPY
	// or ADD instruction copies, and those that a RUN instruction mounts,
	// with a bind mount or a cache mount.
	Sources []Source `json:"sources,omitempty"`
}

// OpType is the kind of a BuildKit operation.
type OpType string

// UnmarshalText sets t from its name, and returns ErrUnknownValue for any
// other text.
func (t *OpType) UnmarshalText(text []byte) error {
	switch v := OpType(text); v {
	case OpExec, OpFile, OpSource, OpBuild, OpMerge, OpDiff, OpNone, OpUnknown:
		*t = v
		return nil
	}
	return fmt.Errorf("operation type: %w", ErrUnknownValue)
}

// The kinds of operation, named as BuildKit names them in a step's
// definition.
const (
	OpExec   OpType = "exec"
	OpFile   OpType = "file"
	OpSource OpType = "source"
	OpBuild  OpType = "build"
	OpMerge  OpType = "merge"
	OpDiff   OpType = "diff"
	// OpNone is a step with no operation, such as the final step, which
	// only names the build's result.
	OpNone OpType = "none"
	// OpUnknown is an operation of a kind that Ichnos does not know.
	OpUnknown OpType = "unknown"
)

// SourceKind is what a source step loads, as its identifier names it.
type SourceKind string

// UnmarshalText sets k from its name, and returns ErrUnknownValue for any
// other text.
func (k *SourceKind) UnmarshalText(text []byte) error {
	switch v := SourceKind(text); v {
	case SourceLocal, SourceGit, SourceHTTP, SourceImage, SourceOther:
		*k = v
		return nil
	}
	return fmt.Errorf("source kind: %w", ErrUnknownValue)
}

const (
	// SourceLocal is a local build context, the main one or a named one.
	SourceLocal SourceKind = "local"
	// SourceGit is a Git repository, as a build context or for ADD.
	SourceGit SourceKind = "git"
	// SourceHTTP is a file that ADD downloads from an HTTP or HTTPS URL.
	SourceHTTP SourceKind = "http"
	// SourceImage is an image from a registry or an OCI layout. BuildKit
	// never reports a cache hit for one: it resolves the image again on
	// every build.
	SourceImage SourceKind = "image"
	// SourceOther is a source of any other kind.
	SourceOther SourceKind = "other"
)

// Lines is a range of Dockerfile lines, counted from 1.
type Lines struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Fingerprints identify the parts of a step's definition. They leave out
// what BuildKit leaves out of its own cache key, such as proxy settings, and
// a withheld value is left out of all of them. The environment's
// fingerprints are in the step's Platform.Environments entry.
type Fingerprints struct {
	// Command identifies the command of a RUN step, or the whole operation
	// of any other step, with its kind.
	Command Fingerprint `json:"command"`
	// Other identifies every other field, such as the user, the working
	// directory, the mounts and the platform.
	Other Fingerprint `json:"other"`
	// Ancestor identifies the step together with every step it descends
	// from, through its inputs. It leaves out the paths and patterns that a
	// local build context sends, because a change to either would otherwise
	// change the ancestors of every step after the context, and the ref and
	// git.* settings of a Git repository, because the commit of a Git build
	// context changes on every build. A withheld input is left out of it
	// too, which Step.WithheldAncestor reports.
	Ancestor Fingerprint `json:"ancestor"`
}

// EnvVar is the fingerprint of one environment variable.
type EnvVar struct {
	Name string `json:"name"`
	// Value identifies the variable's value, or its values in order when
	// the step sets it more than once. It is zero when the value was
	// withheld.
	Value Fingerprint `json:"value,omitzero"`
	// Withheld is true when the name looks like a credential, or redaction
	// changed the entry, so the value was left out.
	Withheld bool `json:"withheld,omitzero"`
}

// Source is one path that a step reads from a local build context.
type Source struct {
	// Context is the index in Platform.Contexts of the build context.
	Context int `json:"context"`
	// Path is the source path relative to the build context, with
	// credentials removed, or "." for the whole context. It may hold
	// wildcards. A path that is not clean, does not stay within the
	// context, or holds a backslash, a colon, a character that is not
	// graphic or a reserved Windows name, is left out, and its step is
	// withheld.
	Path string `json:"path"`
}

// Context is a local build context, the main one or a named one. Steps that
// load equal contexts share one.
type Context struct {
	// Name is the context's name: "context" for the main context, or the
	// name of a named context.
	Name string `json:"name"`
	// Steps are the indexes in Platform.Steps of the steps that load the
	// context, in order.
	Steps []int `json:"steps"`
	// Dir is the context's directory relative to the root of its Git work
	// tree, as buildx records it, with "." for the root. It is empty when
	// the record holds none, or one that is not a plain relative path
	// within the work tree, under the same rules as Source.Path. buildx
	// writes backslashes on Windows, so a record made there has none.
	Dir string `json:"dir,omitempty"`
	// IgnorePatterns are the .dockerignore patterns that BuildKit applied
	// to the context, in their order in the file.
	IgnorePatterns []string `json:"ignorePatterns,omitempty"`
	// Withheld is true when redaction changed the step that loads the
	// context, its name or the directory that buildx recorded for it, when
	// another context has the same redacted name, or when the patterns
	// could not be read. Then Dir and IgnorePatterns may be left out, or
	// not be what the build used.
	Withheld bool `json:"withheld,omitzero"`
}

// Vertex is what BuildKit reported about one vertex of a build.
type Vertex struct {
	Kind VertexKind `json:"kind"`
	// Cached is true when the vertex used the cache. It is false for a
	// vertex that reported a cache hit and then an error, because BuildKit
	// could not load the cached result and ran the vertex again.
	Cached bool `json:"cached,omitzero"`
	// LostHit is true when the vertex reported a cache hit and then an
	// error.
	LostHit bool `json:"lostHit,omitzero"`
	// Errored is true when any report about the vertex had an error.
	Errored bool `json:"errored,omitzero"`
	// Started and Completed are the earliest start and the latest
	// completion that BuildKit reported, or zero when it reported none.
	Started   time.Time `json:"started,omitzero"`
	Completed time.Time `json:"completed,omitzero"`
}

// VertexKind tells a build step from BuildKit's other work.
type VertexKind string

// UnmarshalText sets k from its name, and returns ErrUnknownValue for any
// other text.
func (k *VertexKind) UnmarshalText(text []byte) error {
	switch v := VertexKind(text); v {
	case VertexStep, VertexInternal, VertexCacheImport, VertexCacheExport:
		*k = v
		return nil
	}
	return fmt.Errorf("vertex kind: %w", ErrUnknownValue)
}

// The kinds of vertex, which Ichnos tells apart by the names that BuildKit
// gives them.
const (
	VertexStep        VertexKind = "step"
	VertexInternal    VertexKind = "internal"
	VertexCacheImport VertexKind = "cache-import"
	VertexCacheExport VertexKind = "cache-export"
)

// Cache holds the build's cache settings. BuildKit records none for cache
// exports, which show only as vertices.
type Cache struct {
	// Imports are the caches that the build was asked to import, in the
	// order given. BuildKit does not say which cache-import vertex belongs
	// to which import.
	Imports []CacheImport `json:"imports"`
	// NoCache is nil unless the build was asked not to use the cache.
	NoCache *NoCache `json:"noCache,omitempty"`
}

// CacheImport is one cache that a build was asked to import.
type CacheImport struct {
	Type CacheType `json:"type"`
}

// CacheType is the kind of storage a cache import reads.
type CacheType string

// UnmarshalText sets c from its name, and returns ErrUnknownValue for any
// other text.
func (c *CacheType) UnmarshalText(text []byte) error {
	switch v := CacheType(text); v {
	case CacheLocal, CacheRegistry, CacheGHA, CacheS3, CacheAzblob, CacheUnknown:
		*c = v
		return nil
	}
	return fmt.Errorf("cache type: %w", ErrUnknownValue)
}

// The types of cache, named as the type option of --cache-from names them.
const (
	CacheLocal    CacheType = "local"
	CacheRegistry CacheType = "registry"
	CacheGHA      CacheType = "gha"
	CacheS3       CacheType = "s3"
	CacheAzblob   CacheType = "azblob"
	// CacheUnknown is a cache of another type, or a setting that could not
	// be read.
	CacheUnknown CacheType = "unknown"
)

// NoCache is a request not to use the cache.
type NoCache struct {
	// Stages are the Dockerfile stages that --no-cache-filter named, with
	// credentials removed. It is empty for --no-cache, which covers every
	// stage.
	Stages []string `json:"stages"`
	// Withheld is true when redaction changed the stages.
	Withheld bool `json:"withheld,omitzero"`
}

// BuildArg is one build argument.
type BuildArg struct {
	// Name is the argument's name, with credentials removed.
	Name string `json:"name"`
	// Value identifies the argument's value, or is zero when it was
	// withheld.
	Value Fingerprint `json:"value,omitzero"`
	// Withheld is true when the name looks like a credential, or redaction
	// changed the name or the value, so the value was left out.
	Withheld bool `json:"withheld,omitzero"`
}
