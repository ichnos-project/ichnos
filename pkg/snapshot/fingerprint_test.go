package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFingerprintText(t *testing.T) {
	var f Fingerprint
	for i := range f {
		f[i] = byte(i * 7)
	}
	text, err := f.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() error = %v", err)
	}
	if want := "00070e151c232a31383f464d545b626970777e858c939aa1a8afb6bdc4cbd2d9"; string(text) != want || f.String() != want {
		t.Errorf("MarshalText() = %s, String() = %s, want %s", text, f.String(), want)
	}
	var back Fingerprint
	if err := back.UnmarshalText(text); err != nil || back != f {
		t.Errorf("UnmarshalText(%s) = %x, %v, want %x", text, back, err, f)
	}
}

func TestFingerprintUnmarshalTextRejects(t *testing.T) {
	valid := strings.Repeat("ab", 32)
	for name, text := range map[string]string{
		"empty":       "",
		"short":       valid[:63],
		"long":        valid + "a",
		"upper case":  strings.ToUpper(valid),
		"not hex":     "g" + valid[1:],
		"with prefix": "sha256:" + valid[7:],
		"white space": " " + valid[1:],
	} {
		t.Run(name, func(t *testing.T) {
			f := Fingerprint{1}
			if err := f.UnmarshalText([]byte(text)); !errors.Is(err, ErrMalformedFingerprint) {
				t.Errorf("UnmarshalText(%q) error = %v, want ErrMalformedFingerprint", text, err)
			}
			if f != (Fingerprint{1}) {
				t.Errorf("UnmarshalText(%q) changed the fingerprint to %x", text, f)
			}
		})
	}
}

func TestFingerprintJSON(t *testing.T) {
	v := struct {
		Set  Fingerprint `json:"set"`
		Zero Fingerprint `json:"zero,omitzero"`
	}{Set: Fingerprint{0xff}}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if want := `{"set":"ff` + strings.Repeat("0", 62) + `"}`; string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}
	if !(Fingerprint{}).IsZero() || (Fingerprint{0: 0, 31: 1}).IsZero() {
		t.Error("IsZero() is wrong")
	}
}

func FuzzFingerprintText(f *testing.F) {
	f.Add([]byte(strings.Repeat("0", 64)))
	f.Add([]byte(strings.Repeat("f", 64)))
	f.Add([]byte(strings.Repeat("F", 64)))
	f.Add([]byte("00"))
	f.Fuzz(func(t *testing.T, text []byte) {
		var fp Fingerprint
		err := fp.UnmarshalText(text)
		if err != nil {
			if !errors.Is(err, ErrMalformedFingerprint) {
				t.Fatalf("UnmarshalText(%q) error = %v, want ErrMalformedFingerprint", text, err)
			}
			return
		}
		back, err := fp.MarshalText()
		if err != nil || !bytes.Equal(back, text) {
			t.Fatalf("UnmarshalText(%q) then MarshalText() = %q, %v", text, back, err)
		}
	})
}
