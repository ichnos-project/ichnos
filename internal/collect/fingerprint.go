package collect

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json/jsontext"
	"errors"
	"hash"
	"slices"
	"strings"

	"github.com/ichnos-project/ichnos/pkg/snapshot"
)

// fingerprintPrefix starts the input of every hash that makes up a
// fingerprint. It ties the hashes to Ichnos and to
// snapshot.FingerprintVersion, so that a table of plain SHA-256 hashes of
// common values does not apply to them.
const fingerprintPrefix = "ichnos fingerprint v1\x00"

// The tag after fingerprintPrefix says what a hash covers, so that no two
// kinds of value can share a hash.
const (
	tagString      = 's'
	tagNumber      = 'n'
	tagTrue        = 't'
	tagFalse       = 'f'
	tagNull        = 'z'
	tagArray       = 'a'
	tagObject      = 'o'
	tagAbsent      = 'x'
	tagCommand     = 'c'
	tagEnv         = 'e'
	tagEnvNoValue  = 'E'
	tagEnvRepeated = 'r'
	tagSelf        = 'S'
	tagAncestor    = 'A'
	tagBuildArg    = 'b'
	// tagEnvList identifies a whole environment, to find steps that share
	// one. It is not a fingerprint that a snapshot holds.
	tagEnvList = 'L'
	// tagEnvOrder covers the names of an environment, in order.
	tagEnvOrder = 'O'
	// tagContextKey identifies a local build context, to find steps that
	// load equal ones. It is not a fingerprint that a snapshot holds.
	tagContextKey = 'K'
)

// Flags of an object member while its object is read.
const (
	// memberLeftOut is a member that the object's fingerprint leaves out.
	// It is held until the object ends only so that a name that appears
	// twice is found.
	memberLeftOut uint8 = 1 << iota
	// memberCacheID is a cache mount's ID or sharing, which BuildKit leaves
	// out of its cache key unless the ID is the mount's target.
	memberCacheID
	// memberCacheOpt is a mount's cacheOpt.
	memberCacheOpt
	// memberLocalDetail, memberGitAttr and memberImageAttr are attributes of
	// a source that one of its fingerprints leaves out, by the kind of
	// source, which is known only once the whole source is read.
	memberLocalDetail
	memberGitAttr
	memberImageAttr
	// memberIdentifier and memberAttrs are a source's identifier and
	// attributes, whose hashes differ from one fingerprint to another.
	memberIdentifier
	memberAttrs
)

// member is one member of an object being hashed.
type member struct {
	name, value snapshot.Fingerprint
	flags       uint8
}

// maxHeldMembers caps the members of the objects that one step definition
// has open at once. The hasher holds each, in 65 bytes, until its object
// ends, to sort them. BuildKit's largest objects hold a few dozen members.
const maxHeldMembers = 100_000

var errDuplicateName = errors.New("object has a duplicate name")

// hasher computes the fingerprints of redacted step definitions. It reads
// each definition as a stream, holds one member per member of each open
// object and one running hash per open array, and reuses its buffers from
// one definition to the next.
//
// A fingerprint is a canonical hash of a JSON value: a string hashes its
// unquoted text, a number its literal, an array the hashes of its elements
// in order, and an object the pairs of its members' name and value hashes,
// sorted by name hash, so that the order of names and the spelling of
// escapes do not matter. A name that appears twice is an error.
type hasher struct {
	sha hash.Hash
	// head is fingerprintPrefix and a byte for the tag, and out receives
	// each hash. They are fields so that passing them to a hash.Hash, which
	// makes them escape, allocates nothing.
	head   []byte
	out    [sha256.Size]byte
	j      jsonReader
	scrap  []byte
	held   []member
	arrays []hash.Hash
	open   int
	lim    snapshotLimits
	// reads holds the reads of the definition being read.
	reads []readFact
	// sources is how many reads from another step the definitions have held
	// so far, against the limit.
	sources int
	// absent, nullHash, trueHash and falseHash are the hashes of constant
	// values.
	absent, nullHash, trueHash, falseHash snapshot.Fingerprint
	// names holds the hashes of short member names once namesHashed has
	// reached namesBeforeCache.
	names       map[string]snapshot.Fingerprint
	namesHashed int
	// last holds the environment that the previous RUN step read, in its
	// order, lastRaw its entries as JSON, each ending at lastEnds, and next,
	// nextRaw and nextEnds the same for the environment being read. Each
	// pair is swapped only once an environment has been read whole, so that
	// last, lastRaw and lastEnds always describe one environment, even after
	// a definition that fails.
	last, next         []envEntry
	lastRaw, nextRaw   []byte
	lastEnds, nextEnds []int
	// changed marks the entries of the environment being read that
	// redaction changed.
	changed []bool
	// order sorts the environment being read by name, and vars holds its
	// variables, with envKey their key and varsChanged set when redaction
	// changed any of its entries. varsValid is set while vars describes the
	// environment in last.
	order       []int
	vars        []snapshot.EnvVar
	envKey      snapshot.Fingerprint
	varsChanged bool
	varsValid   bool
}

// Member names repeat from step to step, so once a snapshot has hashed
// namesBeforeCache of them, the hasher keeps the hashes of up to
// maxCachedNames names of at most maxCachedNameBytes. A record with few
// steps never fills the cache.
const (
	namesBeforeCache   = 1024
	maxCachedNames     = 256
	maxCachedNameBytes = 64
)

func newHasher(lim snapshotLimits) *hasher {
	h := &hasher{
		sha:  sha256.New(),
		head: []byte(fingerprintPrefix + "?"),
		lim:  lim,
	}
	h.j.dec = jsontext.NewDecoder(&h.j.buf)
	h.absent = h.sum(tagAbsent, nil)
	h.nullHash, h.trueHash, h.falseHash = h.sum(tagNull, nil), h.sum(tagTrue, nil), h.sum(tagFalse, nil)
	return h
}

// nameHash returns the hash of an object member's name.
func (h *hasher) nameHash(name []byte) snapshot.Fingerprint {
	if h.names == nil {
		if h.namesHashed++; h.namesHashed < namesBeforeCache {
			return h.sum(tagString, name)
		}
		h.names = make(map[string]snapshot.Fingerprint, maxCachedNames)
	}
	if f, ok := h.names[string(name)]; ok {
		return f
	}
	f := h.sum(tagString, name)
	if len(h.names) < maxCachedNames && len(name) <= maxCachedNameBytes {
		h.names[string(name)] = f
	}
	return f
}

// reset starts reading v.
func (h *hasher) reset(v []byte) {
	h.j.reset(v)
	h.held = h.held[:0]
	h.open = 0
}

// sum returns the hash of the prefix, tag and data.
func (h *hasher) sum(tag byte, data []byte) snapshot.Fingerprint {
	h.sha.Reset()
	h.write(h.sha, tag)
	_, _ = h.sha.Write(data)
	return h.finish(h.sha)
}

// write writes the prefix and tag to w.
func (h *hasher) write(w hash.Hash, tag byte) {
	h.head[len(h.head)-1] = tag
	_, _ = w.Write(h.head)
}

func (h *hasher) finish(w hash.Hash) snapshot.Fingerprint {
	w.Sum(h.out[:0])
	return h.out
}

// sumFields returns the hash of the prefix, tag and fields, each field
// after its length, so that no two lists of fields give the same input.
func (h *hasher) sumFields(tag byte, fields ...[]byte) snapshot.Fingerprint {
	h.scrap = h.scrap[:0]
	for _, f := range fields {
		h.scrap = binary.AppendUvarint(h.scrap, uint64(len(f)))
		h.scrap = append(h.scrap, f...)
	}
	return h.sum(tag, h.scrap)
}

// value reads the next value and returns its hash.
func (h *hasher) value() (snapshot.Fingerprint, error) {
	switch h.j.peek() {
	case jsontext.KindBeginObject:
		return h.object(nil)
	case jsontext.KindBeginArray:
		return h.array(nil)
	case jsontext.KindInvalid, jsontext.KindNull, jsontext.KindFalse, jsontext.KindTrue, jsontext.KindString,
		jsontext.KindNumber, jsontext.KindEndObject, jsontext.KindEndArray:
		fallthrough
	default:
		raw, err := h.j.readValue()
		if err != nil {
			return snapshot.Fingerprint{}, err
		}
		return h.scalar(raw)
	}
}

func (h *hasher) scalar(raw jsontext.Value) (snapshot.Fingerprint, error) {
	switch raw.Kind() {
	case jsontext.KindString:
		s, err := h.j.unquoteBytes(raw)
		if err != nil {
			return snapshot.Fingerprint{}, err
		}
		return h.sum(tagString, s), nil
	case jsontext.KindNumber:
		return h.sum(tagNumber, raw), nil
	case jsontext.KindTrue:
		return h.trueHash, nil
	case jsontext.KindFalse:
		return h.falseHash, nil
	case jsontext.KindNull:
		return h.nullHash, nil
	case jsontext.KindInvalid, jsontext.KindBeginObject, jsontext.KindEndObject, jsontext.KindBeginArray,
		jsontext.KindEndArray:
		fallthrough
	default:
		return snapshot.Fingerprint{}, errNotValid
	}
}

// memberFunc reads the value of the member name, which stays valid only
// until the next read, and returns its hash and flags. A nil memberFunc
// hashes every value as it is.
type memberFunc func(name []byte) (snapshot.Fingerprint, uint8, error)

// object reads the next value, which must be an object, and returns its
// hash without the members that read leaves out.
func (h *hasher) object(read memberFunc) (snapshot.Fingerprint, error) {
	start, err := h.readObject(read)
	if err != nil {
		return snapshot.Fingerprint{}, err
	}
	return h.endObject(start, 0)
}

// readObject reads the next value, which must be an object, and holds its
// members from start on until endObject or dropMembers.
func (h *hasher) readObject(read memberFunc) (int, error) {
	if h.j.peek() != jsontext.KindBeginObject {
		return 0, errNotValid
	}
	if err := h.j.next(); err != nil {
		return 0, err
	}
	start := len(h.held)
	for h.j.peek() != jsontext.KindEndObject {
		raw, err := h.j.readValue()
		if err != nil {
			return 0, err
		}
		name, err := h.j.unquoteBytes(raw)
		if err != nil {
			return 0, err
		}
		if len(h.held) == h.lim.heldMembers {
			return 0, &LimitError{What: "object members held while reading a step definition", Limit: int64(h.lim.heldMembers)}
		}
		m := member{name: h.nameHash(name)}
		if read == nil {
			m.value, err = h.value()
		} else {
			m.value, m.flags, err = read(name)
		}
		if err != nil {
			return 0, err
		}
		h.held = append(h.held, m)
	}
	return start, h.j.next()
}

// endObject returns the hash of the object whose members are held from
// start on, without those with memberLeftOut or a flag in skip, and stops
// holding them.
func (h *hasher) endObject(start int, skip uint8) (snapshot.Fingerprint, error) {
	if err := h.sortMembers(start); err != nil {
		return snapshot.Fingerprint{}, err
	}
	f := h.hashMembers(start, skip)
	h.dropMembers(start)
	return f, nil
}

// sortMembers sorts the members held from start on by name hash, and checks
// that no name appears twice.
func (h *hasher) sortMembers(start int) error {
	held := h.held[start:]
	slices.SortFunc(held, func(a, b member) int { return bytes.Compare(a.name[:], b.name[:]) })
	for i := 1; i < len(held); i++ {
		if held[i].name == held[i-1].name {
			return errDuplicateName
		}
	}
	return nil
}

// hashMembers returns the hash of the sorted members held from start on,
// without those with memberLeftOut or a flag in skip.
func (h *hasher) hashMembers(start int, skip uint8) snapshot.Fingerprint {
	held := h.held[start:]
	h.sha.Reset()
	h.write(h.sha, tagObject)
	for i := range held {
		if held[i].flags&(skip|memberLeftOut) == 0 {
			_, _ = h.sha.Write(held[i].name[:])
			_, _ = h.sha.Write(held[i].value[:])
		}
	}
	return h.finish(h.sha)
}

func (h *hasher) dropMembers(start int) { h.held = h.held[:start] }

// array reads the next value, which must be an array, and returns the hash
// of its elements. A nil read hashes every element as it is.
func (h *hasher) array(read func() (snapshot.Fingerprint, error)) (snapshot.Fingerprint, error) {
	if h.j.peek() != jsontext.KindBeginArray {
		return snapshot.Fingerprint{}, errNotValid
	}
	if err := h.j.next(); err != nil {
		return snapshot.Fingerprint{}, err
	}
	if h.open == len(h.arrays) {
		h.arrays = append(h.arrays, sha256.New())
	}
	w := h.arrays[h.open]
	h.open++
	w.Reset()
	h.write(w, tagArray)
	for h.j.peek() != jsontext.KindEndArray {
		var f snapshot.Fingerprint
		var err error
		if read == nil {
			f, err = h.value()
		} else {
			f, err = read()
		}
		if err != nil {
			return snapshot.Fingerprint{}, err
		}
		h.out = f
		_, _ = w.Write(h.out[:])
	}
	h.open--
	return h.finish(w), h.j.next()
}

// stringValue reads the next value, which must be a string, and returns its
// hash and its text, which stays valid until the next read.
func (h *hasher) stringValue() (snapshot.Fingerprint, []byte, error) {
	raw, err := h.j.readValue()
	if err != nil {
		return snapshot.Fingerprint{}, nil, err
	}
	s, err := h.j.unquoteBytes(raw)
	if err != nil {
		return snapshot.Fingerprint{}, nil, err
	}
	return h.sum(tagString, s), s, nil
}

// scalarValue reads the next value, a scalar, and returns it raw, valid only
// until the next read, with its hash.
func (h *hasher) scalarValue() (jsontext.Value, snapshot.Fingerprint, error) {
	raw, err := h.j.readValue()
	if err != nil {
		return nil, snapshot.Fingerprint{}, err
	}
	f, err := h.scalar(raw)
	return raw, f, err
}

// leaveOutNull skips a null, which counts as an absent member, and reports
// whether it did.
func (h *hasher) leaveOutNull() (bool, error) {
	if h.j.peek() != jsontext.KindNull {
		return false, nil
	}
	return true, h.j.skip()
}

// readFact is one path that a step reads from another step, as a copy
// action's source or a mount's selector: the index of the step input
// it reads, and its path there.
type readFact struct {
	input int
	src   string
}

// opFacts is what a snapshot uses from one redacted step definition.
type opFacts struct {
	kind snapshot.OpType
	// command is the fingerprint of the command, and contextCommand the same
	// with the details of a build context left out, for the ancestor
	// fingerprint.
	command, contextCommand snapshot.Fingerprint
	other                   snapshot.Fingerprint
	// env holds the step's environment, sorted by name, and envKey its key.
	// env stays valid until the hasher reads the next definition.
	env    []snapshot.EnvVar
	envKey snapshot.Fingerprint
	source snapshot.SourceKind
	// withheld is set when the definition holds something whose effect on
	// the cache key Ichnos cannot tell, such as a mount of an unknown type.
	withheld bool
	// localName is the name of a local build context, and excludePatterns
	// its local.excludepatterns attribute, when hasPatterns is set.
	localName       string
	excludePatterns string
	hasPatterns     bool
	// reads are the paths the step reads from its inputs. They stay valid
	// until the hasher reads the next definition.
	reads []readFact
}

// stepReader reads one step definition. Its methods are the member
// functions of the definition's objects, at the paths where Ichnos looks
// closer than the canonical hash.
type stepReader struct {
	h     *hasher
	j     *jsonReader
	facts opFacts
	// changedEnv are the positions of the entries of meta.env that
	// redaction changed.
	changedEnv []int
	// args is the hash of an exec step's meta.args.
	args snapshot.Fingerprint
	env  []envEntry
	// sameEnv is set when env has the same entries, byte for byte, as the
	// previous RUN step's.
	sameEnv bool
}

// envEntry is one entry of an exec step's meta.env.
type envEntry struct {
	name     string
	value    snapshot.Fingerprint
	withheld bool
}

// readOp returns the facts of one redacted step definition, a JSON object or
// nil. changedEnv are the positions of the entries of its meta.env that
// redaction changed, whose variables it withholds. It returns
// ErrMalformedRecord when the definition does not have the shape that
// BuildKit writes where Ichnos reads it, and a *LimitError when it holds
// more than the limits allow.
func (h *hasher) readOp(op jsontext.Value, changedEnv []int) (opFacts, error) {
	r := stepReader{h: h, j: &h.j, args: h.absent, changedEnv: changedEnv}
	r.facts.kind = snapshot.OpNone
	r.facts.reads = h.reads[:0]
	if op == nil {
		r.facts.other = h.absent
	} else {
		h.reset(op)
		other, err := h.object(r.root)
		if err == nil {
			err = h.j.end()
		}
		if err != nil {
			return opFacts{}, opError(err)
		}
		r.facts.other = other
	}
	h.reads = r.facts.reads
	switch r.facts.kind {
	case snapshot.OpExec:
		r.facts.command = h.commandFingerprint(snapshot.OpExec, r.args)
		r.facts.contextCommand = r.facts.command
	case snapshot.OpNone:
		r.facts.command = h.commandFingerprint(snapshot.OpNone, h.absent)
		r.facts.contextCommand = r.facts.command
	case snapshot.OpFile, snapshot.OpSource, snapshot.OpBuild, snapshot.OpMerge, snapshot.OpDiff, snapshot.OpUnknown:
	}
	r.facts.env, r.facts.envKey = r.envVars()
	return r.facts, nil
}

// leaveOut reads the next value, which the fingerprint leaves out. It reads
// it through the hasher, so that a name repeated in it is found, as in any
// other value.
func (h *hasher) leaveOut() (snapshot.Fingerprint, uint8, error) {
	_, err := h.value()
	return snapshot.Fingerprint{}, memberLeftOut, err
}

// opError returns the error for a step definition that cannot be read. The
// errors of the JSON reader can quote bytes of the record, so they become a
// fixed description.
func opError(err error) error {
	var le *LimitError
	switch {
	case errors.As(err, &le), errors.Is(err, ErrMalformedRecord):
		return err
	case errors.Is(err, errDuplicateName):
		return malformed("step definition has an object with a duplicate name")
	default:
		return malformed("step definition does not have the shape that BuildKit writes")
	}
}

func (h *hasher) commandFingerprint(kind snapshot.OpType, body snapshot.Fingerprint) snapshot.Fingerprint {
	return h.sumFields(tagCommand, []byte(kind), body[:])
}

// root reads the members of a step definition, whose hash is the other
// fingerprint. It leaves out constraints, which are in no operation's cache
// key, and the operation, except for the fields of an exec operation other
// than its command and environment.
func (r *stepReader) root(name []byte) (snapshot.Fingerprint, uint8, error) {
	switch string(name) {
	case "Op":
		return r.operation()
	case "constraints":
		return r.h.leaveOut()
	}
	f, err := r.h.value()
	return f, 0, err
}

// operation reads Op, an object with one member named after the kind of
// operation, or none.
func (r *stepReader) operation() (snapshot.Fingerprint, uint8, error) {
	r.facts.kind = snapshot.OpNone
	if null, err := r.h.leaveOutNull(); null || err != nil {
		return snapshot.Fingerprint{}, memberLeftOut, err
	}
	f, err := r.h.object(r.body)
	if err != nil {
		return snapshot.Fingerprint{}, 0, err
	}
	if r.facts.kind != snapshot.OpExec {
		return f, memberLeftOut, nil
	}
	return f, 0, nil
}

// body reads the operation itself, whose name is its kind.
func (r *stepReader) body(name []byte) (snapshot.Fingerprint, uint8, error) {
	if r.facts.kind != snapshot.OpNone {
		return snapshot.Fingerprint{}, 0, malformed("step definition has more than one operation")
	}
	kind := opType(name)
	r.facts.kind = kind
	var f snapshot.Fingerprint
	var err error
	switch kind {
	case snapshot.OpExec:
		f, err = r.h.object(r.exec)
		return f, 0, err
	case snapshot.OpFile:
		f, err = r.h.object(r.file)
	case snapshot.OpSource:
		return r.source()
	case snapshot.OpBuild, snapshot.OpMerge, snapshot.OpDiff:
		f, err = r.h.object(nil)
	case snapshot.OpNone, snapshot.OpUnknown:
		fallthrough
	default:
		f, err = r.h.value()
	}
	if err != nil {
		return snapshot.Fingerprint{}, 0, err
	}
	r.facts.command = r.h.commandFingerprint(kind, f)
	r.facts.contextCommand = r.facts.command
	return f, 0, nil
}

// opType returns the kind of operation that name names. It compares name
// with each kind, because converting it would allocate.
func opType(name []byte) snapshot.OpType {
	for _, t := range [...]snapshot.OpType{snapshot.OpExec, snapshot.OpFile, snapshot.OpSource, snapshot.OpBuild, snapshot.OpMerge, snapshot.OpDiff} {
		if string(name) == string(t) {
			return t
		}
	}
	return snapshot.OpUnknown
}

// exec reads the members of an exec operation. A null meta or mounts counts
// as absent.
func (r *stepReader) exec(name []byte) (snapshot.Fingerprint, uint8, error) {
	var f snapshot.Fingerprint
	var err error
	switch string(name) {
	case "meta":
		if null, nullErr := r.h.leaveOutNull(); null || nullErr != nil {
			return snapshot.Fingerprint{}, memberLeftOut, nullErr
		}
		f, err = r.h.object(r.meta)
	case "mounts":
		if null, nullErr := r.h.leaveOutNull(); null || nullErr != nil {
			return snapshot.Fingerprint{}, memberLeftOut, nullErr
		}
		f, err = r.h.array(r.mount)
	default:
		f, err = r.h.value()
	}
	return f, 0, err
}

// meta reads the members of an exec operation's meta. Its fingerprint
// leaves out the command and the values of the environment, which have
// their own, and the proxy settings, which BuildKit leaves out of its cache
// key (solver/llbsolver/ops/exec.go at v0.34.0). It keeps the order of the
// environment's names, which is in that key.
func (r *stepReader) meta(name []byte) (snapshot.Fingerprint, uint8, error) {
	switch string(name) {
	case "args":
		f, err := r.h.value()
		r.args = f
		return f, memberLeftOut, err
	case "env":
		if null, nullErr := r.h.leaveOutNull(); null || nullErr != nil {
			return snapshot.Fingerprint{}, memberLeftOut, nullErr
		}
		f, err := r.readEnv()
		return f, 0, err
	case "proxy_env":
		return r.h.leaveOut()
	case "extraHosts":
		if r.j.peek() == jsontext.KindBeginArray {
			f, err := r.h.array(r.extraHost)
			return f, 0, err
		}
	}
	f, err := r.h.value()
	return f, 0, err
}

// extraHost reads one extra host. BuildKit leaves its IP out of its cache
// key.
func (r *stepReader) extraHost() (snapshot.Fingerprint, error) {
	if r.j.peek() != jsontext.KindBeginObject {
		return r.h.value()
	}
	return r.h.object(func(name []byte) (snapshot.Fingerprint, uint8, error) {
		if string(name) == "IP" {
			return r.h.leaveOut()
		}
		f, err := r.h.value()
		return f, 0, err
	})
}

// mount reads one mount. BuildKit leaves a cache mount's ID and sharing out
// of its cache key, unless the ID is the mount's target, or
// <namespace>/<target>, and sharing is 0, as the Dockerfile frontend sets
// them by default.
//
// It notes the selector of a bind or cache mount, the path of its input
// that it mounts, because BuildKit's cache key covers the content there
// (getMountDeps and CacheMap in solver/llbsolver/ops/exec.go at v0.34.0). A
// secret, SSH or tmpfs mount reads no input, and a mount of a type that
// BuildKit v0.34.0 does not name withholds the step. BuildKit leaves a zero
// input, selector and mount type out of the JSON, so a missing input is
// input 0, a missing selector the whole input, and a missing mount type a
// bind mount. An input, selector or mount type of another JSON type than
// BuildKit writes makes the definition malformed, whatever the order of the
// members.
func (r *stepReader) mount() (snapshot.Fingerprint, error) {
	if r.j.peek() != jsontext.KindBeginObject {
		return r.h.value()
	}
	var dest, selector string
	var opt *cacheOptions
	input, mountType := 0, mountBind
	start, err := r.h.readObject(func(name []byte) (snapshot.Fingerprint, uint8, error) {
		switch string(name) {
		case "input":
			f, n, err := r.index()
			input = n
			return f, 0, err
		case "mountType":
			if r.j.peek() != jsontext.KindNumber {
				return snapshot.Fingerprint{}, 0, errNotValid
			}
			raw, f, err := r.h.scalarValue()
			if mountType = smallInt(raw); mountType < 0 {
				return snapshot.Fingerprint{}, 0, errNotValid
			}
			return f, 0, err
		case "selector":
			if r.j.peek() != jsontext.KindString {
				return snapshot.Fingerprint{}, 0, errNotValid
			}
			f, s, err := r.h.stringValue()
			selector = string(s)
			return f, 0, err
		case "dest":
			if r.j.peek() == jsontext.KindString {
				f, s, err := r.h.stringValue()
				dest = string(s)
				return f, 0, err
			}
		case "cacheOpt":
			if r.j.peek() == jsontext.KindBeginObject {
				o, err := r.cacheOpt()
				opt = &o
				return o.full, memberCacheOpt, err
			}
		}
		f, err := r.h.value()
		return f, 0, err
	})
	if err != nil {
		return snapshot.Fingerprint{}, err
	}
	if opt != nil && clearsCacheOpt(dest, opt.id, opt.sharingSet) {
		for i := start; i < len(r.h.held); i++ {
			if r.h.held[i].flags&memberCacheOpt != 0 {
				r.h.held[i].value = opt.cleared
			}
		}
	}
	switch mountType {
	case mountBind, mountCache:
		if input >= 0 {
			if err := r.addRead(input, selector); err != nil {
				return snapshot.Fingerprint{}, err
			}
		}
	case mountSecret, mountSSH, mountTmpfs:
	default:
		r.facts.withheld = true
	}
	return r.h.endObject(start, 0)
}

// The mount types that BuildKit names in pb.MountType at v0.34.0
// (solver/pb/ops.pb.go).
const (
	mountBind   = 0
	mountSecret = 1
	mountSSH    = 2
	mountCache  = 3
	mountTmpfs  = 4
)

// index reads the index of a step input, a JSON number: a whole number from
// 0 to maxSmallInt, or -1 for none, as BuildKit writes pb.Empty. It returns
// errNotValid for any other value.
func (r *stepReader) index() (snapshot.Fingerprint, int, error) {
	if r.j.peek() != jsontext.KindNumber {
		return snapshot.Fingerprint{}, 0, errNotValid
	}
	raw, f, err := r.h.scalarValue()
	if err != nil {
		return snapshot.Fingerprint{}, 0, err
	}
	if string(raw) == "-1" {
		return f, -1, nil
	}
	n := smallInt(raw)
	if n < 0 {
		return snapshot.Fingerprint{}, 0, errNotValid
	}
	return f, n, nil
}

// cacheOptions is what a cache mount's options hold: their hash with and
// without the ID and sharing, the ID, and whether sharing is set to
// something other than 0, which is SHARED: PRIVATE or LOCKED.
type cacheOptions struct {
	full, cleared snapshot.Fingerprint
	id            string
	sharingSet    bool
}

func (r *stepReader) cacheOpt() (cacheOptions, error) {
	var o cacheOptions
	start, err := r.h.readObject(func(name []byte) (snapshot.Fingerprint, uint8, error) {
		switch string(name) {
		case "ID":
			if r.j.peek() == jsontext.KindString {
				f, s, err := r.h.stringValue()
				o.id = string(s)
				return f, memberCacheID, err
			}
		case "sharing":
			raw, err := r.j.readValue()
			if err != nil {
				return snapshot.Fingerprint{}, 0, err
			}
			o.sharingSet = raw.Kind() != jsontext.KindNull && string(raw) != "0"
			f, err := r.h.scalar(raw)
			return f, memberCacheID, err
		}
		f, err := r.h.value()
		return f, 0, err
	})
	if err != nil {
		return cacheOptions{}, err
	}
	if err := r.h.sortMembers(start); err != nil {
		return cacheOptions{}, err
	}
	o.full = r.h.hashMembers(start, 0)
	o.cleared = r.h.hashMembers(start, memberCacheID)
	r.h.dropMembers(start)
	return o, nil
}

// clearsCacheOpt reports whether BuildKit leaves a cache mount's ID and
// sharing out of its cache key, as checkShouldClearCacheOpts does.
func clearsCacheOpt(dest, id string, sharingSet bool) bool {
	if sharingSet {
		return true
	}
	if id == dest {
		return false
	}
	_, trimmed, ok := strings.Cut(id, "/")
	return !ok || trimmed != dest
}

// readEnv reads an exec operation's meta.env, a list of NAME=VALUE strings,
// and returns the hash of its names in their order. BuildKit lists the
// variables in the order of their last assignment (EnvList.ToArray in
// client/llb/meta.go at v0.34.0), and its cache key holds the list as it
// is, so the same variables in another order make a step miss. The order
// goes into the other fingerprint, and each value into the fingerprint of
// its variable.
//
// Consecutive RUN steps mostly share their environment, so an entry with
// the same JSON as the previous RUN step's entry at the same position takes
// that entry's name, check and hash instead of computing them again. A
// step's environment may hold at most the limit of one step.
func (r *stepReader) readEnv() (snapshot.Fingerprint, error) {
	if r.j.peek() != jsontext.KindBeginArray {
		return snapshot.Fingerprint{}, errNotValid
	}
	if err := r.j.next(); err != nil {
		return snapshot.Fingerprint{}, err
	}
	h := r.h
	next, nextRaw, nextEnds := h.next[:0], h.nextRaw[:0], h.nextEnds[:0]
	same := true
	for r.j.peek() != jsontext.KindEndArray {
		if r.j.peek() != jsontext.KindString {
			return snapshot.Fingerprint{}, errNotValid
		}
		if len(next) == h.lim.stepEnvVars {
			return snapshot.Fingerprint{}, &LimitError{What: "environment variables of one step", Limit: int64(h.lim.stepEnvVars)}
		}
		raw, err := r.j.readValue()
		if err != nil {
			return snapshot.Fingerprint{}, err
		}
		k := len(next)
		nextRaw = append(nextRaw, raw...)
		nextEnds = append(nextEnds, len(nextRaw))
		if k < len(h.last) && bytes.Equal(raw, h.lastRaw[h.lastStart(k):h.lastEnds[k]]) {
			next = append(next, h.last[k])
			continue
		}
		same = false
		entry, err := r.j.unquoteBytes(raw)
		if err != nil {
			return snapshot.Fingerprint{}, err
		}
		name, value, hasValue := bytes.Cut(entry, []byte("="))
		var e envEntry
		if k < len(h.last) && h.last[k].name == string(name) {
			e = envEntry{name: h.last[k].name, withheld: h.last[k].withheld}
		} else {
			e = envEntry{name: string(name), withheld: isCredentialName(string(name))}
		}
		switch {
		case e.withheld:
		case hasValue:
			e.value = h.sumFields(tagEnv, name, value)
		default:
			e.value = h.sumFields(tagEnvNoValue, name)
		}
		next = append(next, e)
	}
	if err := r.j.next(); err != nil {
		return snapshot.Fingerprint{}, err
	}
	r.sameEnv = same && len(next) == len(h.last)
	h.varsValid = h.varsValid && r.sameEnv
	h.last, h.next = next, h.last
	h.lastRaw, h.nextRaw = nextRaw, h.lastRaw
	h.lastEnds, h.nextEnds = nextEnds, h.lastEnds
	r.env = h.last
	h.scrap = h.scrap[:0]
	for _, e := range r.env {
		h.scrap = binary.AppendUvarint(h.scrap, uint64(len(e.name)))
		h.scrap = append(h.scrap, e.name...)
	}
	return h.sum(tagEnvOrder, h.scrap), nil
}

// lastStart returns where the previous RUN step's entry k starts in lastRaw.
func (h *hasher) lastStart(k int) int {
	if k == 0 {
		return 0
	}
	return h.lastEnds[k-1]
}

// envVars returns the fingerprint of each variable, sorted by name, and the
// key of the list. A variable set more than once hashes its values in
// order, and is withheld when any of them is: when its name looks like a
// credential, or when redaction changed the entry. It sorts positions in
// the environment, which the next RUN step reads in its own order. The list
// stays valid until the hasher reads the next definition.
//
// A step whose entries are the previous RUN step's, byte for byte, takes
// that step's list, unless redaction changed an entry of either: the same
// text then need not be the same value.
func (r *stepReader) envVars() ([]snapshot.EnvVar, snapshot.Fingerprint) {
	if len(r.env) == 0 {
		return nil, snapshot.Fingerprint{}
	}
	if r.sameEnv && r.h.varsValid && !r.h.varsChanged && len(r.changedEnv) == 0 {
		return r.h.vars, r.h.envKey
	}
	changed, all := r.changedPositions()
	order := r.h.order[:0]
	for i := range r.env {
		order = append(order, i)
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(r.env[a].name, r.env[b].name) })
	r.h.order = order
	out := r.h.vars[:0]
	for i := 0; i < len(order); {
		first := r.env[order[i]]
		j := i + 1
		for j < len(order) && r.env[order[j]].name == first.name {
			j++
		}
		v := snapshot.EnvVar{Name: first.name, Value: first.value}
		for _, k := range order[i:j] {
			v.Withheld = v.Withheld || r.env[k].withheld || all || k < len(changed) && changed[k]
		}
		switch {
		case v.Withheld:
			v.Value = snapshot.Fingerprint{}
		case j-i > 1:
			r.h.scrap = r.h.scrap[:0]
			for _, k := range order[i:j] {
				r.h.scrap = append(r.h.scrap, r.env[k].value[:]...)
			}
			v.Value = r.h.sum(tagEnvRepeated, r.h.scrap)
		}
		out = append(out, v)
		i = j
	}
	r.h.vars, r.h.envKey = out, r.h.envListKey(out)
	r.h.varsValid, r.h.varsChanged = true, len(r.changedEnv) > 0
	return out, r.h.envKey
}

// changedPositions marks, for each entry of the environment, whether
// redaction changed it, in any order. A position that the environment does
// not have means that redaction read another list than the hasher, so all
// is set, and every variable is withheld.
func (r *stepReader) changedPositions() (changed []bool, all bool) {
	if len(r.changedEnv) == 0 {
		return nil, false
	}
	changed = slices.Grow(r.h.changed[:0], len(r.env))[:len(r.env)]
	clear(changed)
	r.h.changed = changed
	for _, k := range r.changedEnv {
		if k < 0 || k >= len(changed) {
			return changed, true
		}
		changed[k] = true
	}
	return changed, false
}

// envListKey returns a hash that identifies a whole environment, sorted by
// name.
func (h *hasher) envListKey(env []snapshot.EnvVar) snapshot.Fingerprint {
	h.scrap = h.appendEnv(h.scrap[:0], env)
	return h.sum(tagEnvList, h.scrap)
}

// appendEnv appends each variable's name, after its length, and its value,
// or a marker for a withheld one, which counts only by its name.
func (*hasher) appendEnv(b []byte, env []snapshot.EnvVar) []byte {
	for _, e := range env {
		b = binary.AppendUvarint(b, uint64(len(e.Name)))
		b = append(b, e.Name...)
		if e.Withheld {
			b = append(b, 1)
		} else {
			b = append(b, 0)
			b = append(b, e.Value[:]...)
		}
	}
	return b
}

// file reads the members of a file operation, and notes the files that its
// copy actions read from another step.
func (r *stepReader) file(name []byte) (snapshot.Fingerprint, uint8, error) {
	if string(name) == "actions" && r.j.peek() == jsontext.KindBeginArray {
		f, err := r.h.array(r.action)
		return f, 0, err
	}
	f, err := r.h.value()
	return f, 0, err
}

// action reads one file action: {"Action":{"copy":{"src":...}},
// "secondaryInput":N}. BuildKit leaves a zero secondaryInput and an empty
// src out of the JSON, so a missing one is input 0 or the whole input
// (pb.FileAction and pb.FileActionCopy at v0.34.0), and a copy reads src
// from that input unless it is -1 (FileOp.CacheMap in
// solver/llbsolver/ops/file.go). An Action, copy, src or secondaryInput of
// another JSON type than BuildKit writes makes the definition malformed.
func (r *stepReader) action() (snapshot.Fingerprint, error) {
	if r.j.peek() != jsontext.KindBeginObject {
		return r.h.value()
	}
	input := 0
	src, isCopy := "", false
	f, err := r.h.object(func(name []byte) (snapshot.Fingerprint, uint8, error) {
		switch string(name) {
		case "secondaryInput":
			f, n, err := r.index()
			input = n
			return f, 0, err
		case "Action":
			if r.j.peek() != jsontext.KindBeginObject {
				return snapshot.Fingerprint{}, 0, errNotValid
			}
			f, err := r.h.object(func(name []byte) (snapshot.Fingerprint, uint8, error) {
				if string(name) != "copy" {
					f, err := r.h.value()
					return f, 0, err
				}
				if r.j.peek() != jsontext.KindBeginObject {
					return snapshot.Fingerprint{}, 0, errNotValid
				}
				isCopy = true
				f, err := r.h.object(func(name []byte) (snapshot.Fingerprint, uint8, error) {
					if string(name) != "src" {
						f, err := r.h.value()
						return f, 0, err
					}
					if r.j.peek() != jsontext.KindString {
						return snapshot.Fingerprint{}, 0, errNotValid
					}
					f, s, err := r.h.stringValue()
					src = string(s)
					return f, 0, err
				})
				return f, 0, err
			})
			return f, 0, err
		}
		f, err := r.h.value()
		return f, 0, err
	})
	if err != nil {
		return snapshot.Fingerprint{}, err
	}
	if isCopy && input >= 0 {
		if err := r.addRead(input, src); err != nil {
			return snapshot.Fingerprint{}, err
		}
	}
	return f, nil
}

// addRead notes that the step reads src from its input'th input.
func (r *stepReader) addRead(input int, src string) error {
	if r.h.sources == r.h.lim.sources {
		return &LimitError{What: "sources read from other steps", Limit: int64(r.h.lim.sources)}
	}
	r.h.sources++
	r.facts.reads = append(r.facts.reads, readFact{input: input, src: src})
	return nil
}

// maxSmallInt is the largest number that smallInt reads.
const maxSmallInt = 1 << 30

// smallInt returns the value of a JSON number that is a whole number from 0
// to maxSmallInt, or -1. It counts in a uint64, which ten digits cannot
// overflow, so that an int of 32 bits cannot wrap before the check.
func smallInt(raw jsontext.Value) int {
	if raw.Kind() != jsontext.KindNumber || len(raw) > 10 || !isDigits(string(raw)) {
		return -1
	}
	var n uint64
	for _, c := range raw {
		n = n*10 + uint64(c-'0')
	}
	if n > maxSmallInt {
		return -1
	}
	return int(n)
}

// sourceAttrs are the hashes of a source's attributes for each fingerprint:
// all of them, and without the attributes that a local build context, a Git
// source or an image leaves out.
type sourceAttrs struct {
	full, withoutLocal, withoutGit, withoutImage attrsHash
}

// attrsHash is the hash of a source's attributes, and whether none are left
// in it. BuildKit leaves an empty attribute map out of a step's definition,
// so attributes that a fingerprint leaves empty count as absent.
type attrsHash struct {
	hash  snapshot.Fingerprint
	empty bool
}

// source reads a source operation in one pass, and computes both of its
// command fingerprints.
//
// The command covers the whole operation, except an image's attributes that
// are not in BuildKit's cache key for it, which is the resolved manifest's
// digest, the platform and the layer limit (mainManifestKey and
// puller.CacheKey in source/containerimage/pull.go at v0.34.0): the resolve
// mode, the record type and the checksum, which only checks the digest, and
// an OCI layout's session and store.
//
// For the ancestor fingerprint, a local build context also leaves out the
// paths and patterns it sends, because a change to either would otherwise
// change the ancestors of every step after the context, and a Git source
// leaves out its ref and its git.* attributes, because the commit of a Git
// build context changes on every build. BuildKit gives a Git build context
// and ADD <git url> the same operation, so this applies to both.
func (r *stepReader) source() (snapshot.Fingerprint, uint8, error) {
	var identifier string
	var id snapshot.Fingerprint
	var attrs sourceAttrs
	start, err := r.h.readObject(func(name []byte) (snapshot.Fingerprint, uint8, error) {
		switch string(name) {
		case "identifier":
			if r.j.peek() == jsontext.KindString {
				f, s, err := r.h.stringValue()
				identifier, id = string(s), f
				return f, memberIdentifier, err
			}
		case "attrs":
			if r.j.peek() == jsontext.KindBeginObject {
				var err error
				attrs, err = r.sourceAttrs()
				return attrs.full.hash, memberAttrs, err
			}
		}
		f, err := r.h.value()
		return f, 0, err
	})
	if err != nil {
		return snapshot.Fingerprint{}, 0, err
	}
	if err := r.h.sortMembers(start); err != nil {
		return snapshot.Fingerprint{}, 0, err
	}
	kind, localName := classifySource(identifier)
	r.facts.source, r.facts.localName = kind, localName

	commandAttrs, contextAttrs, contextID := attrs.full, attrs.full, id
	switch kind {
	case snapshot.SourceImage:
		commandAttrs, contextAttrs = attrs.withoutImage, attrs.withoutImage
	case snapshot.SourceLocal:
		contextAttrs = attrs.withoutLocal
	case snapshot.SourceGit:
		contextAttrs = attrs.withoutGit
		contextID = r.h.sum(tagString, []byte(withoutGitRef(identifier)))
	case snapshot.SourceHTTP, snapshot.SourceOther:
	}
	body := r.sourceBody(start, id, commandAttrs)
	contextBody := r.sourceBody(start, contextID, contextAttrs)
	r.h.dropMembers(start)

	r.facts.command = r.h.commandFingerprint(snapshot.OpSource, body)
	r.facts.contextCommand = r.h.commandFingerprint(snapshot.OpSource, contextBody)
	return body, 0, nil
}

// sourceBody returns the hash of the source whose sorted members are held
// from start on, with the given hashes of its identifier and attributes,
// and without its attributes when none are left in them.
func (r *stepReader) sourceBody(start int, identifier snapshot.Fingerprint, attrs attrsHash) snapshot.Fingerprint {
	for i := start; i < len(r.h.held); i++ {
		switch {
		case r.h.held[i].flags&memberIdentifier != 0:
			r.h.held[i].value = identifier
		case r.h.held[i].flags&memberAttrs != 0:
			r.h.held[i].value = attrs.hash
		}
	}
	var skip uint8
	if attrs.empty {
		skip = memberAttrs
	}
	return r.h.hashMembers(start, skip)
}

// sourceAttrs reads a source's attributes, notes the .dockerignore patterns
// of a local build context, and returns their hashes for each fingerprint.
func (r *stepReader) sourceAttrs() (sourceAttrs, error) {
	start, err := r.h.readObject(func(name []byte) (snapshot.Fingerprint, uint8, error) {
		flags := attrFlags(name)
		if string(name) == "local.excludepatterns" && r.j.peek() == jsontext.KindString {
			f, s, err := r.h.stringValue()
			r.facts.excludePatterns, r.facts.hasPatterns = string(s), true
			return f, flags, err
		}
		f, err := r.h.value()
		return f, flags, err
	})
	if err != nil {
		return sourceAttrs{}, err
	}
	if err := r.h.sortMembers(start); err != nil {
		return sourceAttrs{}, err
	}
	variant := func(skip uint8) attrsHash {
		empty := true
		for _, m := range r.h.held[start:] {
			empty = empty && m.flags&(skip|memberLeftOut) != 0
		}
		return attrsHash{hash: r.h.hashMembers(start, skip), empty: empty}
	}
	a := sourceAttrs{
		full:         variant(0),
		withoutLocal: variant(memberLocalDetail),
		withoutGit:   variant(memberGitAttr),
		withoutImage: variant(memberImageAttr),
	}
	r.h.dropMembers(start)
	return a, nil
}

// attrFlags returns the flags of a source attribute that some fingerprint
// leaves out.
func attrFlags(name []byte) uint8 {
	switch string(name) {
	case "local.followpaths", "local.excludepatterns":
		return memberLocalDetail
	case "image.resolvemode", "image.recordtype", "image.checksum", "oci.session", "oci.store":
		return memberImageAttr
	}
	if bytes.HasPrefix(name, []byte("git.")) {
		return memberGitAttr
	}
	return 0
}

// classifySource tells what a source identifier loads, and returns the
// name of a local build context.
func classifySource(identifier string) (snapshot.SourceKind, string) {
	switch {
	case strings.HasPrefix(identifier, "local://"):
		return snapshot.SourceLocal, strings.TrimPrefix(identifier, "local://")
	case strings.HasPrefix(identifier, "git://"):
		return snapshot.SourceGit, ""
	case strings.HasPrefix(identifier, "http://"), strings.HasPrefix(identifier, "https://"):
		return snapshot.SourceHTTP, ""
	case strings.HasPrefix(identifier, "docker-image://"), strings.HasPrefix(identifier, "oci-layout://"):
		return snapshot.SourceImage, ""
	}
	return snapshot.SourceOther, ""
}

// withoutGitRef returns a Git source identifier, git://host/path#ref:subdir,
// without its ref, which can be a commit.
func withoutGitRef(identifier string) string {
	repo, fragment, ok := strings.Cut(identifier, "#")
	if !ok {
		return identifier
	}
	if _, subdir, ok := strings.Cut(fragment, ":"); ok {
		return repo + "#:" + subdir
	}
	return repo
}
