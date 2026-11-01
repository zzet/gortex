package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func editProbeBodyCorpus(t *testing.T, language, kind string, count int) Corpus {
	t.Helper()
	c := editProbeSymbolCorpus(t, language)
	c.EditKind = kind
	switch language {
	case "go":
		c.EditBeforeTemplate = "func {{marker}}() int { return 1 }"
		c.EditTemplate = "func {{marker}}() int {\n return Example()\n}"
	case "typescript":
		c.EditBeforeTemplate = "export function {{marker}}(): number { return 1; }"
		c.EditTemplate = "export function {{marker}}(): number {\n return Example();\n}"
	case "python":
		c.EditBeforeTemplate = "def {{marker}}():\n    return 1"
		c.EditTemplate = "def {{marker}}():\n\n    return Example()"
	}
	if kind == "modify_caller_body" {
		c.EditBeforeTemplate = strings.Replace(c.EditBeforeTemplate, "return 1", "return Alternate()", 1)
		c.EditInitialContent += "\n\n" + editProbeFollowingDeclaration(c.File, "Alternate")
	}
	for i := 0; i < count; i++ {
		marker := fmt.Sprintf("GortexEditProbe%016xN%03d", uint64(42), i)
		c.EditInitialContent += "\n\n" + strings.ReplaceAll(c.EditBeforeTemplate, "{{marker}}", marker+"Before") + "\n\n" + editProbeFollowingDeclaration(c.File, marker+"After")
	}
	return c
}

func TestEditProbeBodyChangesRetainTargetAndUnchangedRelativeWitness(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		for _, kind := range []string{"modify_body", "modify_caller_body"} {
			for _, tool := range []string{"edit.file", "edit.write", "edit.symbol", "edit.batch"} {
				t.Run(language+"/"+kind+"/"+tool, func(t *testing.T) {
					c := editProbeBodyCorpus(t, language, kind, 100)
					c.EditTool = tool
					plan, err := makeEditProbePlan(c, 42, 100)
					if err != nil {
						t.Fatal(err)
					}
					for _, step := range plan.Steps {
						if step.TargetSymbol != c.File+"::"+step.Marker+"Before" || step.FollowingSymbol != c.File+"::"+step.Marker+"After" || step.ExpectedLineGap < 3 {
							t.Fatal("body change lost precreated target/follower")
						}
						if len(editProbeOracles(c, step)) != 7 {
							t.Fatal("body cell changed seven-oracle denominator")
						}
					}
				})
			}
		}
	}
}

func TestEditProbeBodyRelativeGapCannotBorrowUpstreamMovementOrNamePresence(t *testing.T) {
	c := editProbeBodyCorpus(t, "go", "modify_body", 1)
	plan, err := makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[0]
	for _, variant := range []string{"after", "upstream_shift_after", "before", "upstream_shift_before", "wrong_follower", "duplicate_target", "missing_line"} {
		t.Run(variant, func(t *testing.T) {
			start := 10
			gap := step.ExpectedLineGap
			if variant == "before" || variant == "upstream_shift_before" {
				gap -= 2
			}
			if strings.HasPrefix(variant, "upstream_shift") {
				start += 200
			}
			target := map[string]any{"id": "owned/" + step.TargetSymbol, "start_line": json.Number(fmt.Sprint(start))}
			follower := map[string]any{"id": "owned/" + step.FollowingSymbol, "start_line": json.Number(fmt.Sprint(start + gap))}
			rows := []any{target, follower}
			switch variant {
			case "wrong_follower":
				follower["id"] = "owned/other.go::Following"
			case "duplicate_target":
				rows = append(rows, target)
			case "missing_line":
				delete(target, "start_line")
			}
			want := variant == "after" || variant == "upstream_shift_after"
			for _, oracle := range []string{"summary", "editing_context", "search_visible"} {
				key := map[string]string{"summary": "nodes", "editing_context": "defines", "search_visible": "results"}[oracle]
				if got := editProbeCorrect(oracle, map[string]any{key: rows}, c, step, plan.Callee); got != want {
					t.Fatalf("%s correct=%v", oracle, got)
				}
			}
		})
	}
}

func TestEditProbeBodySourceAndTextRequireActualNewCallInExactTarget(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			c := editProbeBodyCorpus(t, language, "modify_body", 1)
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			step := plan.Steps[0]
			name := step.Marker + "Before"
			after := strings.ReplaceAll(c.EditTemplate, "{{marker}}", name)
			before := strings.ReplaceAll(c.EditBeforeTemplate, "{{marker}}", name)
			for _, variant := range []string{"after", "before", "wrong_file", "wrong_id", "wrong_callee"} {
				t.Run(variant, func(t *testing.T) {
					body := map[string]any{"id": "owned/" + step.TargetSymbol, "file_path": "owned/" + c.File, "name": name, "source": after}
					switch variant {
					case "before":
						body["source"] = before
					case "wrong_file":
						body["file_path"] = "owned/other.go"
					case "wrong_id":
						body["id"] = "owned/other.go::" + name
					case "wrong_callee":
						body["source"] = strings.ReplaceAll(after, "Example()", "Other()")
					}
					if got := editProbeCorrect("source", body, c, step, plan.Callee); got != (variant == "after") {
						t.Fatalf("source correct=%v", got)
					}
				})
			}
			statement := "return Example()"
			if language == "typescript" {
				statement += ";"
			}
			row := map[string]any{"symbol_id": "owned/" + step.TargetSymbol, "path": "owned/" + c.File, "text": statement}
			if !editProbeCorrect("text", map[string]any{"matches": []any{row}}, c, step, plan.Callee) {
				t.Fatal("actual enclosed new call refused")
			}
			row["symbol_id"] = "owned/" + c.File + "::Other"
			if editProbeCorrect("text", map[string]any{"matches": []any{row}}, c, step, plan.Callee) {
				t.Fatal("foreign target supplied text evidence")
			}
		})
	}
}
