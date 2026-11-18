package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWarnIfWorkspaceConfigIgnored(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, ".gortex.yaml")

	// No file → silent.
	var buf bytes.Buffer
	warnIfWorkspaceConfigIgnored(&buf, root)
	assert.Empty(t, buf.String())

	// Malformed → loud warning with path and parse error.
	require.NoError(t, os.WriteFile(cfgPath, []byte("exclude: [\"broken\n"), 0644))
	buf.Reset()
	warnIfWorkspaceConfigIgnored(&buf, root)
	assert.Contains(t, buf.String(), cfgPath)
	assert.Contains(t, buf.String(), "failed to parse")

	// Valid file with the index.ignore typo → unknown-keys warning.
	require.NoError(t, os.WriteFile(cfgPath, []byte("index:\n  ignore:\n    - build/**\n"), 0644))
	buf.Reset()
	warnIfWorkspaceConfigIgnored(&buf, root)
	assert.Contains(t, buf.String(), "keys gortex does not recognize")
	assert.Contains(t, buf.String(), "index.ignore")

	// Fully valid file → silent.
	require.NoError(t, os.WriteFile(cfgPath, []byte("exclude:\n  - ok/**\n"), 0644))
	buf.Reset()
	warnIfWorkspaceConfigIgnored(&buf, root)
	assert.Empty(t, buf.String())
}

// TestWarnIfWorkspaceConfigIgnored_Agreement pins that init's warning
// uses the daemon's acceptance semantics (config.ParseWorkspaceFile),
// not config.Load: viper's weak decode and the workspace schema rules
// accept/reject a different file set than yaml.Unmarshal does. The
// table mirrors TestWorkspaceParseAgreementWithSurfaces (internal/config)
// and TestRunConfigExcludeList_Agreement — keep the three in sync.
func TestWarnIfWorkspaceConfigIgnored_Agreement(t *testing.T) {
	cases := []struct {
		name string
		body string
		// warn = init must say "failed to parse" (the daemon ignores the
		// file); false = init must stay silent (the daemon accepts it).
		warn bool
	}{
		{"valid list", "exclude:\n  - vendor/**\n", false},
		// The silent-fallback trap: viper accepts a scalar exclude, so
		// routing init through config.Load let the daemon drop the file
		// with no warning from init. With the shared parser it warns.
		{"scalar exclude", "exclude: vendor/\n", true},
		// Schema violations are not parse failures — the daemon accepts
		// and uses the file, so init must not raise a false alarm.
		{"schema violation, project+projects", "project: a\nprojects:\n  - name: p\n    paths: [\"x/**\"]\n", false},
		{"stray quote", "exclude:\n  - \"broken\n", true},
	}

	root := t.TempDir()
	cfgPath := filepath.Join(root, ".gortex.yaml")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(cfgPath, []byte(tc.body), 0644))
			var buf bytes.Buffer
			warnIfWorkspaceConfigIgnored(&buf, root)
			if tc.warn {
				assert.Contains(t, buf.String(), "failed to parse")
			} else {
				assert.Empty(t, buf.String())
			}
		})
	}
}
