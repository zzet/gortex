// ci-result reports one stable CI status for the validated test plan.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type ciNeed struct {
	Result  string            `json:"result"`
	Outputs map[string]string `json:"outputs,omitempty"`
}

func checkCIResult(needs map[string]ciNeed, event string) error {
	require := func(name, expected string) error {
		if needs[name].Result != expected {
			return fmt.Errorf("%s must be %s", name, expected)
		}
		return nil
	}
	if err := require("plan", "success"); err != nil {
		return err
	}
	outputs := needs["plan"].Outputs
	mode, hasTests := outputs["mode"], outputs["has_tests"]
	if (mode != "affected" && mode != "full") || (hasTests != "true" && hasTests != "false") {
		return fmt.Errorf("planning outputs are missing or invalid")
	}
	if hasTests == "false" && (mode != "affected" || outputs["empty_ok"] != "true") {
		return fmt.Errorf("empty test selection was not explicitly validated")
	}
	expected := map[string]string{
		"test": "success", "smoke": "success", "build-native": "success",
		"lint": "success", "build-linux-static": "success", "build-onnx": "success",
		"benchmark": "skipped", "parser-stress": "skipped",
	}
	if hasTests == "false" {
		expected["test"] = "skipped"
	}
	if mode == "full" {
		expected["smoke"] = "skipped"
	}
	if event == "pull_request" {
		expected["benchmark"] = "success"
	}
	if event == "workflow_dispatch" {
		expected["parser-stress"] = "success"
	}
	// Stable ordering also makes receipts with multiple failures diagnostic.
	for _, name := range []string{"test", "smoke", "build-native", "lint", "build-linux-static", "build-onnx", "benchmark", "parser-stress"} {
		if err := require(name, expected[name]); err != nil {
			return err
		}
	}
	return nil
}

func run(payload, event string, stdout, stderr io.Writer) int {
	var needs map[string]ciNeed
	if err := json.Unmarshal([]byte(payload), &needs); err != nil {
		fmt.Fprintf(stderr, "CI result: invalid CI_NEEDS: %v\n", err)
		return 1
	}
	if err := checkCIResult(needs, event); err != nil {
		fmt.Fprintf(stderr, "CI result: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "All required CI checks passed for the validated test selection.")
	return 0
}

func main() {
	os.Exit(run(os.Getenv("CI_NEEDS"), os.Getenv("GITHUB_EVENT_NAME"), os.Stdout, os.Stderr))
}
