# Scenario Lab

Real builds that break the cache in known ways. `lab/record.sh` runs them against a pinned BuildKit and saves each build's record as `testdata/buildkit-<version>/<scenario>/{base,pr}.dockerbuild`: `base` is the base branch's build, and `pr` is the pull request's.

Each build starts on an empty builder, as in CI, so the only cache it can use is the one it imports.

## Scenarios

All scenarios build the same six-line Dockerfile. The base build exports its cache, and the pull request build imports it unless the table says otherwise.

| Scenario | What the pull request changes | Lines that hit | Lines that miss |
|---|---|---|---|
| `control` | Nothing | 2-6 | None |
| `changed-instruction` | `RUN wc -c` becomes `RUN wc -l` on line 4 | 2-3 | 4-6 |
| `changed-copy-file` | The content of `app.txt`, copied on line 5 | 2-4 | 5-6 |
| `changed-base-image` | Alpine 3.22 becomes 3.21 on line 1 | None | 2-6 |
| `no-cache-import` | Nothing, and the build imports no cache | None | 2-6 |

Line 1, `FROM`, is left out of the table because it never reports a hit, even when every later step does. `changed-base-image` changes the text of the `FROM` line, so a tag that moves without a text change is not covered yet.

## Recording

```sh
lab/record.sh
```

This needs Docker with its socket at `/var/run/docker.sock`, and `jq`. It creates a temporary `ichnos-lab` builder and removes it afterward, so it leaves other builders and their records alone. It replaces the records in `testdata/` only after every build has passed.

The records are public, and a record keeps the build folder and the Docker socket path. So the script builds in `/tmp/ichnos-lab` through the default socket, and it stops if a record contains the local user name, host name or macOS temporary path. Check new records by hand as well: fixtures must never use real credentials or private content.

## What the Records Show

These facts come from records made with BuildKit v0.34.0 and buildx v0.28.0-desktop.1 for `linux/amd64`. Update the versions here when you re-record.

### Format

BuildKit documents the [provenance fields](https://github.com/moby/buildkit/blob/v0.34.0/docs/attestations/slsa-definitions.md), except `digestMapping`. The rest of this format is undocumented, so a later version may change it.

- A `.dockerbuild` file is a gzipped tar of an OCI layout with one manifest. Its config is the history record (`application/vnd.buildkit.historyrecord.v0`), and its layers are the trace, the status stream, the provenance and the result's manifest.
- The status stream (`application/vnd.buildkit.status.v0`) is a series of frames: a 4-byte little-endian length, then a `StatusResponse` protobuf message. Each record here has one frame. Vertices have a digest, inputs, a name, a cached flag and times, and they are not in Dockerfile order.
- A vertex repeats within a frame, and later updates leave out `cached`. A step hit the cache if any update says so: keeping only the last update reports hits as misses.
- The provenance (`application/vnd.in-toto+json`) is a bare SLSA predicate with no statement around it. It holds each step's definition, a map from vertex digest to step, each step's Dockerfile line, the resolved base image and the Dockerfile text. History records [always use `mode=max`](https://github.com/moby/buildkit/blob/v0.34.0/solver/llbsolver/history.go#L118-L121), whatever `FrontendAttrs` says.
- `numCachedSteps` is missing, not zero, when no step hit.
- `FrontendAttrs` has `cache-imports` only when the build imports a cache, and its attributes are empty for a local cache.
- `http.request.header.authorization` values in the trace are masked as `****`, but `url.full` values keep their query strings, including CDN signatures. Here they are for public Alpine layers and expire within the hour, but on a private registry they could be live, so the collector has to redact the trace.

### Step digests

The rule in [proposal 002](../docs/proposals/002-ichnos-architecture.md#explanation-algorithm) that a changed digest means a changed input does not hold, and the proposal still states it. In `control`, nothing changes, but lines 3-6 get a new digest and still hit the cache. In `no-cache-import`, the same steps get a new digest and miss. Each build gives its [build context a new session ID](https://github.com/moby/buildkit/blob/v0.34.0/frontend/dockerui/config.go#L463), which changes the digest of the context and of every step that descends from it. Only the numbered steps before the first `COPY` keep their digests, and `[internal]` steps change in every build.

Step definitions in the provenance do not have this problem, because [BuildKit removes the session ID from them](https://github.com/moby/buildkit/blob/v0.34.0/solver/llbsolver/provenance/buildconfig.go#L94-L96). The `op` and `inputs` of each entry were identical in both `control` builds. Whole entries are not, because `resourceUsage` appears only on steps that ran.

Comparing `op` and `inputs` by Dockerfile line, along with each step's ancestors, and taking the first miss, not counting an unchanged `FROM`, found no miss in `control` and the right cause in the other four scenarios:

| First miss | Cause |
|---|---|
| The `FROM` definition changed | The base image changed |
| The step's definition changed | The instruction changed |
| Nothing changed, and the build imported a cache | Copied content changed, or part of the cache is missing |
| Nothing changed, and the build imported no cache | No cache import |

This comparison is a candidate, not a result:

- It was written after looking at these records, on one BuildKit version and one architecture, and nothing in this repository runs it yet.
- No scenario adds or removes a line, so alignment by Dockerfile line is untested. A shifted line compares against the wrong step. `local.followpaths` on the build context also lists every `COPY` source, so adding a `COPY` changes an ancestor of every `COPY` step.
- One record cannot separate changed content from a partly missing cache, which is why the pull request diff is still needed.
