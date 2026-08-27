package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func combinedContentBytes(t *testing.T, out []byte) (int, []string) {
	t.Helper()
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("result must stay a valid MCP envelope: %v", err)
	}
	total := 0
	texts := make([]string, 0, len(env.Content))
	for _, c := range env.Content {
		total += len(c.Text)
		texts = append(texts, c.Text)
	}
	return total, texts
}

// TestFederator_LocalOnlyAnnotationStaysWithinBudget pins the caller's
// byte/token budget on the COMPLETE local-only response: the handler
// spends the whole cap on the primary rendering, so a second content
// item appended outside the budget breaks the contract the cap
// promises. A note that cannot fit in the remaining headroom is
// dropped — same rule as the token-budget decoration — never the
// bytes that push the response past its own cap.
func TestFederator_LocalOnlyAnnotationStaysWithinBudget(t *testing.T) {
	// ~90 bytes of local text: a handler-trimmed page near a tiny cap.
	localText := strings.Repeat("pkg/hot.go::Hot <- l/a.go::LUse (calls)\n", 2) + "edges: 2\n"
	formats := []struct {
		name string
		args string
	}{
		{"compact", `"compact":true`},
		{"gcx", `"format":"gcx"`},
		{"toon", `"format":"toon"`},
		{"mermaid", `"format":"mermaid"`},
		{"dot", `"format":"dot"`},
	}
	budgets := []struct {
		name string
		arg  string
		cap  int
	}{
		{"max_bytes", `"max_bytes":100`, 100},
		{"max_tokens", `"max_tokens":30`, 105}, // 30 tokens * 3.5 bytes
	}
	for _, f := range formats {
		for _, b := range budgets {
			t.Run(f.name+"/"+b.name, func(t *testing.T) {
				remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0,"truncated":false}`})
				body := productionBody(fmt.Sprintf(`{"id":"pkg/hot.go::Hot",%s,%s}`, f.args, b.arg))
				out := testFederator().Augment(context.Background(), "find_usages",
					body, envelope(localText),
					[]ServerEntry{{Slug: "r2", URL: remote.URL}})

				total, texts := combinedContentBytes(t, out)
				if total > b.cap {
					t.Fatalf("local-only response is %d combined content bytes, over the %s cap %d; content: %q", total, b.name, b.cap, texts)
				}
				if texts[0] != localText {
					t.Fatalf("the primary rendering must pass through unchanged")
				}
			})
		}
	}

	t.Run("roomy budget keeps the note", func(t *testing.T) {
		remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0,"truncated":false}`})
		body := productionBody(`{"id":"pkg/hot.go::Hot","format":"mermaid","max_bytes":600}`)
		out := testFederator().Augment(context.Background(), "find_usages",
			body, envelope(localText),
			[]ServerEntry{{Slug: "r2", URL: remote.URL}})
		total, texts := combinedContentBytes(t, out)
		if total > 600 {
			t.Fatalf("combined content %d bytes over the 600 cap", total)
		}
		note := strings.Join(texts[1:], "")
		if !strings.Contains(note, "local") || !strings.Contains(note, "format") {
			t.Fatalf("with headroom the explicit local-only note must survive, got %q", texts)
		}
	})
}
