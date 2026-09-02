package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// Recoverable-condition UX.
//
// Some tool conditions are not failures of the SERVER — they are states the
// agent can recover from on its own: the cwd isn't a tracked repo, a symbol id
// isn't in the index, a file has no indexed symbols. Returning those as an
// isError CallToolResult makes well-behaved clients treat the turn as failed
// and, in the worst case, abandon the session. F3's rule: such conditions
// return a NORMAL (non-isError) result carrying machine-readable guidance — the
// condition code, a human message, the next tools to try, and a `gortex track`
// affordance — so the agent reroutes instead of giving up. isError is reserved
// for security refusals and genuine malfunctions.

// RecoverableGuidance is the success-shaped counterpart to StructuredError. The
// `recoverable: true` flag and `condition` code let a smart client branch; the
// suggested_tools and track_command give a plain agent its next move.
type RecoverableGuidance struct {
	Recoverable    bool           `json:"recoverable"`
	Condition      ErrorCode      `json:"condition"`
	Message        string         `json:"message"`
	SuggestedTools []string       `json:"suggested_tools,omitempty"`
	TrackCommand   string         `json:"track_command,omitempty"`
	Data           map[string]any `json:"data,omitempty"`
}

// newRecoverableResult encodes guidance into a NON-error result. The JSON body
// is the machine-readable form; IsError stays false so no client treats a
// recoverable state as a session-ending failure.
func newRecoverableResult(g RecoverableGuidance) *mcp.CallToolResult {
	g.Recoverable = true
	body, err := json.Marshal(g)
	if err != nil {
		return mcp.NewToolResultText(g.Message)
	}
	res := mcp.NewToolResultText(string(body))
	res.IsError = false
	return res
}

// repoNotTrackedGuidance: the path isn't covered by any tracked repo. Routes to
// the content-search escape hatches and offers the exact `gortex track` command.
func repoNotTrackedGuidance(path string) *mcp.CallToolResult {
	track := "gortex track ."
	if path != "" {
		track = "gortex track " + path
	}
	return newRecoverableResult(RecoverableGuidance{
		Condition:      ErrCodeRepoNotTracked,
		Message:        fmt.Sprintf("%q is not covered by any tracked repository, so the graph has nothing to answer with yet — track it, or fall back to a content search.", path),
		SuggestedTools: []string{"find_files", "search_text"},
		TrackCommand:   track,
		Data:           map[string]any{"path": path},
	})
}

// symbolNotFoundGuidance: the symbol id isn't in the index. Routes to the
// name/usage/text searches that can locate it (or confirm it's a local).
func symbolNotFoundGuidance(id string) *mcp.CallToolResult {
	return newRecoverableResult(RecoverableGuidance{
		Condition:      ErrCodeSymbolNotFound,
		Message:        fmt.Sprintf("no symbol with id %q is in the index — it may be spelled differently, live in an unindexed file, or be a local. Search for it rather than reading by id.", id),
		SuggestedTools: []string{"search_symbols", "find_usages", "search_text"},
		Data:           map[string]any{"id": id},
	})
}

// fileNotIndexedState says WHY a file has no indexed symbols, so the guidance
// can name a next step that can actually answer instead of a generic triple.
type fileNotIndexedState struct {
	// Unindexable: the index walk will never hold this file — an exclude rule,
	// a language no extractor claims, or a file over the size cap.
	Unindexable bool
	// Excluded: Unindexable, and an exclude / ignore RULE is the reason
	// (vendored tree, build output, .gortexignore).
	Excluded bool
	// Indexed: the graph DOES hold this file; it just defines no symbols — a
	// package-doc-only file, a constants file, a shell or SQL file with no
	// definitions. The locators still have rows for it.
	Indexed bool
	// OutOfScope: the graph holds this file but the active repo/project/ref
	// filter excludes it. Not folded into Indexed: every graph tool applies
	// the same filter, so the locators have no rows for it either.
	OutOfScope bool
}

// fileNotIndexedGuidance: a file has no indexed symbols. The suggested tools
// are chosen from the reason, not fixed: find_files and search_text are both
// graph-backed, so for a path the graph will NEVER hold they return zero rows
// and naming them costs the caller two dead round-trips before it reaches the
// only tool that can answer. That narrowing applies to Unindexable alone — an
// Indexed file is in the graph, so both locators do have rows for it and a
// scoped text search answers at a fraction of a whole-file read's tokens; only
// the symbol lookup has nothing to return. The Unindexable distinction also
// rides the Data block — the PreToolUse hook stays silent rather than nudging
// toward graph tools whenever the graph cannot answer for the path.
func fileNotIndexedGuidance(path string, st fileNotIndexedState) *mcp.CallToolResult {
	message := fmt.Sprintf("no symbols are indexed for %q — the file may be new, ignored, or in a language without an extractor. Find or read it directly instead.", path)
	tools := []string{"find_files", "search_text", "read_file"}
	switch {
	case st.Unindexable:
		reason := "the indexer skips it by design — no extractor claims its language, or it is over the size cap"
		if st.Excluded {
			reason = "an exclude rule skips it by design (vendored, ignored, or build output)"
		}
		message = fmt.Sprintf("no symbols are indexed for %q and none ever will be: %s. Only a direct read can answer for it.", path, reason)
		tools = []string{"read_file"}
	case st.OutOfScope:
		// The locators apply the same filter, so they are not offered here.
		message = fmt.Sprintf("%q is indexed but outside the active project/ref scope — every graph tool applies that same filter, so none can answer for it here. Widen the scope or read it directly.", path)
		tools = []string{"set_active_project", "read_file"}
	case st.Indexed:
		// Tools stay the full triple: the graph holds this file, so the
		// locators can answer for it — only the symbol lookup cannot.
		message = fmt.Sprintf("%q is indexed but defines no symbols in scope — a symbol lookup has nothing to return for it. Search its text or read it directly.", path)
	}
	return newRecoverableResult(RecoverableGuidance{
		Condition:      ErrCodeFileNotIndexed,
		Message:        message,
		SuggestedTools: tools,
		Data: map[string]any{
			"path":         path,
			"excluded":     st.Excluded,
			"unindexable":  st.Unindexable,
			"indexed":      st.Indexed,
			"out_of_scope": st.OutOfScope,
		},
	})
}
