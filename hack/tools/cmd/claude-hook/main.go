// Command claude-hook is a Claude Code PreToolUse hook for Bash calls. It
// rejects agent sign-offs, AI trailers and skipped git hooks in git commands.
//
// It reads the hook input as JSON on stdin. Exit code 2 blocks the tool call
// and shows the reason to Claude. When the hook itself fails, it exits with
// 1, which Claude Code reports without blocking, so a broken hook cannot stop
// all work. The check has no CI counterpart and only guards against mistakes.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/ichnos-project/ichnos/hack/tools/internal/gitguard"
	"github.com/ichnos-project/ichnos/hack/tools/internal/safetext"
)

const (
	exitAllow = 0
	exitError = 1
	exitBlock = 2

	maxInputBytes = 32 << 20
)

type hookInput struct {
	ToolInput toolInput `json:"tool_input"`
}

type toolInput struct {
	Command string `json:"command"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stderr))
}

func run(args []string, stdin io.Reader, stderr io.Writer) (code int) {
	// A Go panic exits with status 2, which Claude Code reads as "block", so
	// a bug in the check would block every Bash call.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "claude-hook: internal error: %v\n", r)
			code = exitError
		}
	}()
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: claude-hook < hook-input.json")
		return exitError
	}
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(stdin, maxInputBytes)).Decode(&in); err != nil {
		fmt.Fprintf(stderr, "claude-hook: reading the hook input: %v\n", err)
		return exitError
	}
	violations, err := gitguard.Check(in.ToolInput.Command)
	if err != nil {
		fmt.Fprintf(stderr, "claude-hook: reading the command: %v\n", err)
		return exitError
	}
	if len(violations) == 0 {
		return exitAllow
	}
	for _, v := range violations {
		fmt.Fprintf(stderr, "%s: %s\n", safetext.Text(v.Command), v.Reason)
	}
	return exitBlock
}
