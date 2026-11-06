# Scenario Lab

Real builds that break the cache in known ways. `lab/record.sh` runs them against a pinned BuildKit and saves each build's record as `testdata/buildkit-<version>/<scenario>/{base,pr}.dockerbuild`: `base` is the base branch's build, and `pr` is the pull request's.

Each build starts on an empty builder, as in CI, so the only cache it can use is the one it imports.

## Scenarios

The first eight base builds use the same six-line Dockerfile, plus the extra line in `changed-dockerignore`. Every base build exports its cache with `mode=max`, and the pull request build imports it, unless the table says otherwise. Line numbers are those of the pull request's Dockerfile.

| Scenario | What the pull request changes | Lines that hit | Lines that miss | Expected cause |
|---|---|---|---|---|
| `control` | Nothing | 2-6 | None | None |
| `changed-instruction` | `RUN wc -c` becomes `RUN wc -l` on line 4 | 2-3 | 4-6 | The instruction changed |
| `changed-copy-file` | The content of `app.txt`, copied on line 5 | 2-4 | 5-6 | Copied content changed |
| `changed-base-image` | Alpine 3.22 becomes 3.21 on line 1 | None | 2-6 | The base image changed |
| `no-cache-import` | Nothing, and the build imports no cache | None | 2-6 | `no-cache-import` |
| `added-step` | Inserts `RUN echo ready > ready.txt` as line 3 | 2 | 3-7 | An instruction was added |
| `added-copy` | Adds `COPY extra.txt .` as line 7, and changes the content of `app.txt` | 2-4 | 5-7 | Copied content changed |
| `changed-dockerignore` | `.dockerignore` ignores `extra.txt` instead of `unused.txt`, and both builds end with `COPY . all/` on line 7 | 2-6 | 7 | `.dockerignore` changed |
| `multi-stage` | The content of `app.txt`, copied on line 3 of a two-stage build. Stage 2 copies two files from stage 1, and only the one on line 10 changes | 2, 7-9 | 3-5, 10 | Copied content changed |
| `removed-step` | Removes `RUN echo ready > ready.txt`, which was line 3 | 2 | 3-6 | An instruction was removed |
| `reordered-steps` | Swaps `RUN wc -c` and `COPY app.txt .`, so that they are lines 5 and 4 | 2-3 | 4-6 | An instruction moved |
| `many-steps` | Line 11 of a build of 12 lines | 2-10 | 11-12 | The instruction changed |
| `added-env` | Inserts `ENV MODE=ichnos-fake-mode` as line 3 | 2, 4 | 5-7 | The environment changed: `MODE` |
| `added-user` | Inserts `USER 0:0` as line 3 | 2, 4 | 5-7 | Another field changed: the user |
| `changed-build-arg` | The value of `MODE`, which `RUN` reads on line 5. The Dockerfile does not change | 3-4 | 5-7 | The environment changed: `MODE` |
| `rotating-build-arg` | The value of `BUILD_ID`, a different one in every build. Line 2 declares it, no line reads it, and the Dockerfile does not change | 3-4 | 5-7 | The environment changed: `BUILD_ID` |
| `min-mode-needed` | The base build exports with `mode=min`, and line 7 copies a stage 1 file to a new path | 6 | 2-4, 7 | `incomplete-cache` |
| `overwritten-cache` | Nothing, but a build of another `app.txt`, which is not recorded, overwrites the cache first | 2-4 | 5-6 | `incomplete-cache` |
| `identical-lines` | Repeats `RUN echo step >> log.txt`, which is already there three times, as line 4 | 2-3 | 4-8 | An instruction was added |
| `changed-next-to-added` | Inserts `RUN echo ready > ready.txt` as line 4, and `RUN wc -c` becomes `RUN wc -l` on line 5 | 2-3 | 4-7 | An instruction was added |
| `two-platforms` | The content of `app-arm64.txt`, which `$TARGETARCH` selects on line 6. Both builds target `linux/amd64` and `linux/arm64` | `amd64`: 3-7, `arm64`: 3-5 | `amd64`: none, `arm64`: 6-7 | Copied content changed, on `arm64` only |
| `subdirectory-context` | The content of `services/api/app.txt`, copied on line 5, with `services/api` as the build context of a Git work tree | 2-4 | 5-6 | Copied content changed |
| `wildcard-copy` | The content of `app-two.txt`, which `COPY app-*.txt .` matches on line 5 | 2-4 | 5-6 | Copied content changed |
| `two-cache-imports` | The content of `app.txt`, and adds `COPY extra.txt .` as line 7. It also imports the cache of an unrecorded build with that `app.txt` | 2-6 | 7 | An instruction was added |
| `copy-link` | `RUN wc -c` becomes `RUN wc -l` on line 4, between `COPY --link` lines | 2-3, and the `COPY` half of 5 | 4, 6, and the `LINK` half of 5 | The instruction changed |
| `remote-add` | The content behind the URL of `ADD` on line 3. The Dockerfile and build context do not change | 2 | 3-6 | A remote source changed |
| `named-context` | The content of `shared.txt`, copied on line 5 from a named build context in a Git work tree | 2-4 | 5-6 | Copied content changed |
| `no-cache-filter` | Adds `--no-cache-filter builder` to a two-stage build | 2, 6-9 | 3-4 | `no-cache`, for the `builder` stage |

`FROM` lines are left out of the table because they never report a hit, even when every later step does. `ARG`, `ENV` and `USER` lines have no step. `changed-base-image` changes the text of the `FROM` line, so a tag that moves without a text change is not covered yet.

## Recording

```sh
lab/record.sh
```

This needs Docker with its socket at `/var/run/docker.sock`, `jq` and `git`. `two-platforms` also needs emulation for the other platform, which Docker Desktop includes; on Linux, install binfmt handlers. It creates a temporary `ichnos-lab` builder and removes it afterward, so it leaves other builders and their records alone. For `remote-add`, it also runs a temporary web server in the builder's network, which leaves the pinned BusyBox image on the machine. It replaces the records in `testdata/` only after every build has passed, and a failed run keeps none of them.

Each build pulls its base image again, because the builder starts empty. A full run makes about 60 pulls, and Docker Hub allows 100 an hour for each address without a login, so a second run within the hour can stop partway.

The records are public, and a record keeps the build folder and the Docker socket path. So the script builds in `/tmp/ichnos-lab` through the default socket, and it stops if a record contains the local user name, host name or macOS temporary path. That search cannot read the Dockerfile text, which the provenance stores as base64. Check new records by hand as well: fixtures must never use real credentials or private content, so they use fake values such as `ichnos-fake-mode`, and the Git fixtures have a fake remote.

The `gha` cache and the default Git context only exist on GitHub Actions, so `lab/gha` measures them there with a smaller build. Start it by hand with `gh workflow run lab-gha.yml -f mode=export`, land a commit, then run it with `-f mode=import` from that commit, or `-f mode=import-empty` to import a scope that has no cache. Artifacts in a public repository can be downloaded, so the workflow removes the trace, stops if anything matches its secret pattern or the build has a build argument, whose value could be a secret, and keeps the record for one day. Each descriptor in a reduced record's index, manifest and history record names a blob that the record holds, with that blob's size, so the collector reads it as it reads the full one. It is for inspection, and does not belong in `testdata/`.

## What the Records Show

These facts come from records made with BuildKit v0.34.0 and buildx v0.28.0-desktop.1 for `linux/amd64`, and also `linux/arm64` in `two-platforms`. Update the versions here when you re-record, and keep the pins in `lab/record.sh`, `lab/gha/Dockerfile` and `.github/workflows/lab-gha.yml` together.

### Format

BuildKit documents the [provenance fields](https://github.com/moby/buildkit/blob/v0.34.0/docs/attestations/slsa-definitions.md), except `digestMapping`. The rest of this format is undocumented, so a later version may change it.

- A `.dockerbuild` file is a gzipped tar of an OCI layout with one manifest. Its config is the history record (`application/vnd.buildkit.historyrecord.v0`), and its layers are the trace, the status stream, the provenance and the result's manifest. In every record here the tar holds the blobs in digest order, then `index.json` and `oci-layout`, so a reader learns which blob is which only at the end.
- The status stream (`application/vnd.buildkit.status.v0`) is a series of frames: a 4-byte little-endian length, then a `StatusResponse` protobuf message. Most records here have one frame, but `min-mode-needed/base` has two, the first only 125 bytes, and it had one in an earlier recording, so a reader has to read every frame. The stream also holds every line of the build's output: BuildKit keeps up to [2 MiB of each step's stdout and of its stderr](https://github.com/moby/buildkit/blob/v0.34.0/util/progress/logs/logs.go#L22-L23) by default, set by `BUILDKIT_STEP_LOG_MAX_SIZE`, so it can be far larger than the rest of the record. Vertices have a digest, inputs, a name, a cached flag and times, and they are not in Dockerfile order. Step names start with a bracketed position such as `[2/6]`, `[ 1/12]`, `[builder 2/5]` or `[linux/amd64 2/6]`, and its total changes when a `RUN`, `COPY`, `ADD` or `WORKDIR` line is added. A build with a named context gets `[stage-0 2/6]`, even with one stage.
- A vertex repeats within a frame, and later updates leave out `cached`, so keeping only the last update reports hits as misses. A step hit the cache only if an update says so and no update has an error: a lost blob reports `cached`, then an error, then reruns. `numCachedSteps` still counts those steps.
- The provenance (`application/vnd.in-toto+json`) is a bare SLSA predicate with no statement around it. It holds each step's definition, a map from vertex digest to step, each step's Dockerfile line, the resolved base image and the Dockerfile text. History records [always use `mode=max`](https://github.com/moby/buildkit/blob/v0.34.0/solver/llbsolver/history.go#L118-L121), whatever `FrontendAttrs` says.
- Lines and steps do not map one to one. When two stages repeat a line on the same parent, as lines 2 and 7 do in `multi-stage`, they share one step with two locations, and its name gives only one stage. [`COPY --link`](https://github.com/moby/buildkit/blob/v0.34.0/frontend/dockerfile/dockerfile2llb/convert_copy.go#L337-L352) gives one line two steps, a copy and a `LINK` merge, and in `copy-link` they get different results on line 5. A remote `ADD` has a download step with no location, and its copy step has the same name.
- A build for two platforms has one status stream for both platforms and one provenance for each. Each provenance names its platform in `internalParameters.targetPlatform`, and the top-level `Results` in the history record, not `Result.Results`, maps each platform to its provenance digest. Single-platform records have no top-level `Results`. The provenance layers' order changes between builds, so it means nothing.
- `numCachedSteps` is missing, not zero, when no step hit.
- `FrontendAttrs` has `cache-imports` only when the build imports a cache, with one entry per import, and its attributes are empty for a local cache. Each import has its own `importing cache manifest` vertex, named by a number. Nothing shows which import a hit came from.
- `no-cache` in `FrontendAttrs` holds the value of `--no-cache-filter`, here `builder`. The provenance has no stage field, so a step's stage has to come from the Dockerfile text and the step's line.
- `resolvedDependencies` in the provenance holds a remote `ADD` source with the SHA-256 of its content. In `remote-add`, that digest is the only difference between the two builds' step definitions and materials.
- A local build context is `local://context`, and a named one is `local://<name>`, here `local://assets`, with `context:<name>` set to `local:<name>` in `FrontendAttrs`.
- In a Git work tree, buildx [adds `vcs:revision`, `vcs:source`](https://github.com/moby/buildkit/blob/v0.34.0/docs/attestations/slsa-definitions.md#vcs) and [`vcs:localdir:<name>`](https://github.com/docker/buildx/blob/v0.28.0/build/git.go#L137-L157), the directory of each local context relative to the repository root, to `FrontendAttrs` and the provenance. BuildKit does not verify them, and its provenance documentation leaves out `vcs:localdir`. buildx leaves it out for a relative path through a symlink, such as `/tmp` on macOS. `localState.LocalPath` holds the absolute path of the main context only, and the absolute path of a named context is nowhere in the record.
- `http.request.header.authorization` values in the trace are masked as `****`, but `url.full` values keep their query strings, including CDN signatures. Here they are for public Alpine layers and expire within the hour, but on a private registry they could be live. The collector keeps nothing from the trace, so they cannot reach a snapshot. The trace is the largest entry in every record here, 3.2 to 13.6 times the size of the provenance.

### Step digests

An earlier version of proposal 002 said that a changed digest means a changed input, which does not hold. In `control`, nothing changes, but lines 3-6 get a new digest and still hit the cache. In `no-cache-import`, the same steps get a new digest and miss. Each build gives its [build context a new session ID](https://github.com/moby/buildkit/blob/v0.34.0/frontend/dockerui/config.go#L463), which changes the digest of the context and of every step that descends from it. Only the numbered steps before the first `COPY` keep their digests, and the `[internal]` steps that load the build context, Dockerfile and `.dockerignore` change in every build.

Step definitions in the provenance do not have this problem, because [BuildKit removes the session ID from them](https://github.com/moby/buildkit/blob/v0.34.0/solver/llbsolver/provenance/buildconfig.go#L94-L96). The `op` and `inputs` of each entry were identical in both `control` builds. Whole entries are not, because `resourceUsage` appears only on steps that ran.

[Proposal 002](../docs/proposals/002-ichnos-architecture.md#explanation-algorithm) compares these definitions. A prototype of that comparison, which is not in this repository, was given the changed files of the first eight scenarios, and it found no miss in `control` and the right cause in the other seven. Without those files, it could only list the possible causes for `changed-copy-file`, `added-copy` and `changed-dockerignore`. Each rule is needed: without alignment by Dockerfile text and the `local.followpaths` exception, it gets the causes of `added-step` and `added-copy` wrong, and without the `local.excludepatterns` handling it gets `changed-dockerignore` wrong. These scenarios shaped the comparison, so passing them is not independent evidence.

The other 20 scenarios' expected causes in the table come from how each was built. The prototype gets these wrong:

- `two-platforms`: it merges the two provenances, so its answer depends on their order. In one recording it reported no miss, and in another a changed base image on line 1.
- `subdirectory-context` and `named-context`: it does not use the context's directory, so the changed file does not match the step, and it only knows `local://context`.
- `wildcard-copy`: it compares paths literally, so `app-two.txt` does not match `app-*.txt`.
- `remote-add`: it does not compare materials.
- `changed-next-to-added`: it finds the first miss on the right line, but calls an added line a changed one. This is not a gap in the prototype. Proposal 002 pairs the lines of a gap in order, so `RUN echo ready` takes the place of `RUN wc -c`.

It is still a candidate, not a result:

- Nothing in this repository runs it yet.
- It has seen one BuildKit version. Proposal 002 has a design for `two-platforms`, `subdirectory-context`, `wildcard-copy` and `remote-add`, but nothing implements it. It covers `named-context` only partly, because it gives a build one context directory.
- `no-cache-filter` passed only because the prototype treats the setting as on or off. Matching a filter to a stage is untested.
- It treats a step with two locations as one line of joined text, which `multi-stage` passes only because both builds have the same text between them.
- `two-cache-imports` uses two working local caches, and an empty local import leaves no trace, so an import that fails alongside a working one is untested. It needs a registry.
- Registry caches and `.dockerignore` with a Git context have no record, because they need a registry and a Git server.
- `remote-add` changes the content. [BuildKit's cache key also includes `Last-Modified`](https://github.com/moby/buildkit/blob/v0.34.0/source/http/source.go#L243-L272), so a file uploaded again unchanged may miss with the same digest. That is unmeasured.
- The `gha` results come from single runs of one small build.
- One record cannot separate changed content from an incomplete cache or from a `.dockerignore` change, which is why the pull request diff is still needed. `overwritten-cache` misses a `COPY` with nothing changed, and its records look like those of `changed-copy-file`.

### Cache behavior

- A `COPY` depends on the content of the files it copies, not on the steps that made them. In `multi-stage`, line 9 hit even though the stage 1 step that made its file reran, because the file had the same bytes. Line 10 copied a changed file and missed. So a step can hit after an earlier line missed.
- `--no-cache-filter` covers less than its stage. It [sets `IgnoreCache` on `RUN`](https://github.com/moby/buildkit/blob/v0.34.0/frontend/dockerfile/dockerfile2llb/convert.go#L1414-L1416) and [`COPY`](https://github.com/moby/buildkit/blob/v0.34.0/frontend/dockerfile/dockerfile2llb/convert_copy.go#L332-L334), so `WORKDIR` in that stage still hit. The next stage hit too, including the `COPY --from=builder` on line 8, because the rerun gave the same file.
- [An `ARG` value enters the environment](https://github.com/moby/buildkit/blob/v0.34.0/frontend/dockerfile/dockerfile2llb/convert.go#L1710-L1712) of every later `RUN` in its stage, whether or not the line reads it, so `rotating-build-arg` misses on a line with no `BUILD_ID` in its text. `WORKDIR` and `COPY` are not affected, so they hit until the first `RUN`. `COPY` is not affected by `ENV` or `USER`, and `WORKDIR` is not affected by `ENV`, but [its mkdir takes the `USER`](https://github.com/moby/buildkit/blob/v0.34.0/frontend/dockerfile/dockerfile2llb/convert.go#L1546-L1548).
- `mode=min` left out the first stage, so `min-mode-needed` reran it while line 6 of the final stage hit.

### Other experiments

These ran once on the same BuildKit version. Some are now also covered by recorded scenarios.

- **Planted values:** a `--build-arg` value appeared in step names, the status stream, the trace, `FrontendAttrs` and the provenance. An `ENV` literal appeared in the provenance and the image config. A `--secret` value appeared nowhere.
- **Empty cache:** with `type=local`, the build ran, and the record shows no import at all. With `type=registry`, the build ran, and the `importing cache manifest` vertex has an error.
- **Missing blob:** deleting one layer from a local or a registry cache failed the build, so neither case reached a cache miss.
- **`mode=min`:** an unchanged build hit every step.
- **Git context:** with `https://` contexts at two commits, the source identifier holds the commit and the source has `git.*` attributes. An unchanged file still hit the cache, and a changed file missed.
- **`gha` cache:** on GitHub Actions, with buildx v0.38.1 and the default Git context, a real pull request and a later commit on another branch both read the default branch's cache and hit every step after `FROM`. A changed `app.txt` missed its `COPY` and `RUN`. An empty scope let the build run with no hits and no error on the import. A deleted blob did not fail the build, unlike local and registry caches: when the build needed that blob, the step reported a hit, then `blob not found`, reran and succeeded, and the record keeps the error. When it did not need that blob, nothing changed.
- **GitHub event data:** buildx copies the whole event into the provenance and the build metadata, and `docker/build-push-action` prints the metadata in its log. A `push` event includes the pusher's email address, so `BUILDX_METADATA_PROVENANCE=disabled` keeps it out of the log. `reduce-record.sh` removes the event from the record, and stops if the event or any plain, percent-encoded or JSON-escaped email address is left.
- **Real tokens:** two complete records from GitHub Actions, trace included, held neither the job token nor the runtime token, in plain, base64 or URL-encoded form, with a `gha` cache, a Git context and a registry login in use. All 32 `authorization` headers were masked, and the only signed URLs were CDN links to public image layers.
- **Layer digests:** they match on a hit but differ on every rerun, even with identical content, so they cannot show that content is unchanged.
- **Exported cache:** identical content with fresh timestamps kept all 8 cache records, and changed content changed 1. Reading it needs access to the cache backend.
