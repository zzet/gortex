package platform

import "testing"

func TestProcStatIsZombie(t *testing.T) {
	cases := []struct {
		name string
		stat string
		dead bool
	}{
		{"zombie", "42 (daemon) Z 1 2 3", true},
		{"dead", "42 (daemon) X 1 2 3", true},
		{"running", "42 (daemon) R 1 2 3", false},
		{"sleeping", "42 (daemon) S 1 2 3", false},
		{"stopped", "42 (daemon) T 1 2 3", false},
		{"spaces in name", "42 (daemon worker) Z 1 2 3", true},
		{"parentheses in name", "42 (daemon (worker)) Z 1 2 3", true},
		{"closing parentheses in name", "42 (daemon) R )worker)) Z 1 2 3", true},
		{"zombie marker in live name", "42 (daemon) Z )worker)) S 1 2 3", false},
		{"missing separator", "42 (daemon Z 1 2 3", false},
		{"missing state", "42 (daemon)", false},
		{"empty state", "42 (daemon) \t\n", false},
		{"malformed state", "42 (daemon) Zombie 1 2 3", false},
		{"empty stat", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := procStatIsZombie([]byte(tc.stat)); got != tc.dead {
				t.Errorf("procStatIsZombie(%q) = %v, want %v", tc.stat, got, tc.dead)
			}
		})
	}
}
