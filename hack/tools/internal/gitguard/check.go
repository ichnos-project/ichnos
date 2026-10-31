// Package gitguard finds git commands that an AI agent must not run in
// Ichnos: commands that sign off on the human author's behalf, add AI
// trailers, or turn off git hooks. It reads shell lines well enough to catch
// mistakes. It is not a shell interpreter and not a security boundary: a
// commit message read from a file, or a command built at run time, is not
// seen.
package gitguard

import (
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	reasonSignoff  = "signs off on the human author's behalf; only the human can certify the DCO, so commit without it and ask them to run: ! git rebase --signoff main"
	reasonTrailer  = "adds a Signed-off-by or Co-authored-by trailer; only the human author can certify the DCO"
	reasonNoVerify = "skips git hooks; fix what the hook reports instead"
	reasonHooks    = "changes core.hooksPath, which turns off git hooks"

	// maxDepth limits how deep commands nested in sh -c, aliases and
	// rebase --exec are followed.
	maxDepth = 3

	maxCommandLength = 160
)

// Violation is a git command an agent must not run, and why.
type Violation struct {
	Command string
	Reason  string
}

// Check returns the violations in a shell command line. It returns an error
// when the line cannot be split into words, such as for an unterminated
// quote, which a shell would reject too.
func Check(line string) ([]Violation, error) {
	return check(line, 0)
}

func check(line string, depth int) ([]Violation, error) {
	if depth > maxDepth {
		return nil, nil
	}
	cmds, err := split(line)
	if err != nil {
		return nil, err
	}
	var violations []Violation
	for _, c := range cmds {
		v, err := checkCommand(c, depth)
		if err != nil {
			return nil, err
		}
		violations = append(violations, v...)
	}
	return violations, nil
}

func checkCommand(c *command, depth int) ([]Violation, error) {
	words := skipPrefixes(c.words)
	if len(words) == 0 {
		return nil, nil
	}
	name := path.Base(words[0])
	if name == "git" {
		reasons, err := checkGit(words[1:], append(slices.Clip(c.inputs), pipedText(c)...), depth)
		if err != nil || len(reasons) == 0 {
			return nil, err
		}
		cmd := shorten(strings.Join(words, " "))
		violations := make([]Violation, 0, len(reasons))
		for _, r := range reasons {
			violations = append(violations, Violation{Command: cmd, Reason: r})
		}
		return violations, nil
	}
	if isShell(name) {
		if script, ok := shellScript(words[1:]); ok {
			return check(script, depth+1)
		}
	}
	return nil, nil
}

// checkGit returns the reasons why git with these arguments is not allowed.
func checkGit(args, inputs []string, depth int) ([]string, error) {
	var reasons []string
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch opt := args[i]; {
		case opt == "-c" && i+1 < len(args):
			i++
			key, value, _ := strings.Cut(args[i], "=")
			r, err := checkSetting(key, value, true, depth)
			if err != nil {
				return nil, err
			}
			reasons = append(reasons, r...)
		case strings.HasPrefix(opt, "--config-env="):
			key, _, _ := strings.Cut(strings.TrimPrefix(opt, "--config-env="), "=")
			r, err := checkSetting(key, "", true, depth)
			if err != nil {
				return nil, err
			}
			reasons = append(reasons, r...)
		case slices.Contains([]string{"-C", "--git-dir", "--work-tree", "--namespace", "--super-prefix"}, opt):
			i++
		}
	}
	if i >= len(args) {
		return reasons, nil
	}
	sub, rest := args[i], args[i+1:]
	if sub == "config" {
		key, value, hasValue := configSetting(rest)
		r, err := checkSetting(key, value, hasValue, depth)
		if err != nil {
			return nil, err
		}
		reasons = append(reasons, r...)
	}
	if s, ok := specFor(sub); ok {
		r, err := s.check(rest, inputs, depth)
		if err != nil {
			return nil, err
		}
		reasons = append(reasons, r...)
	}
	slices.Sort(reasons)
	return slices.Compact(reasons), nil
}

// checkSetting checks a git configuration setting: core.hooksPath must not
// be set, and an alias must not expand to a command that is not allowed.
func checkSetting(key, value string, hasValue bool, depth int) ([]string, error) {
	switch {
	case !hasValue:
		return nil, nil
	case strings.EqualFold(key, "core.hooksPath"):
		return []string{reasonHooks}, nil
	case len(key) > len("alias.") && strings.EqualFold(key[:len("alias.")], "alias."):
		script, isShell := strings.CutPrefix(value, "!")
		if !isShell {
			script = "git " + value
		}
		violations, err := check(script, depth+1)
		if err != nil {
			return nil, err
		}
		reasons := make([]string, 0, len(violations))
		for _, v := range violations {
			reasons = append(reasons, "defines an alias that "+v.Reason)
		}
		return reasons, nil
	}
	return nil, nil
}

// configSetting returns the key and value that a git config command sets.
// Commands that only read, list or remove settings set nothing.
func configSetting(args []string) (key, value string, hasValue bool) {
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case isConfigRead(a):
			return "", "", false
		case slices.Contains([]string{"-f", "--file", "--blob", "--type", "--default", "--comment", "--value"}, a):
			i++
		case strings.HasPrefix(a, "-"):
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) > 0 {
		switch positional[0] {
		case "set":
			positional = positional[1:]
		case "get", "list", "unset", "remove-section", "rename-section", "edit":
			return "", "", false
		}
	}
	switch len(positional) {
	case 0:
		return "", "", false
	case 1:
		return positional[0], "", false
	}
	return positional[0], positional[1], true
}

// pipedText returns what a command reads from the command before it in a
// pipeline: that command's heredocs and here-strings, and its arguments with
// \n read as a line break, as printf and echo -e do.
func pipedText(c *command) []string {
	if c.stdin == nil {
		return nil
	}
	text := slices.Clone(c.stdin.inputs)
	for _, w := range c.stdin.words[min(1, len(c.stdin.words)):] {
		text = append(text, strings.ReplaceAll(w, `\n`, "\n"))
	}
	return text
}

// skipPrefixes drops what comes before the command name: variable
// assignments, shell keywords and wrappers such as env, sudo and timeout.
func skipPrefixes(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		if isAssignment(w) || isKeyword(w) {
			words = words[1:]
			continue
		}
		switch path.Base(w) {
		case "env":
			words = skipOptions(words[1:], "uCS")
		case "command", "exec", "nohup", "builtin":
			words = skipOptions(words[1:], "a")
		case "nice":
			words = skipOptions(words[1:], "n")
		case "ionice":
			words = skipOptions(words[1:], "cnp")
		case "stdbuf":
			words = skipOptions(words[1:], "ioe")
		case "time":
			words = skipOptions(words[1:], "f")
		case "timeout":
			words = skipOptions(words[1:], "sk")
			if len(words) > 0 {
				words = words[1:]
			}
		case "sudo", "doas":
			words = skipOptions(words[1:], "ugCDhpr")
		case "xargs":
			words = skipOptions(words[1:], "IdELnPsa")
		default:
			return words
		}
	}
	return words
}

// skipOptions drops leading options. The short options in valued take the
// next word as their value.
func skipOptions(words []string, valued string) []string {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") && words[0] != "-" {
		opt := words[0]
		words = words[1:]
		if opt == "--" {
			break
		}
		if len(opt) == 2 && strings.IndexByte(valued, opt[1]) >= 0 && len(words) > 0 {
			words = words[1:]
		}
	}
	return words
}

// shellScript returns the script passed to a shell with -c, skipping the
// shell's other options.
func shellScript(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "+o" || a == "-O" || a == "+O":
			i++
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && strings.Contains(a, "c"):
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+"):
		default:
			return "", false
		}
	}
	return "", false
}

func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" || isDigit(name[0]) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c != '_' && !isDigit(c) && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func isKeyword(w string) bool {
	switch w {
	case "{", "}", "!", "if", "then", "else", "elif", "do", "while", "until":
		return true
	}
	return false
}

func isConfigRead(opt string) bool {
	switch opt {
	case "--get", "--get-all", "--get-regexp", "--get-urlmatch", "--get-color", "--get-colorbool",
		"-l", "--list", "--unset", "--unset-all", "--remove-section", "--rename-section", "-e", "--edit":
		return true
	}
	return false
}

func isShell(name string) bool {
	switch name {
	case "sh", "bash", "zsh", "dash", "ksh":
		return true
	}
	return false
}

// shorten puts a command on one line and cuts it to maxCommandLength bytes,
// on a character boundary.
func shorten(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	if len(s) <= maxCommandLength {
		return s
	}
	cut := maxCommandLength
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
