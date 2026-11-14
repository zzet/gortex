package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// The producer, facade normalization and semantic oracle all participate. A
// handwritten prose fixture or a rich JSON answer cannot certify compact mode.
func TestEditConsumerCompactActualProducers(t *testing.T) {
	root := t.TempDir()
	const before = "package p\n\nfunc helper() int { return 1 }\n"
	const marker = "CompactConsumerAddedCaller"
	after := before + "\nfunc " + marker + "() int { return helper() }\n"
	file := filepath.Join(root, "main.go")
	if err := os.WriteFile(file, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "baseline")
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := indexer.New(store, registry, config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	if _, err := idx.Index(root); err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil)
	const session = "compact-consumer-producer"
	server.NoteSessionToolPolicy(session, mcp.FacadeSurfaceVersion, "full")
	t.Cleanup(func() { server.ReleaseSession(session) })
	ctx := mcp.WithSessionID(t.Context(), session)
	corpus := editProbeCorpus()
	corpus.File = "main.go"
	corpus.Symbol = "main.go::helper"
	corpus.Query = "helper"
	corpus.ProposedSignature = "func helper(probe int) int"
	call := func(t *testing.T, probe request, compact bool) json.RawMessage {
		t.Helper()
		tool := server.MCPServer().GetTool(probe.Tool)
		if tool == nil {
			t.Fatalf("registered facade %q missing", probe.Tool)
		}
		// A real single-indexer server owns unprefixed graph paths; benchmark
		// multi-repo dispatch prefixing is tested separately below.
		args := copyJSON(probe.Arguments)
		args["output"] = map[string]any{"format": "json", "max_bytes": 0}
		if compact {
			args["output"].(map[string]any)["compact"] = true
		}
		req := mcplib.CallToolRequest{}
		req.Params.Name, req.Params.Arguments = probe.Tool, args
		result, err := tool.Handler(ctx, req)
		if err != nil || result == nil || result.IsError {
			t.Fatalf("real %s result=%+v err=%v", probe.name(), result, err)
		}
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	baseline, err := decodeEditProbeOutput(call(t, editConsumerDeadCodeRequest(), false), "json", "analyze.dead_code")
	if err != nil {
		t.Fatal(err)
	}
	dead, complete := editConsumerDeadRows(baseline, "dead_code", "total")
	if !complete || !editConsumerFresh(baseline) || !editConsumerDeadContains(dead, corpus.File, "helper") {
		t.Fatalf("real pre-edit dead-code membership not established: %v", baseline)
	}
	if err := os.WriteFile(file, []byte(after), 0600); err != nil {
		t.Fatal(err)
	}
	if err := idx.IndexFile(file); err != nil {
		t.Fatal(err)
	}
	step := editProbeStep{Marker: marker}
	runtime := &editProbeRuntime{plan: editProbePlan{Callee: "helper"}, phase: &Phase{EditProbe: &editProbeReport{ConsumerBaseline: &editConsumerBaseline{State: "OBSERVED_DEAD_BEFORE_EDIT", Symbol: "main.go::helper"}}}}
	for _, oracle := range editConsumerOracles(corpus) {
		t.Run(oracle.name, func(t *testing.T) {
			// Prove the actual JSON producer and semantic subject first, independently
			// of the compact parser under test. This is no fallback for compact credit.
			rich, err := decodeEditProbeOutput(call(t, oracle.call, false), "json", oracle.call.name())
			if err != nil {
				t.Fatal(err)
			}
			want := "subject_observed"
			if oracle.name == "consumer_dead_code" {
				want = "fresh_membership_transition"
			}
			if got := runtime.consumerVerdict(oracle.name, rich, corpus, step); got != want {
				t.Fatalf("real JSON positive prerequisite: got %q want %q body=%v", got, want, rich)
			}
			raw := call(t, oracle.call, true)
			text, err := editOutputPayload(raw, false)
			if err != nil {
				t.Fatal(err)
			}
			if json.Valid(text) {
				t.Fatalf("compact producer returned rich JSON; no compact coverage: %s", text)
			}
			body, err := decodeEditProbeOutput(raw, "compact", oracle.call.name())
			if err != nil {
				t.Fatalf("actual compact producer cannot be decoded: %v\n%s", err, text)
			}
			if got := runtime.consumerVerdict(oracle.name, body, corpus, step); got != want {
				t.Fatalf("actual compact semantic evidence: got %q want %q body=%v", got, want, body)
			}
			if oracle.name == "consumer_verify" && (rich["return_usage"] == nil || !reflect.DeepEqual(body["return_usage"], rich["return_usage"])) {
				t.Fatalf("compact lost actual JSON return-usage distribution: compact=%v JSON=%v", body["return_usage"], rich["return_usage"])
			}
		})
	}
	actual, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, []byte(after)) {
		t.Fatal("consumer dry-run/read probes changed fixture bytes")
	}
}

func TestEditConsumerCompactRequestsActualFormat(t *testing.T) {
	for _, oracle := range editConsumerOracles(editProbeCorpus()) {
		t.Run(oracle.name, func(t *testing.T) {
			req := editProbeWithOutput(oracle.call, "compact")
			if got := editProbeRequestOutput(req); got != "compact" {
				t.Fatalf("requested compact silently uses %q", got)
			}
			output := editProbeDispatchOutput(req)
			if output["compact"] != true || output["format"] != "json" || output["max_bytes"] != 0 {
				t.Fatalf("wrong actual compact dispatch: %v", output)
			}
		})
	}
}

func TestEditConsumerCompactRejectsFallbackAndMissingEvidence(t *testing.T) {
	for _, oracle := range editConsumerOracles(editProbeCorpus()) {
		t.Run(oracle.name, func(t *testing.T) {
			for name, raw := range map[string]json.RawMessage{
				"rich_json_fallback":   outputWire(`{"clean":true,"total":0,"dead_code":[],"truncated":false}`, nil),
				"empty":                outputWire("", nil),
				"prose_claim":          outputWire("all consumers are clean\n", nil),
				"byte_budget_omission": outputWire("... trimmed to byte budget; pass max_bytes:0 for the full result\n", nil),
				"tool_error":           json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"clean"}]}`),
			} {
				t.Run(name, func(t *testing.T) {
					if body, err := decodeEditProbeOutput(raw, "compact", oracle.call.name()); err == nil {
						t.Fatalf("unproved/fallback compact evidence accepted: %v", body)
					}
				})
			}
		})
	}
}
