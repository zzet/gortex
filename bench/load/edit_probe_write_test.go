package main

import (
	"os"
	"strings"
	"testing"
)

func editProbeWriteCorpus(t *testing.T, language string) Corpus {
	t.Helper()
	c := editProbeCorpus()
	switch language {
	case "go":
		c.EditInitialContent = "package sample\nfunc Example() int { return 1 }\n" + c.EditAnchor + "\n"
	case "typescript":
		c = editProbeTypeScriptCorpus()
		data, err := os.ReadFile("testdata/edit-probe/typescript/main.ts")
		if err != nil {
			t.Fatal(err)
		}
		c.EditInitialContent = string(data)
	case "python":
		c = editProbePythonCorpus()
		data, err := os.ReadFile("testdata/edit-probe/python/main.py")
		if err != nil {
			t.Fatal(err)
		}
		c.EditInitialContent = string(data)
	}
	c.EditTool = "edit.write"
	return c
}

func TestEditProbeWriteHasImmutableOfferedPrefixAndSameSchedule(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			c := editProbeWriteCorpus(t, language)
			plan, err := makeEditProbePlan(c, 42, 100)
			if err != nil {
				t.Fatal(err)
			}
			file := c
			file.EditTool = ""
			file.EditInitialContent = ""
			reference, err := makeEditProbePlan(file, 42, 100)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Tool != "edit.write" || len(plan.Steps) != 100 {
				t.Fatal("wrong tool/denominator")
			}
			for i, step := range plan.Steps {
				req := step.Request
				content, ok := req.Arguments["content"].(string)
				if !ok || req.Tool != "edit" || req.Operation != "write" || req.Arguments["operation"] != "write" || req.Arguments["target"].(map[string]any)["file"] != c.File || req.Arguments["options"].(map[string]any)["mutation_id"] != step.MutationID {
					t.Fatal("wrong public write arguments", req)
				}
				if step.DueNS != reference.Steps[i].DueNS || step.ActivateNS != reference.Steps[i].ActivateNS || step.Marker != reference.Steps[i].Marker {
					t.Fatal("tool changed offered schedule")
				}
				if !editProbeFixtureSource(c.File, content, step.Marker, plan.Callee) {
					t.Fatal("write missing actual intrafile call")
				}
				for j, other := range plan.Steps {
					if strings.Contains(content, other.Marker) != (j <= i) {
						t.Fatal("content is not exact offered prefix", i, j)
					}
				}
				hash, err := hashJSON(req)
				if err != nil || hash != step.RequestHash {
					t.Fatal("write intent hash mismatch")
				}
			}
			if err := editProbeWriteAdmission(c, []byte(c.EditInitialContent)); err != nil {
				t.Fatal(err)
			}
			if err := editProbeWriteAdmission(c, []byte(c.EditInitialContent+"\n")); err == nil {
				t.Fatal("drifted initial bytes admitted")
			}
			// Apply later intent first, then earlier intent. The oracle must reject the
			// genuinely overwritten later marker; neither request is regenerated.
			earlier := plan.Steps[0].Request.Arguments["content"].(string)
			if editProbeFixtureSource(c.File, earlier, plan.Steps[1].Marker, plan.Callee) {
				t.Fatal("lost later write became correct")
			}
			if got := plan.Steps[1].Request.Arguments["content"].(string); !strings.Contains(got, plan.Steps[0].Marker) {
				t.Fatal("intent was refreshed after reordering")
			}
		})
	}
}

func TestEditProbeWriteRefusesUnknownOrUnpinnedIntent(t *testing.T) {
	c := editProbeWriteCorpus(t, "go")
	for _, bad := range []Corpus{
		func() Corpus { x := c; x.EditTool = "edit.symbol"; return x }(),
		func() Corpus { x := c; x.EditInitialContent = ""; return x }(),
		func() Corpus { x := c; x.EditInitialContent += c.EditAnchor; return x }(),
		func() Corpus { x := c; x.EditInitialContent = "not Go " + c.EditAnchor; return x }(),
		func() Corpus { x := c; x.EditInitialContent += string([]byte{0xff}); return x }(),
		func() Corpus { x := c; x.EditTool = "edit.file"; return x }(),
	} {
		if _, err := makeEditProbePlan(bad, 42, 1); err == nil {
			t.Fatal("invalid write plan admitted", bad.EditTool)
		}
	}
}

func TestEditProbeWriteOraclesRejectForeignStaleAndCommentOnlyBytes(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			c := editProbeWriteCorpus(t, language)
			p, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			step := p.Steps[0]
			content := step.Request.Arguments["content"].(string)
			body := map[string]any{"path": c.File, "content": content}
			if !editProbeCorrect("file", body, c, step, p.Callee) {
				t.Fatal("actual write source refused")
			}
			for _, bad := range []map[string]any{
				{"path": "foreign/other.go", "content": content},
				{"path": c.File, "content": c.EditInitialContent},
				{"path": c.File, "content": strings.ReplaceAll(content, "Example()", "Other()")},
				{"path": c.File, "content": "/*" + content + "*/"},
			} {
				if editProbeCorrect("file", bad, c, step, p.Callee) {
					t.Fatal("wrong file/callee/stale/comment evidence accepted")
				}
			}
			source := map[string]any{"id": c.File + "::" + step.Marker, "file_path": c.File, "name": step.Marker, "source": content}
			if !editProbeCorrect("source", source, c, step, p.Callee) {
				t.Fatal("real source identity refused")
			}
			source["id"] = "foreign.go::" + step.Marker
			if editProbeCorrect("source", source, c, step, p.Callee) {
				t.Fatal("foreign producer identity accepted")
			}
		})
	}
}
