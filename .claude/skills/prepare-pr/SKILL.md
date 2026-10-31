---
name: prepare-pr
description: Writes the Conventional Commits title, commit message and pull request description for the current Ichnos change, and commits it for the human author to sign off. Use when a change is finished and reviewed.
argument-hint: "[issue number]"
disable-model-invocation: true
---

1. Read the full change (`git diff main...HEAD` plus uncommitted changes) and issue $ARGUMENTS.
2. Write the title as described in "Commits and Pull Requests" in AGENTS.md, with `!` before the colon for a breaking change and a scope that names the package or area. Check it with `hack/check-pr-title.sh "<title>"`.
3. Add a body only when the reason is not obvious from the title. Wrap it at 72 characters, explain why rather than what, and end with `BREAKING CHANGE: <what users must do>` when needed.
4. Commit with that message and no trailers.
5. Fill in every section of .github/PULL_REQUEST_TEMPLATE.md: what changed and why, `Fixes #N`, the type of change, and only the checklist items that are true. Ask the user for anything you cannot verify, such as manual testing, and use their answer.
6. Show the user the title and description, and give them the commands to sign off and push:
   ```
   git rebase --signoff main
   git push
   ```
