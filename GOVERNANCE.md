# Ichnos Project Governance

The Ichnos project is dedicated to explaining container build cache behavior in CI, openly and with evidence. This document explains how the project is run while it has a single maintainer.

## Values

* Openness: decisions and their reasons are recorded in public issues, pull requests and discussions, except for security and conduct reports.
* Fairness: every contribution is considered on its merits.
* Vendor neutrality: no company controls the project's direction, and each contributor participates as an individual.

## Decisions

The maintainers listed in [MAINTAINERS.md](MAINTAINERS.md) decide the project's direction and have write access to the [repository](https://github.com/ichnos-project/ichnos). Anyone can raise a concern in an issue or a [GitHub Discussion](https://github.com/ichnos-project/ichnos/discussions).

## Code Changes

All code changes go through the pull request process described in [CONTRIBUTING.md](CONTRIBUTING.md). Pull requests are merged only after approval from a maintainer, or from a reviewer of the affected area, who is not the author. While the project has a single maintainer, the maintainer's own pull requests are merged once all required checks pass.

## Becoming a Maintainer

The usual path is to own an area of the project, such as a CI provider or an audit rule. A contributor with a record of good work in an area is added to [CODEOWNERS](CODEOWNERS) as its reviewer, and given the write access that GitHub requires for code owners. A reviewer who shows sound judgment across the project can then be proposed as a maintainer in a public [GitHub Discussion](https://github.com/ichnos-project/ichnos/discussions), and the existing maintainers decide by simple majority.

## Code of Conduct

Reports of [Code of Conduct](CODE_OF_CONDUCT.md) violations are handled privately by the maintainers. A maintainer who is involved in a report takes no part in resolving it. While the project has a single maintainer, report that maintainer's conduct to [GitHub](https://docs.github.com/en/communities/maintaining-your-safety-on-github/reporting-abuse-or-spam) instead.

## Security

Security reports are handled by the maintainers, as described in the [security policy](SECURITY.md).

## Changing This Governance

The maintainers change this document through a pull request. When the project gains a second maintainer, it will be replaced with the [CNCF maintainer council template](https://github.com/cncf/project-template/blob/main/GOVERNANCE-maintainer.md), which adds voting, removal and emeritus rules.
