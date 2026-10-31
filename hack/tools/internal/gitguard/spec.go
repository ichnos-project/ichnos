package gitguard

import (
	"slices"
	"strings"
)

// spec describes the options of a git subcommand that matter here. Short
// options are listed as strings of option letters.
type spec struct {
	signoffShort  string
	noVerifyShort string
	valueShort    string
	attachedShort string
	messageShort  string
	execShort     string
	signoff       bool
	noVerify      bool
	trailer       bool
	valueLong     []string
	messageLong   []string
	execLong      []string
}

// specFor returns the spec of a git subcommand that can create commits or
// skip hooks.
func specFor(sub string) (spec, bool) {
	switch sub {
	case "commit":
		return spec{
			signoffShort: "s", noVerifyShort: "n", valueShort: "FcCt", attachedShort: "uS", messageShort: "m",
			signoff: true, noVerify: true, trailer: true,
			valueLong:   []string{"--author", "--cleanup", "--date", "--file", "--fixup", "--pathspec-from-file", "--reedit-message", "--reuse-message", "--squash", "--template"},
			messageLong: []string{"--message"},
		}, true
	case "am":
		return spec{signoffShort: "s", attachedShort: "Cp", signoff: true, noVerify: true}, true
	case "cherry-pick", "revert":
		return spec{signoffShort: "s", valueShort: "mX", signoff: true}, true
	case "format-patch":
		return spec{signoffShort: "s", valueShort: "o", signoff: true, valueLong: []string{"--output-directory", "--subject-prefix", "--in-reply-to", "--to", "--cc", "--from"}}, true
	case "rebase":
		return spec{
			valueShort: "sX", attachedShort: "C", execShort: "x",
			signoff: true, noVerify: true,
			valueLong: []string{"--strategy", "--strategy-option", "--onto"},
			execLong:  []string{"--exec"},
		}, true
	case "merge":
		return spec{
			valueShort: "sXF", messageShort: "m",
			signoff: true, noVerify: true,
			valueLong:   []string{"--strategy", "--strategy-option", "--file", "--into-name", "--cleanup"},
			messageLong: []string{"--message"},
		}, true
	case "pull":
		return spec{valueShort: "sX", signoff: true, noVerify: true, valueLong: []string{"--strategy", "--strategy-option"}}, true
	case "push":
		return spec{valueShort: "o", noVerify: true, valueLong: []string{"--push-option", "--repo", "--receive-pack", "--exec"}}, true
	}
	return spec{}, false
}

// check returns the reasons why a subcommand with these arguments is not
// allowed. inputs are heredocs and here-strings read by the command.
func (s spec) check(args, inputs []string, depth int) ([]string, error) {
	var reasons []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			i = len(args)
		case strings.HasPrefix(a, "--"):
			r, consumed, err := s.checkLong(a, args[i+1:], depth)
			if err != nil {
				return nil, err
			}
			reasons = append(reasons, r...)
			i += consumed
		case len(a) > 1 && a[0] == '-':
			r, consumed, err := s.checkShort(a[1:], args[i+1:], depth)
			if err != nil {
				return nil, err
			}
			reasons = append(reasons, r...)
			i += consumed
		}
	}
	if slices.ContainsFunc(inputs, hasTrailerLine) {
		reasons = append(reasons, reasonTrailer)
	}
	return reasons, nil
}

// checkLong checks one long option. It returns how many of the following
// words the option takes as its value.
func (s spec) checkLong(arg string, following []string, depth int) ([]string, int, error) {
	name, value, hasValue := strings.Cut(arg, "=")
	consumed := 0
	takeValue := func() {
		if !hasValue && len(following) > 0 {
			value, consumed = following[0], 1
		}
	}
	switch {
	case s.signoff && abbreviates(name, "--signoff", "--si"):
		return []string{reasonSignoff}, 0, nil
	case s.noVerify && abbreviates(name, "--no-verify", "--no-ve"):
		return []string{reasonNoVerify}, 0, nil
	case s.trailer && abbreviates(name, "--trailer", "--tr"):
		takeValue()
		if isSignatureTrailer(value) {
			return []string{reasonTrailer}, consumed, nil
		}
	case matchesAny(name, s.messageLong):
		takeValue()
		if hasTrailerLine(value) {
			return []string{reasonTrailer}, consumed, nil
		}
	case matchesAny(name, s.execLong):
		takeValue()
		r, err := commandReasons(value, depth)
		return r, consumed, err
	case matchesAny(name, s.valueLong):
		takeValue()
	}
	return nil, consumed, nil
}

// checkShort checks a group of short options such as -sm. It returns how many
// of the following words the group takes as a value.
func (s spec) checkShort(group string, following []string, depth int) ([]string, int, error) {
	var reasons []string
	for j := 0; j < len(group); j++ {
		f := group[j]
		value, consumed := group[j+1:], 0
		if value == "" && len(following) > 0 {
			value, consumed = following[0], 1
		}
		switch {
		case strings.IndexByte(s.signoffShort, f) >= 0:
			reasons = append(reasons, reasonSignoff)
		case strings.IndexByte(s.noVerifyShort, f) >= 0:
			reasons = append(reasons, reasonNoVerify)
		case strings.IndexByte(s.messageShort, f) >= 0:
			if hasTrailerLine(value) {
				reasons = append(reasons, reasonTrailer)
			}
			return reasons, consumed, nil
		case strings.IndexByte(s.execShort, f) >= 0:
			r, err := commandReasons(value, depth)
			return append(reasons, r...), consumed, err
		case strings.IndexByte(s.valueShort, f) >= 0:
			return reasons, consumed, nil
		case strings.IndexByte(s.attachedShort, f) >= 0:
			return reasons, 0, nil
		}
	}
	return reasons, 0, nil
}

// commandReasons checks a command that git runs, such as rebase --exec.
func commandReasons(script string, depth int) ([]string, error) {
	violations, err := check(script, depth+1)
	if err != nil {
		return nil, err
	}
	reasons := make([]string, 0, len(violations))
	for _, v := range violations {
		reasons = append(reasons, "runs a command that "+v.Reason)
	}
	return reasons, nil
}

// abbreviates reports whether name is option or an abbreviation of it at
// least as long as shortest. Git accepts any unique prefix of a long option.
func abbreviates(name, option, shortest string) bool {
	return len(name) >= len(shortest) && strings.HasPrefix(option, name)
}

func matchesAny(name string, options []string) bool {
	return slices.ContainsFunc(options, func(option string) bool {
		return abbreviates(name, option, "--"+option[2:min(len(option), 5)])
	})
}

// isSignatureTrailer reports whether a --trailer value such as
// "Signed-off-by: A <a@example.com>" or "Co-authored-by=B" adds a sign-off
// or co-author.
func isSignatureTrailer(value string) bool {
	key, _, _ := strings.Cut(value, ":")
	key, _, _ = strings.Cut(key, "=")
	return isSignatureKey(key)
}

// hasTrailerLine reports whether a commit message has a Signed-off-by or
// Co-authored-by line.
func hasTrailerLine(message string) bool {
	for line := range strings.SplitSeq(message, "\n") {
		key, _, ok := strings.Cut(line, ":")
		if ok && isSignatureKey(key) {
			return true
		}
	}
	return false
}

func isSignatureKey(key string) bool {
	key = strings.TrimSpace(key)
	return strings.EqualFold(key, "Signed-off-by") || strings.EqualFold(key, "Co-authored-by")
}
