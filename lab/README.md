# Scenario Lab

Real builds that break the cache in known ways. `lab/record.sh` runs them against a pinned BuildKit and saves each build's record as `testdata/buildkit-<version>/<scenario>/{base,pr}.dockerbuild`: `base` is the base branch's build, and `pr` is the pull request's.

Each build starts on an empty builder, as in CI, so the only cache it can use is the one it imports.

## Scenarios

Base builds use the same six-line Dockerfile, plus the extra line in `changed-dockerignore`, and export their cache. The pull request build imports that cache unless the table says otherwise.

| Scenario | What the pull request changes | Lines that hit | Lines that miss |
|---|---|---|---|
| `control` | Nothing | 2-6 | None |
| `changed-instruction` | `RUN wc -c` becomes `RUN wc -l` on line 4 | 2-3 | 4-6 |
| `changed-copy-file` | The content of `app.txt`, copied on line 5 | 2-4 | 5-6 |
| `changed-base-image` | Alpine 3.22 becomes 3.21 on line 1 | None | 2-6 |
| `no-cache-import` | Nothing, and the build imports no cache | None | 2-6 |
| `added-step` | Inserts `RUN echo ready > ready.txt` as line 3 | 2 | 3-7 |
| `added-copy` | Adds `COPY extra.txt .` as line 7, and changes the content of `app.txt` | 2-4 | 5-7 |
| `changed-dockerignore` | `.dockerignore` ignores `extra.txt` instead of `unused.txt`, and both builds end with `COPY . all/` on line 7 | 2-6 | 7 |

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
- The status stream (`application/vnd.buildkit.status.v0`) is a series of frames: a 4-byte little-endian length, then a `StatusResponse` protobuf message. Each record here has one frame. Vertices have a digest, inputs, a name, a cached flag and times, and they are not in Dockerfile order. Step names start with a bracketed position such as `[2/6]`, `[ 1/12]` or `[builder 2/5]`, and its total changes when a `RUN`, `COPY`, `ADD` or `WORKDIR` line is added.
- A vertex repeats within a frame, and later updates leave out `cached`. A step hit the cache if any update says so: keeping only the last update reports hits as misses.
- The provenance (`application/vnd.in-toto+json`) is a bare SLSA predicate with no statement around it. It holds each step's definition, a map from vertex digest to step, each step's Dockerfile line, the resolved base image and the Dockerfile text. History records [always use `mode=max`](https://github.com/moby/buildkit/blob/v0.34.0/solver/llbsolver/history.go#L118-L121), whatever `FrontendAttrs` says.
- `numCachedSteps` is missing, not zero, when no step hit.
- `FrontendAttrs` has `cache-imports` only when the build imports a cache, and its attributes are empty for a local cache.
- `http.request.header.authorization` values in the trace are masked as `****`, but `url.full` values keep their query strings, including CDN signatures. Here they are for public Alpine layers and expire within the hour, but on a private registry they could be live, so the collector has to redact the trace.

### Step digests

The rule in [proposal 002](../docs/proposals/002-ichnos-architecture.md#explanation-algorithm) that a changed digest means a changed input does not hold, and the proposal still states it. In `control`, nothing changes, but lines 3-6 get a new digest and still hit the cache. In `no-cache-import`, the same steps get a new digest and miss. Each build gives its [build context a new session ID](https://github.com/moby/buildkit/blob/v0.34.0/frontend/dockerui/config.go#L463), which changes the digest of the context and of every step that descends from it. Only the numbered steps before the first `COPY` keep their digests, and the `[internal]` steps that load the build context, Dockerfile and `.dockerignore` change in every build.

Step definitions in the provenance do not have this problem, because [BuildKit removes the session ID from them](https://github.com/moby/buildkit/blob/v0.34.0/solver/llbsolver/provenance/buildconfig.go#L94-L96). The `op` and `inputs` of each entry were identical in both `control` builds. Whole entries are not, because `resourceUsage` appears only on steps that ran.

Comparing step definitions found no miss in `control` and the right cause in the other seven scenarios. This is a finding that proposal 002 has not adopted: its alignment differs from step 1 below, and its snapshot does not store what step 2 needs. The comparison works in three steps:

1. Align the numbered steps of both builds in Dockerfile order by instruction text, which is the step name without its bracketed position. Line numbers alone are not enough, because an added line shifts every step after it. Before the first aligned step, between any two and after the last, pair the remaining base and pull request steps in order: each pair is a changed step, and a pull request step left with no partner was added.
2. Compare each aligned step's `op`, and a fingerprint built from that `op` and the fingerprints of its inputs, working down from the first step. `inputs` links steps by ID, and those IDs shift when a step is inserted, so use what they point to, not the IDs. Ignore `local.followpaths` and `local.excludepatterns` here, because both sit on the build context: the first lists every `COPY` source and the second holds the `.dockerignore` patterns, so a change to either would otherwise change every `COPY` step's fingerprint. Compare `local.excludepatterns` separately between the two builds.
3. Take the first miss, not counting an unchanged `FROM`. The first row that matches gives the cause.

| First miss | Cause |
|---|---|
| The build imported no cache | No cache import |
| The step has no partner in the base build | An instruction was added |
| The `FROM` definition changed | The base image changed |
| The step's definition changed | The instruction changed |
| The step does not descend from the build context | Part of the cache is missing |
| The `.dockerignore` patterns differ | `.dockerignore` changed, copied content changed, or part of the cache is missing |
| Anything else | Copied content changed, or part of the cache is missing |

These scenarios shaped the comparison, so passing them is not independent evidence. Each rule is needed, though: without the alignment in step 1 and the `local.followpaths` exception, it gets the causes of `added-step` and `added-copy` wrong, and without the `local.excludepatterns` handling it gets `changed-dockerignore` wrong.

It is still a candidate, not a result:

- Nothing in this repository runs it yet.
- It has seen one BuildKit version, one architecture and eight single-stage scenarios. Multi-stage builds, reordered lines, identical lines, a changed line next to an added one, and builds of 10 or more steps are untested. "An instruction was added" rests on `added-step` alone.
- No scenario covers a partly missing cache, so those causes are unmeasured.
- A removed line has no row, so it would be reported as a changed instruction.
- BuildKit expands variables in `RUN` and `WORKDIR` step names, so a changed build argument could look like a changed line. Lines such as `ENV` create no step of their own, so adding one should appear as a changed `RUN`, not an added step. No scenario here records either case.
- One record cannot separate changed content from a partly missing cache or from a `.dockerignore` change, which is why the pull request diff is still needed.
