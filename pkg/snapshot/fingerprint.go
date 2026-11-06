package snapshot

import (
	"encoding/hex"
	"errors"
)

// Fingerprint is a SHA-256 hash that identifies a value without holding it.
// The zero Fingerprint means there is none, as for a withheld value.
//
// Each hash covers a fixed prefix as well as the value, which stops generic
// lookup tables, but a value that can be guessed can still be found by
// hashing guesses, so a fingerprint does not keep a weak secret.
type Fingerprint [32]byte

// ErrMalformedFingerprint means a fingerprint's text is not 64 lower-case
// hexadecimal digits.
var ErrMalformedFingerprint = errors.New("fingerprint is not 64 lower-case hexadecimal digits")

// IsZero reports whether f is the zero Fingerprint.
func (f Fingerprint) IsZero() bool { return f == Fingerprint{} }

// String returns f as 64 lower-case hexadecimal digits.
func (f Fingerprint) String() string { return hex.EncodeToString(f[:]) }

// MarshalText returns f as 64 lower-case hexadecimal digits.
func (f Fingerprint) MarshalText() ([]byte, error) {
	return hex.AppendEncode(nil, f[:]), nil
}

// UnmarshalText sets f from 64 lower-case hexadecimal digits, and returns
// ErrMalformedFingerprint for any other text.
func (f *Fingerprint) UnmarshalText(text []byte) error {
	if len(text) != hex.EncodedLen(len(f)) {
		return ErrMalformedFingerprint
	}
	for _, c := range text {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ErrMalformedFingerprint
		}
	}
	var out Fingerprint
	if _, err := hex.Decode(out[:], text); err != nil {
		return ErrMalformedFingerprint
	}
	*f = out
	return nil
}
