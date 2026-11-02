# Ichnos Roadmap

This document defines high-level plans for the Ichnos project and is neither complete nor prescriptive. Each item will be tracked in a GitHub [issue](https://github.com/ichnos-project/ichnos/issues).

## Project Vision

Every team that builds containers in CI should know why a build was slow, without reading BuildKit logs by hand. Ichnos aims to be the open, CI-agnostic way to explain container build cache behavior, with explanations backed by evidence.

## Milestones

| Release | Target | Theme | Includes |
|---|---|---|---|
| v0.1 | Jan 2027 | Pinpoint | Read BuildKit build records; compare two builds step by step; name the first miss and, when the records show it, the input that changed; minutes lost per miss; JSON and Markdown output |
| v0.2 | Mar 2027 | Explain in the pull request | Causes with confidence levels; changed files from the pull request diff; builder-version changes; the GitHub Action with fork-safe comments |
| v0.3 | May 2027 | Audit | Cache setup audit rules with fixes; SARIF output; a versioned configuration file with a JSON Schema |
| v0.4 | Aug 2027 | Anywhere | GitLab CI, Buildkite, Jenkins and Tekton; builds without Docker through `buildctl` |
| v0.5 | Oct 2027 | Observe | OpenTelemetry traces and metrics |
| v1.0 | 2028 | Stable | Stable configuration and report formats, and a written deprecation policy |

## Out of Scope

Ichnos is used alongside other tools, so these are out of scope:

* Image size, package and vulnerability comparison ([Docker Scout](https://docs.docker.com/scout/))
* Layer efficiency analysis ([dive](https://github.com/wagoodman/dive))
* Dockerfile style linting ([hadolint](https://github.com/hadolint/hadolint) and Docker build checks)
* AI-generated explanations
* A hosted dashboard or cache service
