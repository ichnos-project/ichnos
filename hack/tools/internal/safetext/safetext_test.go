package safetext_test

import (
	"testing"

	"github.com/ichnos-project/ichnos/hack/tools/internal/safetext"
)

func TestText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"removed unused import", "removed unused import"},
		{"a\x1b[2K\x1b[1Ab", "a[2K[1Ab"},
		{"x\n::error::y", "x::error::y"},
		{"tab\there", "tab here"},
		{"safe\u202eevil", "safeevil"},
		{"café", "café"},
	}
	for _, tt := range tests {
		if got := safetext.Text(tt.in); got != tt.want {
			t.Errorf("Text(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
