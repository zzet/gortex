package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/codex"
	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/toolref"
)

func readInstructionStampVersion(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, profiles.GenerationStampFileName))
	if err != nil {
		t.Fatal(err)
	}
	var stamp struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &stamp); err != nil {
		t.Fatal(err)
	}
	return stamp.Version
}

func TestInstructionsRegenStampsActualRunningVersion(t *testing.T) {
	_, _ = sandboxInstructionsEnv(t)
	dir := profiles.DefaultDir()
	if _, err := profiles.Switch(dir, "localization", "v-old"); err != nil {
		t.Fatal(err)
	}
	cmd, _, _ := captureCmd()
	if err := runInstructionsRegen(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := readInstructionStampVersion(t, dir); got != canonicalVersion() {
		t.Fatalf("CLI omitted running version: %q want %q", got, canonicalVersion())
	}
	if got := profiles.ActiveName(dir); got != "localization" {
		t.Fatalf("regen changed stored selection: %q", got)
	}
}

func TestDaemonStartRefreshesManagedProfilesOnceAndPreservesConflicts(t *testing.T) {
	_, _ = sandboxInstructionsEnv(t)
	dir := profiles.DefaultDir()
	if _, err := profiles.Switch(dir, "localization", "v-old"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(profiles.ActiveEnv, "full")
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	refreshInstructionProfilesOnStart(dir, canonicalVersion(), logger)
	if got := readInstructionStampVersion(t, dir); got != canonicalVersion() {
		t.Fatalf("daemon did not stamp its actual version: %q", got)
	}
	localization, _ := profiles.ByName("localization")
	activeBefore, err := os.ReadFile(filepath.Join(dir, profiles.ActiveFileName))
	if err != nil || string(activeBefore) != localization.Body() {
		t.Fatal("daemon persisted environment override instead of stored selection")
	}
	stampBefore, err := os.ReadFile(filepath.Join(dir, profiles.GenerationStampFileName))
	if err != nil {
		t.Fatal(err)
	}
	refreshInstructionProfilesOnStart(dir, canonicalVersion(), logger)
	stampAfter, err := os.ReadFile(filepath.Join(dir, profiles.GenerationStampFileName))
	if err != nil || string(stampBefore) != string(stampAfter) {
		t.Fatal("second start changed generator metadata")
	}
	activeAfter, err := os.ReadFile(filepath.Join(dir, profiles.ActiveFileName))
	if err != nil || string(activeAfter) != string(activeBefore) {
		t.Fatal("second start changed active profile bytes")
	}
	if got := logs.FilterMessage("daemon: regenerated instruction profiles").Len(); got != 1 {
		t.Fatalf("startup refresh logged %d regeneration lines, want1", got)
	}
	const custom = "my custom instructions\n"
	if err := os.WriteFile(filepath.Join(dir, "core.md"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	refreshInstructionProfilesOnStart(dir, "v-next", logger)
	data, err := os.ReadFile(filepath.Join(dir, "core.md"))
	if err != nil || string(data) != custom {
		t.Fatal("startup overwrote user content")
	}
	warnings := logs.FilterMessage("daemon: could not refresh instruction profiles; custom files preserved").All()
	if len(warnings) != 1 || !strings.Contains(warnings[0].ContextMap()["error"].(string), "move it aside") {
		t.Fatalf("conflict was not actionable/nonfatal: %+v", warnings)
	}
}

// Upgrading profile files does not rewrite an already installed inline block.
// Exercise the supported switch path against an old managed body, preserving
// surrounding user instructions rather than claiming daemon refresh does this.
func TestInstructionsSwitchRefreshesOldManagedInlineToolGuidance(t *testing.T) {
	home, _ := sandboxInstructionsEnv(t)
	dir := profiles.DefaultDir()
	if err := profiles.Generate(dir, "old-install"); err != nil {
		t.Fatal(err)
	}
	path := codex.GlobalInstructionsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	const before = "User instructions before.\n"
	const after = "\nUser instructions after.\n"
	oldBody := strings.Replace(agents.InstructionsBody, toolref.ToolSurfacePolicyText, "", 1)
	oldFile := before + agents.GlobalRulesStartMarker + "\n" + oldBody + "\n" + agents.GlobalRulesEndMarker + after
	if err := os.WriteFile(path, []byte(oldFile), 0600); err != nil {
		t.Fatal(err)
	}
	cmd, _, _ := captureCmd()
	if err := runInstructionsRegen(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != oldFile {
		t.Fatal("regen unexpectedly claimed to refresh inline instructions", err)
	}
	cmd, out, _ := captureCmd()
	if err := runInstructionsSwitch(cmd, []string{"full"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, before) || !strings.HasSuffix(got, after) {
		t.Fatal("switch rewrote surrounding user instructions")
	}
	if !strings.Contains(got, toolref.ToolSurfacePolicyText) || strings.Count(got, agents.GlobalRulesStartMarker) != 1 {
		t.Fatal("old managed inline body was not replaced exactly once")
	}
	if !strings.Contains(out.String(), "NEW sessions only") {
		t.Fatal("missing new-session requirement")
	}
}
