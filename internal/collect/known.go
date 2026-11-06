package collect

import (
	"slices"
	"unicode/utf8"
)

// knownSet finds every occurrence of a set of values in one pass over a
// text, in time linear in the text whatever the values are (Aho-Corasick).
// State 0 is the root. The edges of state s are labels[first[s]:first[s+1]]
// and targets[first[s]:first[s+1]], sorted by label, and the root also has
// a dense table, because a text spends most of its bytes there.
type knownSet struct {
	root    [256]int
	first   []int
	labels  []byte
	targets []int
	// fail is the state of the longest proper suffix that is also a prefix
	// of a value.
	fail []int
	// longest is the length of the longest value that ends in the state, or
	// 0 when none does.
	longest []int
	// maxLen is the length of the longest value.
	maxLen int
}

// newKnownSet builds a knownSet from sorted, distinct, non-empty values. It
// holds about 33 bytes per byte of the values.
func newKnownSet(values []string) *knownSet {
	size, maxLen := 1, 0
	for _, v := range values {
		size += len(v)
		maxLen = max(maxLen, len(v))
	}
	// A trie of the values. Because they are sorted, each state's children
	// appear in label order, and only its last child can be extended again.
	parent := make([]int, 1, size)
	label := make([]byte, 1, size)
	depth := make([]int, 1, size)
	lastChild := make([]int, 1, size)
	ends := make([]bool, 1, size)
	parent[0], lastChild[0] = -1, -1
	for _, v := range values {
		s := 0
		for i := range len(v) {
			if t := lastChild[s]; t >= 0 && label[t] == v[i] {
				s = t
				continue
			}
			t := len(parent)
			parent = append(parent, s)
			label = append(label, v[i])
			depth = append(depth, depth[s]+1)
			lastChild = append(lastChild, -1)
			ends = append(ends, false)
			lastChild[s] = t
			s = t
		}
		ends[s] = true
	}
	n := len(parent)
	k := &knownSet{
		maxLen:  maxLen,
		first:   make([]int, n+1),
		labels:  make([]byte, n),
		targets: make([]int, n),
		fail:    make([]int, n),
		longest: make([]int, n),
	}
	for t := 1; t < n; t++ {
		k.first[parent[t]+1]++
	}
	for s := range n {
		k.first[s+1] += k.first[s]
	}
	next := slices.Clone(k.first[:n])
	for t := 1; t < n; t++ {
		e := next[parent[t]]
		k.labels[e], k.targets[e] = label[t], t
		next[parent[t]]++
	}
	for e := k.first[0]; e < k.first[1]; e++ {
		k.root[k.labels[e]] = k.targets[e]
	}
	queue := make([]int, 1, n)
	for q := 0; q < len(queue); q++ {
		s := queue[q]
		for e := k.first[s]; e < k.first[s+1]; e++ {
			t := k.targets[e]
			if s != 0 {
				k.fail[t] = k.step(k.fail[s], k.labels[e])
			}
			if ends[t] {
				k.longest[t] = depth[t]
			} else {
				k.longest[t] = k.longest[k.fail[t]]
			}
			queue = append(queue, t)
		}
	}
	return k
}

// step returns the state after reading c in state s.
func (k *knownSet) step(s int, c byte) int {
	for s != 0 {
		lo, hi := k.first[s], k.first[s+1]
		for lo < hi {
			m := lo + (hi-lo)/2
			switch l := k.labels[m]; {
			case l == c:
				return k.targets[m]
			case l < c:
				lo = m + 1
			default:
				hi = m
			}
		}
		s = k.fail[s]
	}
	return k.root[c]
}

// contains reports whether text holds any of the values.
func (k *knownSet) contains(text string) bool {
	s := 0
	for i := range len(text) {
		if s = k.step(s, text[i]); k.longest[s] > 0 {
			return true
		}
	}
	return false
}

// mark marks in m every byte of text that an occurrence of a value covers,
// and reports whether there is one.
//
// Occurrences can overlap and nest, so it merges them into runs, and marks a
// run once no later occurrence can reach it: one that ends after byte i
// starts after i+1-maxLen. So it marks each byte once, and holds only the
// runs of the last maxLen bytes, whatever the length of the text.
func (k *knownSet) mark(text string, m *mask) bool {
	type span struct{ start, end int }
	// runs[head:] are the runs that a later match can still reach.
	var runs []span
	head := 0
	s := 0
	hit := false
	for i := range len(text) {
		s = k.step(s, text[i])
		if l := k.longest[s]; l > 0 {
			hit = true
			start, end := i+1-l, i+1
			for len(runs) > head && runs[len(runs)-1].end >= start {
				start = min(start, runs[len(runs)-1].start)
				runs = runs[:len(runs)-1]
			}
			runs = append(runs, span{start, end})
		}
		for len(runs) > head && runs[head].end < i+2-k.maxLen {
			m.mark(runs[head].start, runs[head].end)
			head++
		}
		if head > 64 && head > len(runs)/2 {
			runs = runs[:copy(runs, runs[head:])]
			head = 0
		}
	}
	for _, r := range runs[head:] {
		m.mark(r.start, r.end)
	}
	return hit
}

// fragmentBytes is the length of the runs of bytes of a credential value
// that fragmentSet holds. The Dockerfile frontend expands ${VAR#pattern},
// ${VAR%pattern} and ${VAR/pattern/text} (processDollar in
// frontend/dockerfile/shell/lex.go at BuildKit v0.34.0), so a step
// definition can hold a credential with a few bytes cut off, which matching
// whole values does not find. A shorter run would match ordinary text, such
// as a path, too often; one of 8 bytes leaves at most 7 bytes of a
// credential unseen.
const fragmentBytes = 8

// fragmentSet holds every run of fragmentBytes bytes of a set of values,
// each packed into a uint64. It holds about 36 bytes per byte of the values,
// and finds a run in a text in one pass over it. A Go map keeps an input
// that the author of a pull request chose from making the lookups slow.
type fragmentSet struct {
	runs map[uint64]struct{}
	// firstTwo has a bit for the first two bytes of each run, which rules
	// out most windows of a text without a map lookup.
	firstTwo [1 << 16 / 64]uint64
}

// newFragmentSet returns the runs of the values, or nil when no value is as
// long as a run.
func newFragmentSet(values []string) *fragmentSet {
	n := 0
	for _, v := range values {
		n += max(len(v)-fragmentBytes+1, 0)
	}
	if n == 0 {
		return nil
	}
	f := &fragmentSet{runs: make(map[uint64]struct{}, n)}
	add := func(g uint64) {
		f.runs[g] = struct{}{}
		f.firstTwo[g&0xffff>>6] |= 1 << (g & 63)
	}
	for _, v := range values {
		if len(v) < fragmentBytes {
			continue
		}
		g := firstFragment(v)
		add(g)
		for i := fragmentBytes; i < len(v); i++ {
			g = g>>8 | uint64(v[i])<<56
			add(g)
		}
	}
	return f
}

// has reports whether g is a run of f.
func (f *fragmentSet) has(g uint64) bool {
	if f.firstTwo[g&0xffff>>6]&(1<<(g&63)) == 0 {
		return false
	}
	_, ok := f.runs[g]
	return ok
}

// firstFragment packs the first fragmentBytes bytes of s, which has at
// least that many.
func firstFragment[T string | []byte](s T) uint64 {
	var g uint64
	for i := range fragmentBytes {
		g |= uint64(s[i]) << (8 * i)
	}
	return g
}

// mark marks in m every run of fragmentBytes bytes of text that f holds,
// and reports whether there is one. A run can start or end inside a UTF-8
// character, which a whole value cannot, so each is widened to whole
// characters: removing it then leaves valid UTF-8 valid.
func (f *fragmentSet) mark(text string, m *mask) bool {
	if f == nil || len(text) < fragmentBytes {
		return false
	}
	hit := false
	g := firstFragment(text)
	for i := fragmentBytes; ; i++ {
		if f.has(g) {
			start, end := i-fragmentBytes, i
			for k := 1; k < utf8.UTFMax && start > 0 && !utf8.RuneStart(text[start]); k++ {
				start--
			}
			for k := 1; k < utf8.UTFMax && end < len(text) && !utf8.RuneStart(text[end]); k++ {
				end++
			}
			m.mark(start, end)
			hit = true
		}
		if i == len(text) {
			return hit
		}
		g = g>>8 | uint64(text[i])<<56
	}
}

// sharesFragment reports whether s holds a run of fragmentBytes bytes of a
// value of f.
func sharesFragment[T string | []byte](f *fragmentSet, s T) bool {
	if f == nil || len(s) < fragmentBytes {
		return false
	}
	g := firstFragment(s)
	for i := fragmentBytes; ; i++ {
		if f.has(g) {
			return true
		}
		if i == len(s) {
			return false
		}
		g = g>>8 | uint64(s[i])<<56
	}
}
