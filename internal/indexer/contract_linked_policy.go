package indexer

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// ContractLinkedPolicy decides when a linked-checkout actor's job may start.
// Primary actors and strict requests are never subject to it.
type ContractLinkedPolicy string

const (
	// ContractLinkedIdle starts a linked job only once foreground demand
	// has been absent for the idle window, and stops a running fill at its
	// next checkpoint when demand returns. The zero value means idle.
	ContractLinkedIdle ContractLinkedPolicy = viewmetrics.ContractLinkedIdle
	// ContractLinkedThrottled runs at most one linked job at a time, and
	// starts one linked actor at most once per throttle interval.
	ContractLinkedThrottled ContractLinkedPolicy = viewmetrics.ContractLinkedThrottled
	// ContractLinkedEager schedules linked actors as primary actors are.
	ContractLinkedEager ContractLinkedPolicy = viewmetrics.ContractLinkedEager
)

// ContractLinkedPolicyEnv overrides the configured linked policy.
const ContractLinkedPolicyEnv = "GORTEX_CONTRACT_LINKED_POLICY"

// The shipped idle window and throttle interval.
const (
	contractLinkedIdleWindow       = 5 * time.Second
	contractLinkedThrottleInterval = 30 * time.Second
)

// ContractLinkedSchedule is the resolved linked-checkout scheduling policy.
type ContractLinkedSchedule struct {
	Policy           ContractLinkedPolicy
	IdleWindow       time.Duration
	ThrottleInterval time.Duration
}

// DefaultContractLinkedSchedule is the shipped policy: idle, 5 s, 30 s.
func DefaultContractLinkedSchedule() ContractLinkedSchedule {
	return ContractLinkedSchedule{Policy: ContractLinkedIdle, IdleWindow: contractLinkedIdleWindow, ThrottleInterval: contractLinkedThrottleInterval}
}

// ParseContractLinkedPolicy reads a policy name, ignoring case and blanks.
// Empty is idle.
func ParseContractLinkedPolicy(value string) (ContractLinkedPolicy, error) {
	switch policy := ContractLinkedPolicy(strings.ToLower(strings.TrimSpace(value))); policy {
	case "":
		return ContractLinkedIdle, nil
	case ContractLinkedIdle, ContractLinkedThrottled, ContractLinkedEager:
		return policy, nil
	default:
		return ContractLinkedIdle, fmt.Errorf("contract analysis: unknown linked policy %q (want idle, throttled or eager)", value)
	}
}

// ResolveContractLinkedSchedule resolves the configured block, with the
// environment's policy over the file's. A value that does not parse, or a
// duration that is not positive, keeps its default; every refusal is
// returned joined, beside a schedule that is always usable.
func ResolveContractLinkedSchedule(cfg config.ContractAnalysisConfig, getenv func(string) string) (ContractLinkedSchedule, error) {
	schedule := DefaultContractLinkedSchedule()
	var errs []error
	value := cfg.LinkedPolicy
	if getenv != nil {
		if env := strings.TrimSpace(getenv(ContractLinkedPolicyEnv)); env != "" {
			value = env
		}
	}
	policy, err := ParseContractLinkedPolicy(value)
	if err != nil {
		errs = append(errs, err)
	}
	schedule.Policy = policy
	duration := func(name, value string, into *time.Duration) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			errs = append(errs, fmt.Errorf("contract analysis: %s %q is not a positive duration", name, value))
			return
		}
		*into = parsed
	}
	duration("linked_idle_window", cfg.LinkedIdleWindow, &schedule.IdleWindow)
	duration("linked_throttle_interval", cfg.LinkedThrottleInterval, &schedule.ThrottleInterval)
	return schedule, errors.Join(errs...)
}
