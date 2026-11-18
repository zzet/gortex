package main

import "errors"

// This reconciles explicit observations, not a zero-partial acceptance gate.
// Missing historical fields stay UNOBSERVED; RPC success does not prove full
// metadata, exact bytes or correctness of first-post-ACK reads.
func validateReadMetadataObservations(p phase) error {
	observed := make(map[string]int)
	for _, a := range p.Attempts {
		if a.ReadMetadataState == "" {
			continue
		}
		if a.ReadMetadataState != "partial_pending" || a.Role != "workload" || !scopedReadTool(a.Tool) || a.Outcome != "success" || a.Code == nil || *a.Code != 0 || a.Error != "" {
			return errors.New("unsupported or non-success partial metadata observation")
		}
		observed[a.Tool]++
	}
	for name, metric := range p.Tools {
		if metric.PartialPending == nil {
			if observed[name] != 0 {
				return errors.New("explicit partial metadata lacks its observation counter")
			}
			continue
		}
		if !scopedReadTool(name) || *metric.PartialPending < 0 || *metric.PartialPending != observed[name] || metric.Successes == nil || *metric.PartialPending > *metric.Successes {
			return errors.New("partial metadata count differs from retained RPC-success observations")
		}
		delete(observed, name)
	}
	for _, count := range observed {
		if count != 0 {
			return errors.New("partial metadata observation lacks tool metrics")
		}
	}
	return nil
}
