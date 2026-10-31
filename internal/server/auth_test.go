package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	})
}

func TestWithAuth_EmptyTokenIsPassthrough(t *testing.T) {
	h := WithAuth(okHandler(), "")
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestWithAuth_AcceptsValidBearer(t *testing.T) {
	h := WithAuth(okHandler(), "secret-token")
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestWithAuth_RejectsMissingHeader(t *testing.T) {
	h := WithAuth(okHandler(), "secret-token")
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `Bearer realm="gortex"`, rec.Header().Get("WWW-Authenticate"))
}

func TestWithAuth_RejectsWrongToken(t *testing.T) {
	h := WithAuth(okHandler(), "secret-token")
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestWithAuth_RejectsNonBearerScheme(t *testing.T) {
	h := WithAuth(okHandler(), "secret-token")
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Basic c2VjcmV0LXRva2Vu")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestWithAuth_BypassesOptionsForCORSPreflight(t *testing.T) {
	h := WithAuth(okHandler(), "secret-token")
	req := httptest.NewRequest(http.MethodOptions, "/v1/tools/search_symbols", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestWithAuth_AcceptsBearerToken(t *testing.T) {
	h := WithAuth(okHandler(), "secret-token")
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestWithAuth_RejectsWrongBearerToken(t *testing.T) {
	h := WithAuth(okHandler(), "secret-token")
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
