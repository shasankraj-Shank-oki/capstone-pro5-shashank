package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestIssueAndParseToken(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	token, err := IssueToken(secret, "user-1", RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := ParseToken(token, secret)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Subject != "user-1" || principal.Role != RoleAdmin {
		t.Fatalf("unexpected principal: %#v", principal)
	}
}

func TestParseTokenRejectsExpiredToken(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	token, err := IssueToken(secret, "user-1", RoleCustomer, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := ParseToken(token, secret); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestParseTokenRejectsMissingExpiration(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  "user-1",
		"role": RoleCustomer,
	}).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToken(token, secret); err == nil {
		t.Fatal("expected token without expiration to be rejected")
	}
}
