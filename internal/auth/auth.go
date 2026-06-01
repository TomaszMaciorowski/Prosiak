// Package auth implements a shared-secret bearer token used to authenticate
// every participant of the backup cluster (server, nodes and backupctl).
package auth

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
)

// EnvVar is the environment variable read when no token is configured explicitly.
const EnvVar = "BACKUP_AUTH_TOKEN"

const bearerPrefix = "Bearer "

// Resolve returns the configured token, falling back to the BACKUP_AUTH_TOKEN
// environment variable. The result is trimmed; an empty string means "no token".
func Resolve(configToken string) string {
	token := strings.TrimSpace(configToken)
	if token == "" {
		token = strings.TrimSpace(os.Getenv(EnvVar))
	}
	return token
}

// Middleware enforces the bearer token on every request except those for which
// public reports true (health checks and static dashboard assets).
func Middleware(token string, public func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public != nil && public(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !ValidBearer(r, token) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ValidBearer reports whether the request carries the expected token. The
// comparison is constant time so it does not leak the token through timing.
func ValidBearer(r *http.Request, token string) bool {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, bearerPrefix) {
		return false
	}
	got := strings.TrimSpace(header[len(bearerPrefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// Transport injects the bearer token into every outgoing request.
type Transport struct {
	Token string
	Base  http.RoundTripper
}

// RoundTrip implements http.RoundTripper. It clones the request so the caller's
// headers are never mutated.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", bearerPrefix+t.Token)
	return base.RoundTrip(clone)
}

// Client wraps an existing client so all of its requests carry the bearer token.
// The original client is left untouched.
func Client(client *http.Client, token string) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	wrapped := *client
	wrapped.Transport = &Transport{Token: token, Base: client.Transport}
	return &wrapped
}
