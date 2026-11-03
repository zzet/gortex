package main

import (
	"context"
	"errors"
	"time"
)

type nightlySeedKey struct{}

func validateNightlyWorkloadSeed(o runOptions) error {
	if o.NightlySeed == "" {
		return nil
	}
	if o.NightlySeed != "search-delay-v1" || o.Scenario != "edit" || o.Agents != 0 || o.Editors != 4 || o.Worktrees != 0 || o.EditGateSeed != "" || o.EditGateModes || o.PRSmokeDrain != 0 || o.SearchReadbackAcceptance {
		return errors.New("--nightly-workload-seed requires search-delay-v1 and the standalone four-editor workload without edit/worktree faults")
	}
	return nil
}

// This is a workload-side delay, not product processing time. It runs after
// callTool's attempt start and under the original product/phase deadline.
// Never change scheduled due times, recorded observations or product binaries.
func delayNightlySearch(ctx context.Context, tool, role string) (string, int64, error) {
	seed, _ := ctx.Value(nightlySeedKey{}).(string)
	if seed == "" || tool != "search.symbols" || role != "workload" {
		return "", 0, nil
	}
	if seed != "search-delay-v1" {
		return seed, 0, errors.New("unsupported nightly workload seed")
	}
	start := time.Now()
	err := waitContext(ctx, time.Second)
	return seed, time.Since(start).Nanoseconds(), err
}
