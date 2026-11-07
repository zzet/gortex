package codex

import (
	"os"
	"regexp"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/zzet/gortex/internal/agents"
)

// StaleConfigs reports ~/.codex/config.toml when the Gortex hooks in it no
// longer match what `gortex install --agents=codex` from this binary writes:
// a moved binary, a changed matcher, a new event. The file is never rewritten
// here. Codex records trust against each hook's hash and skips a changed hook
// until the user approves it again, so a silent rewrite after an upgrade
// would turn every Gortex hook off (#1052).
//
// The judgement is install's own: upsertCodexHooks runs on an in-memory copy,
// and the file is stale exactly when install would change it. A config with
// no Gortex hooks (never installed, or --no-hooks) is not reported.
func (a *Adapter) StaleConfigs(env agents.Env) []agents.StaleConfig {
	if env.Mode != agents.ModeGlobal || env.Home == "" {
		return nil
	}
	_, path := UserHookFiles(env.Home)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	root := map[string]any{}
	if toml.Unmarshal(data, &root) != nil {
		return nil
	}
	counts := map[string]int{}
	countGortexHooks(root, counts)
	if totalHooks(counts) == 0 {
		return nil
	}
	// The posture comes from GORTEX_CODEX_HOOK_MODE when install ran, which
	// the process checking now may not have; it is the user's choice, not
	// drift, so compare the hooks as if they carried this process's mode.
	pinCodexHookModes(root, codexHookMode())
	if !upsertCodexHooks(root, env, agents.ApplyOpts{}) {
		return nil
	}
	return []agents.StaleConfig{{Path: path, Note: codexHookTrustNotice}}
}

var codexModeFlag = regexp.MustCompile(`--mode=\S*`)

// pinCodexHookModes rewrites the --mode= flag of every Gortex hook command in
// root to mode.
func pinCodexHookModes(root map[string]any, mode string) {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return
	}
	for _, list := range hooks {
		entries, ok := codexHookList(list)
		if !ok {
			continue
		}
		for _, entry := range entries {
			group, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			handlers, _ := codexHookList(group["hooks"])
			for _, handler := range handlers {
				fields, ok := handler.(map[string]any)
				if !ok {
					continue
				}
				if cmd, _ := fields["command"].(string); codexCommandInvokesCodexHook(cmd) {
					fields["command"] = codexModeFlag.ReplaceAllString(cmd, "--mode="+mode)
				}
			}
		}
	}
}
