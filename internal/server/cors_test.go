package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCORS_Wildcard(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := WithCORS(inner, CORSOptions{AllowOrigins: []string{"*"}})

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_SpecificOrigin(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := WithCORS(inner, CORSOptions{AllowOrigins: []string{"http://localhost:3000", "http://example.com"}})

	// Matching origin.
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, "http://localhost:3000", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "Origin", rec.Header().Get("Vary"))

	// Non-matching origin.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Origin", "http://evil.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_Preflight(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("should not reach"))
	})
	h := WithCORS(inner, CORSOptions{AllowOrigins: []string{"*"}})

	req := httptest.NewRequest(http.MethodOptions, "/v1/tools/echo", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, rec.Body.String(), "preflight should not forward to inner handler")
}

func TestCORS_DefaultDoesNotGrantAccess(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			h := WithCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}), CORSOptions{})
			req := httptest.NewRequest(method, "/v1/tools/write_file", nil)
			req.Header.Set("Origin", "https://evil.example")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Methods"))
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Headers"))
		})
	}
}

func TestCORS_NoOriginHeader(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := WithCORS(inner, CORSOptions{})

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"), "no CORS headers without Origin")
}
