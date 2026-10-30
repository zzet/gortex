package main

import (
	"strings"
	"testing"
)

func TestEditProbeMoveRefusalsDistinguishApplicabilityLanguageAndMissingEvidence(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		for _, tool := range []string{"move_symbol", "refactor.move"} {
			t.Run(language+"/"+tool, func(t *testing.T) {
				c := editProbeSymbolCorpus(t, language)
				c.EditTool = tool
				c.EditKind = "relocate"
				_, err := makeEditProbePlan(c, 42, 1)
				want := "unsupported language"
				if language == "go" {
					want = "lacks per-file publication observations"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("missing explicit refusal: %v", err)
				}
				if language == "go" {
					for _, kind := range []string{"append_leaf", "modify_body", "change_signature", "rename", "delete"} {
						c.EditKind = kind
						if _, err := makeEditProbePlan(c, 42, 1); err == nil || !strings.Contains(err.Error(), "applies to relocation") {
							t.Fatalf("inapplicable move kind %s admitted: %v", kind, err)
						}
					}
				}
			})
		}
	}
}
