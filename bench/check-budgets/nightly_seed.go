package main

import "errors"

// nightlyWorkloadSeed describes an opt-in workload fault, never a replacement
// product revision or a modification of observations or policy thresholds.
// Versioning prevents an old collector from silently interpreting a new fault.
type nightlyWorkloadSeed struct {
	Version int `json:"schema_version"`
	Kind string `json:"kind"`
	CandidateRevision string `json:"candidate_revision"`
}

func validateNightlySeed(c laneConfig) error {
	if c.NightlySeed == nil {
		return nil
	}
	seed := c.NightlySeed
	if seed.Version != 1 || seed.Kind != "search-delay-v1" || c.Lane != "nightly" || c.EditGateSeed != "" || seed.CandidateRevision != c.Builds["candidate"].Revision || !revisionPattern.MatchString(seed.CandidateRevision) {
		return errors.New("nightly_workload_seed requires schema1 search-delay-v1, full nightly collection, no edit seed and the exact candidate revision")
	}
	return nil
}
