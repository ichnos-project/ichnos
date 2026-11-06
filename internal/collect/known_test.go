package collect

import (
	"slices"
	"strings"
	"testing"
)

// naiveReplace is the reference for marking known values and applying the
// mask: it marks every byte that any occurrence of any value covers, and
// replaces each run of marked bytes with withheldValue.
func naiveReplace(values []string, text string) string {
	return naiveApply(text, naiveCover(values, text))
}

// naiveCover reports, for each byte of text, whether an occurrence of one of
// the values covers it.
func naiveCover(values []string, text string) []bool {
	covered := make([]bool, len(text))
	for _, v := range values {
		for i := 0; i+len(v) <= len(text); i++ {
			if text[i:i+len(v)] == v {
				for j := i; j < i+len(v); j++ {
					covered[j] = true
				}
			}
		}
	}
	return covered
}

// naiveApply replaces each run of covered bytes of text with withheldValue.
func naiveApply(text string, covered []bool) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		if !covered[i] {
			b.WriteByte(text[i])
			i++
			continue
		}
		b.WriteString(withheldValue)
		for i < len(text) && covered[i] {
			i++
		}
	}
	return b.String()
}

func knownSetOf(values ...string) *knownSet {
	values = slices.DeleteFunc(slices.Clone(values), func(v string) bool { return v == "" })
	slices.Sort(values)
	return newKnownSet(slices.Compact(values))
}

func TestKnownSetMark(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values []string
		text   string
		want   string
	}{
		{"no match", []string{"abcd"}, "xyz", "xyz"},
		{"one match", []string{"abcd"}, "xabcdy", "x***y"},
		{"overlapping values", []string{"abcd", "cdefgh"}, "abcdefgh", "***"},
		{"value inside another", []string{"bc", "abcd"}, "abcdbc", "***"},
		{"touching matches", []string{"ab", "cd"}, "abcd", "***"},
		{"repeated value", []string{"aa"}, "aaaaa", "***"},
		{"shared prefixes", []string{"aaab", "aaac", "aab"}, "aaaab aaac", "a*** ***"},
		{"every byte value", []string{"\\x00\\xff", "\\xff\\x00"}, "a\\x00\\xff\\x00b", "a***b"},
		{"empty text", []string{"abcd"}, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			k := knownSetOf(tt.values...)
			got, hit := redactWith(k.mark, tt.text)
			if got != tt.want || hit != (got != tt.text) {
				t.Errorf("mark(%q) = %q, %v, want %q", tt.text, got, hit, tt.want)
			}
			if k.contains(tt.text) != hit {
				t.Errorf("contains(%q) = %v, want %v", tt.text, !hit, hit)
			}
		})
	}
}

func FuzzKnownSet(f *testing.F) {
	f.Add("abcd", "cdefgh", "xx", "abcdefgh abcd cdefghi")
	f.Add("aaaa", "aaab", "aab", "aaaaaaab")
	f.Add("a", "ab", "b", "abab")
	f.Fuzz(func(t *testing.T, v1, v2, v3, text string) {
		values := []string{v1, v2, v3}
		k := knownSetOf(values...)
		got, hit := redactWith(k.mark, text)
		if want := naiveReplace(values, text); got != want {
			t.Fatalf("mark(%q) with %q = %q, want %q", text, values, got, want)
		}
		if k.contains(text) != hit {
			t.Fatalf("contains(%q) with %q = %v, but mark reported %v", text, values, !hit, hit)
		}
	})
}

func TestFragmentSet(t *testing.T) {
	f := newFragmentSet([]string{"abcdefghij", "short", "12345678"})
	for text, want := range map[string]bool{
		"abcdefgh": true, "xxbcdefghiyy": true, "cdefghij": true, "x12345678": true, "abcdefghij": true,
		"abcdefg": false, "bcdefgh": false, "abcdefgX": false, "short-and-longer": false, "": false, "1234567": false,
	} {
		if got := sharesFragment(f, text); got != want {
			t.Errorf("sharesFragment(%q) = %v, want %v", text, got, want)
		}
		if got := sharesFragment(f, []byte(text)); got != want {
			t.Errorf("sharesFragment([]byte(%q)) = %v, want %v", text, got, want)
		}
	}
	if newFragmentSet([]string{"short", "1234567"}) != nil || sharesFragment((*fragmentSet)(nil), "abcdefgh") {
		t.Error("values shorter than a run gave runs")
	}
	if n := testing.AllocsPerRun(100, func() { sharesFragment(f, "services/api/main.go") }); n != 0 {
		t.Errorf("sharesFragment() allocates %v times, want 0", n)
	}
}
