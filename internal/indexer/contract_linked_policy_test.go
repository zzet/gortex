package indexer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestContractLinkedScheduleResolvesConfigAndEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     config.ContractAnalysisConfig
		env     string
		want    ContractLinkedSchedule
		invalid bool
	}{
		{name: "empty_is_idle", want: ContractLinkedSchedule{ContractLinkedIdle, 5 * time.Second, 30 * time.Second}},
		{name: "file_throttled", cfg: config.ContractAnalysisConfig{LinkedPolicy: "throttled", LinkedThrottleInterval: "45s"}, want: ContractLinkedSchedule{ContractLinkedThrottled, 5 * time.Second, 45 * time.Second}},
		{name: "file_case_and_blanks", cfg: config.ContractAnalysisConfig{LinkedPolicy: " Eager ", LinkedIdleWindow: "2s"}, want: ContractLinkedSchedule{ContractLinkedEager, 2 * time.Second, 30 * time.Second}},
		{name: "env_overrides_file", cfg: config.ContractAnalysisConfig{LinkedPolicy: "idle"}, env: "eager", want: ContractLinkedSchedule{ContractLinkedEager, 5 * time.Second, 30 * time.Second}},
		{name: "blank_env_keeps_file", cfg: config.ContractAnalysisConfig{LinkedPolicy: "throttled"}, env: "  ", want: ContractLinkedSchedule{ContractLinkedThrottled, 5 * time.Second, 30 * time.Second}},
		{name: "invalid_policy_keeps_default", cfg: config.ContractAnalysisConfig{LinkedPolicy: "lazy", LinkedIdleWindow: "9s"}, want: ContractLinkedSchedule{ContractLinkedIdle, 9 * time.Second, 30 * time.Second}, invalid: true},
		{name: "invalid_env_policy", cfg: config.ContractAnalysisConfig{LinkedPolicy: "throttled"}, env: "sometimes", want: ContractLinkedSchedule{ContractLinkedIdle, 5 * time.Second, 30 * time.Second}, invalid: true},
		{name: "invalid_durations_keep_defaults", cfg: config.ContractAnalysisConfig{LinkedIdleWindow: "soon", LinkedThrottleInterval: "30"}, want: ContractLinkedSchedule{ContractLinkedIdle, 5 * time.Second, 30 * time.Second}, invalid: true},
		{name: "non_positive_durations_keep_defaults", cfg: config.ContractAnalysisConfig{LinkedIdleWindow: "0s", LinkedThrottleInterval: "-1m"}, want: ContractLinkedSchedule{ContractLinkedIdle, 5 * time.Second, 30 * time.Second}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveContractLinkedSchedule(tc.cfg, func(name string) string {
				require.Equal(t, ContractLinkedPolicyEnv, name)
				return tc.env
			})
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, got, "a refusal still resolves to a usable schedule")
		})
	}
	got, err := ResolveContractLinkedSchedule(config.ContractAnalysisConfig{LinkedPolicy: "eager"}, nil)
	require.NoError(t, err)
	require.Equal(t, ContractLinkedEager, got.Policy, "no environment reads the file alone")
}
