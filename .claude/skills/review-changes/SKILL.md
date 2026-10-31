---
name: review-changes
description: Reviews the current Ichnos change with the project's reviewer agents in parallel and reports only findings it has verified. Use before preparing a pull request, or when asked to review a change.
---

1. List the changed paths: `git diff --name-only main...HEAD`, plus uncommitted changes.
2. Start these agents in parallel:
   - `code-reviewer`, always.
   - `security-reviewer`, when Go code or go.mod changed.
   - `workflow-security-reviewer`, when .github/ or docs/github-actions.md changed.
   - `contract-compat-reviewer`, when pkg/, schemas/, internal/cli/ or internal/audit/ changed.
3. Give each agent this brief, filled in and no longer:
   > Review the change on this branch against `main`. Purpose: <one sentence>. Issue: <#N or none>. Changed files: <paths>. Report in your format.

   Give the agents the purpose, not your opinion of the code, so their review stays independent.
4. Check every finding against the code. Drop the ones that are wrong.
5. Report the confirmed findings grouped by file, most severe first, then each dropped finding with a one-line reason.
