package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// bcrypt caps input at 72 bytes and silently ignores the remainder, which
// would make two long passwords sharing a prefix interchangeable. Rejecting
// rather than truncating.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 72
)

var ErrBadCredentials = errors.New("invalid email or password")

// HashPassword applies bcrypt at the library default cost.
func HashPassword(plain string) (string, error) {
	if err := ValidatePassword(plain); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return string(h), nil
}

// CheckPassword compares a candidate against a stored hash.
func CheckPassword(hash, plain string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)); err != nil {
		return ErrBadCredentials
	}
	return nil
}

// DummyCompare burns roughly the same time as a real bcrypt comparison.
//
// The login handler calls this when no user matched, so a request for an
// unknown address costs the same as one for a known address with a wrong
// password. Without it, response timing reveals which addresses are
// registered -- an account enumeration oracle that no amount of returning the
// same error message can close.
func DummyCompare(plain string) {
	// A real bcrypt hash of a fixed string, so the work is genuine rather than
	// a sleep that a profiler or a loaded machine would desynchronise.
	const placeholder = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	_ = bcrypt.CompareHashAndPassword([]byte(placeholder), []byte(plain))
}

// ValidatePassword enforces length only.
//
// Length is the requirement that actually correlates with resistance to
// guessing; composition rules ("one symbol, one digit") push users toward
// predictable mutations. A real deployment should add a breached-password
// check, which catches far more than any composition rule.
func ValidatePassword(plain string) error {
	if n := utf8.RuneCountInString(plain); n < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	// Measured in bytes, because bcrypt's limit is bytes: a 30-character
	// password of multi-byte runes can exceed 72.
	if len(plain) > MaxPasswordLen {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordLen)
	}
	return nil
}

// ConstantTimeEqual compares two secrets without leaking their contents
// through comparison timing.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
