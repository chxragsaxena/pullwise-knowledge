// Package auth authenticates service calls with short-lived signed tokens (JWT, HS256).
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Scopes a token can carry.
const (
	ScopeIndex  = "index"
	ScopeSearch = "search"
)

const (
	// MaxLifetime is the longest a token may be valid for.
	MaxLifetime = 5 * time.Minute
	// leeway tolerates small clock differences between services.
	leeway = 5 * time.Second
	// MinSecretBytes is the shortest HMAC secret accepted.
	MinSecretBytes = 32
)

var tenantPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Claims is the verified content of a token.
type Claims struct {
	// Tenant is whose data this call may read or write ("public", "u42", ...).
	Tenant string   `json:"tenant"`
	Scopes []string `json:"scopes"`
	jwt.RegisteredClaims
}

// Has reports whether the token grants a scope.
func (c Claims) Has(scope string) bool { return slices.Contains(c.Scopes, scope) }

// Verifier checks tokens issued by a trusted peer.
type Verifier struct {
	secret   []byte
	issuer   string
	audience string
}

// NewVerifier returns a Verifier for tokens from issuer addressed to audience.
func NewVerifier(secret []byte, issuer, audience string) (*Verifier, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("jwt secret must be at least %d bytes", MinSecretBytes)
	}
	if issuer == "" || audience == "" {
		return nil, errors.New("jwt issuer and audience are required")
	}
	return &Verifier{secret: secret, issuer: issuer, audience: audience}, nil
}

// Verify validates a token's signature, issuer, audience, expiry and claims.
func (v *Verifier) Verify(raw string) (Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return v.secret, nil },
		// Only HS256: never trust the token's own algorithm header.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(leeway),
	)
	if err != nil {
		return Claims{}, fmt.Errorf("invalid token: %w", err)
	}

	if claims.IssuedAt == nil {
		return Claims{}, errors.New("invalid token: missing issued-at")
	}
	if claims.ExpiresAt.Sub(claims.IssuedAt.Time) > MaxLifetime {
		return Claims{}, errors.New("invalid token: lifetime too long")
	}
	if !tenantPattern.MatchString(claims.Tenant) {
		return Claims{}, errors.New("invalid token: bad tenant")
	}
	return claims, nil
}

type ctxKey struct{}

// FromContext returns the verified claims of the current request.
func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claims)
	return c, ok
}

// Require returns middleware that demands a valid bearer token carrying scope.
func (v *Verifier) Require(scope string, deny func(w http.ResponseWriter, status int, code, message string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			raw, ok := strings.CutPrefix(header, "Bearer ")
			if !ok || raw == "" {
				w.Header().Set("WWW-Authenticate", `Bearer`)
				deny(w, http.StatusUnauthorized, "unauthorized", "A bearer token is required.")
				return
			}
			claims, err := v.Verify(raw)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				deny(w, http.StatusUnauthorized, "unauthorized", "The token is invalid or expired.")
				return
			}
			if !claims.Has(scope) {
				deny(w, http.StatusForbidden, "forbidden", "The token does not allow this operation.")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, claims)))
		})
	}
}

// Issue creates a signed token.
func Issue(secret []byte, issuer, audience, tenant string, scopes []string, ttl time.Duration, now time.Time) (string, error) {
	if len(secret) < MinSecretBytes {
		return "", fmt.Errorf("jwt secret must be at least %d bytes", MinSecretBytes)
	}
	if ttl <= 0 || ttl > MaxLifetime {
		return "", fmt.Errorf("token ttl must be between 0 and %s", MaxLifetime)
	}
	claims := Claims{
		Tenant: tenant,
		Scopes: scopes,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}
