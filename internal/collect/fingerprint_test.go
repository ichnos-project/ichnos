package collect

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ichnos-project/ichnos/pkg/snapshot"
)

// canonical returns the canonical hash of one JSON value.
func canonical(h *hasher, v string) (snapshot.Fingerprint, error) {
	h.reset([]byte(v))
	f, err := h.value()
	if err == nil {
		err = h.j.end()
	}
	return f, err
}

// refSum is sum, written out independently of the hasher.
func refSum(tag byte, data []byte) snapshot.Fingerprint {
	return sha256.Sum256(slices.Concat([]byte(fingerprintPrefix), []byte{tag}, data))
}

// refHash is the canonical hash as fingerprint version 1 defines it,
// computed from a decoded value, with numbers as json.Number. It is the
// reference for the hasher, which streams.
func refHash(tb testing.TB, v any) snapshot.Fingerprint {
	tb.Helper()
	switch x := v.(type) {
	case nil:
		return refSum(tagNull, nil)
	case bool:
		if x {
			return refSum(tagTrue, nil)
		}
		return refSum(tagFalse, nil)
	case json.Number:
		return refSum(tagNumber, []byte(x))
	case string:
		return refSum(tagString, []byte(x))
	case []any:
		var data []byte
		for _, e := range x {
			f := refHash(tb, e)
			data = append(data, f[:]...)
		}
		return refSum(tagArray, data)
	case map[string]any:
		type pair struct{ name, value snapshot.Fingerprint }
		var pairs []pair
		for k, e := range x {
			pairs = append(pairs, pair{refSum(tagString, []byte(k)), refHash(tb, e)})
		}
		slices.SortFunc(pairs, func(a, b pair) int { return bytes.Compare(a.name[:], b.name[:]) })
		var data []byte
		for _, p := range pairs {
			data = append(data, p.name[:]...)
			data = append(data, p.value[:]...)
		}
		return refSum(tagObject, data)
	}
	tb.Fatalf("refHash of %T", v)
	return snapshot.Fingerprint{}
}

func decodeNumbers(tb testing.TB, v string) any {
	tb.Helper()
	dec := json.NewDecoder(strings.NewReader(v))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		tb.Fatalf("decoding %s: %v", v, err)
	}
	return out
}

func TestCanonicalHashMatchesReference(t *testing.T) {
	h := newHasher(defaultSnapshotLimits())
	for _, v := range []string{
		`null`, `true`, `false`, `0`, `-1.5e3`, `""`, `"a\u00e9\ud83d\ude00"`, `[]`, `{}`,
		`{"a":1,"b":[1,2,{"c":null}]}`, `[[[]],{"":{"":""}}]`,
	} {
		got, err := canonical(h, v)
		if err != nil {
			t.Fatalf("canonical(%s) error = %v", v, err)
		}
		if want := refHash(t, decodeNumbers(t, v)); got != want {
			t.Errorf("canonical(%s) = %x, want %x", v, got, want)
		}
	}
}

// TestCanonicalHashWithCachedNames checks that the hashes of member names
// that the hasher caches are those of the reference, and that the cache
// stays within its bounds.
func TestCanonicalHashWithCachedNames(t *testing.T) {
	long := strings.Repeat("n", maxCachedNameBytes+1)
	v := `[` + object(namesBeforeCache, "0") + `,{"` + long + `":2,"` + long + `x":3},` + object(2*maxCachedNames, "1") + `]`
	h := newHasher(defaultSnapshotLimits())
	for range 2 {
		got, err := canonical(h, v)
		if err != nil {
			t.Fatalf("canonical() error = %v", err)
		}
		if want := refHash(t, decodeNumbers(t, v)); got != want {
			t.Errorf("canonical() with cached names = %x, want %x", got, want)
		}
	}
	if len(h.names) != maxCachedNames {
		t.Errorf("the cache holds %d names, want %d", len(h.names), maxCachedNames)
	}
	for name := range h.names {
		if len(name) > maxCachedNameBytes {
			t.Errorf("the cache holds a name of %d bytes", len(name))
		}
	}
}

// TestFingerprintsArePinned pins fingerprint version 1. If it fails, the
// rules changed, and snapshot.FingerprintVersion must change with them.
func TestFingerprintsArePinned(t *testing.T) {
	const value = `{"args":["/bin/sh","-c","make"],"cwd":"/app","n":3}`
	h := newHasher(defaultSnapshotLimits())
	got, err := canonical(h, value)
	if err != nil {
		t.Fatal(err)
	}
	want := "9bba0c40769feb992a9f1b37b1a56f86dc80d1ad2219930eb35c7deeeba789ea"
	if got.String() != want || refHash(t, decodeNumbers(t, value)).String() != want {
		t.Errorf("canonical() = %s, reference %s, want %s", got, refHash(t, decodeNumbers(t, value)), want)
	}
	facts := mustReadOp(t, execStep(t, nil))
	for name, tt := range map[string]struct {
		got  snapshot.Fingerprint
		want string
	}{
		"command":            {facts.command, "5026a67925a4149ead0dffe3b0f36b92105501d20ee55f4ff820b43ae2d5465b"},
		"other":              {facts.other, "8b1b35fe7206eb058fda44d7e06a42d03d95deca959f6a6c0a36b434a46bf4ab"},
		"environment (MODE)": {facts.env[0].Value, "8517b5286a1f2b09bda259f25f2f0386d291bea6abe5ef0e260d5fc9db4876d8"},
		"self":               {h.self(facts), "25b9ac0469990308610b71c38de7cd33a5f604f078c6fafa6a1bc6930591f590"},
	} {
		if tt.got.String() != tt.want {
			t.Errorf("%s fingerprint = %s, want %s", name, tt.got, tt.want)
		}
	}
	args := refHash(t, decodeNumbers(t, `["/bin/sh","-c","make"]`))
	if want := refSum(tagCommand, slices.Concat([]byte("\x04exec\x20"), args[:])); facts.command != want {
		t.Errorf("command fingerprint = %s, reference %s", facts.command, want)
	}
	if want := refSum(tagEnv, []byte("\x04MODE\x04fast")); facts.env[0].Value != want {
		t.Errorf("MODE fingerprint = %s, reference %s", facts.env[0].Value, want)
	}
	envKey := refSum(tagEnvList, slices.Concat([]byte("\x04MODE\x00"), facts.env[0].Value[:], []byte("\x04PATH\x00"), facts.env[1].Value[:]))
	if want := refSum(tagSelf, slices.Concat(facts.contextCommand[:], facts.other[:], envKey[:])); h.self(facts) != want || facts.envKey != envKey {
		t.Errorf("self fingerprint = %s, reference %s", h.self(facts), want)
	}
}

// TestDerivedFingerprintsArePinned pins the fingerprints that combine
// others, each with every tag and encoding it uses. If it fails, the rules
// changed, and snapshot.FingerprintVersion must change with them.
func TestDerivedFingerprintsArePinned(t *testing.T) {
	h := newHasher(defaultSnapshotLimits())
	envValue := func(env ...any) snapshot.Fingerprint {
		f := mustReadOp(t, execStep(t, func(_, _, m map[string]any) { m["env"] = env }))
		if len(f.env) != 1 {
			t.Fatalf("environment %v gave %+v, want one variable", env, f.env)
		}
		return f.env[0].Value
	}
	ancestor := func(steps ...redactedStep) snapshot.Fingerprint {
		s := newSnapshotOf(t, oneBuild(steps...))
		return s.Platforms[0].Steps[len(steps)-1].Fingerprints.Ancestor
	}
	localOp := string(sourceStep(t, "local://context", map[string]string{
		"local.followpaths": `["app.txt"]`, "local.excludepatterns": `["*.log"]`, "local.sharedkeyhint": "context",
	}))
	gitOp := string(sourceStep(t, "git://github.com/ichnos-project/lab#main:app", map[string]string{"git.fullurl": "https://github.com/ichnos-project/lab.git"}))
	cleared := mustReadOp(t, execStep(t, func(_, e, _ map[string]any) {
		e["mounts"] = []any{map[string]any{"dest": "/root/.cache", "mountType": 3, "cacheOpt": map[string]any{"ID": "pip"}}}
	}))
	for name, tt := range map[string]struct {
		got  snapshot.Fingerprint
		want string
	}{
		"ancestor with two inputs and an output index": {ancestor(redStep(imageOp), redStep(runOp("a"), in(0)),
			redStep(`{"Op":{"merge":{"inputs":[{"input":0},{"input":1}]}}}`, in(1), stepInput{step: 0, output: 1})), "464fa015665a0d0d819dddccfbb2ecd4d99d4855a630dec4a424751b4528f3a0"},
		"build argument":                   {h.buildArg(buildArg{name: "MODE", value: "fast"}).Value, "93f5db5360adc17bc32af21b424eb45f41f7069b8005d660e53c2893ac9fcbd5"},
		"variable set twice":               {envValue("A=1", "A=2"), "5fe2458ecae04a3204ed63f9c04da1ff89256fe5e6a29042c857a10cd81b7dcc"},
		"variable with no =":               {envValue("A"), "06f39c104ef817550268649a2bab69d127666f7f65df03975eb7d62b6d36e950"},
		"local context, for its ancestors": {mustReadOp(t, jsontext.Value(localOp)).contextCommand, "56fb8a23bb2ce1c51774fc3f52fffbc3e7c7938e42c94fb0be0b1030bb288a45"},
		"local context, ancestor":          {ancestor(redStep(localOp)), "de90acc3cdfe014e3c9cf444470bad32d4c7de75d18d31895ffb906a1cdf22c7"},
		"Git source, for its ancestors":    {mustReadOp(t, jsontext.Value(gitOp)).contextCommand, "75338441219ffe8686ce989e24ee91eb1273a01e53e84a71a1debb83d4cfc5cb"},
		"Git source, ancestor":             {ancestor(redStep(gitOp)), "71c4600c5ce3c387b6bad394bc1f3cdac4c4e615e2c2d0ebb144d91929d9691f"},
		"cache mount with a cleared ID":    {cleared.other, "c65d7206852396f3df192bd31a1b3ba6bbabf7dd4d865c95fb1921062e1244c2"},
	} {
		if tt.got.String() != tt.want {
			t.Errorf("%s fingerprint = %s, want %s", name, tt.got, tt.want)
		}
	}
}

// TestFingerprintPrefixNamesVersion checks that the prefix of every hash
// names the fingerprint version that a snapshot declares.
func TestFingerprintPrefixNamesVersion(t *testing.T) {
	if want := fmt.Sprintf("ichnos fingerprint v%d\x00", snapshot.FingerprintVersion); fingerprintPrefix != want {
		t.Errorf("fingerprintPrefix = %q, want %q", fingerprintPrefix, want)
	}
}

func TestCanonicalHashIgnoresOrderAndSpelling(t *testing.T) {
	h := newHasher(defaultSnapshotLimits())
	for _, tt := range [][2]string{
		{`{"a":1,"b":[1,2,{"c":null,"d":"e"}]}`, ` { "b" : [ 1 , 2 , { "d" : "e" , "c" : null } ] , "a" : 1 } `},
		{`"A"`, `"\u0041"`},
		{`{"a":"x/y"}`, `{"\u0061":"x\/y"}`},
		{`[]`, "[\n]"},
	} {
		a, errA := canonical(h, tt[0])
		b, errB := canonical(h, tt[1])
		if errA != nil || errB != nil || a != b {
			t.Errorf("canonical(%s), canonical(%s) = %x, %v, %x, %v, want equal", tt[0], tt[1], a, errA, b, errB)
		}
	}
}

func TestCanonicalHashDistinguishes(t *testing.T) {
	h := newHasher(defaultSnapshotLimits())
	for _, tt := range [][2]string{
		{`1`, `"1"`},
		{`1`, `1.0`},
		{`[]`, `{}`},
		{`null`, `false`},
		{`true`, `false`},
		{`""`, `null`},
		{`[1,2]`, `[2,1]`},
		{`{"a":1}`, `{"a":2}`},
		{`{"a":1}`, `{"b":1}`},
		{`{"ab":"c"}`, `{"a":"bc"}`},
		{`["ab"]`, `["a","b"]`},
		{`[[]]`, `[]`},
		{`{"a":{}}`, `{"a":[]}`},
		{`{"a":1,"b":2}`, `{"a":2,"b":1}`},
		{`{"a":[]}`, `{}`},
		{`[{}]`, `[{},{}]`},
	} {
		a, errA := canonical(h, tt[0])
		b, errB := canonical(h, tt[1])
		if errA != nil || errB != nil || a == b {
			t.Errorf("canonical(%s), canonical(%s) = %x, %v, %x, %v, want different", tt[0], tt[1], a, errA, b, errB)
		}
	}
}

func TestCanonicalHashRejectsDuplicateNames(t *testing.T) {
	h := newHasher(defaultSnapshotLimits())
	for _, v := range []string{
		`{"a":1,"a":2}`, `{"a":1,"a":1}`, `{"x":{"a":1,"a":2}}`, `[{"a":1,"a":1}]`, `{"a":1,"\u0061":2}`,
	} {
		if _, err := canonical(h, v); !errors.Is(err, errDuplicateName) {
			t.Errorf("canonical(%s) error = %v, want errDuplicateName", v, err)
		}
	}
}

func TestCanonicalHashRejectsInvalidJSON(t *testing.T) {
	h := newHasher(defaultSnapshotLimits())
	for _, v := range []string{``, `0A0`, `1 2`, `{} x`, `[`, `{"a"}`, `"\xff"`} {
		if f, err := canonical(h, v); err == nil {
			t.Errorf("canonical(%q) = %x, want an error", v, f)
		}
	}
	if _, err := h.readOp([]byte(`{"Op":{}} {}`), nil); !errors.Is(err, ErrMalformedRecord) {
		t.Errorf("readOp() of a definition with data after it error = %v, want ErrMalformedRecord", err)
	}
}

// execStep returns a RUN step's definition in the shape BuildKit writes, after
// edit has changed it.
func execStep(tb testing.TB, edit func(root, exec, meta map[string]any)) jsontext.Value {
	tb.Helper()
	meta := map[string]any{
		"args":       []any{"/bin/sh", "-c", "make"},
		"env":        []any{"PATH=/usr/bin:/bin", "MODE=fast"},
		"cwd":        "/app",
		"proxy_env":  map[string]any{"http_proxy": "http://proxy.example:3128"},
		"extraHosts": []any{map[string]any{"Host": "db", "IP": "10.0.0.1"}},
	}
	exec := map[string]any{
		"meta": meta,
		"mounts": []any{
			map[string]any{"dest": "/"},
			map[string]any{"dest": "/root/.cache", "mountType": 3, "cacheOpt": map[string]any{"ID": "/root/.cache"}},
		},
	}
	root := map[string]any{
		"Op":          map[string]any{"exec": exec},
		"platform":    map[string]any{"Architecture": "amd64", "OS": "linux"},
		"constraints": map[string]any{},
	}
	if edit != nil {
		edit(root, exec, meta)
	}
	return jsontext.Value(mustJSON(tb, root))
}

func mustReadOp(tb testing.TB, op jsontext.Value) opFacts {
	tb.Helper()
	f, err := newHasher(defaultSnapshotLimits()).readOp(op, nil)
	if err != nil {
		tb.Fatalf("readOp(%s) error = %v", op, err)
	}
	return f
}

// changedParts names the fingerprints that differ between two steps.
func changedParts(h *hasher, a, b opFacts) []string {
	var out []string
	if a.command != b.command {
		out = append(out, "command")
	}
	if !slices.Equal(a.env, b.env) {
		out = append(out, "env")
	}
	if a.other != b.other {
		out = append(out, "other")
	}
	if h.self(a) != h.self(b) {
		out = append(out, "self")
	}
	return out
}

func TestReadOpExecParts(t *testing.T) {
	base := mustReadOp(t, execStep(t, nil))
	if base.kind != snapshot.OpExec || base.command != base.contextCommand {
		t.Fatalf("readOp() = kind %q, command %x, contextCommand %x", base.kind, base.command, base.contextCommand)
	}
	if got := []string{base.env[0].Name, base.env[1].Name}; !slices.Equal(got, []string{"MODE", "PATH"}) {
		t.Fatalf("env names = %q, want sorted", got)
	}
	cacheMount := func(id string, sharing int) func(root, exec, meta map[string]any) {
		return func(_, exec, _ map[string]any) {
			opt := map[string]any{"ID": id}
			if sharing != 0 {
				opt["sharing"] = sharing
			}
			exec["mounts"] = []any{map[string]any{"dest": "/root/.cache", "mountType": 3, "cacheOpt": opt}}
		}
	}
	tests := []struct {
		name string
		a, b func(root, exec, meta map[string]any)
		want []string
	}{
		{"args", nil, func(_, _, m map[string]any) { m["args"] = []any{"/bin/sh", "-c", "make test"} }, []string{"command", "self"}},
		{"an environment value", nil, func(_, _, m map[string]any) { m["env"] = []any{"PATH=/usr/bin:/bin", "MODE=slow"} }, []string{"env", "self"}},
		{"an added variable", nil, func(_, _, m map[string]any) { m["env"] = []any{"PATH=/usr/bin:/bin", "MODE=fast", "X=1"} }, []string{"env", "other", "self"}},
		{"variables reordered", nil, func(_, _, m map[string]any) { m["env"] = []any{"MODE=fast", "PATH=/usr/bin:/bin"} }, []string{"other", "self"}},
		{
			"a variable set again, which BuildKit lists last", func(_, _, m map[string]any) { m["env"] = []any{"A=1", "B=2"} },
			func(_, _, m map[string]any) { m["env"] = []any{"B=2", "A=1"} },
			[]string{"other", "self"},
		},
		{
			"no environment and an empty one", func(_, _, m map[string]any) { delete(m, "env") },
			func(_, _, m map[string]any) { m["env"] = nil }, nil,
		},
		{"working directory", nil, func(_, _, m map[string]any) { m["cwd"] = "/src" }, []string{"other", "self"}},
		{"user", nil, func(_, _, m map[string]any) { m["user"] = "0:0" }, []string{"other", "self"}},
		{"platform", nil, func(r, _, _ map[string]any) { r["platform"] = map[string]any{"Architecture": "arm64", "OS": "linux"} }, []string{"other", "self"}},
		{"proxy settings", nil, func(_, _, m map[string]any) { m["proxy_env"] = map[string]any{"https_proxy": "http://other.example"} }, nil},
		{"no proxy settings", nil, func(_, _, m map[string]any) { delete(m, "proxy_env") }, nil},
		{"constraints", nil, func(r, _, _ map[string]any) { r["constraints"] = map[string]any{"filter": []any{"x"}} }, nil},
		{"an extra host's IP", nil, func(_, _, m map[string]any) {
			m["extraHosts"] = []any{map[string]any{"Host": "db", "IP": "10.0.0.2"}}
		}, nil},
		{"an extra host's name", nil, func(_, _, m map[string]any) {
			m["extraHosts"] = []any{map[string]any{"Host": "cache", "IP": "10.0.0.1"}}
		}, []string{"other", "self"}},
		{"a mount's selector", func(_, e, _ map[string]any) {
			e["mounts"] = []any{map[string]any{"dest": "/"}}
		}, func(_, e, _ map[string]any) {
			e["mounts"] = []any{map[string]any{"dest": "/", "selector": "/sub"}}
		}, []string{"other", "self"}},
		{"cache ID from the target to another", cacheMount("/root/.cache", 0), cacheMount("pip", 0), []string{"other", "self"}},
		{"cache ID from one name to another", cacheMount("pip", 0), cacheMount("npm", 0), nil},
		{"cache ID with a namespace", cacheMount("a//root/.cache", 0), cacheMount("b//root/.cache", 0), []string{"other", "self"}},
		{"cache ID and sharing when sharing is set", cacheMount("/root/.cache", 1), cacheMount("pip", 2), nil},
		{"cache sharing set", cacheMount("/root/.cache", 0), cacheMount("/root/.cache", 1), []string{"other", "self"}},
	}
	h := newHasher(defaultSnapshotLimits())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := mustReadOp(t, execStep(t, tt.a)), mustReadOp(t, execStep(t, tt.b))
			if got := changedParts(h, a, b); !slices.Equal(got, tt.want) {
				t.Errorf("changed fingerprints = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadOpEnv(t *testing.T) {
	envOf := func(entries ...any) opFacts {
		return mustReadOp(t, execStep(t, func(_, _, m map[string]any) { m["env"] = entries }))
	}
	secret := envOf("API_TOKEN=ichnos-fake-one", "A=1")
	other := envOf("API_TOKEN=ichnos-fake-two", "A=1")
	if !slices.Equal(secret.env, other.env) || secret.env[1].Name != "API_TOKEN" || !secret.env[1].Withheld || !secret.env[1].Value.IsZero() {
		t.Errorf("env of a credential variable = %+v and %+v, want withheld with no value", secret.env, other.env)
	}
	h := newHasher(defaultSnapshotLimits())
	if h.self(secret) != h.self(other) {
		t.Error("a withheld value changed the step's own fingerprint")
	}
	if a, b := envOf("A=1", "A=2"), envOf("A=2", "A=1"); slices.Equal(a.env, b.env) || len(a.env) != 1 {
		t.Errorf("a variable set twice gave %+v and %+v, want one variable whose value depends on the order", a.env, b.env)
	}
	if a, b, c := envOf("A=1", "A=2"), envOf("A=1", "A=3"), envOf("A=1"); slices.Equal(a.env, b.env) || slices.Equal(a.env, c.env) {
		t.Errorf("a variable set twice gave %+v, %+v and %+v, want its second value to count", a.env, b.env, c.env)
	}
	if a, b := envOf("A"), envOf("A="); slices.Equal(a.env, b.env) {
		t.Errorf("an entry with no = and one with an empty value gave the same %+v", a.env)
	}
	if a, b := envOf("A=b=c"), envOf("A=b"); len(a.env) != 1 || a.env[0].Name != "A" || slices.Equal(a.env, b.env) {
		t.Errorf("an = inside a value gave %+v", a.env)
	}
	if a := envOf("A=1", "TOKEN=x", "A=2"); len(a.env) != 2 || a.env[0].Withheld || !a.env[1].Withheld {
		t.Errorf("env = %+v, want A with its two values and TOKEN withheld", a.env)
	}
	if a := envOf([]any{}...); a.env != nil {
		t.Errorf("an empty environment gave %+v", a.env)
	}
	if a := mustReadOp(t, execStep(t, func(_, _, m map[string]any) { m["env"] = nil })); a.env != nil {
		t.Errorf("a null environment gave %+v", a.env)
	}
}

// TestReadOpReusesEnvEntries checks that an environment entry taken from
// the RUN step before gives what a fresh hasher computes, whether the entry
// at its position is the same, another value of the same variable, or
// another variable.
func TestReadOpReusesEnvEntries(t *testing.T) {
	h := newHasher(defaultSnapshotLimits())
	op := func(env ...any) jsontext.Value {
		return execStep(t, func(_, _, m map[string]any) { m["env"] = env })
	}
	for i, o := range []jsontext.Value{
		op("A=1", "B=2"), op("A=1", "B=2"), op("B=2", "A=1"), op("A=1", "B=3"), op("A=1"),
		op("A=1", "TOKEN=x"), op("A=1", "TOKEN=y"), op("A=1", "B"), op("A=1", "B="),
	} {
		got, err := h.readOp(o, nil)
		if err != nil {
			t.Fatalf("readOp() of step %d error = %v", i, err)
		}
		if want := mustReadOp(t, o).env; !slices.Equal(got.env, want) {
			t.Errorf("step %d environment = %+v, want %+v as a fresh hasher gives it", i, got.env, want)
		}
	}
}

func TestReadOpShapes(t *testing.T) {
	tests := []struct {
		op      string
		kind    snapshot.OpType
		wantErr bool
	}{
		{"", snapshot.OpNone, false},
		{`{}`, snapshot.OpNone, false},
		{`{"Op":{}}`, snapshot.OpNone, false},
		{`{"Op":null}`, snapshot.OpNone, false},
		{`{"Op":{"exec":{}}}`, snapshot.OpExec, false},
		{`{"Op":{"exec":{"meta":null,"mounts":null}}}`, snapshot.OpExec, false},
		{`{"Op":{"file":{"actions":[]}}}`, snapshot.OpFile, false},
		{`{"Op":{"source":{"identifier":"docker-image://docker.io/library/alpine:3.22"}}}`, snapshot.OpSource, false},
		{`{"Op":{"build":{}}}`, snapshot.OpBuild, false},
		{`{"Op":{"merge":{"inputs":[{"input":0}]}}}`, snapshot.OpMerge, false},
		{`{"Op":{"diff":{}}}`, snapshot.OpDiff, false},
		{`{"Op":{"future":{"x":1}}}`, snapshot.OpUnknown, false},
		{`{"Op":{"future":7}}`, snapshot.OpUnknown, false},
		{`{"Op":{"none":{}}}`, snapshot.OpUnknown, false},
		{`{"Op":{"exec":{},"file":{}}}`, "", true},
		{`{"Op":[]}`, "", true},
		{`{"Op":"exec"}`, "", true},
		{`{"Op":{"exec":[]}}`, "", true},
		{`{"Op":{"source":"local://context"}}`, "", true},
		{`{"Op":{"exec":{"meta":{"env":"A=1"}}}}`, "", true},
		{`{"Op":{"exec":{"meta":{"env":[1]}}}}`, "", true},
		{`{"Op":{"exec":{"meta":{"args":["a"],"args":["b"]}}}}`, "", true},
		{`{"Op":{"exec":{}},"platform":{},"platform":{}}`, "", true},
		{`{"Op":{"source":{"identifier":"local://context","attrs":{"local.excludepatterns":"[]","local.excludepatterns":"[]"}}}}`, "", true},
		{`{"Op":{"source":{"identifier":"a","identifier":"b"}}}`, "", true},
		{`{"constraints":{"":{"":{},"":""}}}`, "", true},
		{`{"Op":{"exec":{"meta":{"proxy_env":{"http_proxy":"a","http_proxy":"b"}}}}}`, "", true},
		{`{"Op":{"exec":{"meta":{"extraHosts":[{"Host":"h","IP":{"a":1,"a":2}}]}}}}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.op, func(t *testing.T) {
			var op jsontext.Value
			if tt.op != "" {
				op = jsontext.Value(tt.op)
			}
			got, err := newHasher(defaultSnapshotLimits()).readOp(op, nil)
			if tt.wantErr {
				if !errors.Is(err, ErrMalformedRecord) {
					t.Errorf("readOp() error = %v, want ErrMalformedRecord", err)
				}
				return
			}
			if err != nil || got.kind != tt.kind || got.command.IsZero() || got.other.IsZero() {
				t.Errorf("readOp() = kind %q, command %x, other %x, error %v, want kind %q", got.kind, got.command, got.other, err, tt.kind)
			}
		})
	}
	h := newHasher(defaultSnapshotLimits())
	none, empty := mustReadOp(t, nil), mustReadOp(t, jsontext.Value(`{"Op":{}}`))
	if none.command != empty.command || none.other == empty.other {
		t.Errorf("no definition and an empty operation: %v", changedParts(h, none, empty))
	}
	if a, b := mustReadOp(t, jsontext.Value(`{"Op":{"merge":{}}}`)), mustReadOp(t, jsontext.Value(`{"Op":{"diff":{}}}`)); a.command == b.command {
		t.Error("two kinds of operation with the same body have the same command fingerprint")
	}
	absent := mustReadOp(t, jsontext.Value(`{"Op":{"exec":{"network":1}}}`))
	for _, op := range []string{`{"Op":{"exec":{"meta":null,"network":1}}}`, `{"Op":{"exec":{"mounts":null,"network":1}}}`} {
		if got := mustReadOp(t, jsontext.Value(op)); got.command != absent.command || got.other != absent.other {
			t.Errorf("readOp(%s) differs from the same step without the null member", op)
		}
	}
}

func sourceStep(tb testing.TB, identifier string, attrs map[string]string) jsontext.Value {
	tb.Helper()
	src := map[string]any{"identifier": identifier}
	if attrs != nil {
		src["attrs"] = attrs
	}
	return mustJSON(tb, map[string]any{"Op": map[string]any{"source": src}, "constraints": map[string]any{}})
}

func TestReadOpSources(t *testing.T) {
	local := func(edit func(map[string]string)) opFacts {
		attrs := map[string]string{
			"local.followpaths":     `["app.txt","config.txt"]`,
			"local.excludepatterns": `["*.log","!keep.log"]`,
			"local.sharedkeyhint":   "context",
		}
		if edit != nil {
			edit(attrs)
		}
		return mustReadOp(t, sourceStep(t, "local://context", attrs))
	}
	base := local(nil)
	if base.source != snapshot.SourceLocal || base.localName != "context" || !base.hasPatterns || base.excludePatterns != `["*.log","!keep.log"]` {
		t.Fatalf("readOp() of a local source = %+v", base)
	}
	gitStep := func(identifier string, attrs map[string]string) opFacts {
		return mustReadOp(t, sourceStep(t, identifier, attrs))
	}
	image := func(attrs map[string]string) opFacts {
		return mustReadOp(t, sourceStep(t, "docker-image://docker.io/library/alpine:3.22@sha256:"+strings.Repeat("0", 64), attrs))
	}
	const repo = "git://github.com/ichnos-project/lab"
	gitBase := gitStep(repo+"#main:app", map[string]string{"git.fullurl": "https://github.com/ichnos-project/lab.git", "git.checksum": "abc"})
	tests := []struct {
		name               string
		a, b               opFacts
		command, ancestors bool
	}{
		{"followpaths", base, local(func(a map[string]string) { a["local.followpaths"] = `["app.txt"]` }), true, false},
		{"excludepatterns", base, local(func(a map[string]string) { a["local.excludepatterns"] = `["*.tmp"]` }), true, false},
		{"no excludepatterns", base, local(func(a map[string]string) { delete(a, "local.excludepatterns") }), true, false},
		{"sharedkeyhint", base, local(func(a map[string]string) { a["local.sharedkeyhint"] = "other" }), true, true},
		{"includepattern", base, local(func(a map[string]string) { a["local.includepattern"] = `["src"]` }), true, true},
		{"context name", base, mustReadOp(t, sourceStep(t, "local://assets", map[string]string{
			"local.followpaths": `["app.txt","config.txt"]`, "local.excludepatterns": `["*.log","!keep.log"]`, "local.sharedkeyhint": "context",
		})), true, true},
		{"Git ref", gitBase, gitStep(repo+"#0123456789abcdef0123456789abcdef01234567:app", map[string]string{
			"git.fullurl": "https://github.com/ichnos-project/lab.git", "git.checksum": "abc",
		}), true, false},
		{"Git attributes", gitBase, gitStep(repo+"#main:app", map[string]string{"git.fullurl": "https://example.com/lab.git", "git.checksum": "def", "git.mtime": "commit"}), true, false},
		{"Git subdirectory", gitBase, gitStep(repo+"#main:web", map[string]string{"git.fullurl": "https://github.com/ichnos-project/lab.git", "git.checksum": "abc"}), true, true},
		{"Git repository", gitBase, gitStep("git://github.com/ichnos-project/other#main:app", map[string]string{"git.fullurl": "https://github.com/ichnos-project/lab.git", "git.checksum": "abc"}), true, true},
		{"HTTP URL", mustReadOp(t, sourceStep(t, "https://files.example/a.tgz", nil)), mustReadOp(t, sourceStep(t, "https://files.example/b.tgz", nil)), true, true},
		{"image resolve mode", image(nil), image(map[string]string{"image.resolvemode": "pull"}), false, false},
		{"image record type", image(nil), image(map[string]string{"image.recordtype": "internal"}), false, false},
		{"image checksum", image(nil), image(map[string]string{"image.checksum": "sha256:" + strings.Repeat("a", 64)}), false, false},
		{"image layer limit", image(nil), image(map[string]string{"image.layerlimit": "1"}), true, true},
		{
			"OCI layout session and store", mustReadOp(t, sourceStep(t, "oci-layout://store/alpine@sha256:abc", map[string]string{"oci.session": "a", "oci.store": "b"})),
			mustReadOp(t, sourceStep(t, "oci-layout://store/alpine@sha256:abc", map[string]string{"oci.session": "c", "oci.store": "d"})), false, false,
		},
		{
			"an image attribute on another source", mustReadOp(t, sourceStep(t, "https://files.example/a.tgz", nil)),
			mustReadOp(t, sourceStep(t, "https://files.example/a.tgz", map[string]string{"image.resolvemode": "pull"})), true, true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.command != tt.b.command; got != tt.command {
				t.Errorf("command fingerprint changed = %v, want %v", got, tt.command)
			}
			if got := tt.a.contextCommand != tt.b.contextCommand; got != tt.ancestors {
				t.Errorf("fingerprint for the ancestors changed = %v, want %v", got, tt.ancestors)
			}
		})
	}
	for identifier, want := range map[string]snapshot.SourceKind{
		"local://context": snapshot.SourceLocal, "git://github.com/o/r": snapshot.SourceGit, "http://h/f": snapshot.SourceHTTP, "https://h/f": snapshot.SourceHTTP,
		"docker-image://docker.io/library/alpine:3.22": snapshot.SourceImage, "oci-layout://store": snapshot.SourceImage, "s3://b/k": snapshot.SourceOther,
	} {
		if got := mustReadOp(t, sourceStep(t, identifier, nil)).source; got != want {
			t.Errorf("source kind of %q = %v, want %v", identifier, got, want)
		}
	}
	if got := withoutGitRef("git://h/r"); got != "git://h/r" {
		t.Errorf("withoutGitRef() of an identifier with no ref = %q", got)
	}
}

func TestSmallInt(t *testing.T) {
	for raw, want := range map[string]int{
		`0`: 0, `7`: 7, `1073741824`: 1 << 30, `1073741825`: -1, `4294967297`: -1, `9999999999`: -1,
		`99999999999`: -1, `-1`: -1, `1.5`: -1, `1e3`: -1, `"1"`: -1, `null`: -1,
	} {
		if got := smallInt(jsontext.Value(raw)); got != want {
			t.Errorf("smallInt(%s) = %d, want %d", raw, got, want)
		}
	}
}

// TestReadOpCopies checks the paths that copy actions read, with
// BuildKit's defaults for the members it leaves out, and that a member of
// another type than BuildKit writes makes the definition malformed, in
// either order.
func TestReadOpCopies(t *testing.T) {
	op := jsontext.Value(`{"Op":{"file":{"actions":[
		{"Action":{"copy":{"src":"/app.txt","dest":"/app/"}},"input":0,"output":0,"secondaryInput":1},
		{"Action":{"mkdir":{"path":"/x"}},"input":0,"output":0,"secondaryInput":-1},
		{"Action":{"copy":{"src":"/from-scratch"}},"input":0,"secondaryInput":-1},
		{"Action":{"copy":{"src":"/other.txt"}},"secondaryInput":2,"input":0},
		{"Action":{"copy":{"src":"/first.txt"}}},
		{"secondaryInput":1,"Action":{"copy":{"dest":"/all/"}}}
	]}}}`)
	got := mustReadOp(t, op)
	want := []readFact{{input: 1, src: "/app.txt"}, {input: 2, src: "/other.txt"}, {input: 0, src: "/first.txt"}, {input: 1, src: ""}}
	if !slices.Equal(got.reads, want) {
		t.Errorf("reads = %+v, want %+v", got.reads, want)
	}
	for _, action := range []string{
		`{"Action":{"copy":{"src":7}},"secondaryInput":1}`,
		`{"secondaryInput":1,"Action":{"copy":{"src":7}}}`,
		`{"Action":{"copy":{"src":null}},"secondaryInput":1}`,
		`{"secondaryInput":1,"Action":{"copy":{"src":null}}}`,
		`{"Action":{"copy":"x"},"secondaryInput":1}`,
		`{"Action":7,"secondaryInput":1}`,
		`{"Action":{"copy":{"src":"/a"}},"secondaryInput":1.5}`,
		`{"secondaryInput":"1","Action":{"copy":{"src":"/a"}}}`,
		`{"Action":{"copy":{"src":"/a"}},"secondaryInput":-2}`,
		`{"Action":{"copy":{"src":"/a"}},"secondaryInput":null}`,
	} {
		op := `{"Op":{"file":{"actions":[` + action + `]}}}`
		if _, err := newHasher(defaultSnapshotLimits()).readOp(jsontext.Value(op), nil); !errors.Is(err, ErrMalformedRecord) {
			t.Errorf("readOp(%s) error = %v, want ErrMalformedRecord", op, err)
		}
	}
}

// TestReadOpChangedEnv checks that the positions that redaction changed
// withhold their variables in any order, and that a position the
// environment does not have withholds every variable.
func TestReadOpChangedEnv(t *testing.T) {
	op := jsontext.Value(`{"Op":{"exec":{"meta":{"env":["A=1","B=2","C=3"]}}}}`)
	for _, tt := range []struct {
		changed  []int
		withheld []bool
	}{
		{nil, []bool{false, false, false}},
		{[]int{2, 0}, []bool{true, false, true}},
		{[]int{1, 1}, []bool{false, true, false}},
		{[]int{0, 3}, []bool{true, true, true}},
		{[]int{-1}, []bool{true, true, true}},
	} {
		facts, err := newHasher(defaultSnapshotLimits()).readOp(op, tt.changed)
		if err != nil {
			t.Fatalf("readOp() error = %v", err)
		}
		var got []bool
		for _, e := range facts.env {
			got = append(got, e.Withheld)
		}
		if !slices.Equal(got, tt.withheld) {
			t.Errorf("readOp() with changed positions %v withholds %v, want %v", tt.changed, got, tt.withheld)
		}
	}
}

// TestReadOpMounts checks that the selector of each bind or cache mount is
// read, with BuildKit's defaults for the members it leaves out, that
// secret, SSH and tmpfs mounts read nothing, that a mount of a type that
// BuildKit does not name withholds the step, and that a member of another
// type than BuildKit writes makes the definition malformed, in either
// order.
func TestReadOpMounts(t *testing.T) {
	mounts := func(m ...string) jsontext.Value {
		return jsontext.Value(`{"Op":{"exec":{"meta":{"args":["go","mod","download"]},"mounts":[` + strings.Join(m, ",") + `]}}}`)
	}
	got := mustReadOp(t, mounts(
		`{"dest":"/"}`,
		`{"input":1,"selector":"/go.mod","dest":"/src/go.mod","output":-1,"readonly":true}`,
		`{"input":2,"dest":"/ctx","mountType":0}`,
		`{"input":1,"selector":"/cache","dest":"/cache","output":-1,"mountType":3,"cacheOpt":{"ID":"/cache"}}`,
		`{"input":1,"dest":"/tmp","output":-1,"mountType":4}`,
		`{"input":1,"dest":"/run/secrets/x","output":-1,"mountType":1,"secretOpt":{"ID":"x"}}`,
		`{"input":1,"dest":"/run/ssh","output":-1,"mountType":2,"SSHOpt":{"ID":"default"}}`,
		`{"input":-1,"dest":"/scratch"}`,
	))
	want := []readFact{{input: 0, src: ""}, {input: 1, src: "/go.mod"}, {input: 2, src: ""}, {input: 1, src: "/cache"}}
	if !slices.Equal(got.reads, want) || got.withheld {
		t.Errorf("reads = %+v, withheld %v, want %+v", got.reads, got.withheld, want)
	}
	if got := mustReadOp(t, mounts(`{"input":1,"selector":"/future","dest":"/f","mountType":5}`)); got.reads != nil || !got.withheld {
		t.Errorf("a mount of an unknown type gave reads %+v, withheld %v, want none and withheld", got.reads, got.withheld)
	}
	for _, m := range []string{
		`{"selector":7,"input":1}`, `{"input":1,"selector":7}`,
		`{"selector":null,"input":1}`, `{"input":1,"selector":null}`,
		`{"input":"1","selector":"/a"}`, `{"selector":"/a","input":"1"}`,
		`{"input":1.5}`, `{"input":-3}`, `{"input":null}`,
		`{"mountType":"0","input":1}`, `{"input":1,"mountType":"0"}`,
		`{"mountType":-3}`, `{"mountType":null}`, `{"mountType":1.5}`,
	} {
		if _, err := newHasher(defaultSnapshotLimits()).readOp(mounts(m), nil); !errors.Is(err, ErrMalformedRecord) {
			t.Errorf("readOp() of the mount %s error = %v, want ErrMalformedRecord", m, err)
		}
	}
}

func TestReadOpLimits(t *testing.T) {
	tests := []struct {
		what string
		set  func(*snapshotLimits, int)
		op   func(n int) string
	}{
		{"object members held while reading a step definition", func(l *snapshotLimits, n int) { l.heldMembers = n }, func(n int) string {
			return `{"Op":{"future":` + object(n, "0") + `}}`
		}},
		{"environment variables of one step", func(l *snapshotLimits, n int) { l.stepEnvVars = n }, distinctEnv},
		{"sources read from other steps", func(l *snapshotLimits, n int) { l.sources = n }, func(n int) string {
			return `{"Op":{"file":{"actions":` + list(n, `{"Action":{"copy":{"src":"/a"}},"secondaryInput":1}`) + `}}}`
		}},
		{"sources read from other steps", func(l *snapshotLimits, n int) { l.sources = n }, func(n int) string {
			return `{"Op":{"exec":{"mounts":` + list(n, `{"input":1,"selector":"/a","dest":"/a"}`) + `}}}`
		}},
	}
	for _, tt := range tests {
		t.Run(tt.what, func(t *testing.T) {
			for _, n := range []int{100, 101} {
				lim := defaultSnapshotLimits()
				tt.set(&lim, 100)
				_, err := newHasher(lim).readOp(jsontext.Value(tt.op(n)), nil)
				var le *LimitError
				if got, want := errors.As(err, &le) && le.What == tt.what && le.Limit == 100, n > 100; got != want || (n == 100 && err != nil) {
					t.Errorf("readOp() of %d with a limit of 100 error = %v, want LimitError %v", n, err, want)
				}
			}
			lim := defaultSnapshotLimits()
			tt.set(&lim, 1000)
			op := jsontext.Value(tt.op(1_000_000))
			h := newHasher(lim)
			grew := allocated(func() {
				var le *LimitError
				if _, err := h.readOp(op, nil); !errors.As(err, &le) {
					t.Errorf("readOp() of 1,000,000 with a limit of 1,000 error = %v, want LimitError", err)
				}
			})
			if grew > 1<<20 {
				t.Errorf("readOp() of 1,000,000 with a limit of 1,000 allocated %d bytes, want at most 1 MiB", grew)
			}
		})
	}
}

// distinctEnv returns a RUN step with n distinct environment variables.
func distinctEnv(n int) string {
	var b strings.Builder
	b.WriteString(`{"Op":{"exec":{"meta":{"env":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"V%d=1"`, i)
	}
	b.WriteString(`]}}}}`)
	return b.String()
}

// TestReadOpLargeEnvironment checks that a step of 1,000,000 distinct
// variables, in a definition of 11.9 MB, stops at the default limit of one
// step with little memory.
func TestReadOpLargeEnvironment(t *testing.T) {
	op := jsontext.Value(distinctEnv(1_000_000))
	h := newHasher(defaultSnapshotLimits())
	grew := allocated(func() {
		var le *LimitError
		if _, err := h.readOp(op, nil); !errors.As(err, &le) || le.Limit != maxStepEnvVars {
			t.Errorf("readOp() of 1,000,000 variables error = %v, want LimitError at %d", err, maxStepEnvVars)
		}
	})
	if grew > 8<<20 {
		t.Errorf("readOp() of 1,000,000 variables allocated %d bytes, want at most 8 MiB", grew)
	}
}

// TestReadOpAfterError checks that a definition that fails leaves nothing
// that the next one reuses: one that has two environments, which is an
// error, and one that fails after its environment.
func TestReadOpAfterError(t *testing.T) {
	h := newHasher(defaultSnapshotLimits())
	if _, err := h.readOp(jsontext.Value(`{"Op":{"exec":{"meta":{"env":["A=1"]},"meta":{"env":["B=1","C=1"]}}}}`), nil); !errors.Is(err, ErrMalformedRecord) {
		t.Fatalf("readOp() of two metas error = %v, want ErrMalformedRecord", err)
	}
	next := execStep(t, func(_, _, m map[string]any) { m["env"] = []any{"B=1", "C=1", "D=1"} })
	got, err := h.readOp(next, nil)
	if err != nil {
		t.Fatalf("readOp() after an error = %v", err)
	}
	if want := mustReadOp(t, next); !slices.Equal(got.env, want.env) || got.other != want.other {
		t.Errorf("readOp() after an error = %+v, want %+v as a fresh hasher gives it", got.env, want.env)
	}

	h = newHasher(defaultSnapshotLimits())
	if _, err = h.readOp(jsontext.Value(`{"Op":{"exec":{"meta":{"env":["A=1"]}}}}`), nil); err != nil {
		t.Fatalf("readOp() error = %v", err)
	}
	if _, err = h.readOp(jsontext.Value(`{"Op":{"exec":{"meta":{"env":["B=1"]},"mounts":"x"}}}`), nil); !errors.Is(err, ErrMalformedRecord) {
		t.Fatalf("readOp() of mounts that are not a list error = %v, want ErrMalformedRecord", err)
	}
	same := jsontext.Value(`{"Op":{"exec":{"meta":{"env":["B=1"]}}}}`)
	got, err = h.readOp(same, nil)
	if want := mustReadOp(t, same); err != nil || !slices.Equal(got.env, want.env) || got.envKey != want.envKey {
		t.Errorf("readOp() after a definition that failed after its environment = %+v, %v, want %+v", got.env, err, want.env)
	}
}

// TestReadOpMemory checks that arrays stream: a long list costs nothing,
// whatever its length.
func TestReadOpMemory(t *testing.T) {
	for name, op := range map[string]string{
		"list of numbers": `{"Op":{"future":` + list(1_000_000, "0") + `}}`,
		"list of lists":   `{"Op":{"exec":{"meta":{"args":` + list(300_000, "[[]]") + `}}}}`,
		"long command":    `{"Op":{"exec":{"meta":{"args":` + list(300_000, `"abcdefgh"`) + `}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHasher(defaultSnapshotLimits())
			v := jsontext.Value(op)
			grew := allocated(func() {
				if _, err := h.readOp(v, nil); err != nil {
					t.Errorf("readOp() error = %v", err)
				}
			})
			if grew > 1<<20 {
				t.Errorf("readOp() of %d bytes allocated %d bytes, want at most 1 MiB", len(op), grew)
			}
		})
	}
}

func FuzzCanonicalHash(f *testing.F) {
	for _, s := range []string{`{"a":1,"b":[1,2,{"c":null}]}`, `"\u0041"`, `[1.5e3,-0,true,false,null]`, `{"":{"":[]}}`} {
		f.Add([]byte(s))
	}
	h := newHasher(defaultSnapshotLimits())
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := canonical(h, string(data))
		valid := jsontext.Value(data).IsValid()
		withDuplicates := !valid && jsontext.Value(data).IsValid(jsontext.AllowDuplicateNames(true))
		switch {
		case valid && err != nil:
			t.Fatalf("canonical(%q) of valid JSON error = %v", data, err)
		case withDuplicates && !errors.Is(err, errDuplicateName):
			t.Fatalf("canonical(%q) of JSON with a duplicate name error = %v, want errDuplicateName", data, err)
		case !valid && !withDuplicates && err == nil:
			t.Fatalf("canonical(%q) of invalid JSON gave %x", data, got)
		case !valid:
			return
		}
		if want := refHash(t, decodeNumbers(t, string(data))); got != want {
			t.Fatalf("canonical(%q) = %x, want %x", data, got, want)
		}
	})
}

// reorder returns v with the members of every object in another order and
// other white space, keeping numbers as they are written.
func reorder(tb testing.TB, v jsontext.Value) jsontext.Value {
	tb.Helper()
	var buf bytes.Buffer
	var walk func(any)
	walk = func(x any) {
		switch y := x.(type) {
		case map[string]any:
			buf.WriteString("{ ")
			for i, k := range slices.Backward(slices.Sorted(maps.Keys(y))) {
				if i < len(y)-1 {
					buf.WriteString(" ,\n")
				}
				buf.Write(mustJSON(tb, k))
				buf.WriteString(" : ")
				walk(y[k])
			}
			buf.WriteString(" }")
		case []any:
			buf.WriteString("[ ")
			for i, e := range y {
				if i > 0 {
					buf.WriteString(" , ")
				}
				walk(e)
			}
			buf.WriteString(" ]")
		default:
			buf.Write(mustJSON(tb, y))
		}
	}
	walk(decodeNumbers(tb, string(v)))
	return buf.Bytes()
}

func FuzzReadOp(f *testing.F) {
	for _, path := range fixturePaths(f) {
		for _, b := range mustRedact(f, mustCollect(f, path)).builds {
			for _, st := range b.steps {
				if st.op != nil {
					f.Add([]byte(st.op))
				}
			}
		}
	}
	f.Add([]byte(execStep(f, nil)))
	f.Add([]byte(sourceStep(f, "git://h/r#main:sub", map[string]string{"git.checksum": "x"})))
	f.Fuzz(func(t *testing.T, raw []byte) {
		op, err := stepDefinition(raw)
		if err != nil || op == nil {
			return
		}
		lim := defaultSnapshotLimits()
		got, err := newHasher(lim).readOp(op, nil)
		if err != nil {
			var le *LimitError
			if !errors.Is(err, ErrMalformedRecord) && !errors.As(err, &le) {
				t.Fatalf("readOp(%s) error = %v, want a typed error", op, err)
			}
			return
		}
		if !op.IsValid() {
			t.Fatalf("readOp(%s) of a definition with a duplicate name gave no error", op)
		}
		if got.command.IsZero() || got.contextCommand.IsZero() || got.other.IsZero() {
			t.Fatalf("readOp(%s) gave a zero fingerprint: %+v", op, got)
		}
		for i, e := range got.env {
			if i > 0 && got.env[i-1].Name >= e.Name {
				t.Fatalf("readOp(%s) env is not sorted and distinct: %+v", op, got.env)
			}
			if e.Withheld != e.Value.IsZero() || (isCredentialName(e.Name) && !e.Withheld) {
				t.Fatalf("readOp(%s) env %+v is withheld wrongly", op, e)
			}
		}
		again, err := newHasher(lim).readOp(reorder(t, op), nil)
		if err != nil {
			t.Fatalf("readOp() of %s reordered error = %v", op, err)
		}
		if again.command != got.command || again.contextCommand != got.contextCommand || again.other != got.other ||
			!slices.Equal(again.env, got.env) || again.kind != got.kind || again.source != got.source {
			t.Fatalf("readOp() of %s changed when its members were reordered", op)
		}
	})
}
