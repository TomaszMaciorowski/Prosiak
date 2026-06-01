package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolvePrefersConfigThenEnv(t *testing.T) {
	t.Setenv(EnvVar, "from-env")
	if got := Resolve("  from-config  "); got != "from-config" {
		t.Fatalf("config token should win and be trimmed, got %q", got)
	}
	if got := Resolve(""); got != "from-env" {
		t.Fatalf("empty config should fall back to env, got %q", got)
	}
	t.Setenv(EnvVar, "")
	if got := Resolve(""); got != "" {
		t.Fatalf("no config and no env should yield empty, got %q", got)
	}
}

func TestMiddlewareEnforcesToken(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	public := func(r *http.Request) bool { return r.URL.Path == "/health" }
	handler := Middleware("secret", public, next)

	cases := []struct {
		name   string
		path   string
		header string
		want   int
	}{
		{"public skips auth", "/health", "", http.StatusOK},
		{"missing token", "/files", "", http.StatusUnauthorized},
		{"wrong token", "/files", "Bearer nope", http.StatusUnauthorized},
		{"no bearer prefix", "/files", "secret", http.StatusUnauthorized},
		{"valid token", "/files", "Bearer secret", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestTransportInjectsToken(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	client := Client(srv.Client(), "secret")
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen != "Bearer secret" {
		t.Fatalf("Authorization header = %q, want %q", seen, "Bearer secret")
	}
}
