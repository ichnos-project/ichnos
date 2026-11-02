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

The `gha` cache and the default Git context only exist on GitHub Actions, so `lab/gha` measures them there with a smaller build. Start it by hand with `gh workflow run lab-gha.yml -f mode=export`, land a commit, then run it with `-f mode=import` from that commit, or `-f mode=import-empty` to import a scope that has no cache. Artifacts in a public repository can be downloaded, so the workflow removes the trace, stops if anything matches its secret pattern, and keeps the record for one day. A reduced record still names the deleted trace, so it is for inspection and does not belong in `testdata/`.

## What the Records Show

These facts come from records made with BuildKit v0.34.0 and buildx v0.28.0-desktop.1 for `linux/amd64`. Update the versions here when you re-record, and keep the pins in `lab/record.sh`, `lab/gha/Dockerfile` and `.github/workflows/lab-gha.yml` together.

### Format

BuildKit documents the [provenance fields](https://github.com/moby/buildkit/blob/v0.34.0/docs/attestations/slsa-definitions.md), except `digestMapping`. The rest of this format is undocumented, so a later version may change it.

- A `.dockerbuild` file is a gzipped tar of an OCI layout with one manifest. Its config is the history record (`application/vnd.buildkit.historyrecord.v0`), and its layers are the trace, the status stream, the provenance and the result's manifest.
- The status stream (`application/vnd.buildkit.status.v0`) is a series of frames: a 4-byte little-endian length, then a `StatusResponse` protobuf message. Each record here has one frame. Vertices have a digest, inputs, a name, a cached flag and times, and they are not in Dockerfile order. Step names start with a bracketed position such as `[2/6]`, `[ 1/12]` or `[builder 2/5]`, and its total changes when a `RUN`, `COPY`, `ADD` or `WORKDIR` line is added.
- A vertex repeats within a frame, and later updates leave out `cached`, so keeping only the last update reports hits as misses. A step hit the cache only if an update says so and no update has an error: a lost blob reports `cached`, then an error, then reruns. `numCachedSteps` still counts those steps.
- The provenance (`application/vnd.in-toto+json`) is a bare SLSA predicate with no statement around it. It holds each step's definition, a map from vertex digest to step, each step's Dockerfile line, the resolved base image and the Dockerfile text. History records [always use `mode=max`](https://github.com/moby/buildkit/blob/v0.34.0/solver/llbsolver/history.go#L118-L121), whatever `FrontendAttrs` says.
- `numCachedSteps` is missing, not zero, when no step hit.
- `FrontendAttrs` has `cache-imports` only when the build imports a cache, and its attributes are empty for a local cache.
- `http.request.header.authorization` values in the trace are masked as `****`, but `url.full` values keep their query strings, including CDN signatures. Here they are for public Alpine layers and expire within the hour, but on a private registry they could be live, so the collector has to redact the trace.

### Step digests

An earlier version of proposal 002 said that a changed digest means a changed input, which does not hold. In `control`, nothing changes, but lines 3-6 get a new digest and still hit the cache. In `no-cache-import`, the same steps get a new digest and miss. Each build gives its [build context a new session ID](https://github.com/moby/buildkit/blob/v0.34.0/frontend/dockerui/config.go#L463), which changes the digest of the context and of every step that descends from it. Only the numbered steps before the first `COPY` keep their digests, and the `[internal]` steps that load the build context, Dockerfile and `.dockerignore` change in every build.

Step definitions in the provenance do not have this problem, because [BuildKit removes the session ID from them](https://github.com/moby/buildkit/blob/v0.34.0/solver/llbsolver/provenance/buildconfig.go#L94-L96). The `op` and `inputs` of each entry were identical in both `control` builds. Whole entries are not, because `resourceUsage` appears only on steps that ran.

[Proposal 002](../docs/proposals/002-ichnos-architecture.md#explanation-algorithm) compares these definitions. A prototype of that comparison, which is not in this repository, was given each scenario's changed files, and it found no miss in `control` and the right cause in the other seven. Without those files, it could only list the possible causes for `changed-copy-file`, `added-copy` and `changed-dockerignore`. Each rule is needed: without alignment by Dockerfile text and the `local.followpaths` exception, it gets the causes of `added-step` and `added-copy` wrong, and without the `local.excludepatterns` handling it gets `changed-dockerignore` wrong. These scenarios shaped the comparison, so passing them is not independent evidence.

It is still a candidate, not a result:

- Nothing in this repository runs it yet.
- It has seen one BuildKit version and one platform, and every recorded scenario is a single-stage build that uses a local cache or none. Multi-platform builds, subdirectory contexts, wildcards, several imports and rotating build arguments are untested, and the prototype does not handle most of them yet.
- Identical lines, a changed line next to an added one, and `.dockerignore` with a Git context are untested. "An instruction was added" rests on `added-step` alone.
- The `gha` results come from single runs of one small build.
- One record cannot separate changed content from an incomplete cache or from a `.dockerignore` change, which is why the pull request diff is still needed.

### Other experiments

These ran once on the same BuildKit version, and no record here covers them yet.

- **Planted values:** a `--build-arg` value appeared in step names, the status stream, the trace, `FrontendAttrs` and the provenance. An `ENV` literal appeared in the provenance and the image config. A `--secret` value appeared nowhere.
- **Empty cache:** with `type=local`, the build ran, and the record shows no import at all. With `type=registry`, the build ran, and the `importing cache manifest` vertex has an error.
- **Missing blob:** deleting one layer from a local or a registry cache failed the build, so neither case reached a cache miss.
- **`mode=min`:** an unchanged build hit every step. The first stage reran only when a later line needed it.
- **Overwritten cache:** when another branch overwrote the cache, a `COPY` missed with no change in the pull request. The records alone cannot separate this from changed content, so it needs the pull request diff or source fingerprints.
- **Git context:** with `https://` contexts at two commits, the source identifier holds the commit and the source has `git.*` attributes. An unchanged file still hit the cache, and a changed file missed.
- **`gha` cache:** on GitHub Actions, with buildx v0.38.1 and the default Git context, a real pull request and a later commit on another branch both read the default branch's cache and hit every step after `FROM`. A changed `app.txt` missed its `COPY` and `RUN`. An empty scope let the build run with no hits and no error on the import. A deleted blob did not fail the build, unlike local and registry caches: when the build needed that blob, the step reported a hit, then `blob not found`, reran and succeeded, and the record keeps the error. When it did not need that blob, nothing changed.
- **GitHub event data:** buildx copies the whole event into the provenance and the build metadata, and `docker/build-push-action` prints the metadata in its log. A `push` event includes the pusher's email address, so `BUILDX_METADATA_PROVENANCE=disabled` keeps it out of the log. `reduce-record.sh` removes the event from the record, and stops on any email address that is left.
- **Real tokens:** two complete records from GitHub Actions, trace included, held neither the job token nor the runtime token, in plain, base64 or URL-encoded form, with a `gha` cache, a Git context and a registry login in use. All 32 `authorization` headers were masked, and the only signed URLs were CDN links to public image layers.
- **`no-cache`:** the record keeps it in `FrontendAttrs`.
- **Layer digests:** they match on a hit but differ on every rerun, even with identical content, so they cannot show that content is unchanged.
- **Exported cache:** identical content with fresh timestamps kept all 8 cache records, and changed content changed 1. Reading it needs access to the cache backend.

The comparison also gave the expected result on multi-stage builds, removed and reordered lines, an added `ENV` or `USER` line, a changed build argument and a build of 12 steps.
