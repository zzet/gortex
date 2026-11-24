package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOwnACKReadbackOnlyPreservesPacedOpenLoopPlan(t *testing.T) {
	args := []string{"--binary", "gortex", "--manifest", "manifest.json", "--scratch", "scratch",
		"--scenario", editProbeScenario, "--edit-ack", "local", "--edit-agents", "8", "--worktrees", "0"}
	ordinary, err := parseRun(args)
	require.NoError(t, err)
	selectedArgs := append(args, "--edit-ack-readback-only")
	selected, err := parseRun(selectedArgs)
	require.NoError(t, err)
	before, err := planEditProbe(editProbeCorpus(), ordinary)
	require.NoError(t, err)
	after, err := planEditProbe(editProbeCorpus(), selected)
	require.NoError(t, err)
	require.False(t, before.OwnACKReadbackOnly)
	require.True(t, after.OwnACKReadbackOnly)
	require.Equal(t, before.Steps, after.Steps)
	require.Len(t, after.Steps, 100)
	require.Equal(t, before.Readers, after.Readers)
	require.Equal(t, 4, after.Readers)
	require.Equal(t, before.NoEditWindow, after.NoEditWindow)
	require.Equal(t, int64(15*time.Second), after.MeanNS)
	operations := editProbeOperations(after, editProbeCorpus())
	require.Len(t, operations, 300)
	require.Equal(t, "edit.file", operations[0].name())
	require.Equal(t, "read.file", operations[1].name())
	require.Equal(t, "read.source", operations[2].name())
	for _, override := range [][]string{{"--edit-ack", "full"}, {"--agents", "0"}, {"--worktrees", "1"}, {"--edit-count", "1"}, {"--edit-consumer-probes"}, {"--keep-state"}} {
		_, err := parseRun(append(append([]string(nil), selectedArgs...), override...))
		require.Error(t, err)
	}
	unsupported := editProbeCorpus()
	unsupported.EditTool = "edit.symbol"
	_, err = planEditProbe(unsupported, selected)
	require.Error(t, err)
}

func TestOwnACKReadbackOnlyUsesMainEditorForTheThreeRequiredCalls(t *testing.T) {
	for _, wrong := range []string{"", "receipt", "source", "censored"} {
		t.Run("wrong="+wrong, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := editProbeCorpus()
			root := t.TempDir()
			original := "package p\nfunc Example() int { return 1 }\n" + c.EditAnchor + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(root, c.File), []byte(original), 0o600))
			// The short test fixture uses one internal offer. The CLI admits only the complete 100-offer cell.
			o := runOptions{Seed: 1, EditCount: 1, Editors: 1, Agents: 4, EditACKMode: "local", OwnACKReadbackOnly: true, Deadline: time.Second}
			plan, err := planEditProbe(c, o)
			require.NoError(t, err)
			step := plan.Steps[0]
			declaration := "func " + step.Marker + "() int { return Example() }"
			var mu sync.Mutex
			var calls []stubFrame
			socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
				if frame.Method != "tools/call" {
					return stubContent(map[string]any{}), nil
				}
				mu.Lock()
				calls = append(calls, frame)
				mu.Unlock()
				args, _ := frame.Params["arguments"].(map[string]any)
				switch args["operation"] {
				case "file":
					if frame.Params["name"] == "read" {
						return stubContent(map[string]any{"content": original + declaration + "\n"}), nil
					}
					mutation := step.MutationID
					if wrong == "receipt" {
						mutation = "another-offer"
					}
					return stubContent(map[string]any{"ack_mode": "local", "mutation_id": mutation, "mutation_receipt": "commit-owned"}), nil
				case "source":
					source := declaration
					if wrong == "source" {
						source = "func Other() int { return Example() }"
					}
					return stubContent(map[string]any{"name": step.Marker, "source": source}), nil
				default:
					return stubContent(map[string]any{}), nil
				}
			})
			editor, err := openMCP(ctx, socket, root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = editor.Close() })
			phase := &Phase{Metrics: make(map[string]Metric)}
			p := &privateRun{Root: root, Socket: socket, Repos: []privateRepo{{Path: root, Prefix: c.Label, Corpus: c}}}
			r, probers, err := prepareEditProbe(ctx, o, p, make([]*socketClient, 4), []*socketClient{editor}, phase)
			require.NoError(t, err)
			require.Empty(t, probers)
			t.Cleanup(func() { _ = r.close() })
			r.origin = time.Now()
			if wrong != "censored" {
				r.activate(ctx, step)
				r.doEdit(ctx, step)
			}
			if wrong == "" {
				require.NoError(t, r.finish())
			} else {
				require.Error(t, r.finish())
			}
			require.NoError(t, r.restore(ctx))
			require.False(t, phase.EditProbe.AccountingRequired)
			require.Equal(t, "candidate", phase.EditProbe.Role)
			require.Len(t, phase.EditProbe.Records, 1)
			row := phase.EditProbe.Records[0]
			require.False(t, row.GraphTerminal)
			mu.Lock()
			defer mu.Unlock()
			if wrong == "censored" {
				require.Empty(t, calls)
				require.Nil(t, row.StartedNS)
				require.Nil(t, row.Ack.Value)
				require.Equal(t, "phase_censored", row.Outcome)
				return
			}
			if wrong == "receipt" {
				require.Len(t, calls, 1)
				require.Nil(t, row.Ack.Value)
				require.Equal(t, "invalid_ack", row.Outcome)
				return
			}
			require.Len(t, calls, 3)
			for _, call := range calls {
				require.Equal(t, 1, call.Connection)
			}
			require.Equal(t, "edit", calls[0].Params["name"])
			require.Equal(t, "read", calls[1].Params["name"])
			require.Equal(t, "read", calls[2].Params["name"])
			args := calls[0].Params["arguments"].(map[string]any)
			require.Equal(t, step.MutationID, args["options"].(map[string]any)["mutation_id"])
			require.Equal(t, "commit-owned", row.Receipt)
			require.NotNil(t, row.Ack.Value)
			require.Len(t, row.Readbacks, 2)
			require.Equal(t, "file", row.Readbacks[0].Kind)
			require.True(t, row.Readbacks[0].Correct)
			require.Equal(t, "source", row.Readbacks[1].Kind)
			require.Equal(t, wrong == "", row.Readbacks[1].Correct)
		})
	}
}
