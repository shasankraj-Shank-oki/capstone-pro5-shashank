package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	RoleCustomer = "customer"
	RoleAdmin    = "admin"
)

type Claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

type Principal struct {
	Subject string
	Role    string
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

func FromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	return principal, ok
}

func IssueToken(secret []byte, subject, role string, ttl time.Duration) (string, error) {
	if subject == "" || (role != RoleCustomer && role != RoleAdmin) || ttl <= 0 {
		return "", errors.New("subject, supported role, and positive token lifetime are required")
	}
	claims := Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

func ParseToken(tokenString string, secret []byte) (Principal, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method: %s", token.Method.Alg())
		}
		return secret, nil
	})
	if err != nil || !token.Valid {
		return Principal{}, errors.New("invalid or expired token")
	}
	if claims.Subject == "" || claims.ExpiresAt == nil || claims.ExpiresAt.Before(time.Now()) || (claims.Role != RoleCustomer && claims.Role != RoleAdmin) {
		return Principal{}, errors.New("invalid token claims")
	}
	return Principal{Subject: claims.Subject, Role: claims.Role}, nil
}

func Authenticate(secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			scheme, tokenString, ok := strings.Cut(header, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") || tokenString == "" {
				writeAuthError(w, r, http.StatusUnauthorized, "unauthorized", "Bearer token required")
				return
			}
			principal, err := ParseToken(tokenString, secret)
			if err != nil {
				writeAuthError(w, r, http.StatusUnauthorized, "unauthorized", "Invalid or expired token")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, ok := FromContext(r.Context())
			if !ok || (principal.Role != role && principal.Role != RoleAdmin) {
				writeAuthError(w, r, http.StatusForbidden, "forbidden", "Insufficient role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeAuthError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code": code, "message": message, "requestId": r.Header.Get("X-Request-ID"),
		},
	})
}
