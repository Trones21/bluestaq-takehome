// Package auth handles password hashing, token issuance, and the middleware
// that turns a bearer token into an authenticated user on the request context.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// issuer identifies tokens minted by this service. Validated on the way back
// in so a token from a different system that happens to share a signing key
// is still rejected.
const issuer = "notes-api"

var ErrInvalidToken = errors.New("invalid or expired token")

// Tokens issues and verifies access tokens.
//
// Access tokens only, deliberately: no refresh tokens, no server-side session
// table, and therefore no revocation. A stolen token is valid until it
// expires, which is the cost of stateless auth and the reason the TTL is
// hours rather than weeks. Production would delegate this to an IdP instead of
// minting its own.
type Tokens struct {
	secret []byte
	ttl    time.Duration
}

func NewTokens(secret []byte, ttl time.Duration) *Tokens {
	return &Tokens{secret: secret, ttl: ttl}
}

type Claims struct {
	jwt.RegisteredClaims
}

// Issue mints an access token for a user.
func (t *Tokens) Issue(userID uuid.UUID, now time.Time) (string, time.Time, error) {
	expires := now.Add(t.ttl)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
			ID:        uuid.NewString(),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("signing token: %w", err)
	}
	return signed, expires, nil
}

// Parse verifies a token and returns its subject.
func (t *Tokens) Parse(raw string) (uuid.UUID, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) {
		return t.secret, nil
	},
		// Pinning the algorithm is the important line. Without it a token can
		// declare alg=none, or declare HMAC against an RSA public key, and the
		// library would verify it against the wrong primitive.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: subject is not a uuid", ErrInvalidToken)
	}
	return id, nil
}
