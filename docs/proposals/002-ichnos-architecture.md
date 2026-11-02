# Ichnos Architecture

**Authors**: Rayan Alyasi (@RayanAlyasi)

**Begin Design Discussion**: 2026-10-28

**Status**: provisional

**Checklist**:

- [ ] Snapshot, report and configuration schemas (`schemaVersion` 1, `v1alpha1`)
- [ ] Collectors: `.dockerbuild` bundle, and `rawjson` progress stream for timing and hit rates
- [ ] Step matching, comparison and first-miss explanation
- [ ] Audit rule registry and rules
- [ ] Renderers: Markdown, JSON, SARIF
- [ ] GitHub Action with fork-safe publishing
- [ ] Docs
- [ ] Tests, including the scenario lab

## Summary/Abstract

Ichnos explains why a container build missed its cache. It compares a pull request's BuildKit build record with the base branch's, finds the first cache miss that the pull request's own changes do not explain, names the input that changed, audits the cache configuration, and reports the result as a pull request comment, JSON, SARIF or OpenTelemetry data.

This proposal describes the architecture for v0.1 to v1.0: the data flow and its trust boundaries, the Go package layout and core interfaces, the three versioned contracts (snapshot, report and configuration), the explanation algorithm, the audit rule model, the CLI, the CI integrations, and how the project is tested and released.

## Background

### Motivation and problem space

Docker and BuildKit report *that* a step re-ran, but not *why*. The request to show which input invalidated the cache has been open in moby/moby since 2014 ([moby/moby#9294](https://github.com/moby/moby/issues/9294)). Since 2023, the most frequent and most-reacted Docker cache problems in `docker/build-push-action`, buildx and BuildKit have been cache backend and permission failures, slow cache import and export, and caches that are silently not used ([build-push-action issues](https://github.com/docker/build-push-action/issues)).

Mature build systems have converged on the same answer: compare the cache-key inputs of two builds and start from the first miss. Gradle Develocity compares Build Scans, BuildBuddy ships `bb explain`, and Nx Cloud compares similar tasks. Container-build products, including Docker's build summary, the Docker Desktop Builds view and Depot, report hit rates and timings but do not explain a miss or compare a pull request with its base branch.

### Impact and desired outcome

A pull request comment that names the first unexpected miss, its cause, its cost in minutes and a fix lets reviewers catch a build-performance regression at the moment it is introduced, instead of after CI has been slow for weeks.

### Prior discussion and links

- [moby/moby#9294](https://github.com/moby/moby/issues/9294): show what invalidated the cache
- [Docker cache invalidation rules](https://docs.docker.com/build/cache/invalidation/)
- [Gradle build cache debugging](https://docs.gradle.org/current/userguide/build_cache_debugging.html)
- [BuildBuddy `spawn_diff.proto`](https://github.com/buildbuddy-io/buildbuddy/blob/master/proto/spawn_diff.proto)
- [GitHub Security Lab: preventing pwn requests](https://securitylab.github.com/resources/github-actions-preventing-pwn-requests/)

## User/User Story

- As a **developer**, when my pull request slows the build, I want to know which change caused it and how to fix it, without reading BuildKit logs.
- As a **reviewer**, I want build-performance regressions visible on the pull request, next to the change that caused them.
- As a **platform engineer**, I want to know whether our CI caches are set up correctly across repositories, and how many minutes cache misses cost.

## Goals

1. Name the first unexpected cache miss and the input that changed, with its evidence and a confidence level.
2. Say whether the pull request build received the base branch's cache at all, and if not, why.
3. Audit the cache configuration with rules that fire only on evidence.
4. Report the minutes lost to misses, cache import and cache export.
5. Run safely on pull requests from forks.
6. Work with any CI system, not only GitHub. Explanations need a Dockerfile build, because they rely on its provenance; other BuildKit clients get timing and hit rates.

## Non-Goals

- Image size, package or vulnerability comparison ([Docker Scout](https://docs.docker.com/scout/) covers this).
- Layer efficiency analysis ([dive](https://github.com/wagoodman/dive) covers this).
- Dockerfile style linting.
- AI-generated explanations.
- A hosted dashboard or a cache service.
- Opening fix pull requests automatically. This may be revisited after v1.0.

## Proposal

Ichnos is a single Go binary with a small public library. It runs in three trust zones:

```text
UNTRUSTED  pull request job (fork PRs: read-only token, no secrets)
  BuildKit build ──► ichnos collect ──► snapshot.json   (redacted, build argument values hashed)
  checkout + git diff ──────────────┐
  baseline restore (read only) ─────┤
                                    ▼
           ichnos check: match ─► compare ─► explain ─► audit ─► report.json (schemaVersion N)
                                                                     │ uploaded as an artifact
TRUSTED    publish job (never runs pull request code)                ▼
           ichnos publish: size limit ─► schema check ─► render ─► escape ─► one sticky comment
TRUSTED    push to the default branch
  BuildKit build ──► ichnos collect ──► ichnos baseline save        (the only baseline writer)
```

- The **untrusted job** does all the analysis, because it already holds the checkout, the build record and the diff.
- The **trusted publish job** only validates, renders and posts. It never checks out or runs pull request code.
- Only **pushes to the default branch** write baselines, so a pull request cannot replace the build it is compared against.

Success is measured by the scenario lab: every explanation Ichnos shows without an experimental label has measured accuracy on reproducible cache failures.

## Design Details

### Package layout

One Go module, with a thin `main`, a small public API and an internal engine, following trivy and ko.

```text
cmd/ichnos/main.go                maps typed errors to exit codes; nothing else
pkg/                              public; documented as unstable until v1.0
  engine/                         facade: Check(ctx, Settings, *config.Config, Inputs) (*report.Report, error)
  snapshot/                       normalized build model and its schema version
  report/                         report model and its schema version
  config/                         v1alpha1 types, Default(), Load(), Validate()
internal/
  cli/                            cobra commands, flag table, exit codes
  collect/                        bundle, rawjson and buildctl collectors; redaction and fingerprints
  baseline/                       file, GitHub cache, GitHub artifact, GitLab, OCI providers
  ci/                             CI context: event, refs, merge base, cache mode
  match/ compare/ explain/
  audit/                          rule model, registry, one file per rule
  render/                         markdown, json, sarif, codequality, annotations
  publish/                        privileged side: validate, render, update the comment
  telemetry/                      OpenTelemetry API only
schemas/                          generated JSON Schemas for config, snapshot and report
testdata/                         recorded build records by BuildKit version and scenario
lab/                              scenario lab: fixture builds that break the cache on purpose
hack/                             tools, with a separate go.mod
```

### Core interfaces

Every call takes a `context.Context` first, returns an error, and never exits the process. Logging and telemetry are injected, never global.

```go
package collect

type Collector interface {
	Name() string
	Collect(ctx context.Context, src Source) (*Record, error)
}

package baseline

// Provider returns ErrNoBaseline when nothing usable exists; callers degrade, never fail.
type Provider interface {
	Name() string
	Fetch(ctx context.Context, q Query) (*snapshot.Snapshot, Origin, error)
}

package render

type Writer interface {
	Write(ctx context.Context, w io.Writer, r *report.Report) error
}

package engine

type Settings struct {
	Logger         *slog.Logger
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}
```

A collector returns a raw `Record`, and one function in `collect` turns it into a `snapshot.Snapshot`, redacting values before it hashes them, so no collector can skip either step. The only other way to get a snapshot is to decode a stored baseline. Buildx copies the GitHub Actions cache token and cloud credentials into cache settings, and the [scenario lab](../../lab/README.md#other-experiments) found build argument values in step names, status streams, traces and provenance, and signed URLs in traces, so build records must be treated as secret-bearing input.

### Snapshot

A snapshot is Ichnos's own normalized view of one build. It shields every consumer from BuildKit's undocumented formats. It contains:

- **Steps,** one list per platform: the Dockerfile text and location from BuildKit's provenance, the operation type, the cached flag, start and end times, the kind (build step, cache import, cache export or internal), its fingerprints, and whether it descends from a local or Git build context. Step names are not stored, because BuildKit expands build arguments into them.
- **Materials:** base images, remote `ADD` sources and Git sources, with their digests. URLs are stored as hashes.
- **Build context:** its directory relative to the repository root, the `.dockerignore` patterns and the `COPY` and `ADD` sources. When the build context is on disk, each source that the patterns admit also has a fingerprint of its content.
- **Cache imports and exports:** each import with its type and whether it failed, as an error class and never the raw error; the `no-cache` setting with any stage filter; exports inferred from step names, because BuildKit does not store export settings.
- **Build arguments:** names, with hashed values.
- **`SOURCE_DATE_EPOCH`**, the built commit, the CI event and its cache mode, the builder versions and the fingerprint version.

A build with no provenance has timing and hit rates but no Dockerfile text or fingerprints, so it cannot be compared. This covers the `rawjson` collector and builds that use no Dockerfile. The report says so, and never treats a comparison it could not make as no misses.

Fingerprints are SHA-256 hashes with a fixed prefix, which stops generic lookup tables, computed at collect time from the step definitions in the provenance. Each step has one for its command, one for each environment variable by name, and one for its other fields, such as the user and mounts, so a report can name what changed without storing a value. They leave out fields that BuildKit leaves out of its own cache key, such as proxy settings. Another covers the step and everything it descends from. It is computed once per step, inputs first, and it follows what `inputs` points to, because step IDs shift when a step is inserted. A record with a cycle or an unknown input is rejected.

The ancestor fingerprint keeps build context details out, so that only real changes count. It leaves out `local.followpaths` and `local.excludepatterns`, because a change to either would otherwise look like a change to every `COPY` step. For a Git context it leaves out the commit and the `git.*` attributes, because the commit changes on every build.

A variable whose name looks like a credential is withheld: its value is left out of every fingerprint, and the snapshot records that the step has a withheld input. A value redacted from a definition counts the same way. Dockerfile text goes through the same redaction. A source fingerprint missing on either side counts as no evidence, not as a change.

Two snapshots compare only when their fingerprint versions match. Otherwise every step would look changed, so the report says the comparison is unavailable. When only the BuildKit versions differ, the comparison runs at lower confidence.

### Explanation algorithm

The explanation follows the shape of BuildBuddy's `bb explain`: each step is base-only, PR-only, moved, modified or unchanged; a modified step lists typed changes, whether the pull request explains it, and how many later misses it caused.

1. **Check whether the cache arrived**, and report it for every build, whatever else the analysis finds: each import, whether it failed, and the `no-cache` setting.
2. **Align steps** between the base and pull request builds, one platform at a time, by Dockerfile text and operation type, in Dockerfile order. A platform in only one build is reported as not compared. Text that appears once in both builds anchors the match, and the longest run of anchors in the same order is kept. An anchor left out of that run moved, and stays paired with its base step. Between anchors, equal lines at each end match, and the remaining lines pair in order with lines of the same instruction keyword as modified steps. A line still without a partner is PR-only or base-only.
3. **Mark expected misses.** A miss is expected when any field of the step's definition or its materials changed, when the step is PR-only, when its ancestor fingerprint changed, when a `COPY` or `ADD` source appears in the diff between the two built commits (after `.dockerignore`) or has a changed content fingerprint, when the changed `.dockerignore` patterns can affect its sources, or when it descends from another expected miss. Paths match using the build context directory and BuildKit's wildcard rules.
4. **Report the first miss that nothing explains**, and fold the misses that cascade from it into one line. An image source never reports a hit, so an unchanged one is not a miss.
5. **Classify the cause**, using the first row that matches:

| Evidence | Cause |
|---|---|
| The `no-cache` setting covers the step | `no-cache` |
| The record shows no cache import, and no step hit | `no-cache-import` |
| At least one cache import is recorded, and every one reported an error | `cache-import-failed` |
| A cache import is recorded, and no step hit | `cache-not-used` |
| The step has a withheld input | `unknown` |
| The step has no build context or remote source ancestor | `incomplete-cache` |
| The diff clears the step's copied sources | `incomplete-cache` |
| Anything else | `unknown` |

CI configuration and the CI API can narrow `incomplete-cache` and `cache-not-used` to `scope-mismatch`, `min-mode` or `readonly-trigger`, and snapshots can show `cold-builder` and `builder-version`. An empty `type=local` import leaves no trace in the record, so only CI configuration can tell it from no import. `nondeterministic-input` waits for the repeat-build mode in Future Milestones.

A diff is unavailable under `ichnos compare`, in a shallow checkout, or when a built commit is unknown. That counts as no evidence, never as an empty diff, and so does a build context directory that is unknown. For a step that copies from the build context, the report then gives `unknown` and lists the possible causes in its evidence, including a `.dockerignore` change when the patterns differ. Below the confidence threshold, Ichnos also reports `unknown` and shows its evidence.

Steps are never aligned or compared by digest, line number or step name. Each build gives its build context a new session ID, which changes the digest of every step that descends from it even when nothing changed. A digest rule in an earlier version of this proposal failed for that reason, and the [scenario lab](../../lab/README.md) has the measurements. Line numbers shift when a line is added, and step names contain expanded build arguments. Causes ship as alpha until the lab measures them on the `gha` cache and on more BuildKit versions.

### Audit rules

Rules are listed explicitly in one file, never registered from `init()`. A registry rejects duplicate or malformed IDs.

```go
package audit

type Meta struct {
	ID        string    // kebab-case and permanent: ^[a-z][a-z0-9-]{2,62}$
	Version   int       // bumped on any logic change
	Title     string
	Severity  Severity  // info, warning, error
	Stability Stability // alpha, beta, stable, deprecated
	Requires  []Fact    // record, ci-config, dockerfile, ci-api
}

type Rule interface {
	Meta() Meta
	Check(ctx context.Context, in *Input) ([]Finding, error)
	sealed() // only New creates rules, so methods can be added later without breaking anyone
}
```

Each finding carries the rule ID, a message, a location when known, its evidence, a copy-paste fix and a stable fingerprint. The documentation URL is derived from the rule ID. The first rules are listed in [audit-rules.md](../audit-rules.md).

### Contracts and versioning

| Contract | Version marker | Breaking change policy |
|---|---|---|
| Configuration | `apiVersion` `ichnos/v1alpha1`, then `v1beta1`, then `v1` | Deprecate in release N with a warning; remove no earlier than N+2 or 6 months, whichever is later. Older versions are converted to the internal type. |
| `snapshot.json` | Integer `schemaVersion` | Readers accept the current and the previous version, because baselines outlive the release that wrote them. |
| `report.json` | Integer `schemaVersion` and a `$schema` URL | The publish job accepts only versions it knows. |
| Rule IDs | Permanent | A renamed rule keeps its old ID as an alias. |

Configuration is decoded strictly: unknown keys are errors. JSON Schemas are generated from the Go types and committed.

```yaml
apiVersion: ichnos/v1alpha1
kind: Config
baseline:
  sources: [actions-cache, build-record-artifact]
  requireBaseline: false
explain:
  minConfidence: medium
audit:
  rules:
    shared-cache-target: { enabled: false, reason: "one image per repository" }
report:
  comment: { mode: sticky }
gates:
  mode: report-only
```

### CLI and exit codes

| Command | Runs in | Does |
|---|---|---|
| `ichnos collect` | Build job | Capture a build record; redact; write `snapshot.json` |
| `ichnos check` | Pull request job | Fetch the baseline, compare, explain and audit; write `report.json` |
| `ichnos compare BASE PR` | Anywhere | The same analysis, offline, from two records. It has no pull request diff, so causes that need one are `unknown`. |
| `ichnos audit` | Anywhere | Audit the configuration with no baseline |
| `ichnos render -f FORMAT` | Anywhere | Render `report.json` as markdown, json, sarif or codequality |
| `ichnos publish github` | Trusted job | Validate, render and update the pull request comment |
| `ichnos baseline save` | Default-branch push | Store a snapshot as the baseline |
| `ichnos rules list` | Anywhere | List the audit rules and their documentation |

| Code | Meaning |
|---|---|
| 0 | Finished, and no enforced gate failed. This includes "no baseline" in the default mode. |
| 1 | An enforced gate failed (`gates.mode: enforce` only) |
| 2 | Usage or configuration error |
| 3 | No usable input |
| 4 | Internal failure |

### Integrations

- **GitHub Actions:** a composite action in a separate `ichnos-action` repository. It verifies the binary against SHA-256 checksums embedded in the action, then its Sigstore signature, and fails on any mismatch. Publishing uses two workflows: a `pull_request` workflow that uploads `report.json`, and a `workflow_run` workflow that never checks out pull request code. The trusted job finds the pull request from the run's head repository and branch, and never trusts a pull request number from the artifact.
- **Baselines:** `actions/cache` keyed by commit, written only by default-branch pushes; Docker's `.dockerbuild` artifact as a fallback; OCI artifacts and GitLab job artifacts for other CI systems.
- **GitLab CI:** a Code Quality report first, with merge request comments opt-in through a project access token.
- **Tekton, Buildkite and Jenkins:** a signed Tekton StepAction, a Buildkite plugin, and SARIF plus `TRACEPARENT` for Jenkins.
- **OpenTelemetry:** a `cicd.pipeline.task.internal` span, joined to the CI's trace through `TRACEPARENT` when present, and metrics in Ichnos's own namespace. The exporter endpoint comes only from `OTEL_*` environment variables, never from the repository's configuration.

## Impacts / Key Questions

- Will Docker add a base-versus-pull-request comparison to its own build summary? If it does, Ichnos treats it as another input; its value is in CI neutrality, builders without Docker and an open set of evidence-based rules.
- BuildKit's history record and status stream are undocumented and versioned `v0`, although most provenance fields are documented. How often will they change?
- Does the record show a lost `gha` blob? The lab found that a missing blob fails the build on local and registry caches, but on `gha` the step logged an error, reran and succeeded. If the record keeps that error, an `evicted` cause can be added.
- Does a `pull_request` event reach its base branch's `gha` cache? The lab has measured a branch reading the default branch's cache, not a pull request.
- Should Ichnos explain builds that failed? A missing blob on a local or registry cache is a cache problem that never reaches a cache miss.
- How much does fingerprinting copied sources cost on a large build context? It has not been built or measured yet. Without it, a generated file that the pull request diff cannot see would be reported as `incomplete-cache`.

### Pros

- Uses data BuildKit already records, so users change nothing about their builds.
- The pull request diff gives Ichnos an input that local tools do not have.
- The trust-zone split keeps it safe on pull requests from forks.

### Cons

- Depends on undocumented BuildKit formats, which needs a compatibility test corpus.
- Some causes can only be inferred, not proven.
- Full value on GitHub needs two workflows instead of one step.

## Risks and Mitigations

- **Format changes in BuildKit and buildx:** the snapshot isolates consumers, and a compatibility corpus parses records from every buildx minor release since v0.23.0.
- **Wrong explanations:** confidence and stability levels, `unknown` below the threshold, and measured accuracy from the scenario lab.
- **Docker shipping the same feature:** CI neutrality and BuildKit-level support remain Ichnos's ground.

### Security Considerations

- **Permissions:** the analysis job runs with a read-only token. Only the publish job can comment, and it never runs pull request code.
- **Information flow:** build records may contain cache tokens, credentials, build argument values, signed URLs and, on GitHub Actions, the whole event payload, which includes the pusher's email address on a `push`. Redaction runs before any snapshot is written, and snapshots never copy the event payload. Snapshots store redacted Dockerfile text instead of step names, build argument and environment values are stored only as hashes, and import errors are stored as a class, never as raw text. Dockerfile text otherwise keeps what the repository commits, including literal `ENV` values. A hash does not hide a value that can be guessed, so a secret must use a BuildKit secret, which the lab found in no part of a build record.
- **Surface exposed to untrusted input:** `report.json` crosses the trust boundary and is checked against its schema and size limit before use. Dockerfile text and file paths are escaped, and `@` mentions are neutralized, before posting.
- **Supply chain:** every action is pinned to a full commit SHA, there is no `pull_request_target`, and releases are signed and immutable.

## Future Milestones

- Third-party audit rules as separate executables, after v1.0.
- An active repeat-build mode to detect non-deterministic inputs.
- Comparison against several base builds, to reduce noise from flaky caches.

## Implementation Details

### Testing Plan

- Table-driven unit tests, and golden files for every renderer (`go test ./... -update`).
- CLI tests with `testscript` and `.txtar` files.
- Native Go fuzz tests for every parser of untrusted input.
- A compatibility corpus of recorded build records for each supported buildx version.
- A scenario lab of fixture builds that break the cache in every known way, run against real BuildKit, with published accuracy.
- SARIF output validated against the OASIS SARIF 2.1.0 schema.

### Update/Rollback Compatibility

Readers accept the current and the previous snapshot schema version, so a baseline written by an older release can still be read. It can be compared only if its fingerprint version matches. Configuration files from older `apiVersion`s are converted. Rolling back the Action is a change of its pinned SHA.

### Scalability

Ichnos holds the records it compares in memory. Parsers enforce size limits on records, reports and comments. Alignment is `n log n` and step fingerprints take one pass over the steps, so a very large Dockerfile cannot make them slow. Source fingerprints grow with the size of the build context, so they need a size budget.

### Implementation Phases/History

| Release | Target | Scope |
|---|---|---|
| v0.1 | Jan 2027 | Offline compare; first miss and, when the records show it, the changed input; time accounting; JSON and Markdown |
| v0.2 | Mar 2027 | Causes with confidence; changed files from the diff; builder versions; GitHub Action |
| v0.3 | May 2027 | Audit rules; SARIF; configuration file and JSON Schema; opt-in gates |
| v0.4 | Aug 2027 | GitLab, Buildkite, Jenkins and Tekton; `buildctl` collector; OCI baselines |
| v0.5 | Oct 2027 | OpenTelemetry traces and metrics |
| v1.0 | 2028 | Stable contracts and deprecation policy |

- 2026-10-28: proposal opened
- 2026-11-02: replaced the digest rule with a comparison of step definitions, after the scenario lab disproved it
