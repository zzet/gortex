package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestEditProbeDispatchPreservesExplicitSparseFields(t *testing.T) {
	const fields = "id,name,file_path,start_line,end_line"
	for _, format := range []string{"json", "toon"} {
		for _, operation := range []string{"summary", "editing_context"} {
			req := request{Tool: "read", Operation: operation, Arguments: map[string]any{"operation": operation, "target": map[string]any{"file": "sample.go"}, "output": map[string]any{"format": format, "fields": fields, "max_bytes": 0}}}
			got := dispatchArgs(req, privateRepo{Prefix: "owned"})
			want := map[string]any{"format": format, "fields": fields, "max_bytes": 0}
			if !reflect.DeepEqual(got["output"], want) {
				t.Fatalf("%s/%s output=%v, want %v", format, operation, got["output"], want)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var cliArgs map[string]any
			if err := json.Unmarshal(encoded, &cliArgs); err != nil {
				t.Fatal(err)
			}
			if cliArgs["output"].(map[string]any)["fields"] != fields {
				t.Fatal("CLI request lost sparse fields")
			}
		}
	}
	corpus := editProbeCorpus()
	step := editProbeStep{Marker: "Added"}
	for _, oracle := range []string{"summary", "editing_context"} {
		key := "nodes"
		if oracle == "editing_context" {
			key = "defines"
		}
		if editProbeCorrect(oracle, map[string]any{key: []any{map[string]any{"name": step.Marker}}}, corpus, step, "Example") {
			t.Fatal("a fieldset that removed identity silently proved coverage")
		}
	}
}

func TestEditProbeCLIPreservesExplicitSparseFields(t *testing.T) {
	const fields = "id,name,file_path,start_line,end_line"
	argvFile := filepath.Join(t.TempDir(), "argv.json")
	run, state, oracle := editProbeCLIFixture(t, 10*time.Second, "LOAD_EDIT_PROBE_CLI_FIXTURE=reply", "LOAD_EDIT_PROBE_CLI_ARGV="+argvFile, "LOAD_EDIT_PROBE_CLI_STDOUT={}")
	oracle.call.Arguments = copyJSON(oracle.call.Arguments)
	oracle.call.Arguments["output"] = map[string]any{"format": "json", "fields": fields, "max_bytes": 0}
	_, _, err := run.performCLI(t.Context(), state, oracle.call, "oracle:summary", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		t.Fatal(err)
	}
	if len(argv) != 11 {
		t.Fatalf("unexpected CLI argv %v", argv)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argv[5]), &args); err != nil {
		t.Fatal(err)
	}
	if args["output"].(map[string]any)["fields"] != fields {
		t.Fatal("actual CLI argv dropped explicit sparse fields")
	}
}

func TestEditProbeBuiltInSparseFieldsPreserveCoverage(t *testing.T) {
	const fields = "id,name,file_path,start_line,end_line"
	c := editProbeCorpus()
	c.EditKind = "add_intrafile_call"
	plan, err := makeEditProbePlan(c, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"json", "toon", "compact", "facade", "cli"} {
		t.Run(mode, func(t *testing.T) {
			corpus := c
			corpus.EditOutput = mode
			for _, oracle := range editProbeOracles(corpus, plan.Steps[0]) {
				output := dispatchArgs(oracle.call, privateRepo{Prefix: "owned"})["output"].(map[string]any)
				if oracle.name == "summary" || oracle.name == "editing_context" {
					if output["fields"] != fields {
						t.Fatalf("built-in %s/%s lost selected definition fields: %v", mode, oracle.name, output)
					}
				} else if _, changed := output["fields"]; changed {
					t.Fatalf("unrelated %s oracle was reshaped", oracle.name)
				}
			}
		})
	}
	for _, mode := range []string{"toon", "compact"} {
		t.Run("explicit_"+mode, func(t *testing.T) {
			req := request{Tool: "read", Operation: "summary", Arguments: map[string]any{"operation": "summary", "output": map[string]any{"fields": fields}}}
			output := dispatchArgs(editProbeWithOutput(req, mode), privateRepo{Prefix: "owned"})["output"].(map[string]any)
			if output["fields"] != fields {
				t.Fatalf("format selection dropped explicit sparse fields: %v", output)
			}
		})
	}
}
