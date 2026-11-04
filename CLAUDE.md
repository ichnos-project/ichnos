@AGENTS.md

## Claude Code

- Work on code through the `implement` skill, which runs the research, the plan, tests, checks and `review-changes`. Finish with `/prepare-pr`.
- Brief a subagent in a few lines: the goal, the files, what is already known, the output you need and the limits. Subagents load AGENTS.md themselves, so do not repeat it, and give a reviewer the change rather than your opinion of it.
- Review a branch's changes to `.claude/` before starting a session on it: its settings can run commands as soon as the session starts.
