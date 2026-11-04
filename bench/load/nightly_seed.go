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
	edit := o.NightlySeed == "search-delay-v1" && o.Scenario == "edit" && o.Agents == 0 && o.Editors == 4
	read := o.NightlySeed == "search-delay-read16-v1" && o.Scenario == "read" && o.Agents == 16 && o.Editors == 0
	if (!edit && !read) || o.Worktrees != 0 || o.EditGateSeed != "" || o.EditGateModes || o.SearchReadbackAcceptance {
		return errors.New("nightly search seed requires standalone edit4/search-delay-v1 or read16/search-delay-read16-v1 without edit/worktree faults")
	}
	if o.PRSmokeDrain != 0 {
		version := 2
		if read { version = 1 }
		if prSmokeDrainVersion(o) != version {
			return errors.New("nightly search seed drain differs from its read/edit protocol")
		}
		return validatePRSmokeDrain(o)
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
	if seed != "search-delay-v1" && seed != "search-delay-read16-v1" {
		return seed, 0, errors.New("unsupported nightly workload seed")
	}
	start := time.Now()
	err := waitContext(ctx, time.Second)
	return seed, time.Since(start).Nanoseconds(), err
}
