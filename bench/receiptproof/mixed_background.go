package receiptproof

import "strings"

// MixedBackgroundTimingOnlyError admits only the known B-attribution refusal.
// It does not waive any missing or invalid A/P interval.
func MixedBackgroundTimingOnlyError(value string) bool {
	if !coalescedTimingOnlyError(value) {
		return false
	}
	found := false
	reasons, _ := strings.CutPrefix(value, "A/P/B accounting unavailable: ")
	for _, reason := range strings.Split(reasons, "; ") {
		if reason == "requested B observed unavailable: serving_attempt_binding_mismatch" {
			return false
		}
		found = found || reason == "requested B observed unavailable: preexisting_path_work"
	}
	return found
}
