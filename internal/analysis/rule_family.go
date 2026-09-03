package analysis

import (
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// RuleFamily is one pluggable body of change-gate rules. Guards, architecture,
// event boundaries, and (in the security theme) taint all implement it, so the
// change_contract evaluator runs "every registered family over the changed
// set" instead of hard-wiring each evaluator by hand. Adding a rule family is
// then a registration, not a new branch in the pipeline.
type RuleFamily interface {
	// Name identifies the family for provenance on each finding.
	Name() string
	// Evaluate checks a set of changed symbol IDs against the family's rules
	// and returns the violations, each carrying its own severity. Families
	// only read, so the caller hands over its own reader and an
	// overlay-active request is gated on the buffers it pushed.
	Evaluate(g graph.Reader, changedSet []string) []GuardViolation
}

// ArchitectureFamily adapts the declarative architecture: layer DSL.
type ArchitectureFamily struct {
	Config config.ArchitectureConfig
}

func (f ArchitectureFamily) Name() string { return "architecture" }

func (f ArchitectureFamily) Evaluate(g graph.Reader, changedSet []string) []GuardViolation {
	if f.Config.IsEmpty() {
		return nil
	}
	return EvaluateArchitecture(g, f.Config, changedSet)
}
