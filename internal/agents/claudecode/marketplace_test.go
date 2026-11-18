package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmitMarketplace_Layout(t *testing.T) {
	dir := t.TempDir()
	written, err := EmitMarketplace(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{
		filepath.Join(".claude-plugin", "marketplace.json"),
		"README.md",
		"LICENSE",
		filepath.Join("plugins", "gortex", ".claude-plugin", "plugin.json"),
		filepath.Join("plugins", "gortex", ".mcp.json"),
		filepath.Join("plugins", "gortex", "skills", "explore", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected %s: %v", rel, err)
		}
	}
	// The marketplace root must not also look like a plugin.
	for _, rel := range []string{
		filepath.Join(".claude-plugin", "plugin.json"),
		".mcp.json",
		"skills",
		"hooks",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("marketplace root must not contain %s (stat err = %v)", rel, err)
		}
	}
	for _, rel := range written {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("reported path %s was not written: %v", rel, err)
		}
	}
}

func TestEmitMarketplace_ManifestPointsAtPlugin(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitMarketplace(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"}); err != nil {
		t.Fatal(err)
	}

	var m struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Owner       struct {
			Name string `json:"name"`
		} `json:"owner"`
		Plugins []struct {
			Name    string `json:"name"`
			Source  string `json:"source"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("marketplace.json: %v", err)
	}
	if m.Name != "gortex" || m.Owner.Name == "" {
		t.Errorf("marketplace name/owner = %q/%q", m.Name, m.Owner.Name)
	}
	// claude plugin validate warns when the description is missing.
	if m.Description == "" {
		t.Error("marketplace.json must carry a description")
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(m.Plugins))
	}
	entry := m.Plugins[0]
	if !strings.HasPrefix(entry.Source, "./") {
		t.Errorf("relative source must start with ./, got %q", entry.Source)
	}
	// plugin.json owns the version; a second copy in the entry would drift.
	if entry.Version != "" {
		t.Errorf("entry must not pin a version, got %q", entry.Version)
	}

	var plugin struct {
		Name string `json:"name"`
	}
	raw, err = os.ReadFile(filepath.Join(dir, filepath.FromSlash(entry.Source), ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatalf("entry source %q has no plugin.json: %v", entry.Source, err)
	}
	if err := json.Unmarshal(raw, &plugin); err != nil {
		t.Fatal(err)
	}
	if plugin.Name != entry.Name {
		t.Errorf("entry name %q must match plugin.json name %q", entry.Name, plugin.Name)
	}
}

func TestEmitMarketplace_RefusesFlatBundle(t *testing.T) {
	dir := t.TempDir()
	// A bundle from before the marketplace layout, with the plugin at
	// the target root.
	if _, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"}); err != nil {
		t.Fatal(err)
	}

	_, err := EmitMarketplace(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err == nil || !strings.Contains(err.Error(), "older flat layout") {
		t.Fatalf("expected a flat-layout refusal, got %v", err)
	}
	// Refusing must leave the target untouched.
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("flat bundle must be left in place: %v", err)
	}
	for _, rel := range []string{
		filepath.Join(".claude-plugin", "marketplace.json"),
		"plugins",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("refused emit must not write %s (stat err = %v)", rel, err)
		}
	}
}

func TestEmitMarketplace_KeepsUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	// Files someone keeps next to the marketplace, including names the
	// plugin also uses, must survive an emit.
	userFiles := []string{
		filepath.Join(dir, ".mcp.json"),
		filepath.Join(dir, "skills", "explore", "SKILL.md"),
	}
	for _, full := range userFiles {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := EmitMarketplace(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"}); err != nil {
		t.Fatal(err)
	}
	for _, full := range userFiles {
		b, err := os.ReadFile(full)
		if err != nil || string(b) != "keep" {
			t.Errorf("%s must be left alone (err = %v, body = %q)", full, err, b)
		}
	}
}

func TestEmitMarketplace_Idempotent(t *testing.T) {
	dir := t.TempDir()
	first, err := EmitMarketplace(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{}
	for _, rel := range first {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		bodies[rel] = string(b)
	}

	second, err := EmitMarketplace(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Fatalf("second emit: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("file count drift between runs: %d vs %d", len(first), len(second))
	}
	for _, rel := range second {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("%s missing after second emit: %v", rel, err)
		}
		if bodies[rel] != string(b) {
			t.Errorf("non-idempotent output at %s", rel)
		}
	}
}

func TestEmitMarketplace_RejectsEmptyTarget(t *testing.T) {
	if _, err := EmitMarketplace(PluginBundleSpec{Version: "0.18.2"}); err == nil {
		t.Error("expected an error for an empty TargetDir")
	}
}
