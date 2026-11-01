# Contributing Guide

* [New Contributor Guide](#contributing-guide)
  * [Ways to Contribute](#ways-to-contribute)
  * [Find an Issue](#find-an-issue)
  * [Ask for Help](#ask-for-help)
  * [Pull Request Lifecycle](#pull-request-lifecycle)
  * [Development Environment Setup](#development-environment-setup)
  * [AI-Assisted Contributions](#ai-assisted-contributions)
  * [Sign Your Commits](#sign-your-commits)
  * [Pull Request Checklist](#pull-request-checklist)

Welcome! We are glad that you want to contribute to Ichnos! 💖

As you get started, you are in the best position to give us feedback on areas of our project that we need help with including:

* Problems found during setting up a new developer environment
* Gaps in our documentation
* Cache misses that Ichnos explains wrongly, or cannot explain at all

If anything doesn't make sense, or doesn't work when you run it, please open a [bug report](https://github.com/ichnos-project/ichnos/issues/new/choose) and let us know!

## Ways to Contribute

We welcome many different types of contributions including:

* New features
* New audit rules
* Support for more CI providers and builders
* Builds, CI/CD
* Bug fixes
* Documentation
* Issue Triage
* Answering questions in GitHub Discussions
* Release management

Not everything happens through a GitHub pull request. Please [start a discussion](https://github.com/ichnos-project/ichnos/discussions) and let's discuss how we can work together.

## Find an Issue

We have good first issues for new contributors and help wanted issues suitable for any contributor. [good first issue](https://github.com/ichnos-project/ichnos/issues?q=is%3Aopen+label%3A%22good+first+issue%22) has extra information to help you make your first contribution. [help wanted](https://github.com/ichnos-project/ichnos/issues?q=is%3Aopen+label%3A%22help+wanted%22) are issues suitable for someone who isn't a core maintainer and is good to move onto after your first pull request.

Sometimes there won't be any issues with these labels. That's ok! There is likely still something for you to work on. If you want to contribute but you don't know where to start or can't find a suitable issue, ask in [GitHub Discussions](https://github.com/ichnos-project/ichnos/discussions) and a maintainer will help you find one.

Once you see an issue that you'd like to work on, please post a comment saying that you want to work on it. Something like "I want to work on this" is fine.

## Ask for Help

The best way to reach us with a question when contributing is to ask on:

* The original GitHub issue
* [GitHub Discussions](https://github.com/ichnos-project/ichnos/discussions)

## Pull Request Lifecycle

1. Open a draft pull request early if you would like feedback on your approach.
2. Mark the pull request ready for review when the [Pull Request Checklist](#pull-request-checklist) is complete. Reviewers are assigned automatically from [CODEOWNERS](CODEOWNERS).
3. A reviewer will respond within 5 business days. If you have not heard back after that, comment on the pull request.
4. Address review feedback by pushing new commits. Please don't force-push during review, so reviewers can see what changed.
5. A maintainer merges the pull request once it is approved and all checks pass. [Code Changes](GOVERNANCE.md#code-changes) covers the maintainer's own pull requests. Pull requests are squash-merged, and the pull request title becomes the commit message, so it follows [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/): `type(scope): summary`, at most 72 characters, in the imperative mood, lowercase and with no trailing period. For example, `fix(explain): align steps by Dockerfile location`. A breaking change adds `!` before the colon: `type(scope)!: summary`. CI checks the title.
6. A pull request with no activity from its author for 30 days may be closed. You are welcome to reopen it.

Larger changes, such as changes to the configuration or report schemas, new commands or support for a new CI provider, start with a design proposal. See [docs/proposals](docs/proposals/README.md).

## Development Environment Setup

You will need:

* Go 1.26 or later
* `make`
* [golangci-lint](https://golangci-lint.run/), for `make lint`

| Command | Description |
|---|---|
| `make test` | Run the tests, with the race detector |
| `make fuzz` | Run each fuzz test for a fixed number of inputs, as CI does |
| `make lint` | Run `golangci-lint` |

The code of Ichnos itself has not landed yet. `make build`, `make test-e2e` and `make generate` arrive with it, and the end-to-end tests will need Docker with Buildx or a BuildKit daemon.

Generated files are committed. Regenerate the artwork with `python3 hack/artwork/generate.py` instead of editing it.

Code follows the [Coding Conventions](docs/coding-conventions.md).

### Testing

* New functionality must come with automated tests.
* A bug fix must include a test that fails without the fix.
* A change to output must update the golden files (`go test ./... -update`).
* CI treats linter warnings as errors.

### Adding an Audit Rule

Each audit rule needs:

1. A permanent ID in lowercase kebab-case, such as `shared-cache-target`. Rule IDs are never reused or renamed.
2. Evidence: a rule may only fire on facts from the build record, the CI configuration, the Dockerfile or the CI provider's API.
3. A documentation page that explains the problem, shows the fix and links to the upstream issues that justify the rule.
4. Tests for at least one case where the rule fires and one where it must not.

## AI-Assisted Contributions

You may use AI tools to help you write code, tests or documentation. You remain the author, and you are responsible for the whole contribution:

* Read, understand and test every change before you submit it. You must be able to explain it and answer questions about it in review.
* Open the pull request yourself, and write or check its description. Pull requests opened by autonomous AI agents will be closed.
* Say in the pull request whether you used AI tools, and briefly how.
* Do not post issues, reviews or comments generated by AI tools without checking them first.
* Your DCO sign-off certifies that you have the right to submit the change, whatever tools you used. AI tools must not add `Signed-off-by` or `Co-authored-by` lines, because only a person can make that certification.

Instructions for AI coding agents working in this repository are in [AGENTS.md](AGENTS.md).

## Sign Your Commits

### DCO

Licensing is important to open source projects. It provides some assurances that the software will continue to be available based under the terms that the author(s) desired. We require that contributors sign off on commits submitted to our project's repositories. The [Developer Certificate of Origin (DCO)](https://probot.github.io/apps/dco/) is a way to certify that you wrote and have the right to contribute the code you are submitting to the project.

You sign-off by adding the following to your commit messages. Your sign-off must match the git user and email associated with the commit.

    This is my commit message

    Signed-off-by: Your Name <your.name@example.com>

Git has a `-s` command line option to do this automatically:

    git commit -s -m 'This is my commit message'

If you forgot to do this and have not yet pushed your changes to the remote repository, you can amend your commit with the sign-off by running

    git commit --amend -s

## Pull Request Checklist

When you submit your pull request, or you push new commits to it, our automated systems will run some checks on your new code. We require that your pull request passes these checks, but we also have more criteria than just that before we can accept and merge it. We recommend that you check the following things locally before you submit your code:

* `make test` and `make lint` pass
* New behavior has tests, and golden files change only where output changed on purpose
* New comments follow the [Comments](docs/coding-conventions.md#comments) rules
* User-facing changes are documented
* The pull request title follows [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/)
* Every commit is signed off (`git commit -s`)
