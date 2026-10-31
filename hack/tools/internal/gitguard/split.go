package gitguard

import (
	"errors"
	"strings"
)

var (
	// errUnterminated is returned for a line that a shell would reject
	// because a quote or a substitution is never closed.
	errUnterminated = errors.New("unterminated quote or substitution")
	// errTooDeep is returned for substitutions nested deeper than
	// maxNesting, which no real command needs.
	errTooDeep = errors.New("command substitutions nested too deeply")
)

const maxNesting = 16

// command is one simple command from a shell line: its words after quote
// removal, the text of any heredocs and here-strings it reads, and the
// command it reads from in a pipeline.
type command struct {
	words  []string
	inputs []string
	stdin  *command
}

type heredoc struct {
	delim     string
	stripTabs bool
	owner     *command
}

type lexer struct {
	src         string
	pos         int
	nesting     int
	cmds        []*command
	cur         *command
	pending     []heredoc
	lastWordEnd int
}

// split divides a shell line into simple commands, including the commands
// inside $(...) and `...` substitutions. It handles quotes, escapes, line
// continuations, separators, pipelines, comments, redirections, heredocs and
// here-strings. It does not expand variables, and a substitution stays in its
// word as literal text.
func split(src string) ([]*command, error) {
	l := &lexer{src: src, cur: &command{}, lastWordEnd: -1}
	if err := l.run(false); err != nil {
		return nil, err
	}
	return l.cmds, nil
}

// run reads commands to the end of the input or, inside a substitution, up
// to the ")" that closes it.
func (l *lexer) run(inSubstitution bool) error {
	parens := 0
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t':
			l.pos++
		case c == '\\' && l.peek(1) == '\n':
			l.pos += 2
		case c == '\n':
			l.pos++
			l.endCommand(false)
			l.readHeredocs()
		case c == '#':
			l.skipComment()
		case c == '&' && l.peek(1) == '>', c == '<' || c == '>':
			if err := l.redirect(); err != nil {
				return err
			}
		case c == ')' && inSubstitution && parens == 0:
			l.endCommand(false)
			return nil
		case c == '(' || c == ')':
			if c == '(' {
				parens++
			} else {
				parens--
			}
			l.pos++
			l.endCommand(false)
		case (c == '|' || c == '&') && l.peek(1) == c:
			l.pos += 2
			l.endCommand(false)
		case c == '|':
			l.pos++
			l.endCommand(true)
		case c == ';' || c == '&':
			l.pos++
			l.endCommand(false)
		default:
			if err := l.addWord(); err != nil {
				return err
			}
		}
	}
	if inSubstitution {
		return errUnterminated
	}
	l.endCommand(false)
	l.readHeredocs()
	return nil
}

func (l *lexer) peek(n int) byte {
	if l.pos+n < len(l.src) {
		return l.src[l.pos+n]
	}
	return 0
}

func (l *lexer) addWord() error {
	w, err := l.word()
	if err != nil {
		return err
	}
	l.cur.words = append(l.cur.words, w)
	l.lastWordEnd = l.pos
	return nil
}

// endCommand finishes the current command. After a pipe, the next command
// reads from it.
func (l *lexer) endCommand(pipe bool) {
	var prev *command
	if len(l.cur.words) > 0 || len(l.cur.inputs) > 0 {
		l.cmds = append(l.cmds, l.cur)
		prev = l.cur
	}
	l.cur = &command{}
	if pipe {
		l.cur.stdin = prev
	}
}

func (l *lexer) skipComment() {
	if end := strings.IndexByte(l.src[l.pos:], '\n'); end >= 0 {
		l.pos += end
		return
	}
	l.pos = len(l.src)
}

// word reads one word and removes its quotes and escapes.
func (l *lexer) word() (string, error) {
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case strings.IndexByte(" \t\n<>;&|()", c) >= 0:
			return b.String(), nil
		case c == '\\':
			if l.pos+1 < len(l.src) && l.src[l.pos+1] != '\n' {
				b.WriteByte(l.src[l.pos+1])
			}
			l.pos += 2
		case c == '\'':
			if err := l.singleQuoted(&b, l.pos+1, false); err != nil {
				return "", err
			}
		case c == '$' && l.peek(1) == '\'':
			if err := l.singleQuoted(&b, l.pos+2, true); err != nil {
				return "", err
			}
		case c == '"':
			if err := l.doubleQuoted(&b); err != nil {
				return "", err
			}
		case c == '$' && l.peek(1) == '(', c == '`':
			if err := l.substitution(&b); err != nil {
				return "", err
			}
		default:
			b.WriteByte(c)
			l.pos++
		}
	}
	return b.String(), nil
}

// singleQuoted reads a '...' string that starts at from. A $'...' string
// also decodes backslash escapes, so its \n becomes a line break.
func (l *lexer) singleQuoted(b *strings.Builder, from int, escapes bool) error {
	for i := from; i < len(l.src); i++ {
		switch {
		case escapes && l.src[i] == '\\' && i+1 < len(l.src):
			i++
			b.WriteByte(ansiEscape(l.src[i]))
		case l.src[i] == '\'':
			l.pos = i + 1
			return nil
		default:
			b.WriteByte(l.src[i])
		}
	}
	return errUnterminated
}

func (l *lexer) doubleQuoted(b *strings.Builder) error {
	l.pos++
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == '\\' && l.pos+1 < len(l.src) && strings.IndexByte("\"\\$`\n", l.src[l.pos+1]) >= 0:
			if l.src[l.pos+1] != '\n' {
				b.WriteByte(l.src[l.pos+1])
			}
			l.pos += 2
		case c == '"':
			l.pos++
			return nil
		case c == '$' && l.peek(1) == '(', c == '`':
			if err := l.substitution(b); err != nil {
				return err
			}
		default:
			b.WriteByte(c)
			l.pos++
		}
	}
	return errUnterminated
}

// substitution reads a $(...) or `...` substitution. Its text stays in the
// word, and the commands inside it are added to the commands of the line.
func (l *lexer) substitution(b *strings.Builder) error {
	if l.nesting >= maxNesting {
		return errTooDeep
	}
	start := l.pos
	inner := &lexer{src: l.src, nesting: l.nesting + 1, cur: &command{}, lastWordEnd: -1}
	if l.src[l.pos] == '`' {
		end := strings.IndexByte(l.src[l.pos+1:], '`')
		if end < 0 {
			return errUnterminated
		}
		inner.src = l.src[l.pos+1 : l.pos+1+end]
		if err := inner.run(false); err != nil {
			return err
		}
		l.pos += end + 2
	} else {
		inner.pos = l.pos + 2
		if err := inner.run(true); err != nil {
			return err
		}
		l.pos = inner.pos + 1
	}
	l.cmds = append(l.cmds, inner.cmds...)
	b.WriteString(l.src[start:l.pos])
	return nil
}

// redirect reads a redirection, heredoc or here-string. Redirection targets
// are dropped; heredoc and here-string text is kept as command input. A file
// descriptor number written against the operator, as in 2>/dev/null, is not
// a word of the command.
func (l *lexer) redirect() error {
	if n := len(l.cur.words); n > 0 && l.lastWordEnd == l.pos && isNumber(l.cur.words[n-1]) {
		l.cur.words = l.cur.words[:n-1]
	}
	rest := l.src[l.pos:]
	switch {
	case strings.HasPrefix(rest, "<<<"):
		l.pos += 3
		text, err := l.nextWord()
		l.cur.inputs = append(l.cur.inputs, text)
		return err
	case strings.HasPrefix(rest, "<<"):
		l.pos += 2
		stripTabs := l.peek(0) == '-'
		if stripTabs {
			l.pos++
		}
		delim, err := l.nextWord()
		l.pending = append(l.pending, heredoc{delim: delim, stripTabs: stripTabs, owner: l.cur})
		return err
	}
	for l.pos < len(l.src) && strings.IndexByte("<>&|", l.src[l.pos]) >= 0 {
		l.pos++
	}
	if l.src[l.pos-1] == '&' && (isDigit(l.peek(0)) || l.peek(0) == '-') {
		for l.pos < len(l.src) && (isDigit(l.src[l.pos]) || l.src[l.pos] == '-') {
			l.pos++
		}
		return nil
	}
	_, err := l.nextWord()
	return err
}

func (l *lexer) nextWord() (string, error) {
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t') {
		l.pos++
	}
	return l.word()
}

// readHeredocs reads the bodies of the heredocs started on the line that just
// ended. A body runs to its delimiter line, or to the end of the input.
func (l *lexer) readHeredocs() {
	for _, h := range l.pending {
		var body []string
		for l.pos < len(l.src) {
			line := l.src[l.pos:]
			if end := strings.IndexByte(line, '\n'); end >= 0 {
				line = line[:end]
				l.pos += end + 1
			} else {
				l.pos = len(l.src)
			}
			delim := line
			if h.stripTabs {
				delim = strings.TrimLeft(line, "\t")
			}
			if delim == h.delim {
				break
			}
			body = append(body, line)
		}
		h.owner.inputs = append(h.owner.inputs, strings.Join(body, "\n"))
	}
	l.pending = nil
}

func ansiEscape(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'r':
		return '\r'
	}
	return c
}

func isNumber(w string) bool {
	if w == "" {
		return false
	}
	for i := 0; i < len(w); i++ {
		if !isDigit(w[i]) {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool {
	return '0' <= c && c <= '9'
}
