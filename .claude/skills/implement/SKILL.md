---
name: implement
description: Takes an Ichnos issue or code change from research and an approved plan to a tested, reviewed diff. Use when asked to implement a feature, fix a bug or change code.
argument-hint: "[issue number or task]"
---

Task: $ARGUMENTS

1. Do the steps in "Before You Write Code" in AGENTS.md. Use the `researcher` agent when the documentation lookups take more than a few fetches. If the change needs a design proposal that does not exist yet, stop and say so.
2. Present the plan and wait for the user's go-ahead. Write no code before it.
3. Write tests first, covering the behavior and every edge case in the plan, and confirm they fail for the right reason.
4. Make the smallest correct change that passes them, with nothing left unfinished.
5. Run the checks in the CONTRIBUTING.md Pull Request Checklist, and show each command and its result. Fix failures in the code, never by weakening tests or linter settings.
6. Run the `review-changes` skill and resolve each confirmed finding.
7. Report what changed, the evidence, the sources and any follow-ups.
