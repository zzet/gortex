package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestFederator_NonMergeableFormatIsExplicitlyLocalOnly pins the gate
// for renderings that cannot round-trip as JSON (compact, gcx, toon,
// mermaid, dot): the merge cannot fold remote rows into them, so the
// response must SAY it is local-only instead of silently presenting a
// partial result as federated.
func TestFederator_NonMergeableFormatIsExplicitlyLocalOnly(t *testing.T) {
	cases := []struct {
		name      string
		localText string
		args      string
	}{
		{"mermaid", "graph TD;\n  A-->B;\n", `{"id":"pkg/hot.go::Hot","format":"mermaid"}`},
		{"compact", "pkg/hot.go::Hot <- l/a.go::LUse (calls)\nedges: 1 of 1 total\n", `{"id":"pkg/hot.go::Hot","compact":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
				"nodes":[{"id":"r/b.go::RUse"}],
				"edges":[{"from":"r/b.go::RUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/b.go","line":7}],
				"total_nodes":1,"total_edges":1,"truncated":false}`})
			out := testFederator().Augment(context.Background(), "find_usages",
				productionBody(tc.args), envelope(tc.localText),
				[]ServerEntry{{Slug: "r2", URL: remote.URL}})

			var env struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(out, &env); err != nil {
				t.Fatalf("result must stay a valid MCP envelope: %v", err)
			}
			if len(env.Content) == 0 || env.Content[0].Text != tc.localText {
				t.Fatalf("the local rendering must pass through unchanged")
			}
			note := ""
			for _, c := range env.Content[1:] {
				note += c.Text
			}
			if !strings.Contains(note, "local") || !strings.Contains(note, "format") {
				t.Fatalf("a non-mergeable format with enabled peers must carry an explicit local-only note, got content: %+v", env.Content)
			}
		})
	}
}
