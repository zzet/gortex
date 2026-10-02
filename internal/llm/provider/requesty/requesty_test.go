package requesty

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

func TestNew_MissingKey(t *testing.T) {
	t.Setenv("REQUESTY_API_KEY", "")
	if _, err := New(llm.RemoteConfig{Model: "openai/gpt-4o-mini"}); err == nil {
		t.Fatal("expected error when API key env is unset")
	}
}

func TestNew_MissingModel(t *testing.T) {
	t.Setenv("REQUESTY_API_KEY", "k")
	if _, err := New(llm.RemoteConfig{}); err == nil {
		t.Fatal("expected error when model is empty")
	}
}

func TestComplete_StructuredUsesJSONSchema(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer test-key" {
			t.Errorf("authorization=%q", r.Header.Get("authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"terms\":[\"jwt\"]}"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("REQUESTY_API_KEY", "test-key")
	p, err := New(llm.RemoteConfig{Model: "openai/gpt-4o-mini", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "Query: auth"}},
		Shape:     llm.ShapeExpandTerms,
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"terms":["jwt"]}` {
		t.Errorf("text=%q", resp.Text)
	}
	if gotBody["model"] != "openai/gpt-4o-mini" {
		t.Errorf("model=%v want openai/gpt-4o-mini", gotBody["model"])
	}
	rf, ok := gotBody["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing/invalid: %v", gotBody["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type=%v want json_schema", rf["type"])
	}
}

// TestNew_BaseURLVariants asserts the request path is
// "/v1/chat/completions" whether base_url is given with or without the
// "/v1" segment. Requesty documents its base URLs with "/v1", so a
// user copying one from the docs must not end up at "/v1/v1/...".
func TestNew_BaseURLVariants(t *testing.T) {
	for _, suffix := range []string{"", "/v1", "/v1/"} {
		t.Run("base"+suffix, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			}))
			defer srv.Close()

			t.Setenv("REQUESTY_API_KEY", "k")
			p, err := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL + suffix})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if _, err := p.Complete(context.Background(), llm.CompletionRequest{
				Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
			}); err != nil {
				t.Fatal(err)
			}
			if gotPath != "/v1/chat/completions" {
				t.Errorf("path=%q want /v1/chat/completions", gotPath)
			}
		})
	}
}

func TestComplete_FreeformNoResponseFormat(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"plain text"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("REQUESTY_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Shape:    llm.ShapeFreeform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "plain text" {
		t.Errorf("text=%q", resp.Text)
	}
	if _, ok := gotBody["response_format"]; ok {
		t.Error("freeform request must not send response_format")
	}
}

func TestComplete_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"origin":"router","message":"Invalid authorization token"}}`)
	}))
	defer srv.Close()

	t.Setenv("REQUESTY_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestName(t *testing.T) {
	t.Setenv("REQUESTY_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m"})
	if p.Name() != "requesty" {
		t.Errorf("Name()=%q", p.Name())
	}
}

func TestComplete_SendsReasoningEffort(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("REQUESTY_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "openai/o4-mini", BaseURL: srv.URL, Effort: "high"})
	defer p.Close()
	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if gotBody["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort=%v want high", gotBody["reasoning_effort"])
	}
}
