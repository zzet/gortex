package serverstack

import (
	"testing"

	"github.com/zzet/gortex/internal/config"
)

func TestGoTypesEnrichEnabled_DefaultAndPrecedence(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name       string
		goTypes    *bool
		env        string
		configWant bool
		want       bool
	}{
		{"default on", nil, "", true, true},
		{"config on", &on, "", true, true},
		{"config off", &off, "", false, false},
		{"env one overrides off", &off, "1", false, true},
		{"env true overrides off", &off, "TrUe", false, true},
		{"env zero overrides on", &on, "0", true, false},
		{"env false overrides on", &on, "FaLsE", true, false},
		{"env zero overrides default", nil, "0", true, false},
		{"invalid env overrides on", &on, "invalid", true, false},
		{"whitespace is not true", &on, " true ", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_GO_TYPES", tc.env)
			sem := config.SemanticConfig{GoTypes: tc.goTypes}
			if got := sem.GoTypesEnabledOrDefault(); got != tc.configWant {
				t.Errorf("GoTypesEnabledOrDefault() = %v, want %v", got, tc.configWant)
			}
			if got := goTypesEnrichEnabled(sem); got != tc.want {
				t.Errorf("goTypesEnrichEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
