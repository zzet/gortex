package mcp

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/testenv"
)

// TestMain pins the instruction-profile env override to the default
// profile for the whole package: a developer machine that ran
// `gortex instructions switch` must not change the behavior of
// unrelated internal/mcp tests. Tests that exercise the profile path
// stub activeInstructionPreset directly.
func TestMain(m *testing.M) {
	os.Setenv(profiles.ActiveEnv, profiles.DefaultName)

	// NewServer constructs the query logger and mounts the global memory store
	// eagerly. Sandbox the whole process so both XDG_CACHE_HOME and the
	// XDG_DATA_HOME-backed ~/.gortex/memories path stay out of the developer's
	// real home.
	restore, err := testenv.SandboxProcess()
	if err != nil {
		fmt.Fprintf(os.Stderr, "internal/mcp: cannot sandbox the test environment: %v\n", err)
		os.Exit(1)
	}
	// query_log.go lets an ambient GORTEX_QUERY_LOG override CacheDir, so clear
	// it and let the logger resolve the sandboxed cache directory.
	_ = os.Unsetenv("GORTEX_QUERY_LOG")

	code := m.Run()
	restore()
	os.Exit(code)
}

// stubActiveProfilePreset swaps the machine-state reader for the test.
func stubActiveProfilePreset(t *testing.T, preset string) {
	t.Helper()
	prev := activeInstructionPreset
	activeInstructionPreset = func() string { return preset }
	t.Cleanup(func() { activeInstructionPreset = prev })
}

func TestInstructionProfilePolicy_AppliesOnDefaultConfig(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	stubActiveProfilePreset(t, "localization")

	p := srv.instructionProfilePolicy()
	require.NotNil(t, p, "shipped-default config must let the active profile refine the surface")
	require.Equal(t, "localization", p.preset)
	require.True(t, p.deferMode(), "profile policy inherits the server's defer mode")
	require.True(t, p.allows("search_symbols"))
	require.True(t, p.allows("smart_context"))
	require.False(t, p.allows("edit_file"), "the localization surface is read-only")

	// Session resolution: the profile beats the client-aware default…
	sp := srv.resolveSessionPolicy("", "", true)
	require.NotNil(t, sp)
	require.Equal(t, "localization", sp.preset)

	// …but a forwarded spec beats the profile.
	sp = srv.resolveSessionPolicy("edit", "", true)
	require.NotNil(t, sp)
	require.Equal(t, "edit", sp.preset)
}

func TestInstructionProfilePolicy_DefaultProfileIsNoOp(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	stubActiveProfilePreset(t, "")
	require.Nil(t, srv.instructionProfilePolicy(),
		"the core profile carries no preset and must resolve exactly as before")
}

func TestInstructionProfilePolicy_OperatorPinWins(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "agent", Mode: "defer"})
	stubActiveProfilePreset(t, "localization")
	require.Nil(t, srv.instructionProfilePolicy(),
		"an operator-pinned mcp.tools config must not be overridden by the profile")
}

func TestOperatorPinnedToolPolicy(t *testing.T) {
	cases := []struct {
		name string
		cfg  ToolPolicyConfig
		want bool
	}{
		{"zero config", ToolPolicyConfig{}, false},
		{"shipped default", ToolPolicyConfig{Preset: "core", Mode: "defer"}, false},
		{"explicit shipped values", ToolPolicyConfig{Preset: "core", Mode: "defer", OperatorPinned: true}, true},
		{"default alias", ToolPolicyConfig{Preset: "default", Mode: "defer"}, false},
		{"core in hide mode", ToolPolicyConfig{Preset: "core", Mode: "hide"}, true},
		{"named preset", ToolPolicyConfig{Preset: "agent", Mode: "defer"}, true},
		{"allow delta", ToolPolicyConfig{Preset: "core", Mode: "defer", Allow: []string{"analyze"}}, true},
		{"deny delta", ToolPolicyConfig{Deny: []string{"edit_file"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, operatorPinnedToolPolicy(tc.cfg))
		})
	}
}

func TestOperatorPinnedToolPolicy_EnvPins(t *testing.T) {
	t.Setenv(toolPresetEnv, "nav")
	require.True(t, operatorPinnedToolPolicy(ToolPolicyConfig{Preset: "core", Mode: "defer"}))
}

// TestLocalizationPresetMatchesProfileTable is the cross-package
// no-drift gate: the preset registered here must be exactly the eager
// list the instruction-profile table declares.
func TestLocalizationPresetMatchesProfileTable(t *testing.T) {
	require.Equal(t, profiles.LocalizationEagerTools(), localizationPresetTools)
	set, denyMutating, known := builtinToolPresetSet("localization")
	require.True(t, known)
	require.False(t, denyMutating)
	require.Equal(t, toToolSet(profiles.LocalizationEagerTools()), set)
}
