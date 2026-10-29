package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	secret = []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	now    = time.Now()
)

func verifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := NewVerifier(secret, "pullwise", "pullwise-knowledge")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func issue(t *testing.T, mutate func(*Claims)) string {
	t.Helper()
	claims := Claims{
		Tenant: "u42",
		Scopes: []string{ScopeSearch},
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "pullwise",
			Audience:  jwt.ClaimStrings{"pullwise-knowledge"},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
	}
	if mutate != nil {
		mutate(&claims)
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifyAcceptsAGoodToken(t *testing.T) {
	tok, err := Issue(secret, "pullwise", "pullwise-knowledge", "u42", []string{ScopeIndex, ScopeSearch}, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, err := verifier(t).Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Tenant != "u42" || !c.Has(ScopeIndex) || !c.Has(ScopeSearch) || c.Has("admin") {
		t.Fatalf("claims = %+v", c)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	otherSecret := []byte("ffffffffffffffffffffffffffffffff")
	wrongKey, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Tenant: "u42", RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "pullwise", Audience: jwt.ClaimStrings{"pullwise-knowledge"},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
	}).SignedString(otherSecret)
	hs512, _ := jwt.NewWithClaims(jwt.SigningMethodHS512, Claims{
		Tenant: "u42", RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "pullwise", Audience: jwt.ClaimStrings{"pullwise-knowledge"},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
	}).SignedString(secret)
	noneAlg, _ := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{
		Tenant: "u42", RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "pullwise", Audience: jwt.ClaimStrings{"pullwise-knowledge"},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)

	tests := []struct {
		name  string
		token string
	}{
		{"signed with another secret", wrongKey},
		{"different HMAC algorithm", hs512},
		{"alg none", noneAlg},
		{"expired", issue(t, func(c *Claims) {
			c.IssuedAt = jwt.NewNumericDate(now.Add(-time.Hour))
			c.ExpiresAt = jwt.NewNumericDate(now.Add(-30 * time.Minute))
		})},
		{"wrong issuer", issue(t, func(c *Claims) { c.Issuer = "someone-else" })},
		{"wrong audience", issue(t, func(c *Claims) { c.Audience = jwt.ClaimStrings{"another-service"} })},
		{"no expiry", issue(t, func(c *Claims) { c.ExpiresAt = nil })},
		{"no issued-at", issue(t, func(c *Claims) { c.IssuedAt = nil })},
		{"lives too long", issue(t, func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(time.Hour)) })},
		{"missing tenant", issue(t, func(c *Claims) { c.Tenant = "" })},
		{"tenant with odd characters", issue(t, func(c *Claims) { c.Tenant = "u42' OR '1'='1" })},
		{"garbage", "not-a-jwt"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := verifier(t).Verify(tt.token); err == nil {
				t.Fatal("token was accepted")
			}
		})
	}
}

func TestNewVerifierAndIssueRefuseWeakInputs(t *testing.T) {
	if _, err := NewVerifier([]byte("short"), "i", "a"); err == nil {
		t.Error("a short secret must be refused")
	}
	if _, err := NewVerifier(secret, "", "a"); err == nil {
		t.Error("an empty issuer must be refused")
	}
	if _, err := Issue([]byte("short"), "i", "a", "t", nil, time.Minute, now); err == nil {
		t.Error("issuing with a short secret must be refused")
	}
	if _, err := Issue(secret, "i", "a", "t", nil, time.Hour, now); err == nil {
		t.Error("a token lifetime above the maximum must be refused")
	}
}

func deny(w http.ResponseWriter, status int, code, message string) {
	http.Error(w, code+": "+message, status)
}

func TestRequireMiddleware(t *testing.T) {
	var gotTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := FromContext(r.Context())
		gotTenant = c.Tenant
		w.WriteHeader(http.StatusNoContent)
	})
	h := verifier(t).Require(ScopeSearch, deny)(next)

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic abc", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"bad token", "Bearer nope", http.StatusUnauthorized},
		{"valid token without the scope", "Bearer " + issue(t, func(c *Claims) { c.Scopes = []string{ScopeIndex} }), http.StatusForbidden},
		{"valid token with the scope", "Bearer " + issue(t, nil), http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTenant = ""
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("a 401 should say how to authenticate")
			}
			if tt.wantStatus == http.StatusNoContent && gotTenant != "u42" {
				t.Errorf("handler saw tenant %q, want the token's u42", gotTenant)
			}
			if tt.wantStatus != http.StatusNoContent && gotTenant != "" {
				t.Error("the handler ran for a rejected request")
			}
		})
	}
}
