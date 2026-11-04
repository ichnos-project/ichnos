# Coding Conventions

These conventions apply to all code in Ichnos, whoever or whatever writes it. They follow the style guides of CNCF Go projects and patterns taken from the source of oras, notation, kubescape, ko, flux, helm, containerd and in-toto. [golangci-lint](../.golangci.yml) enforces many of them in CI. The design they serve is in [proposal 002](proposals/002-ichnos-architecture.md).

* [Style Guides](#style-guides)
* [Design Principles](#design-principles)
* [Structure](#structure)
* [Errors](#errors)
* [Type Safety](#type-safety)
* [Logging and Output](#logging-and-output)
* [Untrusted Input](#untrusted-input)
* [Secrets](#secrets)
* [Network](#network)
* [Performance](#performance)
* [Dependencies](#dependencies)
* [Testing](#testing)
* [Comments](#comments)

## Style Guides

Where these sources disagree, the first one wins:

1. This document
2. [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)
3. [Google Go Style Guide](https://google.github.io/styleguide/go/)
4. [Effective Go](https://go.dev/doc/effective_go)

Code is formatted with `gofumpt` and `goimports`, and every linter warning is an error.

## Design Principles

* **Single responsibility.** A package owns one concern from the architecture proposal, and a function does one thing. If a name needs "and", split it.
* **Open/closed.** Add a collector, baseline provider, renderer or audit rule by writing a new type and adding it to its registry list, not by editing switch statements elsewhere.
* **Liskov substitution.** Every implementation keeps its interface's documented contract, errors included: a baseline provider with nothing to return gives `ErrNoBaseline`, never `nil, nil`.
* **Interface segregation.** Interfaces are small, usually one to three methods, and defined by the package that uses them.
* **Dependency inversion.** The engine depends on interfaces it defines; `internal/cli` builds the implementations and passes them in.
* **DRY.** Each fact has one source: schemas come from the Go types, rule documentation from rule metadata. Code that only looks alike but changes for different reasons is not duplication.
* **KISS.** Choose the simplest design that meets the requirement: plain functions before generics, the standard library before a dependency, and no reflection.
* **YAGNI.** Build nothing for a need that does not exist yet: no unused options, no feature flags or compatibility shims, and no interface until a second implementation or a test fake needs it.
* **Separation of concerns.** Collection, comparison, explanation, auditing, rendering and publishing stay in their own packages. I/O stays at the edges, so comparison and explanation are pure functions that tests call directly.

## Structure

* Keep to the package layout in the architecture proposal: the public API in `pkg/`, everything else in `internal/`. Name a package after what it provides; `util`, `common`, `helpers` and `types` are not accepted.
* `main` only calls `run`, which creates the context with `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`, because CI cancels jobs with SIGTERM. Only `main` calls `os.Exit`.
* Build the command tree in constructors such as `NewRootCommand(streams)`, never in package variables or `init()`, so every test gets a fresh tree.
* Each command fills an options struct from its flags, calls `opts.validate()`, then `run(ctx, opts, deps)`. Set `SilenceUsage` only after validation passes. Flags live only in `internal/cli`.
* Registries are explicit lists that reject duplicate and unknown names.
* Inject access to the environment, the clock and the file system, so tests never change global state.
* Pass a `context.Context` as the first argument to every function that does I/O or may block. Never store one in a struct, and never call `context.Background()` below `main`.
* Run concurrent work with `errgroup.WithContext` and `SetLimit` (from `golang.org/x/sync`, which needs approval like any dependency), and have each goroutine write `results[i]`, so output order never depends on timing. Start no goroutine without a clear end.
* Assert every implementation, fakes included, with `var _ Collector = (*bundleCollector)(nil)`.
* Change the design through a [design proposal](proposals/README.md), not inside an unrelated pull request.

## Errors

* Return errors. Code never panics, and `main` maps errors to the exit codes in the proposal by finding a typed exit error with `errors.As`.
* Write error strings in lowercase with no trailing punctuation, and wrap with `%w` at the end to add context: `fmt.Errorf("reading snapshot: %w", err)`.
* Give callers a sentinel (`ErrNoBaseline`) or a typed error for every condition they act on, and compare with `errors.Is` and `errors.As`, never by text. Never return `nil, nil`.
* Handle every error once: return it or log it, not both.
* Errors shown to users say what to do next.

## Type Safety

* Give domain values their own types, such as `type RuleID string` and `type Severity int`, with constants, a `Parse` function that returns a sentinel error for unknown values, and `String()`. Parse at the boundary; inside, pass only typed values.
* Flag types implement `pflag.Value`, and configuration types implement `encoding.TextUnmarshaler`, so invalid values fail when they are read.
* Switches over these types are exhaustive, and a `default:` case never hides a missing value.
* Keep `any` out of APIs; decode into concrete types. Check every type assertion.
* Carry units in types and names: `time.Duration`, `maxRecordBytes`. Check integer conversions for overflow.

## Logging and Output

* stdout carries only the result. Logs, progress and errors go to stderr, and machine-readable formats get no progress output.
* Log with the `*slog.Logger` from `engine.Settings`, using key/value pairs. A nil logger discards, and only `cmd` sets a default.
* Write results to an injected `io.Writer`. Sort data and inject the clock, so the same input always gives the same bytes.
* Take tracers and meters from `engine.Settings`, never from the global OpenTelemetry providers.

## Untrusted Input

Build records, snapshots, reports, configuration files, CI events and API responses can all be written by an attacker.

* Check every input against an allowlist, and reject anything else.
* Read at most `limit+1` bytes through `io.LimitReader` against one budget for the whole input, and return a typed error when it is exceeded. Never trust a declared size, and cap counts of steps, files and findings.
* Accept only regular files: open the path without blocking (`O_NONBLOCK` on Unix), then `Stat` the opened file, so a pipe or device cannot hang a run.
* Read the version of a versioned file first, decode strictly with the matching type (unknown fields and trailing data are errors), validate it, and convert it to the one internal type. Reject unknown versions. BuildKit records are third-party, so decode them tolerantly and validate only the fields Ichnos uses.
* Open files under a directory through `os.Root`, check names taken from data with `filepath.IsLocal`, and reject a symlinked configuration file. Never check a path and then open it separately.
* Read an archive with one size budget shared by all entries, and reject symlinks, unsupported entry types and size mismatches.
* Never decode untrusted JSON into `map[string]any`, `[]any`, or a slice or map of any type without a limit on its length: decoded that way, a value can take about 70 times its size in memory. Read it as a stream with `encoding/json/jsontext`, and check each limit before keeping another element.
* Escape text for where it goes, in one package per destination. Markdown: put untrusted text in code spans fenced by more backticks than it contains, and neutralize `@` mentions. Terminal: strip control and bidirectional characters. GitHub workflow commands: escape them, and cap annotations and summary size.
* Give every parser of untrusted input a fuzz test, seeded from `testdata/`, that checks properties rather than only the absence of a panic.

## Secrets

* Never write tokens, credentials or build argument values to logs, errors, reports or telemetry.
* Every collector's data passes through one redaction step, which fails closed: when a credential key appears, the whole value is withheld. URL user information and query values are always removed.
* A test plants a known secret in every input and checks that it appears in no output.

## Network

* Create every HTTP client through one constructor: a clone of `http.DefaultTransport`, a timeout for each purpose, and the user agent `ichnos/<version>`.
* Retry only idempotent requests, or requests whose body can be replayed, with jitter and a capped `Retry-After`. Treat a GitHub 403 with `x-ratelimit-remaining: 0` as rate limiting.
* Check the status before reading a body. Limit every body, and read at most 1 KiB of an error body before draining it.
* Send credentials only to the configured host, and redact authorization headers and query strings from debug logs.

## Performance

* Stream large inputs, and never read past the input budget.
* Keep matching and comparison linear or `n log n`: index steps in maps instead of comparing every pair.
* Parse each input once, and pass the typed result along.
* Allocate slices and maps at their known size.
* Measure before optimizing: a change to a hot path comes with a `BenchmarkXxx` and the numbers before and after.

## Dependencies

Every dependency is code the project must keep secure, so prefer the standard library.

* A new module needs a maintainer's approval in the issue or pull request before it is added.
* Check that the module exists at that exact path, is maintained, comes from its original source rather than a fork, has a tagged release at least 14 days old (or a newer one that fixes a vulnerability), has no `init()` side effects, and uses a license allowed by the [CNCF license policy](https://github.com/cncf/foundation/blob/main/policies-guidance/allowed-third-party-license-policy.md).
* Pin GitHub Actions to a full commit SHA, and tools to an exact version, following the same rule.
* `govulncheck ./...` must report no vulnerabilities in code Ichnos calls.

## Testing

* New behavior comes with tests, and a bug fix comes with a test that fails without it.
* Prefer table-driven tests with `t.Run`. A failure message names the call and prints the result before the expected value: `Parse(%q) = %v, want %v`.
* Test every edge case in the plan: empty input, input exactly at and one byte over each limit, malformed and truncated data, unknown versions, Unicode and cancellation.
* Every renderer has golden files, updated with `go test ./... -update` only when output changes on purpose.
* CLI tests run the command in-process with injected streams, and check stdout, stderr and the exit code.
* Use hand-written fakes, not mocking frameworks: one error field per method, and `httptest` for the GitHub API.
* Run tests with the race detector, and run each fuzz target in CI for a fixed number of inputs. Aim for at least 80% statement coverage, the OpenSSF Best Practices silver level.
* Never delete, skip or loosen a test, or add a `//nolint` directive, to make a check pass.

## Comments

Code says what it does; a comment says what the code cannot, usually why it is written this way. Every comment must be read by each reader and kept true by each maintainer, so write one only when deleting it would lose information.

Write:

* A doc comment on every package and every exported identifier, as [Go Doc Comments](https://go.dev/doc/comment) describes: complete sentences that start with the name and tell callers what they need beyond the signature.
* A comment on unexported code whose behavior is not obvious.
* The reason behind code that would otherwise look wrong: a workaround (with a link to the upstream issue), a security constraint, an invariant or a deliberate trade-off.
* `//nolint:linter // reason`, when a linter is wrong about a specific line.

Do not write:

* Comments that restate the code, such as `// loop over the steps`.
* Comments about the edit itself: what was removed, replaced, moved, renamed or fixed, what the code "now" does, or how it "used to" work. The commit message records the change, and such a comment is out of date the moment it merges.
* Commented-out code. Delete it; git keeps it.
* TODO, FIXME, HACK or XXX notes. Open an issue instead.
* Section banners, or notes addressed to a reviewer or an AI assistant.

When you change code, update or delete every comment the change makes wrong.

| Check | Catches |
|---|---|
| `revive` (`exported`, `package-comments`), `staticcheck` (ST1020–ST1022), `godot` | Missing or malformed doc comments |
| `gocritic` (`commentedOutCode`, `docStub`), `godox` | Commented-out code, placeholder doc comments and TODO-style notes in Go |
| `nolintlint` | `//nolint` without a linter name and a reason |

No tool can reliably tell a comment that describes an edit or restates the code from a needed one, so reviewers check for those, and for TODO notes outside Go.
