package collect

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/url"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustRedact(tb testing.TB, r *Record) redacted {
	tb.Helper()
	out, err := redact(r)
	if err != nil {
		tb.Fatalf("redact() error = %v", err)
	}
	return out
}

// opJSON encodes a step definition for a test Record. A nil map is a step
// with no definition.
func opJSON(tb testing.TB, op map[string]any) jsontext.Value {
	tb.Helper()
	if op == nil {
		return nil
	}
	return jsontext.Value(mustJSON(tb, op))
}

// opTree decodes a step definition, with numbers as json.Number.
func opTree(tb testing.TB, op jsontext.Value) any {
	tb.Helper()
	if op == nil {
		return nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(op))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		tb.Fatalf("step definition %s is not valid JSON: %v", op, err)
	}
	return v
}

// opText returns a step definition as JSON, then each of its names and
// strings unescaped, then the text of each base64 string, so that a value
// shows up in a dump however the definition encodes it.
func opText(op jsontext.Value) string {
	var v any
	if err := json.Unmarshal(op, &v); err != nil {
		return string(op) + "\n"
	}
	return string(op) + "\n" + treeText(v)
}

func treeText(v any) string {
	var sb strings.Builder
	switch t := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			sb.WriteString(k + "\n" + treeText(t[k]))
		}
	case []any:
		for _, x := range t {
			sb.WriteString(treeText(x))
		}
	case string:
		sb.WriteString(t + "\n")
		if b, err := base64.StdEncoding.DecodeString(t); err == nil {
			sb.Write(b)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func dumpRecord(r *Record) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%#v\n%#v\n%#v\n", r.attrs, r.vertices, r.builds)
	for _, b := range r.builds {
		for _, s := range b.steps {
			sb.WriteString(opText(s.op))
		}
	}
	return sb.String()
}

func dumpRedacted(x redacted) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%#v\n", x)
	for _, b := range x.builds {
		for _, s := range b.steps {
			sb.WriteString(opText(s.op))
		}
	}
	return sb.String()
}

func redactedEqual(a, b redacted) bool { return reflect.DeepEqual(a, b) }

// child returns m[key] as an object, and adds an empty one when it is
// missing.
func child(m map[string]any, key string) map[string]any {
	c, ok := m[key].(map[string]any)
	if !ok {
		c = map[string]any{}
		m[key] = c
	}
	return c
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// dig returns the value at path in an op tree, where a string is an object
// key and an int is a list index.
func dig(tb testing.TB, v any, path ...any) any {
	tb.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				tb.Fatalf("no object for key %q in %v", k, path)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				tb.Fatalf("no list item %d in %v", k, path)
			}
			v = l[k]
		}
	}
	return v
}

func (b *bundle) editHistory(edit func(map[string]any)) { b.editJSON(b.configHex(), edit) }

func (b *bundle) editAttrs(edit func(map[string]any)) {
	b.editHistory(func(h map[string]any) { edit(child(h, "FrontendAttrs")) })
}

func (b *bundle) editProvenance(edit func(map[string]any)) {
	b.editJSON(b.layerHex(mediaTypeInToto), edit)
}

// editOps calls edit on the body of every step whose op has kind, such as
// "exec", "file" or "source".
func (b *bundle) editOps(kind string, edit func(map[string]any)) {
	b.t.Helper()
	found := false
	b.editProvenance(func(p map[string]any) {
		defs, _ := child(child(child(p, "buildDefinition"), "internalParameters"), "buildConfig")["llbDefinition"].([]any)
		for _, d := range defs {
			op := child(child(asMap(d), "op"), "Op")
			if body, ok := op[kind].(map[string]any); ok {
				edit(body)
				found = true
			}
		}
	})
	if !found {
		b.t.Fatalf("fixture has no %s step", kind)
	}
}

// addFileAction adds an action to the first step that has file actions.
func (b *bundle) addFileAction(action map[string]any) {
	b.t.Helper()
	added := false
	b.editOps("file", func(op map[string]any) {
		if added {
			return
		}
		actions, _ := op["actions"].([]any)
		op["actions"] = append(actions, map[string]any{"Action": action, "input": -1, "output": -1, "secondaryInput": -1})
		added = true
	})
}

func metadata(p map[string]any) map[string]any {
	return child(child(child(p, "runDetails"), "metadata"), "buildkit_metadata")
}

func (b *bundle) editDockerfile(edit func(string) string) {
	b.editProvenance(func(p map[string]any) {
		infos, _ := child(metadata(p), "source")["infos"].([]any)
		info := asMap(infos[0])
		data, _ := info["data"].(string)
		text, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			b.t.Fatal(err)
		}
		info["data"] = base64.StdEncoding.EncodeToString([]byte(edit(string(text))))
	})
}

func (b *bundle) appendStatus(data []byte) {
	hex := b.layerHex(mediaTypeStatus)
	b.setBlob(hex, concat(b.blob(hex), data))
}

func (b *bundle) setLayer(mediaType string, data []byte) {
	b.setBlob(b.layerHex(mediaType), data)
}

// rawText is every byte of the record, with the Dockerfile text of each
// provenance decoded from base64.
func (b *bundle) rawText() string {
	var sb strings.Builder
	for _, name := range b.order {
		sb.Write(b.files[name])
	}
	for _, l := range b.layers() {
		if l["mediaType"] != mediaTypeInToto {
			continue
		}
		var p struct {
			RunDetails struct {
				Metadata struct {
					BuildKitMetadata struct {
						Source struct {
							Infos []struct {
								Data []byte `json:"data"`
							} `json:"infos"`
						} `json:"source"`
					} `json:"buildkit_metadata"`
				} `json:"metadata"`
			} `json:"runDetails"`
		}
		if json.Unmarshal(b.blob(digestHex(l["digest"])), &p) == nil {
			for _, info := range p.RunDetails.Metadata.BuildKitMetadata.Source.Infos {
				sb.Write(info.Data)
			}
		}
	}
	return sb.String()
}

func TestPlantedSecrets(t *testing.T) {
	fixture := fixturePath(t, "control", "pr")
	tests := []struct {
		site    string
		decoded bool
		plant   func(b *bundle, v string)
	}{
		{"history build-arg", true, func(b *bundle, v string) {
			b.editAttrs(func(a map[string]any) { a["build-arg:API_TOKEN"] = v })
		}},
		{"history cache-imports attribute", true, func(b *bundle, v string) {
			b.editAttrs(func(a map[string]any) {
				a["cache-imports"] = `[{"Type":"gha","Attrs":{"token":"` + v + `","url":"https://cache.example/"}}]`
			})
		}},
		{"history cache-imports URL", true, func(b *bundle, v string) {
			b.editAttrs(func(a map[string]any) {
				a["cache-imports"] = `[{"Type":"registry","Attrs":{"ref":"https://user:` + v + `@registry.example/cache"}}]`
			})
		}},
		{"history malformed cache-imports", true, func(b *bundle, v string) {
			b.editAttrs(func(a map[string]any) { a["cache-imports"] = "[" + v })
		}},
		{"history label", true, func(b *bundle, v string) {
			b.editAttrs(func(a map[string]any) { a["label:org.example.note"] = v })
		}},
		{"history vcs:source", true, func(b *bundle, v string) {
			b.editAttrs(func(a map[string]any) { a["vcs:source"] = "https://user:" + v + "@git.example/org/repo.git" })
		}},
		{"history unknown attribute", true, func(b *bundle, v string) {
			b.editAttrs(func(a map[string]any) { a["ichnos-test-attribute"] = v })
		}},
		{"history ExporterResponse", false, func(b *bundle, v string) {
			b.editHistory(func(h map[string]any) { child(h, "ExporterResponse")["containerimage.config"] = v })
		}},
		{"history Exporters", false, func(b *bundle, v string) {
			b.editHistory(func(h map[string]any) {
				h["Exporters"] = []any{map[string]any{"Type": "image", "Attrs": map[string]any{"name": "registry.example/" + v}}}
			})
		}},
		{"history localState", false, func(b *bundle, v string) {
			b.editHistory(func(h map[string]any) { child(h, "localState")["LocalPath"] = "/home/" + v })
		}},
		{"history Ref", false, func(b *bundle, v string) {
			b.editHistory(func(h map[string]any) { h["Ref"] = v })
		}},
		{"history error", false, func(b *bundle, v string) {
			b.editHistory(func(h map[string]any) { h["error"] = map[string]any{"code": 2, "message": "failed: " + v} })
		}},
		{"status vertex name", false, func(b *bundle, v string) {
			b.appendStatus(vertices(update{digest: digestOf('7'), name: "[1/1] RUN echo " + v}))
		}},
		{"status vertex error", false, func(b *bundle, v string) {
			b.appendStatus(vertices(update{digest: digestOf('7'), err: "failed: " + v}))
		}},
		{"status vertex digest", true, func(b *bundle, v string) {
			b.appendStatus(vertices(update{digest: "sha256:" + v}))
		}},
		{"status statuses", false, func(b *bundle, v string) {
			b.appendStatus(frame(pbBytes(nil, 2, concat(pbString(nil, 1, v), pbString(nil, 2, digestOf('7')), pbString(nil, 3, v)))))
		}},
		{"status logs", false, func(b *bundle, v string) {
			b.appendStatus(frame(pbBytes(nil, 3, concat(pbString(nil, 1, digestOf('7')), pbString(nil, 4, v)))))
		}},
		{"status warnings", false, func(b *bundle, v string) {
			b.appendStatus(frame(pbBytes(nil, 4, concat(pbString(nil, 1, digestOf('7')), pbString(nil, 3, v)))))
		}},
		{"provenance env", true, func(b *bundle, v string) {
			b.editOps("exec", func(op map[string]any) {
				meta := child(op, "meta")
				env, _ := meta["env"].([]any)
				meta["env"] = append(env, "API_TOKEN="+v)
			})
		}},
		{"provenance args", true, func(b *bundle, v string) {
			b.editOps("exec", func(op map[string]any) {
				child(op, "meta")["args"] = []any{"/bin/sh", "-c", "API_TOKEN=" + v + " ./deploy"}
			})
		}},
		{"provenance proxy_env", true, func(b *bundle, v string) {
			b.editOps("exec", func(op map[string]any) {
				child(op, "meta")["proxy_env"] = map[string]any{"http_proxy": "http://user:" + v + "@proxy.example:3128"}
			})
		}},
		{"provenance proxy_env with no scheme", true, func(b *bundle, v string) {
			b.editOps("exec", func(op map[string]any) {
				meta := child(op, "meta")
				meta["proxy_env"] = map[string]any{"https_proxy": "user:" + v + "@proxy.example:3128"}
				env, _ := meta["env"].([]any)
				meta["env"] = append(env, "HTTPS_PROXY=user:"+v+"@proxy.example:3128")
			})
			b.editAttrs(func(a map[string]any) { a["build-arg:HTTPS_PROXY"] = "user:" + v + "@proxy.example:3128" })
		}},
		{"provenance source identifier", true, func(b *bundle, v string) {
			b.editOps("source", func(op map[string]any) { op["identifier"] = "https://user:" + v + "@files.example/app.tgz" })
		}},
		{"provenance http.header", true, func(b *bundle, v string) {
			b.editOps("source", func(op map[string]any) { child(op, "attrs")["http.header.Authorization"] = "Bearer " + v })
		}},
		{"provenance git.fullurl", true, func(b *bundle, v string) {
			b.editOps("source", func(op map[string]any) {
				child(op, "attrs")["git.fullurl"] = "https://user:" + v + "@git.example/repo.git"
			})
		}},
		{"provenance COPY src", true, func(b *bundle, v string) {
			b.editAttrs(func(a map[string]any) { a["build-arg:API_TOKEN"] = v })
			copied := false
			b.editOps("file", func(op map[string]any) {
				actions, _ := op["actions"].([]any)
				for _, a := range actions {
					if c, ok := child(a.(map[string]any), "Action")["copy"].(map[string]any); ok {
						c["src"] = "/" + v + ".txt"
						copied = true
					}
				}
			})
			if !copied {
				t.Fatal("fixture has no COPY step")
			}
		}},
		{"provenance heredoc with a build argument", true, func(b *bundle, v string) {
			b.editAttrs(func(a map[string]any) { a["build-arg:NPM_TOKEN"] = v })
			b.addFileAction(map[string]any{"mkfile": map[string]any{
				"path": "/root/.npmrc", "mode": 384,
				"data": base64.StdEncoding.EncodeToString([]byte("//registry.npmjs.org/:token-from-argument " + v + "\n")),
			}})
		}},
		{"provenance heredoc with an assignment", true, func(b *bundle, v string) {
			b.addFileAction(map[string]any{"mkfile": map[string]any{
				"path": "/run.sh", "mode": 493,
				"data": base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\nexport API_TOKEN=" + v + "\n")),
			}})
		}},
		{"provenance nested build definition", true, func(b *bundle, v string) {
			b.editProvenance(func(p map[string]any) {
				config := child(child(child(p, "buildDefinition"), "internalParameters"), "buildConfig")
				defs, _ := config["llbDefinition"].([]any)
				config["llbDefinition"] = append(defs, map[string]any{"id": "nested", "op": map[string]any{"Op": map[string]any{
					"build": map[string]any{"def": map[string]any{"def": []any{base64.StdEncoding.EncodeToString([]byte("binary llb " + v))}}},
				}}})
			})
		}},
		{"provenance Dockerfile text", true, func(b *bundle, v string) {
			b.editDockerfile(func(s string) string {
				return strings.Replace(s, "RUN wc -c", "RUN API_TOKEN="+v+" wc -c", 1)
			})
		}},
		{"provenance Dockerfile continuation line", true, func(b *bundle, v string) {
			lines := 0
			b.editDockerfile(func(s string) string {
				lines = strings.Count(s, "\n")
				return strings.TrimSuffix(s, "\n") + " \\\n    && export API_TOKEN=" + v + "\n"
			})
			b.editProvenance(func(p map[string]any) {
				for _, l := range asMap(child(metadata(p), "source")["locations"]) {
					for _, loc := range asList(asMap(l)["locations"]) {
						for _, r := range asList(asMap(loc)["ranges"]) {
							end := child(asMap(r), "end")
							if fmt.Sprint(end["line"]) == fmt.Sprint(lines) {
								end["line"] = lines + 1
							}
						}
					}
				}
			})
		}},
		{"provenance Dockerfile line outside any step", false, func(b *bundle, v string) {
			b.editDockerfile(func(s string) string { return s + "# " + v + "\n" })
		}},
		{"provenance infos llbDefinition", false, func(b *bundle, v string) {
			b.editProvenance(func(p map[string]any) {
				infos, _ := child(metadata(p), "source")["infos"].([]any)
				asMap(infos[0])["llbDefinition"] = []any{map[string]any{"id": "step0", "op": map[string]any{
					"Op": map[string]any{"source": map[string]any{"identifier": "https://user:" + v + "@example.invalid/"}},
				}}}
			})
		}},
		{"provenance externalParameters", false, func(b *bundle, v string) {
			b.editProvenance(func(p map[string]any) {
				child(child(child(p, "buildDefinition"), "externalParameters"), "request")["args"] = map[string]any{"build-arg:API_TOKEN": v}
			})
		}},
		{"provenance resolvedDependencies", false, func(b *bundle, v string) {
			b.editProvenance(func(p map[string]any) {
				child(p, "buildDefinition")["resolvedDependencies"] = []any{map[string]any{"uri": "https://user:" + v + "@example.invalid/x"}}
			})
		}},
		{"provenance github_event_payload", false, func(b *bundle, v string) {
			b.editProvenance(func(p map[string]any) {
				child(child(p, "buildDefinition"), "internalParameters")["github_event_payload"] = map[string]any{
					"pusher": map[string]any{"email": v + "@example.invalid"},
				}
			})
		}},
		{"provenance buildkit_metadata.vcs", false, func(b *bundle, v string) {
			b.editProvenance(func(p map[string]any) {
				metadata(p)["vcs"] = map[string]any{"source": "https://user:" + v + "@example.invalid/repo"}
			})
		}},
		{"provenance proxy uri", false, func(b *bundle, v string) {
			b.editProvenance(func(p map[string]any) {
				metadata(p)["network"] = map[string]any{"proxy": map[string]any{"incomplete": []any{map[string]any{"uri": "https://user:" + v + "@example.invalid/"}}}}
			})
		}},
		{"trace url.full", false, func(b *bundle, v string) {
			b.setLayer("application/vnd.buildkit.otlp.json.v0", []byte(`{"url.full":"https://cdn.example/layer?Signature=`+v+`"}`))
		}},
		{"trace authorization", false, func(b *bundle, v string) {
			b.setLayer("application/vnd.buildkit.otlp.json.v0", []byte(`{"http.request.header.authorization":"Bearer `+v+`"}`))
		}},
		{"result manifest", false, func(b *bundle, v string) {
			b.setLayer("application/json", []byte(`{"annotations":{"note":"`+v+`"}}`))
		}},
		{"SBOM", false, func(b *bundle, v string) {
			b.addLayer(mediaTypeInToto, map[string]string{annotationPredicateType: "https://spdx.dev/Document"}, []byte(`{"name":"`+v+`"}`))
		}},
		{"external error blob", false, func(b *bundle, v string) {
			data := []byte("rpc status: " + v)
			b.addLayer("application/vnd.googeapis.google.rpc.status+proto", nil, data)
			b.editHistory(func(h map[string]any) {
				h["externalError"] = map[string]any{"media_type": "application/vnd.googeapis.google.rpc.status+proto", "size": len(data)}
			})
		}},
	}
	seen := map[string]bool{}
	for i, tt := range tests {
		t.Run(tt.site, func(t *testing.T) {
			value := fmt.Sprintf("ichnos-fake-secret-%02d", i)
			if seen[value] {
				t.Fatalf("value %q is not distinct", value)
			}
			seen[value] = true
			b := loadBundle(t, fixture)
			tt.plant(b, value)
			path := filepath.Join(t.TempDir(), "planted.dockerbuild")
			b.write(t, path)

			rec, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if tt.decoded {
				if !strings.Contains(dumpRecord(rec), value) {
					t.Fatalf("the planted value did not reach the Record")
				}
			} else if !strings.Contains(b.rawText(), value) {
				t.Fatalf("the planted value is not in the raw record")
			}

			outputs := map[string]string{"redacted": dumpRedacted(mustRedact(t, rec))}
			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				outputs["Record "+verb] = fmt.Sprintf(verb, rec)
				outputs["*Record "+verb] = fmt.Sprintf(verb, *rec)
			}
			var text, js bytes.Buffer
			slog.New(slog.NewTextHandler(&text, nil)).Info("collected", "record", rec, "value", *rec)
			slog.New(slog.NewJSONHandler(&js, nil)).Info("collected", "record", rec, "value", *rec)
			outputs["slog text"], outputs["slog JSON"] = text.String(), js.String()

			for name, out := range outputs {
				for form, encoded := range map[string]string{
					"plain":       value,
					"base64":      base64.StdEncoding.EncodeToString([]byte(value)),
					"URL-encoded": url.QueryEscape(value),
				} {
					if strings.Contains(out, encoded) {
						t.Errorf("%s holds the %s value", name, form)
					}
				}
			}
		})
	}
}

// TestPlantedSecretsInErrors plants values where they make the collector
// fail, and checks that no error holds them.
func TestPlantedSecretsInErrors(t *testing.T) {
	const value = "ichnos-fake-secret-error"
	tests := map[string]func(b *bundle){
		"non-string attribute": func(b *bundle) {
			b.editAttrs(func(a map[string]any) { a["build-arg:"+value] = 5 })
		},
		"provenance syntax": func(b *bundle) {
			b.setLayer(mediaTypeInToto, []byte(`{"buildDefinition": `+value+`}`))
		},
		"op type": func(b *bundle) {
			b.editProvenance(func(p map[string]any) {
				defs, _ := child(child(child(p, "buildDefinition"), "internalParameters"), "buildConfig")["llbDefinition"].([]any)
				asMap(defs[0])["op"] = value
			})
		},
		"location type": func(b *bundle) {
			b.editProvenance(func(p map[string]any) {
				child(metadata(p), "source")["locations"] = map[string]any{"step0": value}
			})
		},
		"bad step reference": func(b *bundle) {
			b.editProvenance(func(p map[string]any) {
				child(child(child(p, "buildDefinition"), "internalParameters"), "buildConfig")["digestMapping"] = map[string]any{value: value}
			})
		},
		"status": func(b *bundle) {
			b.appendStatus(frame(pbBytes(nil, 1, pbString(nil, 3, value))))
		},
	}
	for name, plant := range tests {
		t.Run(name, func(t *testing.T) {
			b := loadBundle(t, fixturePath(t, "control", "pr"))
			plant(b)
			path := filepath.Join(t.TempDir(), "planted.dockerbuild")
			b.write(t, path)
			_, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
			if !isTyped(err) {
				t.Fatalf("Collect() error = %v, want a typed error", err)
			}
			if strings.Contains(err.Error(), value) {
				t.Errorf("Collect() error %q holds the planted value", err)
			}
		})
	}
}

func TestKindStrings(t *testing.T) {
	for _, tt := range []struct{ got, want string }{
		{cacheUnknown.String(), "unknown"},
		{cacheLocal.String(), "local"},
		{cacheRegistry.String(), "registry"},
		{cacheGHA.String(), "gha"},
		{cacheS3.String(), "s3"},
		{cacheAzblob.String(), "azblob"},
		{cacheType(99).String(), "cacheType(99)"},
		{kindStep.String(), "step"},
		{kindInternal.String(), "internal"},
		{kindImport.String(), "import"},
		{kindExport.String(), "export"},
		{vertexKind(99).String(), "vertexKind(99)"},
		{provenanceRead.String(), "read"},
		{provenanceMissing.String(), "missing"},
		{provenanceUnsupported.String(), "unsupported"},
		{provenanceNoSteps.String(), "no steps"},
		{provenanceState(99).String(), "provenanceState(99)"},
	} {
		if tt.got != tt.want {
			t.Errorf("String() = %q, want %q", tt.got, tt.want)
		}
	}
}

func TestIsCredentialName(t *testing.T) {
	for _, name := range []string{
		"API_TOKEN", "apiKey", "APIKey", "GITHUBTOKEN", "Authorization", "http.header.Authorization",
		"api-key", "DB_PASSWORD", "PGPASSWORD", "AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "NPM_TOKEN",
		"session_cookie", "X-Auth", "MYAPIKEY", "TOKEN2", "client.secret", "--password", "sig",
		"DB_PASS", "DOCKER_PASS", "REGISTRY_PASSWD", "MYSQL_PWD", "GPG_PASSPHRASE", "GH_PAT", "GITHUB_PAT",
		"CI_JOB_JWT", "BEARER", "AWS_CREDENTIALS", "NPM_CREDS", "API_KEYS", "KEYS", "TOKENS", "SECRETS",
		"DB_PASSWORDS", "session_cookies", "githubTokens",
		"GPGPASSPHRASE", "DBPASSWD", "MYSQLPWD", "sigs", "auths", "PATS", "JWTS", "creds", "keys",
	} {
		if !isCredentialName(name) {
			t.Errorf("isCredentialName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"TOKENIZERS_PARALLELISM", "AUTHOR", "KEYBOARD", "local.sharedkeyhint", "PATH", "MODE", "BUILD_ID",
		"SOURCE_DATE_EPOCH", "signature", "secretenv", "monkey", "", "_", "authors",
		"PATCH", "CREDIT", "BYPASS", "COMPASS", "passthrough", "PASSED", "keystore", "s", "S",
		"secretOpt", "git.authheadersecret", "git.authtokensecret", "http.authheadersecret",
		"git.sig.pubkey", "http.sig.pubkey", "http.sig.signature",
	} {
		if isCredentialName(name) {
			t.Errorf("isCredentialName(%q) = true, want false", name)
		}
	}
}

// TestBuildKitOpKeys checks every key that BuildKit v0.34.0 can write in a
// step definition: the JSON names in solver/pb/ops.pb.go and json.go, and
// the attribute names in solver/pb/attr.go.
func TestBuildKitOpKeys(t *testing.T) {
	keys := []string{
		"actions", "all_proxy", "allowEmptyWildcard", "allowNotFound", "allowWildcard",
		"alwaysReplaceExistingDestPaths", "Architecture", "args", "attemptUnpackDockerCompatibility", "attrs",
		"builder", "cacheOpt", "caps", "cdiDevices", "cgroupParent", "character", "constraints", "contentCache",
		"cpuPeriod", "cpuQuota", "cpusetCpus", "cpusetMems", "cpuShares", "createDestPath", "cwd", "data", "def",
		"definition", "description", "dest", "digest", "dirCopyContents", "end", "env", "exclude_patterns",
		"export_cache", "extraHosts", "filename", "filter", "followSymlink", "ftp_proxy", "gid", "group", "Hard",
		"Host", "hostname", "http_proxy", "https_proxy", "id", "ID", "identifier", "ignore_cache",
		"include_patterns", "index", "infos", "input", "inputs", "IP", "language", "line", "linux_resources",
		"locations", "lower", "makeParents", "memory", "memorySwap", "meta", "metadata", "mode", "modeStr",
		"mounts", "mountType", "name", "Name", "network", "newpath", "no_proxy", "oldpath", "optional", "OS",
		"OSFeatures", "OSVersion", "output", "outputs", "owner", "path", "platform", "progress_group",
		"proxy_env", "ranges", "readonly", "removeMountStubsRecursive", "required_paths", "resultID",
		"secondaryInput", "secretenv", "security", "selector", "sharing", "size", "Soft", "Source",
		"sourceIndex", "src", "SSHOpt", "start", "timestamp", "TmpfsOpt", "uid", "ulimit", "upper", "user",
		"validExitCodes", "Value", "Variant", "weak",
		"Op", "exec", "source", "file", "build", "merge", "diff", "passthrough", "Action", "copy", "mkfile",
		"mkdir", "rm",
		"git.keepgitdir", "git.fullurl", "git.knownsshhosts", "git.mountsshsock", "git.checksum",
		"git.skipsubmodules", "git.mtime", "git.fetchbycommit", "git.bundle", "git.checkoutbundle",
		"git.advice", "local.session", "local.unique", "local.includepattern", "local.followpaths",
		"local.excludepatterns", "local.sharedkeyhint", "local.metadatatransfer",
		"local.metadatatransferexclude", "local.differ", "llbbuild.filename", "http.checksum", "http.filename",
		"http.perm", "http.uid", "http.gid", "image.resolvemode", "image.recordtype", "image.layerlimit",
		"image.checksum", "oci.session", "oci.store", "oci.layerlimit",
		"secretOpt", "git.authheadersecret", "git.authtokensecret", "http.authheadersecret",
		"git.sig.pubkey", "git.sig.rejectexpired", "git.sig.requiresignedtag", "git.sig.ignoresignedtag",
		"http.sig.pubkey", "http.sig.signature",
	}
	for _, k := range keys {
		if isCredentialName(k) {
			t.Errorf("isCredentialName(%q) = true for a BuildKit op key", k)
		}
	}
}

// TestRedactRulesReadTheOriginalText checks that no rule hides a value from
// another: a known value that covers part of an assignment, a URL or a proxy
// setting does not stop the rule for it, and a rule that removes part of a
// known value does not stop it being removed whole.
func TestRedactRulesReadTheOriginalText(t *testing.T) {
	text, proxy, env := scrubber.text, scrubber.proxy, scrubber.envEntry
	tests := []struct {
		name   string
		known  string
		redact func(scrubber, string, *bool) string
		in     string
		want   string
	}{
		{"known value across the = of an assignment", "N=hu", text, "RUN API_TOKEN=hunter2 make", "RUN API_TOKE*** make"},
		{"known value across the :// of a URL", "s://u", text, "ADD https://user:pass@h.example/f /", "ADD http***@h.example/f /"},
		{"known value across the ? of a URL", "f?si", text, "ADD https://h.example/f?sig=abcd /", "ADD https://h.example/***g=*** /"},
		{"known value across the @ of a proxy", "ss@p", proxy, "user:pass@proxy.example:3128", "***roxy.example:3128"},
		{"known value across a legacy ENV", "KEN a", text, "ENV API_TOKEN abcd efgh", "ENV API_TO***"},
		{"known value across the = of an environment entry", "KEN=", env, "API_TOKEN=000L", "API_TO***"},
		{"known value across the = of a proxy entry", "OXY=", env, "HTTP_PROXY=proxy.example:3128", "HTTP_PR***proxy.example:3128"},
		{"known value with a space in an assignment", "word1 word2", text, "RUN TOKEN=word1 word2 make", "RUN TOKEN=*** make"},
		{"known value with a / in the password of a URL", "pa/ss", text, "RUN curl https://alice:pa/ss@h.example/", "RUN curl https://***@h.example/"},
		{"known value with a / in the password of a proxy", "pa/ss", proxy, "alice:pa/ss@proxy.example:3128", "***@proxy.example:3128"},
		{"known value across the = of a query parameter", "00=1", proxy, "?000=1=", "?***"},
		{"known value with a # in a query", "1#xy", text, "https://h/?a=1#xy&b=c0de", "https://h/?a=***&b=***"},
		{"known values in two URLs", "pa/ss", text, "https://a:pa/ss@h https://b:pa/ss@i", "https://***@h https://***@i"},
		{"known value at both ends", "abcd", text, "abcd TOKEN=x abcd", "*** TOKEN=*** ***"},
		{"known value holding the quote that opens a value", `"abc`, text, `RUN TOKEN="abc def" make`, "RUN TOKEN=*** make"},
		{"assignment in the user information of a URL", "", text, "://TOKEN=p@ss x", "://*** x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := newScrubber(&Record{&recordData{attrs: map[string]string{"build-arg:API_TOKEN": tt.known}}})
			if err != nil {
				t.Fatalf("newScrubber() error = %v", err)
			}
			var withheld bool
			if got := tt.redact(s, tt.in, &withheld); got != tt.want || !withheld {
				t.Errorf("redact(%q) = %q, %v, want %q, true", tt.in, got, withheld, tt.want)
			}
		})
	}
}

func TestMask(t *testing.T) {
	x := strings.Repeat("abcdefgh", 17)
	tests := []struct {
		name  string
		spans [][2]int
		want  string
	}{
		{"nothing", nil, x},
		{"empty spans", [][2]int{{0, 0}, {5, 5}, {136, 136}}, x},
		{"first byte", [][2]int{{0, 1}}, "***" + x[1:]},
		{"last byte", [][2]int{{135, 136}}, x[:135] + "***"},
		{"whole string", [][2]int{{0, 136}}, "***"},
		{"across a word of bits", [][2]int{{62, 66}}, x[:62] + "***" + x[66:]},
		{"touching spans", [][2]int{{2, 4}, {4, 6}}, x[:2] + "***" + x[6:]},
		{"overlapping spans", [][2]int{{10, 20}, {2, 12}}, x[:2] + "***" + x[20:]},
		{"separate spans", [][2]int{{1, 2}, {3, 4}}, x[:1] + "***" + x[2:3] + "***" + x[4:]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mask{n: len(x)}
			for _, s := range tt.spans {
				m.mark(s[0], s[1])
			}
			if got := m.apply(x); got != tt.want {
				t.Errorf("apply() = %q, want %q", got, tt.want)
			}
		})
	}
	var empty mask
	if got := empty.apply(""); got != "" {
		t.Errorf("apply() of an empty string = %q", got)
	}
	unmarked := mask{n: 10}
	if got := unmarked.next(3, true); got != 10 {
		t.Errorf("next(3, true) of an unmarked mask = %d, want 10", got)
	}
	if got := unmarked.next(3, false); got != 3 {
		t.Errorf("next(3, false) of an unmarked mask = %d, want 3", got)
	}
	full := mask{n: 70}
	full.mark(0, 70)
	if got := full.next(0, false); got != 70 {
		t.Errorf("next(0, false) of a full mask = %d, want 70", got)
	}
}

// TestRedactTimeIsLinear checks that a string takes time in proportion to
// its length, in the shapes that cost most: many known values, with a rule
// mark after all of them or across all of them.
func TestRedactTimeIsLinear(t *testing.T) {
	s, err := newScrubber(&Record{&recordData{attrs: map[string]string{"build-arg:API_TOKEN": "abcd"}}})
	if err != nil {
		t.Fatalf("newScrubber() error = %v", err)
	}
	fastest := func(x string) time.Duration {
		best := time.Duration(math.MaxInt64)
		for range 5 {
			start := time.Now()
			var withheld bool
			s.text(x, &withheld)
			best = min(best, time.Since(start))
		}
		return best
	}
	for _, tt := range []struct {
		name string
		text func(n int) string
	}{
		{"rule mark after the known values", func(n int) string { return strings.Repeat("abcdX", n/5) + " TOKEN=y" }},
		{"rule mark across the known values", func(n int) string { return "TOKEN=" + strings.Repeat("abcdX", n/5) }},
		{"URLs whose port is not a number, with no @", func(n int) string { return strings.Repeat("a://x:y/", n/8) }},
		{"URLs that start with a scheme, with no @", func(n int) string { return strings.Repeat("a://", n/4) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			small, large := tt.text(maxStringBytes/16-64), tt.text(maxStringBytes-64)
			if ratio := float64(fastest(large)) / float64(fastest(small)); ratio > 64 {
				t.Errorf("text() of 16 times the length took %.1f times as long, want about 16", ratio)
			}
		})
	}
}

// naiveScrub is the reference for scrubber.scrub. It marks every byte that
// an occurrence of a known value covers, the bytes that the rules mark in x,
// and, when x holds a known value, the bytes of x behind those that the
// rules mark in x with the known values replaced, and then with every
// marked byte replaced.
func naiveScrub(known []string, x string, r ruleSet) (string, bool) {
	covered := naiveCover(known, x)
	hasKnown := slices.Contains(covered, true)
	if hasKnown {
		naiveReread(x, covered, r)
	}
	mx := mask{n: len(x)}
	hit := r.mark(x, &mx) || hasKnown
	for i := range len(x) {
		covered[i] = covered[i] || mx.next(i, true) == i
	}
	if hasKnown {
		naiveReread(x, covered, r)
	}
	return naiveApply(x, covered), hit
}

// naiveReread marks the bytes of x behind those that the rules mark in x
// with each run of covered bytes replaced.
func naiveReread(x string, covered []bool, r ruleSet) {
	var y []byte
	// origin holds the bytes of x behind each byte of y.
	var origin [][2]int
	for i := 0; i < len(x); {
		j := i + 1
		if !covered[i] {
			y = append(y, x[i])
			origin = append(origin, [2]int{i, j})
			i = j
			continue
		}
		for j < len(x) && covered[j] {
			j++
		}
		for k := range len(withheldValue) {
			y = append(y, withheldValue[k])
			origin = append(origin, [2]int{i, j})
		}
		i = j
	}
	my := mask{n: len(y)}
	r.mark(string(y), &my)
	for k, o := range origin {
		if my.next(k, true) == k {
			for i := o[0]; i < o[1]; i++ {
				covered[i] = true
			}
		}
	}
}

func FuzzScrub(f *testing.F) {
	f.Add("N=hu", "", "RUN API_TOKEN=hunter2 make", uint8(textRules))
	f.Add("s://u", "", "ADD https://user:pass@h.example/f /", uint8(stringRules))
	f.Add("ss@p", "pass", "user:pass@proxy.example:3128", uint8(proxyRules))
	f.Add("KEN=", "", "API_TOKEN=000L", uint8(envRules))
	f.Add("pa/ss", "", "https://a:pa/ss@h https://b:pa/ss@i", uint8(textRules))
	f.Add("1#xy", "#xy&", "https://h/?a=1#xy&b=c0de", uint8(stringRules))
	f.Add("1#xy", "", "://?00&1#xy", uint8(stringRules))
	f.Add("00=1", "", "?000=1=", uint8(proxyRules))
	f.Add("00??", "", "00??&@", uint8(proxyRules))
	f.Add("0000", "", "0://0://0://0@0", uint8(textRules))
	f.Add("abcd", "cdef", "ENV API_TOKEN abcdef ghij", uint8(textRules))
	f.Fuzz(func(t *testing.T, k1, k2, x string, kind uint8) {
		r := ruleSet(kind % 4)
		s, err := newScrubber(&Record{&recordData{attrs: map[string]string{"build-arg:API_TOKEN": k1, "build-arg:OTHER_TOKEN": k2}}})
		if err != nil || len(x) > maxStringBytes {
			return
		}
		var known []string
		for _, v := range []string{k1, k2} {
			if scrubbable(v) {
				known = append(known, v)
			}
		}
		var withheld bool
		got := s.scrub(x, &withheld, r)
		if want, hit := naiveScrub(known, x, r); got != want || withheld != hit {
			t.Fatalf("scrub(%q) with %q = %q, %v, want %q, %v", x, known, got, withheld, want, hit)
		}
		if strings.Contains(k1+k2, "*") {
			return
		}
		for _, v := range known {
			if strings.Contains(got, v) {
				t.Fatalf("scrub(%q) = %q, which holds the known value %q", x, got, v)
			}
		}
		if twice := s.scrub(got, &withheld, r); twice != got {
			t.Fatalf("scrub(%q) with %q = %q, then %q", x, known, got, twice)
		}
	})
}

// redactWith applies one rule to s.
func redactWith(rule func(string, *mask) bool, s string) (string, bool) {
	m := mask{n: len(s)}
	hit := rule(s, &m)
	return m.apply(s), hit
}

func TestRedactURLs(t *testing.T) {
	image := "docker-image://docker.io/library/alpine:3.22@sha256:" + hex64('a')
	tests := []struct {
		in      string
		want    string
		matched bool
	}{
		{"https://user:pass@example.com/path", "https://***@example.com/path", true},
		{"https://user:p@ss@example.com/", "https://***@example.com/", true},
		{"https://example.com/file?a=1&b=two&c", "https://example.com/file?a=***&b=***&***", true},
		{"https://example.com/file?a=1#frag?x=y", "https://example.com/file?a=***#frag?x=y", true},
		{"https://example.com/repo.git#main:sub", "https://example.com/repo.git#main:sub", false},
		{"HTTPS://USER:PASS@EXAMPLE.COM/", "HTTPS://***@EXAMPLE.COM/", true},
		{"http://user:pass@[::1]:8080/x?k=v", "http://***@[::1]:8080/x?k=***", true},
		{"http://[::1]:8080/x", "http://[::1]:8080/x", false},
		{image, image, false},
		{"docker-image://alpine@sha256:" + hex64('b'), "docker-image://alpine@sha256:" + hex64('b'), false},
		{"oci-layout://store@sha256:" + hex64('c'), "oci-layout://store@sha256:" + hex64('c'), false},
		{"curl -u x https://u:p@a.example/f -o out", "curl -u x https://***@a.example/f -o out", true},
		{`"https://u:p@a.example"`, `"https://***@a.example"`, true},
		{"https://a.example/?u=https://u:p@b.example/?t=1", "https://a.example/?u=https://***@b.example/?t=***", true},
		{"https://proxy.example/https://u:p@b.example/", "https://proxy.example/https://***@b.example/", true},
		{"https://a.example/#https://u:p@b.example/", "https://a.example/#https://***@b.example/", true},
		{"://u:p@host", "://***@host", true},
		{"https://@host/", "https://@host/", false},
		{"local://context", "local://context", false},
		{"https://\xc3\xbc:p\xc3\xa4ss@b\xc3\xbccher.example/f", "https://***@b\xc3\xbccher.example/f", true},
		{"https://example.com/?q=\xc3\xbc#\xc3\xa9", "https://example.com/?q=***#\xc3\xa9", true},
		{"no url here", "no url here", false},
		{"https://example.com/?", "https://example.com/?", false},
		{"https://example.com/?a=&&b", "https://example.com/?a=&&***", true},
		{"https://alice:pa/ss@h.example/f", "https://***@h.example/f", true},
		{"https://alice:pa?ss@h.example/f", "https://***@h.example/f", true},
		{"https://alice:pa#ss@h.example/f", "https://***@h.example/f", true},
		{"https://alice:/ss@h.example/f", "https://***@h.example/f", true},
		{"https://alice:pa/s?s#s@h.example/f?k=v", "https://***@h.example/f?k=***", true},
		{"http://[::1]:pa/ss@h.example/", "http://***@h.example/", true},
		{"https://alice://ss@h.example/f", "https://***@h.example/f", true},
		{"https://alice:pa'ss@h.example/f", "https://***@h.example/f", true},
		{`https://alice:pa"ss@h.example/f`, "https://***@h.example/f", true},
		{"https://alice:pa<ss>@h.example/f", "https://***@h.example/f", true},
		{"https://alice:p@ss/word@h.example/f", "https://***@h.example/f", true},
		{"https://alice:p@ss?word@h.example/f", "https://***@h.example/f", true},
		{"https://alice:p@ss#word@h.example/f", "https://***@h.example/f", true},
		{"git+https://alice:tok@github.com/org/repo.git@v1", "git+https://***@v1", true},
		{"git+ssh://git@github.com/org/repo.git@v1.2.3", "git+ssh://***@github.com/org/repo.git@v1.2.3", true},
		{"https://http://u:p@b.example/?t=1", "https://***@b.example/?t=***", true},
		{"https://http://b.example/?t=1", "https://http://b.example/?t=***", true},
		{"https://cdn.jsdelivr.net/npm/react@18.3.1/index.js", "https://cdn.jsdelivr.net/npm/react@18.3.1/index.js", false},
		{"http://localhost:4873/@types/node", "http://***@types/node", true},
		{"http://[::1]:8080/@x", "http://***@x", true},
		{"http://[::1]/@x", "http://[::1]/@x", false},
		{"http://10.0.0.5/x@y", "http://10.0.0.5/x@y", false},
		{"https://www.mail-archive.com/users@tomcat.apache.org/m.html", "https://www.mail-archive.com/users@tomcat.apache.org/m.html", false},
		{"git+https://github.com/org/repo.git@v1.2", "git+https://github.com/org/repo.git@v1.2", false},
		{"https://user:1234/x@h.example/f", "https://***@h.example/f", true},
		{"https://first.last:1234/x@h.example/f", "https://***@h.example/f", true},
		{"https://john@example.com:se/cret@git.example/repo", "https://***@git.example/repo", true},
		{"https://john@example.com:se?cret@git.example/repo", "https://***@git.example/repo", true},
		{"https://john@example.com:1234/x@git.example/repo", "https://***@git.example/repo", true},
		{"git+ssh://git@gitlab.corp.example:2222/org/repo.git@v1", "git+ssh://***@v1", true},
		{"git+https://git.corp.example:8443/org/repo.git@v1", "git+https://***@v1", true},
		{"https://user:ab:12/x@h.example/f", "https://***@h.example/f", true},
		{"https://tok/en@h.example/f", "https://***@h.example/f", true},
		{"https://to+k.en/x@h.example/f", "https://***@h.example/f", true},
		{"http://localhost/x@y", "http://***@y", true},
		{"file:///app/@types/x", "file:///app/@types/x", false},
		{"http://[::1/ss@h.example/", "http://***@h.example/", true},
		{"http://[::1]pa/ss@h.example/", "http://***@h.example/", true},
		{"http://host:port/path", "http://host:port/path", false},
		{"https://a.example:pa/x https://b.example/@y", "https://a.example:pa/x https://b.example/@y", false},
	}
	for _, tt := range tests {
		got, matched := redactWith(markURLs, tt.in)
		if got != tt.want || matched != tt.matched {
			t.Errorf("markURLs(%q) = %q, %v, want %q, %v", tt.in, got, matched, tt.want, tt.matched)
		}
		if again, _ := redactWith(markURLs, got); again != got {
			t.Errorf("markURLs(%q) = %q, want it unchanged", got, again)
		}
	}
}

func TestRedactText(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		matched bool
	}{
		{"ENV API_TOKEN=abcd", "ENV API_TOKEN=***", true},
		{"ENV \xc3\xa9API_TOKEN=abcd", "ENV \xc3\xa9API_TOKEN=***", true},
		{"ENV API_TOKEN=\"caf\xc3\xa9 \xe2\x80\x9cx\xe2\x80\x9d\" C=d", "ENV API_TOKEN=*** C=d", true},
		{"ENV API_TOKEN=\xff\xfe\xfd C=d", "ENV API_TOKEN=*** C=d", true},
		{"ENV A=b API_TOKEN=abcd C=d", "ENV A=b API_TOKEN=*** C=d", true},
		{"ENV A=b \\\n    API_TOKEN=abcd \\\n    C=d", "ENV A=b \\\n    API_TOKEN=*** \\\n    C=d", true},
		{`ENV API_TOKEN="a b c" C=d`, "ENV API_TOKEN=*** C=d", true},
		{`ENV API_TOKEN='a b' C=d`, "ENV API_TOKEN=*** C=d", true},
		{`ENV API_TOKEN="a \" b" C=d`, "ENV API_TOKEN=*** C=d", true},
		{`ENV API_TOKEN="unterminated C=d`, "ENV API_TOKEN=***", true},
		{`ENV API_TOKEN=a\ b C=d`, "ENV API_TOKEN=*** C=d", true},
		{"ENV API_TOKEN=a=b=c", "ENV API_TOKEN=***", true},
		{"ENV API_TOKEN= C=d", "ENV API_TOKEN= C=d", true},
		{"ENV API_TOKEN abcd efgh", "ENV API_TOKEN ***", true},
		{"env api_token abcd", "env api_token ***", true},
		{"ENV API_TOKEN \\\n    abcd", "ENV API_TOKEN \\\n    ***", true},
		{"ENV \\\r\n API_TOKEN \\\r\n    abcd", "ENV \\\r\n API_TOKEN \\\r\n    ***", true},
		{"ENV\\\nAPI_TOKEN abcd", "ENV\\\nAPI_TOKEN ***", true},
		{"ONBUILD ENV API_TOKEN abcd", "ONBUILD ENV API_TOKEN ***", true},
		{"ENV MODE abcd", "ENV MODE abcd", false},
		{"ENV API_TOKEN", "ENV API_TOKEN", true},
		{"ARG API_TOKEN", "ARG API_TOKEN", false},
		{"ARG API_TOKEN=abcd", "ARG API_TOKEN=***", true},
		{"RUN <<EOF\nexport API_TOKEN=abcd\necho hi\nEOF", "RUN <<EOF\nexport API_TOKEN=***\necho hi\nEOF", true},
		{"RUN curl --password=abcd https://x", "RUN curl --password=*** https://x", true},
		{"RUN X=TOKEN=abcd make", "RUN X=TOKEN=*** make", true},
		{"ADD https://u:p@example.com/f.tgz /", "ADD https://***@example.com/f.tgz /", true},
		{"FROM alpine:3.22@sha256:" + hex64('a'), "FROM alpine:3.22@sha256:" + hex64('a'), false},
		{"RUN echo $TOKEN", "RUN echo $TOKEN", false},
		{"COPY --chown=app:app . .", "COPY --chown=app:app . .", false},
	}
	for _, tt := range tests {
		var withheld bool
		got := scrubber{}.text(tt.in, &withheld)
		if got != tt.want || withheld != tt.matched {
			t.Errorf("text(%q) = %q, %v, want %q, %v", tt.in, got, withheld, tt.want, tt.matched)
		}
		var again bool
		if twice := (scrubber{}).text(got, &again); twice != got || again != tt.matched {
			t.Errorf("text(%q) = %q, %v, want it unchanged and %v", got, twice, again, tt.matched)
		}
	}
}

func recordWith(tb testing.TB, attrs map[string]string, ops ...map[string]any) *Record {
	tb.Helper()
	steps := make([]step, len(ops))
	for i, op := range ops {
		steps[i] = step{id: fmt.Sprint("step", i), op: opJSON(tb, op), vertex: -1}
	}
	return &Record{&recordData{attrs: attrs, builds: []build{{platform: "linux/amd64", steps: steps}}}}
}

func TestRedactOps(t *testing.T) {
	const value = "ichnos-fake-known-value"
	big := json.Number("123456789012345678901234567890")
	authID := "git-auth-id"
	rec := recordWith(t,
		map[string]string{"build-arg:API_TOKEN": value, "build-arg:MODE": "fast"},
		map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{
			"args": []any{"/bin/sh", "-c", "echo " + value},
			"env":  []any{"PATH=/bin", "NOVALUE", "EMPTY=", "A=b=c", "API_TOKEN=x=y", "SESSION_COOKIE=", "short=" + value},
		}}}},
		map[string]any{"Op": map[string]any{"file": map[string]any{"actions": []any{map[string]any{
			"Action": map[string]any{"copy": map[string]any{"src": "/" + value + "/app.txt", "dest": "/app/", "mode": big}},
		}}}}},
		map[string]any{"Op": map[string]any{"source": map[string]any{
			"identifier": "docker-image://docker.io/library/alpine:" + value,
			"attrs": map[string]any{
				"git.authtokensecret": authID,
				"local.sharedkeyhint": "context",
				"token":               map[string]any{"nested": value, "list": []any{value}},
			},
		}}},
		map[string]any{"Op": map[string]any{"source": map[string]any{"identifier": "https://example.com/" + value + ".tgz"}}},
		map[string]any{"Op": map[string]any{"source": map[string]any{"identifier": "local://context"}}},
		nil,
	)
	got := mustRedact(t, rec)
	ops := make([]any, len(got.builds[0].steps))
	var withheld []bool
	for i, s := range got.builds[0].steps {
		ops[i] = opTree(t, s.op)
		withheld = append(withheld, s.withheld)
	}
	if dump := dumpRedacted(got); strings.Contains(dump, value) {
		t.Errorf("redact() kept the known value: %s", dump)
	}
	if want := []bool{true, true, true, true, false, false}; !slices.Equal(withheld, want) {
		t.Errorf("withheld = %v, want %v", withheld, want)
	}
	env := dig(t, ops[0], "Op", "exec", "meta", "env")
	if want := []any{"PATH=/bin", "NOVALUE", "EMPTY=", "A=b=c", "API_TOKEN=***", "SESSION_COOKIE=", "short=***"}; !reflect.DeepEqual(env, want) {
		t.Errorf("env = %q, want %q", env, want)
	}
	copyOp := asMap(dig(t, ops[1], "Op", "file", "actions", 0, "Action", "copy"))
	if copyOp["mode"] != big || copyOp["src"] != "/***/app.txt" {
		t.Errorf("copy = %v, want mode %v and src /***/app.txt", copyOp, big)
	}
	attrs := asMap(dig(t, ops[2], "Op", "source", "attrs"))
	if attrs["git.authtokensecret"] != authID || attrs["local.sharedkeyhint"] != "context" || attrs["token"] != withheldValue {
		t.Errorf("attrs = %v, want allowed keys kept and the token subtree withheld", attrs)
	}
	if op := got.builds[0].steps[5].op; op != nil {
		t.Errorf("nil op = %s, want nil", op)
	}
	wantArgs := []buildArg{{name: "API_TOKEN", withheld: true}, {name: "MODE", value: "fast"}}
	if !reflect.DeepEqual(got.buildArgs, wantArgs) {
		t.Errorf("buildArgs = %+v, want %+v", got.buildArgs, wantArgs)
	}
}

func TestRedactOpEdges(t *testing.T) {
	const value = "ichnos-fake-collision"
	rec := recordWith(t,
		map[string]string{"build-arg:API_TOKEN": value},
		map[string]any{"Op": map[string]any{"a" + value: "first", "a***": "second"}},
		map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"env": "NOT_A_LIST"}}}},
	)
	got := mustRedact(t, rec)
	collided := got.builds[0].steps[0]
	if want := map[string]any{"Op": map[string]any{"a***": "second"}}; !reflect.DeepEqual(opTree(t, collided.op), want) || !collided.withheld {
		t.Errorf("op with colliding keys = %s, withheld %v, want %v, true", collided.op, collided.withheld, want)
	}
	if env := dig(t, opTree(t, got.builds[0].steps[1].op), "Op", "exec", "meta", "env"); env != "NOT_A_LIST" {
		t.Errorf("env that is not a list = %v, want it kept", env)
	}
}

func TestRedactOpStream(t *testing.T) {
	const value = "ichnos-fake-stream"
	clean := `{"args":["a \u0026\u0026 b","\u00e9"],"big":123456789012345678901234567890,"exp":1.5e300,"neg":-0,"t":true,"n":null,"e":{},"l":[]}`
	tests := []struct {
		name, op, want string
		wantWithheld   bool
	}{
		{"clean bytes kept", clean, clean, false},
		{"duplicate names each redacted", `{"env":["API_TOKEN=one"],"env":["API_TOKEN=two"]}`, `{"env":["API_TOKEN=***"],"env":["API_TOKEN=***"]}`, true},
		{"renamed member left out", `{"a` + value + `":"x","b":"y"}`, `{"b":"y"}`, true},
		{"scrubbed string encoded again", `{"s":"q\"` + value + `\\\u00e9"}`, "{\"s\":\"q\\\"***\\\\\u00e9\"}", true},
		{"credential name with an object value", `{"token":{"a":[1,2]},"k":"v"}`, `{"token":"***","k":"v"}`, true},
		{"known value deep inside", strings.Repeat(`{"a":[`, 200) + `"` + value + `"` + strings.Repeat(`]}`, 200), strings.Repeat(`{"a":[`, 200) + `"***"` + strings.Repeat(`]}`, 200), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &Record{&recordData{
				attrs:  map[string]string{"build-arg:API_TOKEN": value},
				builds: []build{{steps: []step{{op: jsontext.Value(tt.op)}}}},
			}}
			got := mustRedact(t, rec).builds[0].steps[0]
			if string(got.op) != tt.want || got.withheld != tt.wantWithheld {
				t.Errorf("redact() of %s = %s, withheld %v, want %s, %v", tt.op, got.op, got.withheld, tt.want, tt.wantWithheld)
			}
		})
	}
}

func TestRedactLongStrings(t *testing.T) {
	long := strings.Repeat("x", maxStringBytes+1)
	var withheld bool
	if got := (scrubber{}).str(long, &withheld); got != withheldValue || !withheld {
		t.Errorf("str() of %d bytes = %d bytes, withheld %v, want %q, true", len(long), len(got), withheld, withheldValue)
	}
	withheld = false
	if got := (scrubber{}).str(long[1:], &withheld); got != long[1:] || withheld {
		t.Errorf("str() of %d bytes changed it, withheld %v", len(long)-1, withheld)
	}
	rec := &Record{&recordData{builds: []build{{steps: []step{{op: jsontext.Value(`{"a":"` + long + `","b":"y"}`)}}}}}}
	got := mustRedact(t, rec).builds[0].steps[0]
	if string(got.op) != `{"a":"***","b":"y"}` || !got.withheld {
		t.Errorf("redact() of a step with a long string = %.40s, withheld %v, want it withheld", got.op, got.withheld)
	}
}

func TestRedactOpTruncated(t *testing.T) {
	const value = "ichnos-fake-truncated"
	op := `{"Op":{"exec":{"meta":{"args":["sh","-c","` + value + `"],"env":["API_TOKEN=` + value + `",["x"],{"env":[1]}],` +
		`"proxy_env":{"http_proxy":"u:p@h"}},"mounts":[{"token":{"a":1}}]},"file":{"actions":[{"mkfile":{"data":"YQ=="}}]},` +
		`"build":{"def":"x"},"n":1}}`
	if !jsontext.Value(op).IsValid() {
		t.Fatalf("test definition is not valid JSON")
	}
	for i := range len(op) {
		rec := &Record{&recordData{
			attrs:  map[string]string{"build-arg:API_TOKEN": value},
			builds: []build{{steps: []step{{op: jsontext.Value(op[:i])}}}},
		}}
		_, err := redact(rec)
		if !errors.Is(err, ErrMalformedRecord) || strings.Contains(err.Error(), value) || strings.Contains(err.Error(), "Op") {
			t.Fatalf("redact() of the first %d bytes error = %v, want ErrMalformedRecord with no record bytes", i, err)
		}
	}
}

// distinctNames returns n object members with distinct names, each followed
// by a comma.
func distinctNames(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `"a%d":0,`, i)
	}
	return b.String()
}

// TestRedactOpMemory checks that redaction streams a step definition
// instead of decoding it into maps and slices, which would allocate tens of
// times its size for these shapes.
func TestRedactOpMemory(t *testing.T) {
	const value = "ichnos-fake-memory"
	for _, tt := range []struct{ name, body string }{
		{"many objects", strings.Repeat(`{"":0},`, 300_000) + "{}"},
		{"many distinct names", "{" + distinctNames(300_000) + `"b":0}`},
		{"many strings", strings.Repeat(`"x",`, 500_000) + `"x"`},
		{"one long URL query", `"https://h/?` + strings.Repeat("a&", 2_000_000) + `a"`},
		{"one long string just under the limit", `"https://h/?` + strings.Repeat("a=&", (maxStringBytes-20)/3) + `"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			op := jsontext.Value(`{"Op":{"x":[` + tt.body + `]}}`)
			rec := &Record{&recordData{
				attrs:  map[string]string{"build-arg:API_TOKEN": value},
				builds: []build{{steps: []step{{op: op}}}},
			}}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			mustRedact(t, rec)
			runtime.ReadMemStats(&after)
			if ratio := float64(after.TotalAlloc-before.TotalAlloc) / float64(len(op)); ratio > 10 {
				t.Errorf("redact() allocated %.1f times the size of the step definition, want at most 10", ratio)
			}
		})
	}
}

func TestRedactProxies(t *testing.T) {
	const proxy = "u:pw@proxy.example"
	rec := recordWith(t,
		map[string]string{"build-arg:http_proxy": proxy + ":3128", "build-arg:NO_PROXY": "localhost"},
		map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{
			"env": []any{"HTTP_PROXY=" + proxy + ":3128", "HTTPS_PROXY=http://" + proxy, "NO_PROXY=localhost"},
			"proxy_env": map[string]any{
				"http_proxy": proxy + ":3128", "https_proxy": "http://" + proxy, "no_proxy": "localhost,.example",
			},
		}}}},
	)
	got := mustRedact(t, rec)
	wantArgs := []buildArg{{name: "NO_PROXY", value: "localhost"}, {name: "http_proxy", value: "***@proxy.example:3128", withheld: true}}
	if !reflect.DeepEqual(got.buildArgs, wantArgs) {
		t.Errorf("buildArgs = %+v, want %+v", got.buildArgs, wantArgs)
	}
	meta := dig(t, opTree(t, got.builds[0].steps[0].op), "Op", "exec", "meta")
	wantEnv := []any{"HTTP_PROXY=***@proxy.example:3128", "HTTPS_PROXY=http://***@proxy.example", "NO_PROXY=localhost"}
	if env := dig(t, meta, "env"); !reflect.DeepEqual(env, wantEnv) {
		t.Errorf("env = %q, want %q", env, wantEnv)
	}
	wantProxy := map[string]any{"http_proxy": "***@proxy.example:3128", "https_proxy": "http://***@proxy.example", "no_proxy": "localhost,.example"}
	if proxy := dig(t, meta, "proxy_env"); !reflect.DeepEqual(proxy, wantProxy) {
		t.Errorf("proxy_env = %v, want %v", proxy, wantProxy)
	}
	if !got.builds[0].steps[0].withheld {
		t.Error("step with a proxy password is not withheld")
	}
}

// TestRedactProxyPasswords checks that a proxy setting loses its whole user
// information when its password holds a raw /, ? or #, which would end it
// in another URL.
func TestRedactProxyPasswords(t *testing.T) {
	proxy, env := scrubber.proxy, scrubber.envEntry
	tests := []struct {
		redact   func(scrubber, string, *bool) string
		in       string
		want     string
		withheld bool
	}{
		{proxy, "alice:pa/ss@proxy.example:3128", "***@proxy.example:3128", true},
		{proxy, "alice:pa?ss@proxy.example:3128", "***@proxy.example:3128", true},
		{proxy, "alice:pa#ss@proxy.example:3128", "***@proxy.example:3128", true},
		{proxy, "alice:12/34@proxy.example:3128", "***@proxy.example:3128", true},
		{proxy, "alice:p@s/s@proxy.example:3128/", "***@proxy.example:3128/", true},
		{proxy, "http://alice:12/34@proxy.example:3128", "http://***@proxy.example:3128", true},
		{proxy, "HTTP://alice:pa://ss@proxy.example:3128", "HTTP://***@proxy.example:3128", true},
		{proxy, "bob://secret@proxy.example:3128", "***@proxy.example:3128", true},
		{proxy, "alice:pa'ss@proxy.example:3128", "***@proxy.example:3128", true},
		{proxy, `http://alice:pa"ss@proxy.example:3128`, "http://***@proxy.example:3128", true},
		{proxy, "alice:pa<s s>@proxy.example:3128", "***@proxy.example:3128", true},
		{proxy, " alice:pa/ss@proxy.example:3128", "***@proxy.example:3128", true},
		{proxy, "SOCKS5H://alice:pa/ss@proxy.example:1080", "SOCKS5H://***@proxy.example:1080", true},
		{proxy, "\u017focks5://alice@proxy.example:1080", "***@proxy.example:1080", true},
		{proxy, "proxy.example:3128", "proxy.example:3128", false},
		{proxy, "http://proxy.example:3128/", "http://proxy.example:3128/", false},
		{env, "HTTPS_PROXY=http://alice:pa/ss@proxy.example:3128", "HTTPS_PROXY=http://***@proxy.example:3128", true},
		{env, "HTTP_PROXY=alice:pa://ss@proxy.example:3128", "HTTP_PROXY=***@proxy.example:3128", true},
		{env, "https_proxy=alice:12/34@proxy.example:3128/", "https_proxy=***@proxy.example:3128/", true},
		{env, "HTTP_PROXY=bob://secret@proxy.example:3128", "HTTP_PROXY=***@proxy.example:3128", true},
		{env, "HTTPS_PROXY=http://alice:pa'ss@proxy.example:3128", "HTTPS_PROXY=http://***@proxy.example:3128", true},
		{env, "NO_PROXY=localhost,.example.com,10.0.0.0/8", "NO_PROXY=localhost,.example.com,10.0.0.0/8", false},
	}
	for _, tt := range tests {
		var withheld bool
		if got := tt.redact(scrubber{}, tt.in, &withheld); got != tt.want || withheld != tt.withheld {
			t.Errorf("redact(%q) = %q, %v, want %q, %v", tt.in, got, withheld, tt.want, tt.withheld)
		}
	}
}

func TestRedactFileData(t *testing.T) {
	clean := base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\necho ready\n"))
	for _, tt := range []struct {
		name         string
		data         any
		want         any
		wantWithheld bool
	}{
		{"clean", clean, clean, false},
		{"assignment", base64.StdEncoding.EncodeToString([]byte("API_TOKEN=abcd\n")), base64.StdEncoding.EncodeToString([]byte("API_TOKEN=***\n")), true},
		{"not base64", "not base64!", withheldValue, true},
		{"not a string", json.Number("5"), json.Number("5"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := mustRedact(t, recordWith(t, nil, map[string]any{"Op": map[string]any{"file": map[string]any{"actions": []any{
				map[string]any{"Action": map[string]any{"mkfile": map[string]any{"path": "/run.sh", "data": tt.data}}},
			}}}}, map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{
				"proxy_env": map[string]any{"http_proxy": json.Number("5")},
			}}}}))
			step := got.builds[0].steps[0]
			if data := dig(t, opTree(t, step.op), "Op", "file", "actions", 0, "Action", "mkfile", "data"); data != tt.want || step.withheld != tt.wantWithheld {
				t.Errorf("data = %v, withheld %v, want %v, %v", data, step.withheld, tt.want, tt.wantWithheld)
			}
			if proxy := dig(t, opTree(t, got.builds[0].steps[1].op), "Op", "exec", "meta", "proxy_env", "http_proxy"); proxy != json.Number("5") {
				t.Errorf("proxy that is not a string = %v, want it kept", proxy)
			}
		})
	}
}

func TestRedactFileDataHoldingKnownValue(t *testing.T) {
	const value = "000="
	rec := recordWith(t, map[string]string{"build-arg:API_TOKEN": value},
		map[string]any{"Op": map[string]any{"file": map[string]any{"actions": []any{
			map[string]any{"Action": map[string]any{"mkfile": map[string]any{"path": "/f", "data": "AAA" + value}}},
		}}}},
	)
	step := mustRedact(t, rec).builds[0].steps[0]
	if data := dig(t, opTree(t, step.op), "Op", "file", "actions", 0, "Action", "mkfile", "data"); data != withheldValue || !step.withheld {
		t.Errorf("data = %v, withheld %v, want %q, true", data, step.withheld, withheldValue)
	}
}

func TestRedactFileDataEncodedAgainHoldingKnownValue(t *testing.T) {
	rec := recordWith(t, map[string]string{"build-arg:API_TOKEN": "000qKio"},
		map[string]any{"Op": map[string]any{
			"exec": map[string]any{"meta": map[string]any{"env": []any{"TOKEN=cdef"}}},
			"file": map[string]any{"actions": []any{
				map[string]any{"Action": map[string]any{"mkfile": map[string]any{"data": "001jZGVm"}}},
			}},
		}},
	)
	step := mustRedact(t, rec).builds[0].steps[0]
	if data := dig(t, opTree(t, step.op), "Op", "file", "actions", 0, "Action", "mkfile", "data"); data != withheldValue {
		t.Errorf("data = %v, want %q", data, withheldValue)
	}
}

func TestRedactKnownValueLimits(t *testing.T) {
	values := func(n, size int) map[string]string {
		attrs := make(map[string]string, n)
		for i := range n {
			id := fmt.Sprintf("%08d", i)
			attrs["build-arg:TOKEN_"+id] = strings.Repeat("v", size-len(id)) + id
		}
		return attrs
	}
	over := values(128, 8<<10)
	over["build-arg:TOKEN_EXTRA"] = "abcd"
	same := map[string]string{}
	short := map[string]string{}
	for i := range 20_000 {
		same[fmt.Sprintf("build-arg:TOKEN_%d", i)] = strings.Repeat("s", 8<<10)
		short[fmt.Sprintf("build-arg:TOKEN_%d", i)] = fmt.Sprintf("%c%c%c", 'a'+i%26, 'a'+i/26%26, 'a'+i/676%26)
	}
	for _, tt := range []struct {
		name    string
		attrs   map[string]string
		wantErr func(error) bool
	}{
		{"total at the limit", values(128, 8<<10), nil},
		{"total over the limit", over, isLimit("bytes of credential values")},
		{"one value at the limit", values(1, maxKnownBytes), nil},
		{"one value over the limit", values(1, maxKnownBytes+1), isLimit("bytes of credential values")},
		{"many small values", values(20_000, 8), nil},
		{"a repeated value counts once", same, nil},
		{"short values do not count", short, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := redact(&Record{&recordData{attrs: tt.attrs}})
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("redact() error = %v, want nil", err)
				}
				return
			}
			if !tt.wantErr(err) {
				t.Fatalf("redact() error = %v, want a different error", err)
			}
		})
	}
}

func TestRedactOverlappingValues(t *testing.T) {
	rec := recordWith(t, map[string]string{"build-arg:API_TOKEN": "abcd", "build-arg:OTHER_TOKEN": "cdefgh"},
		map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []any{"x abcdefgh y cdefgh abcd"}}}}},
	)
	args := dig(t, opTree(t, mustRedact(t, rec).builds[0].steps[0].op), "Op", "exec", "meta", "args")
	if want := []any{"x *** y *** ***"}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

func TestRedactKeepsFlagValuesInOtherSteps(t *testing.T) {
	const name = "AUTH_ENABLED"
	for _, source := range []string{"build argument", "environment variable"} {
		for _, tt := range []struct {
			value string
			kept  bool
		}{
			{"true", true},
			{"FALSE", true},
			{"Enabled", true},
			{"disabled", true},
			{"Disable", true},
			{"enable", true},
			{"true-ish", false},
			{"fal\u017fe", false},
			{"di\u017fabled", false},
		} {
			t.Run(source+" "+tt.value, func(t *testing.T) {
				cmd := "npm config set strict-ssl=" + tt.value
				var attrs map[string]string
				var env []any
				if source == "build argument" {
					attrs = map[string]string{"build-arg:" + name: tt.value}
				} else {
					env = []any{name + "=" + tt.value}
				}
				rec := recordWith(t, attrs,
					map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"env": env}}}},
					map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []any{"/bin/sh", "-c", cmd}}}}},
				)
				rec.builds[0].steps[1].text = "RUN " + cmd
				got := mustRedact(t, rec)
				st := got.builds[0].steps[1]
				wantCmd := cmd
				if !tt.kept {
					wantCmd = "npm config set strict-ssl=***"
				}
				if args := dig(t, opTree(t, st.op), "Op", "exec", "meta", "args"); !reflect.DeepEqual(args, []any{"/bin/sh", "-c", wantCmd}) {
					t.Errorf("args = %q, want the command %q", args, wantCmd)
				}
				if st.text != "RUN "+wantCmd || st.withheld == tt.kept {
					t.Errorf("text = %q, withheld = %v, want %q, %v", st.text, st.withheld, "RUN "+wantCmd, !tt.kept)
				}
				if source == "build argument" {
					if want := []buildArg{{name: name, withheld: true}}; !reflect.DeepEqual(got.buildArgs, want) {
						t.Errorf("buildArgs = %+v, want %+v", got.buildArgs, want)
					}
				} else if env := dig(t, opTree(t, got.builds[0].steps[0].op), "Op", "exec", "meta", "env"); !reflect.DeepEqual(env, []any{name + "=***"}) {
					t.Errorf("env = %q, want the value withheld", env)
				}
			})
		}
	}
}

func TestRedactScrubLongestFirst(t *testing.T) {
	rec := recordWith(t,
		map[string]string{"build-arg:API_TOKEN": "abcd", "build-arg:LONG_TOKEN": "abcdefgh", "build-arg:SHORT_TOKEN": "abc"},
		map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []any{"x abcdefgh abcd abc"}}}}},
	)
	got := mustRedact(t, rec)
	args := dig(t, opTree(t, got.builds[0].steps[0].op), "Op", "exec", "meta", "args")
	if want := []any{"x *** *** abc"}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

func TestRedactAttributes(t *testing.T) {
	tests := []struct {
		name        string
		attrs       map[string]string
		wantImports []cacheType
		wantNoCache noCacheSetting
	}{
		{"no import", map[string]string{}, nil, noCacheSetting{}},
		{"local", map[string]string{"cache-imports": `[{"Type":"local","Attrs":{}}]`}, []cacheType{cacheLocal}, noCacheSetting{}},
		{
			"every type",
			map[string]string{"cache-imports": `[{"Type":"registry"},{"Type":"gha"},{"Type":"s3"},{"Type":"azblob"},{"Type":"local"},{"Type":"other"}]`},
			[]cacheType{cacheRegistry, cacheGHA, cacheS3, cacheAzblob, cacheLocal, cacheUnknown},
			noCacheSetting{},
		},
		{"unreadable", map[string]string{"cache-imports": "{"}, []cacheType{cacheUnknown}, noCacheSetting{}},
		{"empty string", map[string]string{"cache-imports": ""}, []cacheType{cacheUnknown}, noCacheSetting{}},
		{"empty list", map[string]string{"cache-imports": "[]"}, []cacheType{}, noCacheSetting{}},
		{"no-cache for every stage", map[string]string{"no-cache": ""}, nil, noCacheSetting{set: true}},
		{"no-cache for stages", map[string]string{"no-cache": "builder,final"}, nil, noCacheSetting{set: true, stages: "builder,final"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustRedact(t, &Record{&recordData{attrs: tt.attrs}})
			if !reflect.DeepEqual(got.imports, tt.wantImports) || got.noCache != tt.wantNoCache {
				t.Errorf("imports, noCache = %v, %+v, want %v, %+v", got.imports, got.noCache, tt.wantImports, tt.wantNoCache)
			}
		})
	}
}

func TestRedactVertexKinds(t *testing.T) {
	var vs []vertex
	for _, kind := range []vertexKind{kindStep, kindInternal, kindImport, kindExport} {
		vs = append(vs, vertex{digest: "sha256:" + kind.String(), kind: kind, cached: true})
	}
	got := mustRedact(t, &Record{&recordData{vertices: vs}})
	for i, v := range vs {
		if got.vertices[i].kind != v.kind || !got.vertices[i].cached {
			t.Errorf("redact() of a vertex of kind %v = %+v, want the same kind", v.kind, got.vertices[i])
		}
	}
	if strings.Contains(dumpRedacted(got), "sha256:") {
		t.Errorf("redact() kept a vertex digest")
	}
}

func TestRedactIsPure(t *testing.T) {
	path := fixturePath(t, "control", "pr")
	rec, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	rec.attrs["build-arg:API_TOKEN"] = "ichnos-fake-pure"
	before := dumpRecord(rec)
	once := mustRedact(t, rec)
	if after := dumpRecord(rec); after != before {
		t.Fatalf("redact() changed the Record")
	}
	if twice := mustRedact(t, rec); !redactedEqual(once, twice) {
		t.Errorf("redact() twice gave different results")
	}
	clear(rec.builds[0].steps[0].op)
	rec.builds[0].steps[0].inputs = append(rec.builds[0].steps[0].inputs, "changed")
	if !redactedEqual(once, mustRedact(t, mustCollect(t, path))) {
		t.Errorf("changing the Record after redact() changed the redacted value")
	}
	checkNoPointers(t, reflect.ValueOf(once), "redacted")
}

func mustCollect(tb testing.TB, path string) *Record {
	tb.Helper()
	rec, err := NewBundleCollector().Collect(tb.Context(), Source{Path: path})
	if err != nil {
		tb.Fatal(err)
	}
	rec.attrs["build-arg:API_TOKEN"] = "ichnos-fake-pure"
	return rec
}

func checkNoPointers(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	k := v.Kind()
	if k == reflect.Pointer || k == reflect.UnsafePointer || k == reflect.Chan || k == reflect.Func {
		t.Errorf("%s is a %s", path, k)
	}
	if k == reflect.Interface && !v.IsNil() {
		checkNoPointers(t, v.Elem(), path)
	}
	if k == reflect.Struct {
		for i := range v.NumField() {
			checkNoPointers(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	}
	if k == reflect.Slice || k == reflect.Array {
		for i := range v.Len() {
			checkNoPointers(t, v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	}
	if k == reflect.Map {
		for _, key := range v.MapKeys() {
			checkNoPointers(t, v.MapIndex(key), fmt.Sprintf("%s[%v]", path, key))
		}
	}
}

func TestRedactFixtures(t *testing.T) {
	signed := false
	for _, path := range fixturePaths(t) {
		rec := func() *Record {
			r, err := NewBundleCollector().Collect(t.Context(), Source{Path: path})
			if err != nil {
				t.Fatalf("Collect(%s) error = %v", path, err)
			}
			return r
		}()
		got := mustRedact(t, rec)
		b := loadBundle(t, path)
		if strings.Contains(string(b.blob(b.layerHex("application/vnd.buildkit.otlp.json.v0"))), "Signature=") {
			signed = true
		}
		if strings.Contains(dumpRedacted(got), "Signature=") {
			t.Errorf("%s: redacted value holds a signed URL", path)
		}
		stepVertices := map[int]bool{}
		for _, b := range rec.builds {
			for _, s := range b.steps {
				stepVertices[s.vertex] = true
			}
		}
		for i, v := range got.vertices {
			if v.kind == kindStep && !stepVertices[i] {
				t.Errorf("%s: vertex %q is a build step, but no step points to it", path, rec.vertices[i].digest)
			}
		}
		for i, rb := range rec.builds {
			gb := got.builds[i]
			if gb.platform != rb.platform || len(gb.steps) != len(rb.steps) {
				t.Fatalf("%s: build %d changed", path, i)
			}
			for j, rs := range rb.steps {
				gs := gb.steps[j]
				if gs.withheld || gs.id != rs.id || gs.text != rs.text || gs.lines != rs.lines || gs.vertex != rs.vertex ||
					!slices.Equal(gs.inputs, rs.inputs) || !bytes.Equal(gs.op, rs.op) {
					t.Errorf("%s: redact() changed step %q of a clean record", path, rs.id)
				}
			}
		}
		for _, a := range got.buildArgs {
			if a.withheld || a.value != rec.attrs["build-arg:"+a.name] {
				t.Errorf("%s: redact() changed build argument %q of a clean record", path, a.name)
			}
		}
	}
	if !signed {
		t.Error("no fixture trace holds Signature=, so this test checks nothing")
	}
}

func FuzzRedactText(f *testing.F) {
	f.Add("RUN echo ", "API_TOKEN", "fake-value-1234", " && make")
	f.Add("ENV A=b \\\n  ", "apiKey", "v", "")
	f.Add("ENV ", "TOKEN", "x", " y")
	f.Add("https://u:p@h/?", "token", "q", "#f")
	f.Add("", "Authorization", "Bearer", "")
	f.Add("", "TOKEN", "pa/s?s#s", "")
	f.Add("", "TOKEN", "12/34", "")
	f.Add("", "TOKEN", "//", "")
	f.Add("0", "0", "//0", "")
	f.Add("", "TOKEN", "pa'ss", "")
	f.Add("", "TOKEN", "p@ss/word", "")
	f.Add("", "TOKEN", "1234/x", "")
	f.Add("", "TOKEN", "/DEF+ghi", "")
	f.Add("://0://0://", "@", "0", "0")
	f.Fuzz(func(t *testing.T, prefix, name, value, suffix string) {
		text := prefix + name + "=" + value + suffix
		var withheld bool
		once := scrubber{}.text(text, &withheld)
		var again bool
		if twice := (scrubber{}).text(once, &again); twice != once {
			t.Fatalf("text(%q) = %q, then %q", text, once, twice)
		}
		checkURLPassword(t, prefix, value, suffix)
		checkProxyPassword(t, value)
		checkURLToken(t, value)
		checkKnownValue(t, prefix, value, suffix)
		if !isCredentialName(name) || value == "" || !validName(name) {
			return
		}
		if prefix != "" && isNameByte(prefix[len(prefix)-1]) {
			return
		}
		if suffix != "" && !strings.ContainsRune(" \t\r\n", rune(suffix[0])) {
			return
		}
		if strings.ContainsAny(value, " \t\r\n\"'\\*") || strings.Contains(prefix+name+"=", value) || strings.Contains(suffix, value) {
			return
		}
		if !withheld {
			t.Errorf("text(%q) did not report a withheld value", text)
		}
		if strings.Contains(once, value) {
			t.Errorf("text(%q) = %q, which holds the value %q", text, once, value)
		}
	})
}

func FuzzRedactOp(f *testing.F) {
	for _, path := range fixturePaths(f) {
		for _, b := range mustCollect(f, path).builds {
			for _, st := range b.steps {
				if st.op != nil {
					f.Add([]byte(st.op), "ichnos-fake-fuzz")
				}
			}
		}
	}
	f.Add([]byte(`{"env":["API_TOKEN=abcdefgh","A=abcdefgh"],"mkfile":{"data":"YWJjZGVmZ2g="},"build":{"def":"x"},"http_proxy":"u:p@h","u":"https://u:p@h/?a=1#f","n":1e999,"abcdefgh":1}`), "abcdefgh")
	f.Add([]byte(`{ "x" : 1 ,  "abcdefgh" : { "a" : [ 1 ] } , "k" :"v" }`), "abcdefgh")
	f.Add([]byte(`{"abcdefgh":1 , "token" :  2}`), "abcdefgh")
	f.Add([]byte(`{"env":["AUTH_ENABLED=True"],"args":["--strict-ssl=True"]}`), "True")
	f.Add([]byte(`{"u":"https://h/?000=1="}`), "00=1")
	f.Add([]byte(`{"":"://0://0://@"}`), "0")
	f.Fuzz(func(t *testing.T, raw []byte, known string) {
		if !jsontext.Value(raw).IsValid(jsontext.AllowDuplicateNames(true)) {
			return
		}
		op, err := stepDefinition(raw)
		if err != nil || op == nil || len(known) > maxKnownBytes {
			return
		}
		redactOne := func(op jsontext.Value) redactedStep {
			rec := &Record{&recordData{
				attrs:  map[string]string{"build-arg:API_TOKEN": known},
				builds: []build{{steps: []step{{op: op}}}},
			}}
			got, err := redact(rec)
			if err != nil {
				t.Fatalf("redact() of the valid definition %q error = %v", op, err)
			}
			return got.builds[0].steps[0]
		}
		once := redactOne(op)
		if once.op.Kind() != '{' || !once.op.IsValid(jsontext.AllowDuplicateNames(true)) {
			t.Fatalf("redact() of %q gave invalid JSON %q", op, once.op)
		}
		if !once.withheld && !bytes.Equal(once.op, op) {
			t.Fatalf("redact() changed %q to %q without withholding anything", op, once.op)
		}
		if strings.Contains(known, "*") {
			return
		}
		if twice := redactOne(once.op); !bytes.Equal(twice.op, once.op) {
			t.Fatalf("redact() of %q gave %q, then %q", op, once.op, twice.op)
		}
		tree := opTree(t, once.op)
		if scrubbable(known) {
			for _, s := range treeStrings(tree) {
				if strings.Contains(s, known) {
					t.Fatalf("redact() of %q kept the known value %q: %q", op, known, once.op)
				}
			}
		}
		checkCredentialMembers(t, tree)
	})
}

// treeStrings returns every name and string in a decoded definition.
func treeStrings(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			out = append(out, k)
			out = append(out, treeStrings(child)...)
		}
	case []any:
		for _, child := range x {
			out = append(out, treeStrings(child)...)
		}
	case string:
		out = append(out, x)
	}
	return out
}

// checkCredentialMembers checks that every member whose name looks like a
// credential, and every such environment entry, has its value withheld.
func checkCredentialMembers(t *testing.T, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if isCredentialName(k) && child != withheldValue {
				t.Fatalf("member %q kept its value %v", k, child)
			}
			if entries, ok := child.([]any); ok && k == "env" {
				for _, e := range entries {
					entry, _ := e.(string)
					if name, value, ok := strings.Cut(entry, "="); ok && isCredentialName(name) && value != "" && value != withheldValue {
						t.Fatalf("environment entry %q kept its value", entry)
					}
				}
			}
			checkCredentialMembers(t, child)
		}
	case []any:
		for _, child := range x {
			checkCredentialMembers(t, child)
		}
	}
}

// urlUser is the user name next to the passwords that the fuzz tests
// plant. A user name that survives shows that part of the user information
// did.
const urlUser = "ichnos-user"

// checkURLPassword checks that the user information of a URL never
// survives, whatever its password holds, with a plain user name or an
// email address as one.
func checkURLPassword(t *testing.T, prefix, password, suffix string) {
	t.Helper()
	const after = "@host.example/"
	if password == "" || strings.ContainsAny(password, "*") || strings.IndexFunc(password, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 ||
		strings.Contains(after+suffix, password) || strings.Contains(prefix+password+suffix, urlUser) {
		return
	}
	for _, before := range []string{"https://" + urlUser + ":", "https://" + urlUser + "@example.com:"} {
		if strings.Contains(prefix+before, password) {
			continue
		}
		text := prefix + before + password + after + suffix
		var withheld bool
		if got := (scrubber{}).text(text, &withheld); strings.Contains(got, password) || strings.Contains(got, urlUser) || !withheld {
			t.Errorf("text(%q) = %q, %v, which keeps the user information", text, got, withheld)
		}
	}
}

// urlToken starts the tokens that checkURLToken plants as a user name.
const urlToken = "ichnos-token"

// checkURLToken checks that a token used alone as a URL's user name never
// survives. It may hold anything that a token of a known format can, which
// is never an @ or a colon, unless what comes before its first /, ?, #,
// quote or angle bracket reads as a host name with a dot, as in
// https://a.b/c@host, which every URL parser reads as a URL to a.b.
func checkURLToken(t *testing.T, token string) {
	t.Helper()
	token = urlToken + token
	if strings.ContainsAny(token, "*@:") || strings.IndexFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 ||
		strings.Count(token, urlToken) != 1 {
		return
	}
	if i := strings.IndexAny(token, "/?#\"'<>`"); i >= 0 && keepsPath(token[:i]) {
		return
	}
	text := "https://" + token + "@host.example/"
	var withheld bool
	if got := (scrubber{}).str(text, &withheld); strings.Contains(got, urlToken) || !withheld {
		t.Errorf("str(%q) = %q, %v, which keeps the token", text, got, withheld)
	}
}

// checkProxyPassword checks that the user information of a proxy setting
// never survives, with or without a scheme, whatever its password holds.
func checkProxyPassword(t *testing.T, password string) {
	t.Helper()
	if password == "" || strings.Contains(password, "*") || strings.Contains(password, urlUser) {
		return
	}
	for _, setting := range []string{urlUser + ":" + password + "@proxy.example:3128", "http://" + urlUser + ":" + password + "@proxy.example:3128/"} {
		for _, tt := range []struct {
			redact func(scrubber, string, *bool) string
			text   string
		}{
			{scrubber.proxy, setting},
			{scrubber.envEntry, "HTTPS_PROXY=" + setting},
		} {
			if strings.Count(tt.text, password) != 1 {
				continue
			}
			var withheld bool
			if got := tt.redact(scrubber{}, tt.text, &withheld); strings.Contains(got, password) || strings.Contains(got, urlUser) || !withheld {
				t.Errorf("redact(%q) = %q, %v, which keeps the user information", tt.text, got, withheld)
			}
		}
	}
}

// checkKnownValue checks that the value of a credential build argument
// never survives in other text.
func checkKnownValue(t *testing.T, prefix, value, suffix string) {
	t.Helper()
	if !scrubbable(value) || len(value) > maxKnownBytes || strings.Contains(value, "*") {
		return
	}
	s, err := newScrubber(&Record{&recordData{attrs: map[string]string{"build-arg:API_TOKEN": value}}})
	if err != nil {
		t.Fatalf("newScrubber() error = %v", err)
	}
	text := prefix + value + suffix
	var withheld bool
	if got := s.text(text, &withheld); strings.Contains(got, value) || !withheld {
		t.Errorf("text(%q) = %q, %v, which keeps the known value %q", text, got, withheld, value)
	}
}

func validName(name string) bool {
	for i := range len(name) {
		if !isNameByte(name[i]) {
			return false
		}
	}
	return name != ""
}

// BenchmarkRedact redacts the lab records; a record of 1,000 steps that each
// use a credential in a URL, an assignment and environment variables; and
// strings of nearly maxStringBytes with a known value every 5 bytes, which
// cost the most per byte.
func BenchmarkRedact(b *testing.B) {
	b.Run("lab records", func(b *testing.B) {
		var records []*Record
		for _, p := range fixturePaths(b) {
			records = append(records, mustCollect(b, p))
		}
		benchmarkRedact(b, records...)
	})
	b.Run("known values", func(b *testing.B) {
		const planted = "ichnos-fake-value-1234"
		ops := make([]map[string]any, 1000)
		for i := range ops {
			cmd := fmt.Sprintf("curl -fsSL https://ci:%s@registry.example/v2/%d?sig=%s -o /tmp/x && API_TOKEN=%s make", planted, i, planted, planted)
			ops[i] = map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{
				"args": []any{"/bin/sh", "-c", cmd},
				"env":  []any{"PATH=/usr/local/bin:/usr/bin", "API_TOKEN=" + planted, "HTTP_PROXY=http://ci:" + planted + "@proxy.example:3128"},
			}}}}
		}
		rec := recordWith(b, map[string]string{"build-arg:API_TOKEN": planted}, ops...)
		for i := range rec.builds[0].steps {
			rec.builds[0].steps[i].text = "RUN " + fmt.Sprint(ops[i]["Op"])
		}
		benchmarkRedact(b, rec)
	})
	b.Run("crafted strings", func(b *testing.B) {
		n := maxStringBytes/5 - 16
		rec := recordWith(b, map[string]string{"build-arg:API_TOKEN": "abcd"},
			map[string]any{"args": []any{strings.Repeat("abcdX", n) + " TOKEN=y", "TOKEN=" + strings.Repeat("abcdX", n)}})
		benchmarkRedact(b, rec)
	})
}

func benchmarkRedact(b *testing.B, records ...*Record) {
	b.Helper()
	b.ReportAllocs()
	for b.Loop() {
		for _, r := range records {
			if _, err := redact(r); err != nil {
				b.Fatal(err)
			}
		}
	}
}
