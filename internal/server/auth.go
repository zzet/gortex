package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// WithAuth wraps h with a bearer-token check. Every request must carry
// `Authorization: Bearer <token>`; mismatches get a 401 JSON error.
//
// When token is empty, WithAuth returns h unchanged — the caller has
// opted into unauthenticated mode. Callers must restrict this to loopback
// binds and independently reject untrusted browser Origins: a loopback
// listener is reachable from web pages as well as local clients.
//
// CORS preflights (OPTIONS) bypass the check so browsers on a different
// origin can negotiate headers before the real request is issued.
//
// The token is accepted only via the `Authorization` header. The query
// string was dropped (see fix/drop-query-string-auth-token) because a
// query token leaks into access logs, proxy logs, and Referrer headers,
// and — paired with a CORS `*` wildcard — lets a cross-origin simple GET
// exfiltrate it without any custom request header.
func WithAuth(h http.Handler, token string) http.Handler {
	if token == "" {
		return h
	}
	return WithAuthFunc(h, func() string { return token })
}

// WithAuthFunc is WithAuth with a per-request token source: tokenFn is
// resolved on every request, so an operator can rotate the expected token
// (e.g. update $GORTEX_DAEMON_HTTP_TOKEN) without restarting the server.
// A tokenFn that returns "" serves the request unauthenticated, identical
// to a static empty token — so the token can also be added or removed at
// runtime, not only changed.
func WithAuthFunc(h http.Handler, tokenFn func() string) http.Handler {
	if tokenFn == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := tokenFn()
		if token == "" {
			h.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodOptions {
			h.ServeHTTP(w, r)
			return
		}
		if authMatches([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="gortex"`)
		WriteJSONError(w, http.StatusUnauthorized, "missing or invalid bearer token")
	})
}

// authMatches does a constant-time comparison to defeat timing attacks
// on token validation.
func authMatches(got, expected []byte) bool {
	if !strings.HasPrefix(string(got), "Bearer ") {
		return false
	}
	return tokenMatches(got, expected)
}

func tokenMatches(got, expected []byte) bool {
	// subtle.ConstantTimeCompare needs equal-length slices to return 1.
	// For unequal lengths the answer is obviously "no match" but we
	// still scan a fixed-size buffer so an attacker can't learn the
	// token length from timing.
	if len(got) != len(expected) {
		_ = subtle.ConstantTimeCompare(expected, expected)
		return false
	}
	return subtle.ConstantTimeCompare(got, expected) == 1
}
