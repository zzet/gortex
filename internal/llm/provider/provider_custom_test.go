package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// TestNew_CustomProviderDispatches proves an unknown provider name
// resolves to a registered custom OpenAI-compatible endpoint, addressed
// at <base_url>/chat/completions with a Bearer key.
func TestNew_CustomProviderDispatches(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi from gateway"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("MY_GATEWAY_KEY", "secret")
	cfg := llm.Config{
		Provider: "mygw",
		Custom: map[string]llm.CustomProvider{
			"mygw": {BaseURL: srv.URL + "/v1", Model: "some-model", APIKeyEnv: "MY_GATEWAY_KEY"},
		},
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if p.Name() != "custom" {
		t.Errorf("Name()=%q want custom", p.Name())
	}
	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hi from gateway" {
		t.Errorf("text=%q", resp.Text)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path=%q want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("authorization=%q want Bearer secret", gotAuth)
	}
}

// TestNew_CustomBaseURLPathIsKept proves a base URL whose path is not
// /v1 (Opper's /v3/compat) keeps that path when /chat/completions is
// appended.
func TestNew_CustomBaseURLPathIsKept(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("MY_GATEWAY_KEY", "secret")
	cfg := llm.Config{
		Provider: "mygw",
		Custom: map[string]llm.CustomProvider{
			"mygw": {BaseURL: srv.URL + "/v3/compat", Model: "some-model", APIKeyEnv: "MY_GATEWAY_KEY"},
		},
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v3/compat/chat/completions" {
		t.Errorf("path=%q want /v3/compat/chat/completions", gotPath)
	}
}

func TestNew_CustomKeylessOmitsAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	cfg := llm.Config{
		Provider: "localgw",
		Custom: map[string]llm.CustomProvider{
			"localgw": {BaseURL: srv.URL + "/v1", Model: "m"},
		},
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("keyless provider must not send an Authorization header, got %q", gotAuth)
	}
}

func TestNew_CustomMissingKeyErrors(t *testing.T) {
	t.Setenv("ABSENT_KEY", "")
	cfg := llm.Config{
		Provider: "gw",
		Custom: map[string]llm.CustomProvider{
			"gw": {BaseURL: "https://x/v1", Model: "m", APIKeyEnv: "ABSENT_KEY"},
		},
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected an error when the configured API key env is unset")
	}
}

func TestNew_CustomBadSchemeErrors(t *testing.T) {
	cfg := llm.Config{
		Provider: "gw",
		Custom:   map[string]llm.CustomProvider{"gw": {BaseURL: "ftp://x/v1", Model: "m"}},
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected an error for a non-http base_url")
	}
}

func TestNew_UnknownProviderStillErrors(t *testing.T) {
	if _, err := New(llm.Config{Provider: "totally-unknown"}); err == nil {
		t.Fatal("expected an error for an unknown provider with no custom entry")
	}
}

func TestNew_CustomBadPromptProfileErrors(t *testing.T) {
	cfg := llm.Config{
		Provider: "gw",
		Custom:   map[string]llm.CustomProvider{"gw": {BaseURL: "https://x/v1", Model: "m", PromptProfile: "huge"}},
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected an error for an unknown prompt_profile")
	}
}

// TestNew_CustomEnvModelAndEffortReachRequest proves GORTEX_LLM_MODEL and
// GORTEX_LLM_EFFORT land in a custom provider's request body.
func TestNew_CustomEnvModelAndEffortReachRequest(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("GORTEX_LLM_PROVIDER", "gw")
	t.Setenv("GORTEX_LLM_MODEL", "env-model")
	t.Setenv("GORTEX_LLM_EFFORT", "high")
	cfg := llm.Config{Custom: map[string]llm.CustomProvider{
		"gw": {BaseURL: srv.URL + "/v1", Model: "file-model"},
	}}.MergeEnv()
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "env-model" || body["reasoning_effort"] != "high" {
		t.Errorf("model=%v reasoning_effort=%v, want env-model / high", body["model"], body["reasoning_effort"])
	}
}
