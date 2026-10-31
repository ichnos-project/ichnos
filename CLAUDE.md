@AGENTS.md

## Claude Code

- Work on code through the `implement` skill, which runs the research, the plan, tests, checks and `review-changes`. Finish with `/prepare-pr`.
- Brief a subagent in a few lines: the goal, the files, what is already known, the output you need and the limits. Subagents load AGENTS.md themselves, so do not repeat it, and give a reviewer the change rather than your opinion of it.
- A hook in `.claude/settings.json` rejects sign-offs and AI trailers in commits, and skipped git hooks. It catches mistakes in Bash calls and is not a security boundary. When it blocks you, fix what it reports.
- The hook is built from `hack/tools` with Go 1.26 or later when a session starts. If the build fails, the session start shows the error and the hook stays off or stale until it succeeds. Because the hook runs code from this checkout, review a branch's changes to `.claude/` and `hack/tools/` before starting a session on it.
