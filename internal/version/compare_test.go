package version

import "testing"

// TestComparePrecedence pins the SemVer 2.0.0 §11 precedence chain on
// Compare: the canonical ascending pre-release ladder (each adjacent
// pair in spec order), build metadata being ignored for precedence
// (§10), and numeric identifiers ranking below alphanumeric ones (§11).
// Each row asserts the comparator's exact sign (-1 / 0 / +1) in the
// argument order given.
func TestComparePrecedence(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int
	}{
		// §11's ascending example chain, adjacent pair by adjacent pair.
		{"alpha lt alpha.1 (larger field set ranks higher)", "v1.0.0-alpha", "v1.0.0-alpha.1", -1},
		{"alpha.1 lt alpha.beta (numeric lt alphanumeric)", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", -1},
		{"alpha.beta lt beta (ASCII sort)", "v1.0.0-alpha.beta", "v1.0.0-beta", -1},
		{"beta lt beta.2 (larger field set ranks higher)", "v1.0.0-beta", "v1.0.0-beta.2", -1},
		{"beta.2 lt beta.11 (numeric compare, not ASCII)", "v1.0.0-beta.2", "v1.0.0-beta.11", -1},
		{"beta.11 lt rc.1 (ASCII sort)", "v1.0.0-beta.11", "v1.0.0-rc.1", -1},
		{"rc.1 lt release (pre-release ranks below release)", "v1.0.0-rc.1", "v1.0.0", -1},
		{"release gt rc.1 (same pair, reversed)", "v1.0.0", "v1.0.0-rc.1", 1},
		// §10: build metadata MUST be ignored when determining precedence.
		{"build metadata ignored", "v1.0.0+a", "v1.0.0+b", 0},
		// §11: numeric identifiers always rank below alphanumeric ones.
		{"bare numeric ident lt alphanumeric ident", "v1.0.0-1", "v1.0.0-alpha", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := MustParse(tc.a)
			b := MustParse(tc.b)
			if got := Compare(a, b); got != tc.want {
				t.Fatalf("Compare(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
