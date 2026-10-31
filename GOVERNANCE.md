# Ichnos Project Governance

The Ichnos project is dedicated to explaining container build cache behavior in CI, openly and with evidence. This governance explains how the project is run.

- [Values](#values)
- [Maintainers](#maintainers)
- [Becoming a Maintainer](#becoming-a-maintainer)
- [Code Changes](#code-changes)
- [Meetings](#meetings)
- [Code of Conduct Enforcement](#code-of-conduct)
- [Security Response Team](#security-response-team)
- [Voting](#voting)
- [Modifications](#modifying-this-charter)

## Values

Ichnos and its leadership embrace the following values:

* Openness: Communication and decision-making happens in the open and is discoverable for future reference. As much as possible, all discussions and work take place in public forums and open repositories.

* Fairness: All stakeholders have the opportunity to provide feedback and submit contributions, which will be considered on their merits.

* Community over Product or Company: Sustaining and growing our community takes priority over shipping code or sponsors' organizational goals. Each contributor participates in the project as an individual.

* Vendor Neutrality: The project direction and decisions are not controlled by any single organization. Maintainer selection, roadmap prioritization, and release decisions are made based on project merit, not employer affiliation.

* Inclusivity: We innovate through different perspectives and skill sets, which can only be accomplished in a welcoming and respectful environment.

* Participation: Responsibilities within the project are earned through participation, and there is a clear path up the [contributor ladder](CONTRIBUTOR_LADDER.md) into leadership positions.

## Maintainers

Ichnos Maintainers have write access to the [project GitHub repository](https://github.com/ichnos-project/ichnos). They can merge their own patches or patches from others. The current maintainers can be found in [MAINTAINERS.md](./MAINTAINERS.md). Maintainers collectively manage the project's resources and contributors.

This privilege is granted with some expectation of responsibility: maintainers are people who care about the Ichnos project and want to help it grow and improve. A maintainer is not just someone who can make changes, but someone who has demonstrated their ability to collaborate with the team, get the most knowledgeable people to review code and docs, contribute high-quality code, and follow through to fix issues (in code or tests).

A maintainer is a contributor to the project's success and a citizen helping the project succeed.

The collective team of all Maintainers is known as the Maintainer Council, which is the governing body for the project.

### Becoming a Maintainer

To become a Maintainer you need to demonstrate the following:

  * commitment to the project:
    * participate in discussions, contributions, code and documentation reviews for 3 months or more,
    * perform reviews for 10 non-trivial pull requests,
    * contribute 10 non-trivial pull requests and have them merged,
  * ability to write quality code and/or documentation,
  * ability to collaborate with the team,
  * understanding of how the team works (policies, processes for testing and code review, etc),
  * understanding of the project's code base and coding and documentation style.

A new Maintainer must be proposed by an existing maintainer by opening a public [GitHub Discussion](https://github.com/ichnos-project/ichnos/discussions). A simple majority vote of existing Maintainers approves the application. Maintainer nominations will be evaluated without prejudice to employer or demographics and should consider the organizational diversity of the maintainer group.

The maintainers will avoid nominating new maintainers from any organization that already employs 50% or more of existing maintainers.

Maintainers who are selected will be granted the necessary GitHub rights, and added to [MAINTAINERS.md](./MAINTAINERS.md) and [CODEOWNERS](./CODEOWNERS).

### Removing a Maintainer

Maintainers may resign at any time if they feel that they will not be able to continue fulfilling their project duties.

Maintainers may also be removed after being inactive, failure to fulfill their Maintainer responsibilities, violating the Code of Conduct, or other reasons. Inactivity is defined as a period of very low or no activity in the project for 12 months or more, with no definite schedule to return to full Maintainer activity.

A Maintainer may be removed at any time by a 2/3 vote of the remaining maintainers.

### Emeritus Maintainers

Depending on the reason for removal or resignation, a Maintainer may be converted to Emeritus status. Emeritus Maintainers are recognized for their past contributions and may still be consulted on project matters, but do not have voting rights or merge access. Emeritus Maintainers are listed in [MAINTAINERS.md](./MAINTAINERS.md) under a separate Emeritus section.

An Emeritus Maintainer may be reinstated to active Maintainer status by a simple majority vote of existing Maintainers, provided they meet the current Maintainer requirements and can commit to ongoing participation.

## Code Changes

All code changes go through the pull request process described in [CONTRIBUTING.md](CONTRIBUTING.md). Pull requests are merged only after approval from at least one maintainer, or from a reviewer of the affected area, who is not the author. While the project has a single maintainer, the maintainer's own pull requests stay open for at least 2 business days before merging, so that others can comment.

We do not vote formally on every code change, but we do expect that every code change merged has the same community support as if the change were approved by a formal vote. When a merge occurs without sufficient community support, the change should be reverted until the dispute is resolved through discussion. Any team member who feels that a technical decision cannot be reached can call for a formal vote following the rules outlined below in either the pull request or a separate issue.

## Meetings

Maintainers will have ad-hoc closed meetings in order to discuss security reports or Code of Conduct violations. Such meetings should be scheduled by any Maintainer on receipt of a security issue or CoC report. All current Maintainers must be invited to such closed meetings, except for any Maintainer who is accused of a CoC violation.

## Code of Conduct

[Code of Conduct](./CODE_OF_CONDUCT.md) violations by community members will be discussed and resolved privately by the Maintainers. If a Maintainer is directly involved in the report, they will take no part in resolving it.

## Security Response Team

The Maintainers will appoint a Security Response Team to handle security reports. This committee may simply consist of the Maintainer Council themselves. If this responsibility is delegated, the Maintainers will appoint a team of at least two contributors to handle it. The Maintainers will review who is assigned to this at least once a year.

The Security Response Team is responsible for handling all reports of security holes and breaches according to the [security policy](SECURITY.md).

## Voting

While most business in Ichnos is conducted by "[lazy consensus](https://community.apache.org/committers/lazyConsensus.html)", periodically the Maintainers may need to vote on specific actions or changes. A vote can be taken in a public [GitHub Discussion](https://github.com/ichnos-project/ichnos/discussions), or privately among the Maintainers for security or conduct matters. Any Maintainer may demand a vote be taken.

Most votes require a simple majority of all Maintainers to succeed, except where otherwise noted. Two-thirds majority votes mean at least two-thirds of all existing maintainers.

No single organization's employees may cast more than half of the total votes on any decision.

## When to Evolve This Governance

The Maintainer Council model works well for focused projects with a small, cohesive group of contributors. As the project grows, watch for these signals that a governance transition may be needed:

* **Decisions stall.** When the maintainer group is too large for lazy consensus to work, or when decisions affect subgroups differently, a delegation structure (working groups, SIGs) helps.
* **New contributors cannot find a path in.** If the only path to influence is "become a maintainer," the project needs intermediate roles (reviewer, approver).
* **A single organization dominates.** When one company holds a majority of maintainer seats, consider company representation caps or transitioning to an elected steering committee.

These transitions are a sign of project growth, not governance failure.

## Modifying this Charter

Changes to this Governance and its supporting documents may be approved by a 2/3 vote of the Maintainers.
