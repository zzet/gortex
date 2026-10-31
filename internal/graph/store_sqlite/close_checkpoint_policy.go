package store_sqlite

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// closeCheckpointPolicy is resolved once by the owning Close. The WAL's
// committed frames remain available for recovery when the final copy is
// skipped; this does not change the existing NORMAL/OFF synchronous policy.
type closeCheckpointPolicy struct {
	full    bool
	max     time.Duration // zero retains an unlimited explicit full drain
	warning string
}

func resolveCloseCheckpointPolicy() closeCheckpointPolicy {
	var policy closeCheckpointPolicy
	rawMax := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S"))
	if rawMax != "" {
		secs, err := strconv.Atoi(rawMax)
		if err == nil && secs >= 0 {
			// A valid legacy setting is an opt-in, including explicit 0 (the
			// old unlimited behavior). Positive caps retain the 24h clamp.
			policy.full = true
			policy.max = time.Duration(min(secs, 24*3600)) * time.Second
		} else {
			policy.warning = "invalid GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S; expected a nonnegative integer"
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_SQLITE_CLOSE_CHECKPOINT"))) {
	case "":
	case "full":
		policy.full = true
	case "skip":
		policy.full = false
	default:
		if policy.warning != "" {
			policy.warning += "; "
		}
		policy.warning += "invalid GORTEX_SQLITE_CLOSE_CHECKPOINT; expected skip or full; using valid legacy opt-in or skip"
	}
	return policy
}

func (p closeCheckpointPolicy) mode() string {
	if p.full {
		return "full"
	}
	return "skip"
}

func (p closeCheckpointPolicy) deadline(pending int64) time.Duration {
	if !p.full || p.max <= 0 {
		return 0
	}
	return min(p.max, closeCheckpointEstimate(pending))
}

// CloseCheckpointPolicy reports the daemon/store process's effective choice,
// not the querying CLI's environment. Missing status fields remain legacy.
func (s *Store) CloseCheckpointPolicy() string { return resolveCloseCheckpointPolicy().mode() }
