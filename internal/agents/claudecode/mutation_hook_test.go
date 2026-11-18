package claudecode

import (
	"io"
	"path/filepath"
	"regexp"
	"testing"
)

func TestInstallMutationPostToolUseInEveryHookMode(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, mode := range []string{HookModeDeny, HookModeEnrich, HookModeConsultUnlock, HookModeAdaptiveNudge} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.local.json")
			if _, err := InstallHookWithMode(io.Discard, path, mode, agentsApplyOptsZero()); err != nil {
				t.Fatal(err)
			}
			hooks := readSettingsHooks(t, path)
			groups, ok := hooks["PostToolUse"].([]any)
			if !ok || len(groups) != 1 {
				t.Fatalf("expected one managed observer, got %#v", hooks["PostToolUse"])
			}
			matcher := regexp.MustCompile("^(" + groups[0].(map[string]any)["matcher"].(string) + ")$")
			for _, prefix := range []string{userMCPToolPrefix, pluginMCPToolPrefix} {
				for _, verb := range []string{"edit", "edit_file", "write_file", "edit_symbol", "batch_edit"} {
					if !matcher.MatchString(prefix + verb) {
						t.Errorf("mutation %s%s missing in %s", prefix, verb, mode)
					}
				}
			}
			for _, unrelated := range []string{"Edit", "Write", "mcp__other__edit_file", "mcp__gortex__preview_edit"} {
				if matcher.MatchString(unrelated) {
					t.Errorf("mutation observer matched unrelated tool %s", unrelated)
				}
			}
		})
	}
}
