package codex

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

func TestInstallMutationPostToolUseCodexEveryHookMode(t *testing.T) {
	for _, mode := range []string{"deny", "enrich", "rewrite", "suppress"} {
		t.Run(mode, func(t *testing.T) {
			env := codexGlobalEnv(t)
			t.Setenv(codexHookModeEnvVar, mode)
			if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
				t.Fatal(err)
			}
			cfg := readCodexConfig(t, env)
			requireHookEntry(t, cfg, "PostToolUse", codexPostToolUseMatcher, strings.TrimSuffix(testCodexHookCommand, "enrich")+mode)
			matcher := regexp.MustCompile(codexPostToolUseMatcher)
			for _, prefix := range []string{"mcp__gortex__", "gortex__"} {
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
