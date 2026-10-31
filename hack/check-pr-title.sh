#!/usr/bin/env bash
# Checks that a pull request title is a Conventional Commits header, because
# the squash merge turns it into the commit message on main.
#
# Usage: hack/check-pr-title.sh TITLE
set -euo pipefail

title=${1?usage: hack/check-pr-title.sh TITLE}
types='feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert'
pattern="^($types)(\([a-z0-9][a-z0-9-]*\))?!?: [a-z0-9](.*[^.[:space:]])?$"

if [[ ! $title =~ $pattern ]]; then
  echo "The title is not type(scope): summary. Use one of the types $types; start the summary with a lowercase letter or a digit, and end it without a period or a space." >&2
  exit 1
fi

if ((${#title} > 72)); then
  echo "The title is ${#title} characters long. The limit is 72." >&2
  exit 1
fi
