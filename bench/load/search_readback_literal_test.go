package main

import (
	"strings"
	"testing"
)

func TestSearchReadbackLiteralExistsOnlyAfterOfferedRename(t *testing.T) {
	corpus := editProbeRenameCorpus(t, "go", 1000)
	corpus.EditTool = "edit.file"
	options, err := parseRun(append(searchAcceptanceArgs(4, 1000), "--search-readback-acceptance"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planEditProbe(corpus, options)
	if err != nil {
		t.Fatal(err)
	}
	repo := privateRepo{Prefix: "owned", Corpus: corpus}
	for _, index := range []int{0, 499, 999} {
		step := plan.Steps[index]
		for _, kind := range []string{"text", "search_visible"} {
			options, _ := searchReadbackRequest(kind, repo, step).Arguments["options"].(map[string]any)
			if _, scoped := options["path"]; scoped {
				t.Fatalf("%s positive probe preselected its file and can bypass the native search cache", kind)
			}
		}
		query, _ := searchReadbackRequest("text", repo, step).Arguments["query"].(string)
		replacement, _ := step.Request.Arguments["replacement"].(string)
		novelTrigram := false
		for offset := 0; offset+3 <= len(query); offset++ {
			if !strings.Contains(strings.ToLower(corpus.EditInitialContent), strings.ToLower(query[offset:offset+3])) {
				novelTrigram = true
				break
			}
		}
		if query == "" || strings.Contains(corpus.EditInitialContent, query) || !strings.Contains(replacement, query) || !novelTrigram {
			t.Fatalf("edit%d text query must require new postings: query=%q already_present=%v replacement=%q", index, query, strings.Contains(corpus.EditInitialContent, query), replacement)
		}
		old, _ := searchReadbackRequest("old_text_absent", repo, step).Arguments["query"].(string)
		if old == "" || !strings.Contains(corpus.EditInitialContent, old) || strings.Contains(replacement, old) {
			t.Fatalf("edit%d negative probe lost the exact removed name: %q", index, old)
		}
		if got := searchReadbackRequest("search_visible", repo, step).Arguments["query"]; got != step.Marker {
			t.Fatalf("symbol name query changed: %v", got)
		}
	}
}
