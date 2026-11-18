package workflowtest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type publishStep struct {
	Name string            `yaml:"name"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
}

func publishSteps(t *testing.T) []publishStep {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "publish-claude-plugin.yml"))
	require.NoError(t, err)
	var workflow struct {
		Jobs map[string]struct {
			Steps []publishStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(data, &workflow))
	steps := workflow.Jobs["publish"].Steps
	require.NotEmpty(t, steps)
	return steps
}

func findPublishStep(t *testing.T, name string) publishStep {
	t.Helper()
	for _, step := range publishSteps(t) {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("missing publish step %q", name)
	return publishStep{}
}

// These tests run in the existing go test ./... CI job. Inspect the actual
// workflow so a future reintroduction of expression-to-shell interpolation
// cannot evade the behavioral tests by bypassing environment variables.
func TestPublishPluginExpressionsStayOutOfShell(t *testing.T) {
	for _, step := range publishSteps(t) {
		require.NotContains(t, step.Run, "${{", "%s: pass expressions through env or action inputs", step.Name)
	}
	resolve := findPublishStep(t, "Resolve gortex ref")
	require.Equal(t, "${{ inputs.ref }}", resolve.Env["DISPATCH_REF"])
	summary := findPublishStep(t, "Summary")
	require.Equal(t, "${{ steps.resolve.outputs.ref }}", summary.Env["GORTEX_REF"])
	require.Equal(t, "${{ steps.diff.outputs.changed }}", summary.Env["CONTENT_CHANGED"])
	checkout := findPublishStep(t, "Checkout gortex")
	require.Equal(t, "${{ steps.resolve.outputs.ref }}", checkout.With["ref"])
	require.Equal(t, "false", checkout.With["persist-credentials"])
}

// Execute only the resolver/summary shell from the workflow, in a temporary
// directory with no credentials. Never check out, build, publish, or dispatch
// a workflow as part of these tests.
func runPublishScript(t *testing.T, step publishStep, values map[string]string) (string, string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the publishing workflow runs Bash on ubuntu-latest")
	}
	root := t.TempDir()
	output := filepath.Join(root, "output")
	summary := filepath.Join(root, "summary")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", step.Run)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"GITHUB_OUTPUT=" + output,
		"GITHUB_STEP_SUMMARY=" + summary,
	}
	for key, value := range values {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	logs, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("%s: %s", step.Name, logs)
	}
	_, markerErr := os.Stat(filepath.Join(root, "injected"))
	require.True(t, os.IsNotExist(markerErr), "input executed a command: %v", markerErr)
	read := func(path string) string {
		data, readErr := os.ReadFile(path)
		if os.IsNotExist(readErr) {
			return ""
		}
		require.NoError(t, readErr)
		return string(data)
	}
	return read(output), read(summary), err
}

func TestPublishPluginResolveRef(t *testing.T) {
	resolve := findPublishStep(t, "Resolve gortex ref")
	for _, event := range []string{"workflow_dispatch", "release"} {
		t.Run(event, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				ref  string
				ok   bool
			}{
				{"release tag", "v1.2.3", true},
				{"prerelease", "v1.2.3-rc.1+build.42", true},
				{"branch", "main", true},
				{"branch path", "release/plugin-fix_1", true},
				{"qualified tag", "refs/tags/v1.2.3", true},
				{"commit", strings.Repeat("a", 40), true},
				{"empty", "", false},
				{"quote breakout", `main"; printf injected > injected; #`, false},
				{"command substitution", "main$(printf injected > injected)", false},
				{"backticks", "main`printf injected > injected`", false},
				{"valid git ref with shell syntax", "v1.2.3$(>injected)", false},
				{"output injection", "main\nextra=value", false},
				{"carriage return", "main\rextra=value", false},
				{"trailing newline", "main\n", false},
				{"leading option", "-main", false},
				{"spaces", " main ", false},
				{"revision expression", "main~1", false},
				{"double dot", "release/../main", false},
				{"double slash", "release//main", false},
				{"lock suffix", "release/main.lock", false},
				{"trailing dot", "main.", false},
				{"hidden component", "release/.hidden", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					values := map[string]string{
						"GITHUB_EVENT_NAME": event,
						"DISPATCH_REF":      "unused$(>injected)",
						"GITHUB_REF_NAME":   "unused$(>injected)",
					}
					if event == "workflow_dispatch" {
						values["DISPATCH_REF"] = tc.ref
					} else {
						values["GITHUB_REF_NAME"] = tc.ref
					}
					output, _, err := runPublishScript(t, resolve, values)
					if tc.ok {
						require.NoError(t, err)
						require.Equal(t, "ref="+tc.ref+"\n", output)
					} else {
						require.Error(t, err)
						require.Empty(t, output, "invalid input must not write a workflow output")
					}
				})
			}
		})
	}
}

func TestPublishPluginSummaryTreatsValuesAsData(t *testing.T) {
	summary := findPublishStep(t, "Summary")
	for _, value := range []string{"v1.2.3", "", `main"; printf injected > injected; #`, "$(printf injected > injected)", "`printf injected > injected`"} {
		t.Run(value, func(t *testing.T) {
			output, result, err := runPublishScript(t, summary, map[string]string{
				"GORTEX_REF":      value,
				"PLUGIN_REPO":     value,
				"CONTENT_CHANGED": value,
			})
			require.NoError(t, err)
			require.Empty(t, output)
			require.Equal(t, "### Publish summary\n\n- Source ref: `"+value+"`\n- Target repo: `"+value+"`\n- Content changed: `"+value+"`\n", result)
		})
	}
}
