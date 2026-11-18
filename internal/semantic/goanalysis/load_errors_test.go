package goanalysis

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/semantic"
)

// TestStrippedSiblingsLoadWithSoftErrorsOnly: a handle-rooted load strips
// the bodies of the root's sibling files, so their imports go unused. The
// type checker reports those as soft errors; they change no type and must
// not count as a degraded load, while a real type error still does.
func TestStrippedSiblingsLoadWithSoftErrorsOnly(t *testing.T) {
	root := typecheckCacheFixture(t)
	writeFile(t, root, "impl/fmtuser.go", "package impl\n\nimport \"fmt\"\n\n// Describe uses fmt only in its body.\nfunc Describe() string { return fmt.Sprint(1) }\n")
	cached := newTestProvider(t)
	program := func() loadErrorSummary {
		t.Helper()
		st := cached
		scope := semantic.WithCheckoutCompilerScope(t.Context(), cachedScope)
		result, err := st.EnrichRepoContext(scope, scopeHandleGraph(), "", root, nil)
		require.NoError(t, err)
		require.Empty(t, result.Compiler.Cache.Bypass)
		plan := handleRootPlan{rootDirs: map[string]struct{}{root + "/impl": {}}, handleAbs: map[string]struct{}{root + "/impl/impl.go": {}}, patterns: []string{"./impl"}}
		stats := &semantic.CompilerCacheStats{}
		out, err := cached.loadCheckoutProgramCached(t.Context(), root, "", plan, cachedScope, stats)
		require.NoError(t, err)
		defer out.release()
		return classifyLoadErrors(out.program.pkgs)
	}
	sum := program()
	require.Equal(t, 0, sum.hard, "stripped siblings leave only soft errors: %v", sum.sample)
	require.Greater(t, sum.soft, 0, "the stripped sibling's fmt import is unused")
	require.True(t, strings.Contains(strings.Join(sum.sample, "\n"), "\"fmt\" imported and not used"), sum.sample)

	writeFile(t, root, "impl/broken.go", "package impl\n\n// Broken does not type-check.\nvar Broken int = \"x\"\n")
	sum = program()
	require.Equal(t, 1, sum.hard, "a real type error degrades the load")
	require.Equal(t, 1, sum.kinds["type"])
	require.Contains(t, strings.Join(sum.sample, "\n"), "cannot use")
}
