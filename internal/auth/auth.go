package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

func Middleware(token string) func(http.Handler) http.Handler {
	expected := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Unauthenticated endpoints
			if r.URL.Path == "/healthz" {
				next.ServeHTTP(w, r)
				return
			}

			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}

			provided := []byte(strings.TrimPrefix(h, "Bearer "))
			if subtle.ConstantTimeCompare(provided, expected) != 1 {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
