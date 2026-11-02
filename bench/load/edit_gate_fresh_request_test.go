package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEditGateRequestsAnswerFreshnessWithoutChangingAutoViewOrOrdinaryReads(t *testing.T) {
	for _, cell := range []struct {
		mode       string
		gate, want bool
	}{{"json", true, true}, {"toon", true, true}, {"facade", true, true}, {"compact", true, false}, {"json", false, false}} {
		t.Run(cell.mode+map[bool]string{true: "-gate", false: "-ordinary"}[cell.gate], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			c := editProbeCorpus()
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			plan.GateModes = cell.gate
			plan.Steps[0].ProbeMode = cell.mode
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.File), []byte("package fixture\n"+c.EditAnchor+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			observed := make(chan map[string]any, 64)
			run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(frame stubFrame) (any, *rpcError) {
				if frame.Method == "tools/call" {
					args, _ := frame.Params["arguments"].(map[string]any)
					row := map[string]any{"tool": frame.Params["name"], "operation": args["operation"], "require_fresh": args["require_fresh"], "view": args["view"]}
					select {
					case observed <- row:
					default:
					}
				}
				return stubContent(map[string]any{}), nil
			})
			state := run.states[0]
			defer state.finishRaw()
			run.probe(ctx, cancel, state)
			seen := map[string]bool{}
			for len(seen) < 7 {
				select {
				case row := <-observed:
					tool, _ := row["tool"].(string)
					operation, _ := row["operation"].(string)
					seen[tool+"."+operation] = true
					if (row["require_fresh"] == true) != cell.want {
						t.Errorf("actual %s.%s freshness request=%v, want%t", tool, operation, row["require_fresh"], cell.want)
					}
					if row["view"] != nil {
						t.Errorf("gate silently changed AUTO scope to%v", row["view"])
					}
				case <-ctx.Done():
					t.Fatalf("did not observe all seven actual graph probe operations: %v", seen)
				}
			}
			cancel()
			state.probes.Wait()
		})
	}
}
