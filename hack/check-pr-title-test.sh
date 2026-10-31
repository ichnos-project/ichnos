#!/usr/bin/env bash
# Tests hack/check-pr-title.sh against titles it must accept and reject.
set -euo pipefail

hack=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
failures=0

valid=(
  "feat(audit): add the git-dir-in-context rule"
  "fix: handle an empty build record"
  "feat(config)!: rename baseline.sources"
  "ci(deps): bump the github-actions group with 2 updates"
  "revert: feat(audit): add the git-dir-in-context rule"
  "docs(proposals): describe 2 baseline providers"
  "fix: x"
)
invalid=(
  ""
  "Add the rule"
  "feat: Add the rule"
  "feat: add the rule."
  "feat(Audit): add the rule"
  "feature(audit): add the rule"
  "feat(audit) add the rule"
  "feat(audit):add the rule"
  "feat: add the rule "
  "fix: X"
  "fix: ."
  "feat(audit): add a rule that explains every cache miss in one long title!"
)

for title in "${valid[@]}"; do
  if ! "$hack/check-pr-title.sh" "$title" 2>/dev/null; then
    echo "rejected, but must be accepted: \"$title\""
    failures=$((failures + 1))
  fi
done

for title in "${invalid[@]}"; do
  if "$hack/check-pr-title.sh" "$title" 2>/dev/null; then
    echo "accepted, but must be rejected: \"$title\""
    failures=$((failures + 1))
  fi
done

[[ $failures -eq 0 ]] || exit 1
echo "check-pr-title: all cases pass"
