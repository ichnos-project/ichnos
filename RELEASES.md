# Release Process

This document describes the release process for Ichnos.

## Release Cadence

Ichnos follows a feature-based release cadence. Until v1.0, a minor release is made when a [roadmap](ROADMAP.md) milestone is complete, approximately every 2 to 3 months. Patch releases are made as needed.

## Versioning

Ichnos follows [Semantic Versioning](https://semver.org/):

* **Major (X.0.0):** Incompatible API changes
* **Minor (x.Y.0):** New functionality, backwards compatible
* **Patch (x.y.Z):** Bug fixes, backwards compatible

The CLI, the configuration file and the report format are the project's API. Until v1.0, a minor release may include breaking changes, and each one is listed in the release notes.

### Deprecation Policy

* A deprecated configuration or report field produces a warning, and is removed no earlier than two minor releases or six months later, whichever is later.
* A deprecated CLI flag remains for at least two minor releases.
* Audit rule IDs are never reused or renamed.

## Release Types

| Type | Description | Example |
| ---- | ----------- | ------- |
| Stable | Production-ready releases | v1.2.0 |
| Pre-release | Release candidates and betas | v1.3.0-rc.1 |
| Security | Patches for security vulnerabilities | v1.2.1 |

## Branch Strategy

* `main` - Development branch for the next release
* `release-x.y` - Maintenance branches for supported releases

## Supported Versions

Until v1.0, Ichnos maintains support for the most recent minor release only. The support policy for v1.0 and later will be published here before v1.0.

| Version | Status |
| ------- | ------ |
| Latest release | Active |

## Release Artifacts

Each release includes:

* Source code archives (tar.gz, zip)
* Container images published to `ghcr.io/ichnos-project/ichnos`
* Binary releases for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 and windows/arm64
* Checksums file (SHA256)
* [Sigstore](https://www.sigstore.dev/) signatures, SLSA provenance and a Software Bill of Materials (SBOM)

Releases are published as immutable GitHub releases.

## Creating a Release

1. The changelog and release notes are generated from the Conventional Commit titles of merged pull requests, in a release pull request.
2. A maintainer reviews and merges the release pull request, which creates a signed tag.
3. The tag triggers the release workflow, which builds, signs and publishes every artifact.
4. The release notes list every fixed vulnerability, with its CVE identifier where one exists.

## Release Roles

Release management is handled by the project maintainers.
