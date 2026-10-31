# Ichnos Architecture

**Authors**: Rayan Alyasi (@RayanAlyasi)

**Begin Design Discussion**: 2026-10-28

**Status**: provisional

**Checklist**:

- [ ] Snapshot, report and configuration schemas (`schemaVersion` 1, `v1alpha1`)
- [ ] Collectors: `.dockerbuild` bundle and `rawjson` progress stream
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
6. Work with any CI system and any BuildKit client, not only Docker and GitHub.

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
  collect/                        bundle, rawjson and buildctl collectors; redaction
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
	Collect(ctx context.Context, src Source) (*snapshot.Snapshot, error)
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

Redaction is a decorator that the constructor wraps around every collector, so no collector can skip it. Buildx copies the GitHub Actions cache token and cloud credentials into cache settings, so build records must be treated as secret-bearing input.

### Snapshot

A snapshot is Ichnos's own normalized view of one build. It shields every consumer from BuildKit's undocumented formats. It contains:

- **Steps:** a stable key, the step name without its `#N` prefix, the digest, the cached flag, start and end times, the kind (build step, cache import, cache export or internal), and the Dockerfile location from BuildKit's provenance.
- **Materials:** base images and their digests.
- **Cache imports and exports:** imports from the record; exports inferred from step names, because BuildKit does not store export settings.
- **Build arguments:** names, with hashed values.
- **`SOURCE_DATE_EPOCH`**, the CI event and its cache mode, and the builder versions.

### Explanation algorithm

The explanation follows the shape of BuildBuddy's `bb explain`: each step is base-only, PR-only, modified or unchanged; a modified step lists typed changes, whether the pull request explains it, and how many later misses it caused.

1. **Align steps** between the base and pull request builds by Dockerfile location and instruction. Never align by digest, because a digest changes whenever anything above it changes.
2. **Mark expected misses.** A miss is expected when, under Docker's documented invalidation rules, the instruction text or a build argument changed, the base image digest changed, a `COPY` or `ADD` source appears in the pull request diff (after `.dockerignore`), `SOURCE_DATE_EPOCH` changed, or an earlier expected miss cascades into it.
3. **Report the first miss that nothing explains**, and fold the misses that cascade from it into one line.
4. **Classify the cause:** `no-cache-import`, `scope-mismatch`, `min-mode`, `evicted`, `readonly-trigger`, `cold-builder`, `builder-version`, `nondeterministic-input` or `unknown`.

When a step's digest changed, the changed input is read directly from the records. When the digest is unchanged but the step missed the cache, either copied content changed or the cache never arrived. That rule is inferred from BuildKit's design, so causes that rely on it ship as alpha until the scenario lab measures them. Below the confidence threshold, Ichnos reports `unknown` and shows its evidence.

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
| `ichnos compare BASE PR` | Anywhere | The same analysis, offline, from two records |
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
- BuildKit's build record formats are undocumented and versioned `v0`. How often will they change?
- Is the unchanged-digest inference correct? The scenario lab must answer this before those causes leave alpha.

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
- **Information flow:** build records may contain cache tokens and credentials. Redaction runs before any snapshot is written, and build argument values are stored only as hashes.
- **Surface exposed to untrusted input:** `report.json` crosses the trust boundary and is checked against its schema and size limit before use. Step names and Dockerfile snippets are escaped, and `@` mentions are neutralized, before posting.
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

Readers accept the current and the previous snapshot schema version, so a baseline written by an older release can still be compared. Configuration files from older `apiVersion`s are converted. Rolling back the Action is a change of its pinned SHA.

### Scalability

Ichnos processes one build record per run, in memory. Parsers enforce size limits on records, reports and comments.

### Implementation Phases/History

| Release | Target | Scope |
|---|---|---|
| v0.1 | Jan 2027 | Offline compare; first miss and changed input; time accounting; JSON and Markdown |
| v0.2 | Mar 2027 | Causes with confidence; changed files from the diff; builder versions; GitHub Action |
| v0.3 | May 2027 | Audit rules; SARIF; configuration file and JSON Schema; opt-in gates |
| v0.4 | Aug 2027 | GitLab, Buildkite, Jenkins and Tekton; `buildctl` collector; OCI baselines |
| v0.5 | Oct 2027 | OpenTelemetry traces and metrics |
| v1.0 | 2028 | Stable contracts and deprecation policy |

- 2026-10-28: proposal opened
