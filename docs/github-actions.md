# Using Ichnos with GitHub Actions

> [!IMPORTANT]
> Ichnos is not released yet. This page describes the planned setup, which may change.

Ichnos runs in two workflows, the pattern GitHub recommends for commenting safely on pull requests from forks:

- the **build** workflow runs pull request code, so it gets a read-only token;
- the **comment** workflow has permission to comment, so it never checks out or runs pull request code.

## Build Workflow

```yaml
# .github/workflows/build.yml: runs pull request code, so it gets no write access
name: build
on:
  pull_request:
  push:
    branches: [main]
permissions: {}
jobs:
  image:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@<full-commit-sha>             # pin every action to a full commit SHA
        with:
          persist-credentials: false
      - uses: docker/setup-buildx-action@<full-commit-sha>
      - uses: docker/build-push-action@<full-commit-sha>     # uploads a build record by default
        with:
          context: .
          cache-from: type=gha
          cache-to: ${{ github.event_name == 'push' && 'type=gha,mode=max' || '' }}
      - uses: ichnos-project/ichnos-action/check@<full-commit-sha>
        # On pushes to main this saves the baseline; on pull requests it writes report.json.
```

## Comment Workflow

```yaml
# .github/workflows/ichnos-comment.yml: never checks out or runs pull request code
name: ichnos-comment
on:
  workflow_run:
    workflows: [build]
    types: [completed]
permissions: {}
jobs:
  comment:
    if: github.event.workflow_run.event == 'pull_request'
    runs-on: ubuntu-latest
    permissions:
      actions: read
      pull-requests: write
    steps:
      - uses: ichnos-project/ichnos-action/comment@<full-commit-sha>
        # Checks report.json against its schema, then updates a single comment.
```

## Why Two Workflows?

A workflow that runs pull request code must not hold a token that can write to the repository, because the pull request author controls that code. The comment workflow runs from the default branch with permission to comment, and only reads `report.json`, which it checks against the report schema before using it. Only builds of `main` save baselines, so a pull request cannot replace the build it is compared against.
