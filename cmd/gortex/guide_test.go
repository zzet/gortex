package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestGuideCmd_PrintsRelocatedReference verifies the `gortex guide` verb
// returns the relocated reference content — the CLI path for harnesses
// without MCP-resource support.
func TestGuideCmd_PrintsRelocatedReference(t *testing.T) {
	run := func(args ...string) string {
		var buf bytes.Buffer
		guideCmd.SetOut(&buf)
		guideCmd.SetErr(&buf)
		guideCmd.Run(guideCmd, args)
		return buf.String()
	}

	full := run()
	if !strings.Contains(full, "# Gortex Guide") {
		t.Error("full guide missing header")
	}
	if !strings.Contains(full, "bedrock` / `deepseek` / `requesty`") {
		t.Error("full guide missing the provider matrix")
	}

	providers := run("providers")
	if !strings.Contains(providers, "AWS Bedrock Converse") {
		t.Error("`gortex guide providers` missing provider detail")
	}

	analyze := run("analyze")
	if !strings.Contains(analyze, "analyze") {
		t.Error("`gortex guide analyze` missing the analyze catalog")
	}
}
