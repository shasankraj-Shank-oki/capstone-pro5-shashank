package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"
)

// Account is a configured login. Passwords come from environment variables /
// Secrets Manager. This is meant for testing and small deployments; use a real
// identity provider (for example Amazon Cognito) for end users.
type Account struct {
	Username string
	Password string
	Role     string
}

// LoginHandler exchanges a username and password for a signed, expiring JWT.
func LoginHandler(secret []byte, ttl time.Duration, accounts []Account) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeAuthError(w, r, http.StatusBadRequest, "invalid_request", "Invalid request body")
			return
		}
		var matched *Account
		for i := range accounts {
			userOK := subtle.ConstantTimeCompare(digest(input.Username), digest(accounts[i].Username)) == 1
			passOK := subtle.ConstantTimeCompare(digest(input.Password), digest(accounts[i].Password)) == 1
			if userOK && passOK && matched == nil {
				matched = &accounts[i]
			}
		}
		if matched == nil {
			writeAuthError(w, r, http.StatusUnauthorized, "unauthorized", "Invalid credentials")
			return
		}
		token, err := IssueToken(secret, matched.Username, matched.Role, ttl)
		if err != nil {
			writeAuthError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": token, "tokenType": "Bearer", "expiresIn": int(ttl.Seconds()),
		})
	})
}

func digest(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}
