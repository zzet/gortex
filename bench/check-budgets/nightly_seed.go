package main

import "errors"

// nightlyWorkloadSeed describes an opt-in workload fault, never a replacement
// product revision or a modification of observations or policy thresholds.
// Versioning prevents an old collector from silently interpreting a new fault.
type nightlyWorkloadSeed struct {
	Version           int    `json:"schema_version"`
	Kind              string `json:"kind"`
	CandidateRevision string `json:"candidate_revision"`
}

func validateNightlySeed(c laneConfig) error {
	if c.NightlySeed == nil {
		return nil
	}
	seed := c.NightlySeed
	supported := seed.Kind == "search-delay-v1" && c.Lane == "nightly" || seed.Kind == "search-delay-read16-v1" && (c.Lane == "smoke" || c.Lane == "nightly")
	if seed.Version != 1 || !supported || c.EditGateSeed != "" || seed.CandidateRevision != c.Builds["candidate"].Revision || !revisionPattern.MatchString(seed.CandidateRevision) {
		return errors.New("nightly_workload_seed requires a supported schema1 search seed, no edit seed and the exact candidate revision")
	}
	return nil
}

// Raw fault identity is separate from the normalized offered workload. It must
// agree with its versioned candidate scenario. The read16 demonstration uses
// a measured OFF/ON doubling gate, never the edit4 absolute budget.
// MAIN/tag and other cells stay clean.
func validateNightlySeedReport(c inputs, input runInput, r report) error {
	expected := ""
	if c.NightlySeed != nil {
		if err := validateNightlySeed(laneConfig{Lane: "nightly", NightlySeed: c.NightlySeed, Builds: map[string]laneBuild{"candidate": {Revision: c.CandidateRevision}}}); err != nil {
			return err
		}
		if input.Role == "candidate" && input.Scenario == c.NightlySeed.scenario() {
			expected = c.NightlySeed.Kind
		}
	}
	if r.NightlySeed != expected {
		return errors.New("missing or foreign nightly workload fault provenance")
	}
	observed := false
	for _, p := range r.Phases {
		for _, a := range p.Attempts {
			seeded := expected != "" && a.Tool == "search.symbols" && a.Role == "workload"
			if seeded {
				if a.NightlySeed != expected || a.NightlyDelayNS < 1000000000 {
					return errors.New("nightly search fault lacks actual one-second raw delay")
				}
				if expected == "search-delay-read16-v1" && (a.LogicalMS == nil || !finiteNonnegative(*a.LogicalMS) || *a.LogicalMS < float64(a.NightlyDelayNS)/1000000) {
					return errors.New("read16 search fault duration is absent from the actual logical latency")
				}
				observed = true
			} else if a.NightlySeed != "" || a.NightlyDelayNS != 0 {
				return errors.New("foreign nightly workload fault attempt")
			}
		}
	}
	if expected != "" && !observed {
		return errors.New("nightly search fault did not reach an actual workload request")
	}
	return nil
}

func (s *nightlyWorkloadSeed) scenario() string {
 if s.Kind == "search-delay-read16-v1" { return "load-16" }
 return "loadedit-4"
}
