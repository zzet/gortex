package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSharedEditProbeScopeRequiresActualPhysicalBindings(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	for _, dir := range []string{root, other} {
		if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("package p\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"shared", "separate_worktree", "unknown_session", "linked", "routed", "single_writer", "different_file"} {
		t.Run(mode, func(t *testing.T) {
			p := &privateRun{Repos: []privateRepo{{Path: root}}}
			plan := editProbePlan{GateModes: true, Editors: 2, Kind: "add_intrafile_call", Tool: "edit.file", File: "sample.go"}
			for editor := range 2 {
				plan.Steps = append(plan.Steps, editProbeStep{Editor: editor, Request: request{Tool: "edit", Operation: "file", Arguments: map[string]any{"target": map[string]any{"file": "sample.go"}}}})
			}
			editors := []*socketClient{{cwd: root}, {cwd: root}}
			switch mode {
			case "separate_worktree":
				editors[1].cwd = other // Same file name/contents does not establish shared ownership.
			case "unknown_session":
				editors[1].cwd = ""
			case "linked":
				plan.LinkedWorktrees = 1
			case "routed":
				plan.Routes = []editProbeRoute{{ID: "other"}}
			case "single_writer":
				plan.Steps[1].Editor = 0
			case "different_file":
				plan.Steps[1].Request.Arguments["target"] = map[string]any{"file": "other.go"}
			}
			scope := sharedEditProbePhysicalScope(p, plan, editors)
			if (scope != nil) != (mode == "shared") {
				t.Fatalf("physical scope=%+v for %s", scope, mode)
			}
		})
	}
}
