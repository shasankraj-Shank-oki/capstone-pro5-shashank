package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginIssuesRoleTokensAndRejectsBadCredentials(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	handler := LoginHandler(secret, time.Minute, []Account{
		{Username: "admin", Password: "admin-password-1", Role: RoleAdmin},
		{Username: "user", Password: "user-password-12", Role: RoleCustomer},
	})
	login := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(body)))
		return recorder
	}
	for _, tc := range []struct{ body, role string }{
		{`{"username":"admin","password":"admin-password-1"}`, RoleAdmin},
		{`{"username":"user","password":"user-password-12"}`, RoleCustomer},
	} {
		response := login(tc.body)
		if response.Code != http.StatusOK {
			t.Fatalf("login %s: got %d: %s", tc.role, response.Code, response.Body)
		}
		var result struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		principal, err := ParseToken(result.Token, secret)
		if err != nil || principal.Role != tc.role {
			t.Fatalf("token for %s: principal=%#v err=%v", tc.role, principal, err)
		}
	}
	for _, body := range []string{
		`{"username":"admin","password":"wrong-password"}`,
		`{"username":"user","password":"admin-password-1"}`,
		`{"username":"nobody","password":"x"}`,
	} {
		if code := login(body).Code; code != http.StatusUnauthorized {
			t.Fatalf("bad credentials %s: got %d, want 401", body, code)
		}
	}
	if code := login(`not json`).Code; code != http.StatusBadRequest {
		t.Fatalf("malformed body: got %d, want 400", code)
	}
}
