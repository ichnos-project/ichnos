# What a Snapshot Holds

> [!IMPORTANT]
> Ichnos is not released yet, and no command writes `snapshot.json` yet. This page describes schema version 1, which may change before the first release.

A snapshot is Ichnos's record of one build: the steps it ran, whether each one used the cache, and fingerprints of what each one depends on. Ichnos takes it from BuildKit's build record, removes credentials first, and compares two snapshots to explain a cache miss. Baselines are stored as snapshots, so a snapshot can end up in a cache or an artifact that other people can read.

The Go types are in [`pkg/snapshot`](../pkg/snapshot). The design is in [proposal 002](proposals/002-ichnos-architecture.md#snapshot).

## Contents

| Field | Holds |
|---|---|
| `schemaVersion`, `fingerprintVersion` | The versions of the format and of the fingerprint rules. Fingerprints compare only when their versions match. |
| `provenance` | `read`, or why the record had no provenance Ichnos reads: `missing`, `unsupported` or `no-steps`. Without it, there are no platforms, only vertices. |
| `platforms` | One build for each target platform, with its steps, its distinct environments and its local build contexts. |
| `vertices` | What BuildKit reported for each vertex: its kind, whether it used the cache, whether it lost a cache hit to an error, whether it had an error, and when it started and completed. Steps point to their vertex. |
| `cache` | The type of each cache import the build asked for, and `noCache` when the build was asked not to use the cache: with the `--no-cache-filter` stages, or with no stages for `--no-cache`. |
| `buildArgs` | Each build argument's name, with its value as a fingerprint. |
| `commit`, `dirty` | The Git commit that buildx recorded for the build context's work tree, if any, and whether buildx recorded it as dirty. buildx checks only when `BUILDX_GIT_CHECK_DIRTY` is set and never records a clean tree, so `dirty` missing does not mean clean. |
| `sourceDateEpoch` | The `SOURCE_DATE_EPOCH` build argument, when it is a number. |

Each step holds its operation type, what it loads when it is a source step, the Dockerfile instruction it comes from and its line numbers, its inputs, its environment, its fingerprints, whether it was withheld, whether it descends from a local build context, a Git repository or an HTTP download, and the paths that a `COPY` or `ADD` copies from a local build context, and the sources mounted into `RUN` steps from one, by a bind mount or a cache mount with `from=`. A secret, SSH or tmpfs mount reads no source, and a mount of a type that BuildKit v0.34.0 does not name withholds its step. BuildKit's cache key covers the content of those paths. Each local build context holds its name, the steps that load it, its directory in the Git work tree and its `.dockerignore` patterns. Steps that load equal contexts share one, so a long pattern list is stored once.

Step names are not kept, because BuildKit expands build argument values into them.

The cached flag, the lost hit, the times and the kind are on `vertices[]`, and a step points to its vertex with `vertex`: a record without provenance has vertices but no steps, and so still has timing and hit rates, cache imports and exports are vertices with no step, and a vertex that two platforms share is stored once.

## Field Reference

All names are as in the JSON. A field left out means `false`, zero or empty, with two exceptions:

- `sourceDateEpoch` left out means that `SOURCE_DATE_EPOCH` was not set to a number, and 0 is a value.
- `noCache` left out means that the build was not asked to skip the cache. `noCache` with an empty `stages` list means every stage, as `--no-cache` asks.

A step's `vertex` and `environment` are always written, because 0 is an index and -1 means none.

| Field | Type |
|---|---|
| `schemaVersion`, `fingerprintVersion` | Integer, 1 |
| `provenance` | `read`, `missing`, `unsupported` or `no-steps` |
| `platforms[]` | `platform` (string), `withheld`, `steps[]`, `environments[][]`, `contexts[]` |
| `platforms[].steps[]` | `op`, `sourceKind`, `text`, `lines` (`start`, `end`), `vertex`, `inputs[]`, `environment`, `fingerprints` (`command`, `other`, `ancestor`), `withheld`, `withheldAncestor`, `fromLocalContext`, `fromGit`, `fromHTTP`, `sources[]` (`context`, `path`) |
| `platforms[].environments[][]` | `name`, `value` (fingerprint), `withheld`, sorted by name |
| `platforms[].contexts[]` | `name`, `steps[]`, `dir`, `ignorePatterns[]`, `withheld` |
| `vertices[]` | `kind`, `cached`, `lostHit`, `errored`, `started`, `completed` |
| `cache` | `imports[]` (`type`), `noCache` (`stages[]`, `withheld`) |
| `buildArgs[]` | `name`, `value` (fingerprint), `withheld`, sorted by name |
| `commit`, `dirty`, `sourceDateEpoch` | String, boolean, integer |

Values:

- `op`: `exec`, `file`, `source`, `build`, `merge`, `diff`, `none` (a step with no operation, such as the final one) or `unknown`.
- `sourceKind`, on source steps only: `local` (a build context), `git`, `http`, `image` (from a registry or an OCI layout) or `other`. BuildKit never reports a cache hit for an image source, because it resolves the image again on every build.
- Vertex `kind`: `step`, `internal`, `cache-import` or `cache-export`.
- Cache `type`: `local`, `registry`, `gha`, `s3`, `azblob` or `unknown`.
- A fingerprint is 64 lower-case hexadecimal digits.
- Times are RFC 3339, in UTC.
- Indexes count from 0: a step's `vertex` is an index in `vertices`, or -1 when BuildKit reported none; its `inputs` and a context's `steps` are indexes in the platform's `steps`, and every input comes before the step that reads it; its `environment` is an index in the platform's `environments`, or -1 for a step with no environment; a source's `context` is an index in the platform's `contexts`.
- Lines count from 1.

## Plain Text and Fingerprints

These are kept as text, after redaction: Dockerfile instructions, platform names, the source paths of `COPY`, `ADD` and the sources mounted into `RUN` steps, `.dockerignore` patterns, build context names and directories, build argument and environment variable names, stage names, the commit and `sourceDateEpoch`. Dockerfile instructions are kept as the repository commits them, so a literal value on an `ENV` line stays unless its name looks like a credential.

All of that text comes from a build record, which the author of a pull request controls. Anything that shows it, such as a report or a pull request comment, must escape every text field for where it goes. A source path is relative to its build context, and a directory to the root of the Git work tree. Treat both as untrusted: check that each stays local, as Go's `filepath.IsLocal` does, and open it only within its root, as through an `os.Root`. A source path or directory that is not a clean relative path, or that holds a backslash, a colon, a character that is not graphic or a name that Windows reserves, such as `NUL` or `com1.txt`, is left out, and a step that loses a source path this way is withheld. buildx records the directory with the path separator of the system it runs on, so a record made on Windows has no `dir`.

Everything else that a step depends on is kept only as a fingerprint: a SHA-256 hash over a fixed prefix and the value. That covers each command, each environment variable's value, each build argument's value and every other field of a step's definition, including the order of its environment variables, which BuildKit's cache key holds. A fingerprint shows that a value changed without showing the value. It does not protect a value that can be guessed, such as a short password or a word: anyone can hash guesses with the same prefix and compare.

## What Redaction Removes

Before anything is kept or hashed, Ichnos removes:

- the value of every build argument, environment variable and option whose name looks like a credential, such as `API_TOKEN`, `NPM_AUTH`, `password` or `--token`;
- every other place where the value of such a build argument or environment variable appears, unless it is shorter than 4 characters or a word such as `true`;
- the user information and query values of URLs, and of proxy settings;
- the values in `Authorization: ...` headers, `password: ...` lines, `-u user:password` options and similar labels;
- every run of 8 bytes, in any string, that is part of the value of such a build argument or environment variable when that value has 8 bytes or more. The Dockerfile frontend expands `${VAR#pattern}`, `${VAR%pattern}` and `${VAR/pattern/text}` (`frontend/dockerfile/shell/lex.go` at BuildKit v0.34.0), so `COPY ${NPM_TOKEN%?} .` would otherwise keep all of the token but its last byte, as text or in a fingerprint.

A step that lost a value this way is marked `withheld`, and Ichnos does not claim to know whether that value changed. Every step that descends from it is marked `withheldAncestor`, because its ancestor fingerprint cannot show that change either.

Removing every 8 bytes of a credential also removes what a credential shares with other text, such as the host of a URL-shaped credential, and withholds those steps. That is the accepted cost of leaving no part of a credential behind. Measured on the 56 lab records, which have 500 steps: a credential such as `https://ci-user:token@registry.example.com/`, or a GitHub URL with a token, withholds no step, because no step there holds a `https://` URL; one that shares `localhost:8080/` with the `ADD` URL of the `remote-add` scenario withholds 4 steps in its 2 records, and 8 of the 382 steps from Dockerfile lines are withheld or descend from one.

Redaction replaces what it removes from kept text with `***`. That can appear in a step's `text` and source `path`s, a platform name, a context's `name` and `ignorePatterns`, the `noCache` stages, and the names of build arguments and environment variables. Each comes with `withheld` set where it is kept: on the step, the platform, the context, `noCache`, the build argument or the environment variable. A withheld build argument or environment variable has no `value`, and a withheld context directory is left out. A list of `.dockerignore` patterns that holds a credential, runs across one or shares 8 bytes with one is withheld whole, so the context keeps no patterns. A context whose name redaction changed has no `dir`, and neither has one whose redacted name another context has too. Text can hold `***` of its own, as a `COPY` wildcard does, so only `withheld` says whether redaction changed it.

Redaction cannot recognize every credential. A token used alone as the user name of a URL is kept when what comes before its first `/`, `?` or `#` reads as a host name with a dot, as in `https://ab.cd/ef@host/`, because that is also how `git+https://github.com/org/repo.git@v1` is written. A part of a credential shorter than 8 bytes is kept, as text and in fingerprints. To keep credentials out of build records, and so out of snapshots:

- pass them as [BuildKit secrets](https://docs.docker.com/build/building/secrets/) (`RUN --mount=type=secret`), which BuildKit keeps out of the build record;
- or pass them as build arguments or environment variables whose name looks like a credential, such as `REGISTRY_TOKEN`;
- and percent-encode them when they must appear in a URL.

## Not Yet in a Snapshot

Proposal 002 lists more than schema version 1 holds. Not yet kept:

- materials: the digests of base images, remote `ADD` sources and Git sources;
- the class of error of a cache import that failed (only whether it failed);
- builder versions. The record does not hold BuildKit's: min-mode provenance leaves the builder ID empty, and the history record names no version. Only the trace has it, as `buildkit/v0.34` without the patch number, in the user agent of the HTTP requests BuildKit made. The provenance does name the Dockerfile frontend's version (`dockerfileVersion`);
- the CI event and its cache mode;
- content fingerprints of the files a `COPY` or `ADD` reads from the build context;
- a JSON Schema for `snapshot.json`.

## Versions

- `schemaVersion` changes with any change to the JSON shape: a field added, removed, renamed or retyped, or a new enum value. The [coding conventions](coding-conventions.md) decode a stored versioned file strictly, with unknown fields as errors, so even an added field is not compatible. A reader reads the version first, and accepts only the current version and the one before it, because baselines outlive the release that wrote them.
- `fingerprintVersion` changes whenever the same build would give different fingerprints or different kept text: a change to how fingerprints are computed or what they leave out, or to the redaction rules. Two snapshots are compared only when their fingerprint versions match. A new version makes stored baselines incomparable until the default branch is built again, so the release notes list it, and the report says so instead of reporting every step as changed.

Nothing reads a stored snapshot yet, and decoding an untrusted `snapshot.json` into the `pkg/snapshot` types with Go's `encoding/json` is not safe. It accepts `{}` and any `schemaVersion`, it accepts duplicate names and names in another case, it leaves a missing enum value as `""` and a missing `vertex` or `environment` as 0, and it accepts lists of any length. Enum values and fingerprints are checked only when present, and index ranges are not checked at all.
