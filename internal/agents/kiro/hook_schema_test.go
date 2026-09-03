package kiro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

func TestHookFilesUseKiroV1Schema(t *testing.T) {
	type action struct {
		Type   string `json:"type"`
		Prompt string `json:"prompt"`
	}
	type hook struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Trigger     string `json:"trigger"`
		Matcher     string `json:"matcher"`
		Action      action `json:"action"`
	}
	type document struct {
		Version string `json:"version"`
		Hooks   []hook `json:"hooks"`
	}

	want := map[string]struct {
		trigger string
		matcher string
	}{
		"gortex-smart-context.json": {trigger: "UserPromptSubmit"},
		"gortex-post-edit.json":     {trigger: "PostFileSave", matcher: `\.(go|ts|tsx|js|jsx|py|rs|java|kt|scala|swift|rb|cs|php)$`},
		"gortex-pre-read.json":      {trigger: "PreToolUse", matcher: "read"},
	}

	for name, expected := range want {
		t.Run(name, func(t *testing.T) {
			body, ok := HookFiles[name]
			if !ok {
				t.Fatalf("missing hook file %q", name)
			}
			var doc document
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("invalid hook JSON: %v\n%s", err, body)
			}
			if doc.Version != "v1" || len(doc.Hooks) != 1 {
				t.Fatalf("hook envelope = %#v, want version v1 and one hook", doc)
			}
			hook := doc.Hooks[0]
			if hook.Name == "" || hook.Description == "" || hook.Trigger != expected.trigger || hook.Matcher != expected.matcher {
				t.Fatalf("hook = %#v, want trigger %q matcher %q", hook, expected.trigger, expected.matcher)
			}
			if hook.Action.Type != "agent" || hook.Action.Prompt == "" {
				t.Fatalf("hook action = %#v, want non-empty agent prompt", hook.Action)
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(body), &raw); err != nil {
				t.Fatal(err)
			}
			if _, oldWhen := raw["when"]; oldWhen {
				t.Fatal("legacy when field remains in Kiro v1 hook")
			}
			if _, oldThen := raw["then"]; oldThen {
				t.Fatal("legacy then field remains in Kiro v1 hook")
			}
		})
	}
}

func TestKiroMigratesExactLegacyHookSchemas(t *testing.T) {
	for name, legacy := range legacyHookFiles {
		t.Run(name, func(t *testing.T) {
			env, _ := agentstest.NewEnv(t)
			path := filepath.Join(env.Root, ".kiro", "hooks", name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}

			// Reformat the shipped document to prove ownership is semantic, not
			// dependent on byte-for-byte whitespace.
			var document any
			if err := json.Unmarshal([]byte(legacy), &document); err != nil {
				t.Fatal(err)
			}
			reformatted, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, reformatted, 0o644); err != nil {
				t.Fatal(err)
			}

			a := New()
			if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
				t.Fatalf("apply: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != HookFiles[name] {
				t.Fatalf("legacy Kiro hook was not migrated:\n%s", got)
			}
			agentstest.AssertIdempotent(t, a, env)
		})
	}
}

func TestKiroPreservesCustomizedLegacyHook(t *testing.T) {
	for name, body := range map[string]string{
		"modified prompt":      strings.Replace(legacyHookPreRead, "For indexed source code", "Follow the team policy, then use indexed source code", 1),
		"modified description": strings.Replace(legacyHookPreRead, "Use indexed source context", "Use company source context", 1),
		"extra field":          strings.Replace(legacyHookPreRead, `  "version"`, "  \"custom\": true,\n  \"version\"", 1),
		"custom name":          strings.Replace(legacyHookPreRead, "Gortex: Enrich Source Read", "Gortex: Custom Source Read", 1),
		"malformed JSON":       `{"name":"Gortex: Enrich Source Read"`,
		"current v1":           HookFiles["gortex-pre-read.json"],
	} {
		t.Run(name, func(t *testing.T) {
			env, _ := agentstest.NewEnv(t)
			path := filepath.Join(env.Root, ".kiro", "hooks", "gortex-pre-read.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
				t.Fatalf("apply: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != body {
				t.Fatalf("customized hook was overwritten:\n--- got ---\n%s\n--- want ---\n%s", got, body)
			}
		})
	}
}
