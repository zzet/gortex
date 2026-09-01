package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestReserveLocalNoteBudget pins the headroom reservation for the
// local-only note: a non-mergeable-format request forwarded to the
// local handler carries a max_bytes reduced by the note's size, so
// the note that must ride on the response has room by construction —
// the same reserve-first rule the token-budget decoration follows —
// while JSON requests, opt-outs, and note-dwarfing caps pass through
// untouched.
func TestReserveLocalNoteBudget(t *testing.T) {
	f := testFederator()
	noteLen := len(localOnlyNote(2))

	maxBytesOf := func(t *testing.T, body []byte) (int, bool) {
		t.Helper()
		args := argsMapFromBody(body)
		v, present := args["max_bytes"]
		if !present {
			return 0, false
		}
		n, _ := v.(float64)
		return int(n), true
	}

	t.Run("mermaid request reserves the note bytes", func(t *testing.T) {
		body := productionBody(`{"id":"pkg/hot.go::Hot","format":"mermaid","max_bytes":1000}`)
		got := f.ReserveLocalNoteBudget("find_usages", body, 2)
		mb, ok := maxBytesOf(t, got)
		if !ok || mb != 1000-noteLen {
			t.Fatalf("want forwarded max_bytes %d, got %v (present=%v)", 1000-noteLen, mb, ok)
		}
	})

	t.Run("default budget is reserved too", func(t *testing.T) {
		// No explicit budget still budgets to the project default, and
		// the note must fit under that cap as well.
		body := productionBody(`{"id":"pkg/hot.go::Hot","compact":true}`)
		got := f.ReserveLocalNoteBudget("find_usages", body, 2)
		mb, ok := maxBytesOf(t, got)
		if !ok || mb >= 40_000 || mb != 40_000-noteLen {
			t.Fatalf("want forwarded max_bytes %d under the default cap, got %v (present=%v)", 40_000-noteLen, mb, ok)
		}
	})

	t.Run("max_tokens axis min-merges with the reserve", func(t *testing.T) {
		// 200 tokens ≈ 700 bytes; the injected max_bytes must undercut
		// it by the note's size so the tighter axis is the reserved one.
		body := productionBody(`{"id":"pkg/hot.go::Hot","format":"dot","max_tokens":200}`)
		got := f.ReserveLocalNoteBudget("find_usages", body, 2)
		mb, ok := maxBytesOf(t, got)
		if !ok || mb != 700-noteLen {
			t.Fatalf("want forwarded max_bytes %d, got %v (present=%v)", 700-noteLen, mb, ok)
		}
	})

	unchanged := []struct {
		name string
		tool string
		body string
	}{
		{"json request", "find_usages", `{"id":"pkg/hot.go::Hot","max_bytes":1000}`},
		{"budget opt-out", "find_usages", `{"id":"pkg/hot.go::Hot","format":"mermaid","max_bytes":0}`},
		{"cap smaller than the note", "find_usages", `{"id":"pkg/hot.go::Hot","format":"mermaid","max_bytes":100}`},
		{"non-federated tool", "index_repository", `{"path":".","format":"mermaid"}`},
	}
	for _, tc := range unchanged {
		t.Run(tc.name+" passes through", func(t *testing.T) {
			body := productionBody(tc.body)
			got := f.ReserveLocalNoteBudget(tc.tool, body, 2)
			var a, b any
			if err := json.Unmarshal(body, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got, &b); err != nil {
				t.Fatal(err)
			}
			am, _ := json.Marshal(a)
			bm, _ := json.Marshal(b)
			if string(am) != string(bm) {
				t.Fatalf("body must pass through unchanged, got %s", bm)
			}
		})
	}

	t.Run("zero peers pass through", func(t *testing.T) {
		body := productionBody(`{"id":"pkg/hot.go::Hot","format":"mermaid","max_bytes":1000}`)
		got := f.ReserveLocalNoteBudget("find_usages", body, 0)
		if mb, _ := maxBytesOf(t, got); mb != 1000 {
			t.Fatalf("no peers, nothing to note: body must pass through, got max_bytes %d", mb)
		}
	})
}

// TestCallLocalFederatedForwardsTheReservedBudget pins the wiring: the
// LOCAL dispatch runs on the reserved body while the fan-out and the
// note enforcement keep the caller's ORIGINAL budget — reserving on
// both sides would shrink the cap the note was reserved out of.
func TestCallLocalFederatedForwardsTheReservedBudget(t *testing.T) {
	var localBody []byte
	r := NewRouter(RouterConfig{
		LocalExecute: func(_ context.Context, _ string, body []byte) ([]byte, int, error) {
			localBody = body
			return envelope("graph TD;\n  A-->B;\n"), 200, nil
		},
	})
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0,"truncated":false}`})

	body := productionBody(`{"id":"pkg/hot.go::Hot","format":"mermaid","max_bytes":1000}`)
	out, _, err := r.callLocalFederated(context.Background(), "find_usages", body,
		RouteContext{EnabledRemotes: []ServerEntry{{Slug: "r2", URL: remote.URL}}})
	if err != nil {
		t.Fatal(err)
	}

	wantLocal := 1000 - len(localOnlyNote(1))
	args := argsMapFromBody(localBody)
	if mb, _ := args["max_bytes"].(float64); int(mb) != wantLocal {
		t.Fatalf("local dispatch must see the reserved cap %d, got %v", wantLocal, args["max_bytes"])
	}
	total, texts := combinedContentBytes(t, out)
	if total > 1000 {
		t.Fatalf("combined response %d bytes over the caller's cap", total)
	}
	note := ""
	for _, text := range texts[1:] {
		note += text
	}
	if !strings.Contains(note, "local") {
		t.Fatalf("with the reserve in place the note must ride on the response, got %q", texts)
	}
}
