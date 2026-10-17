package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

// searchSymbolsVisibilityAnswer is the part of a search_symbols answer the
// visibility claim is about: the typed page and the raw content section that
// rides beside it when the page is empty.
type searchSymbolsVisibilityAnswer struct {
	Results []struct {
		Name string `json:"name"`
		File string `json:"file_path"`
	} `json:"results"`
	ContentMatches *struct {
		Matches []struct {
			Path string `json:"path"`
			Text string `json:"text"`
		} `json:"matches"`
	} `json:"content_matches"`
}

func (w *worktreeSearchStack) searchSymbols(t *testing.T, cwd, query string) searchSymbolsVisibilityAnswer {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{"query": query}
	ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), cwd)
	res, err := w.srv.wrapToolHandler(w.srv.handleSearchSymbols)(ctx, req)
	if err != nil {
		t.Fatalf("search_symbols %q from %s: %v", query, cwd, err)
	}
	text := viewResultText(t, res)
	if res.IsError {
		t.Fatalf("search_symbols %q from %s was refused: %s", query, cwd, text)
	}
	var answer searchSymbolsVisibilityAnswer
	if err := json.Unmarshal([]byte(text), &answer); err != nil {
		t.Fatalf("decode search_symbols %q: %v\n%s", query, err, text)
	}
	return answer
}

// TestSearchSymbolsRoutedViewHidesARenamedBaseName is the visibility claim for
// the symbol lane's content recovery: a name the committed base still holds,
// renamed away in the worktree, is absent from the worktree's view in BOTH
// halves of a search_symbols answer. The typed page is composed with the
// generation masks; the content section must be too — it answers from the
// view's working copy, never from the canonical checkout's bytes.
//
// The canonical checkout is asked the same question as the control: it still
// holds the name, so an empty worktree answer is not an empty index.
func TestSearchSymbolsRoutedViewHidesARenamedBaseName(t *testing.T) {
	stack := newWorktreeSearchStack(t)

	previous := stack.dirtyGeneration(t)
	refWriteFiles(t, stack.worktree, map[string]string{
		"keep.go": "package repo\n\nfunc Retainer() {\n\t// retainerWorktreeOnlyMarker\n}\n",
	})
	stack.awaitDirtyGenerationAfter(t, previous)

	base := stack.searchSymbols(t, stack.primary, "Keeper")
	if len(base.Results) == 0 && (base.ContentMatches == nil || len(base.ContentMatches.Matches) == 0) {
		t.Fatalf("the canonical checkout does not answer the base name, so the worktree check below proves nothing: %+v", base)
	}

	got := stack.searchSymbols(t, stack.worktree, "Keeper")
	for _, result := range got.Results {
		if result.Name == "Keeper" {
			t.Errorf("the worktree view returned the renamed base symbol: %+v", result)
		}
	}
	if got.ContentMatches != nil {
		for _, match := range got.ContentMatches.Matches {
			if strings.Contains(match.Text, "Keeper") {
				t.Errorf("the worktree view returned a content match for the renamed base name from %s: %q", match.Path, match.Text)
			}
		}
	}

	// The new name is visible through the same lane, so the absence above is
	// the view's own answer and not a lane that answers nothing.
	renamed := stack.searchSymbols(t, stack.worktree, "Retainer")
	found := false
	for _, result := range renamed.Results {
		if result.Name == "Retainer" {
			found = true
		}
	}
	if !found {
		t.Errorf("the worktree view does not answer the new name: %+v", renamed)
	}

	// The content section itself still answers — out of the worktree's bytes.
	// A marker only the worktree holds is found through the worktree view and
	// not through the canonical checkout, so the fix narrowed the lane to the
	// view rather than switching it off.
	marker := stack.searchSymbols(t, stack.worktree, "retainerWorktreeOnlyMarker")
	if marker.ContentMatches == nil || len(marker.ContentMatches.Matches) != 1 ||
		marker.ContentMatches.Matches[0].Path != "repo/keep.go" {
		t.Errorf("the worktree view's content section did not answer the worktree-only marker from repo/keep.go: %+v", marker.ContentMatches)
	}
	if canonical := stack.searchSymbols(t, stack.primary, "retainerWorktreeOnlyMarker"); canonical.ContentMatches != nil &&
		len(canonical.ContentMatches.Matches) > 0 {
		t.Errorf("the canonical checkout answered a marker only the worktree holds: %+v", canonical.ContentMatches)
	}
}
