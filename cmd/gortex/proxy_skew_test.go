package main

import "testing"

func TestDaemonSkewWarning(t *testing.T) {
	cases := []struct{ name, daemonV, localV, want string }{
		{"equal versions", "v0.63.4+5f5fce2", "v0.63.4+5f5fce2", ""},
		{"daemon older", "v0.63.4+5f9ce2a", "v0.63.5+abcdef1",
			"warning: daemon v0.63.4+5f9ce2a != binary v0.63.5+abcdef1 — run 'gortex daemon restart' to upgrade the daemon"},
		{"daemon newer", "v0.63.5+abcdef1", "v0.63.4+5f9ce2a",
			"warning: daemon v0.63.5+abcdef1 != binary v0.63.4+5f9ce2a — this binary is older than the running daemon — run 'gortex upgrade'"},
		{"same version different build", "v0.63.4+aaaaaaa", "v0.63.4+bbbbbbb",
			"warning: daemon v0.63.4+aaaaaaa != binary v0.63.4+bbbbbbb — run 'gortex daemon restart' or 'gortex upgrade'"},
		{"unparseable daemon version", "strawberry", "v0.63.5+abcdef1",
			"warning: daemon strawberry != binary v0.63.5+abcdef1 — run 'gortex daemon restart' or 'gortex upgrade'"},
		{"daemon version empty", "", "v0.63.5+abcdef1", ""},
		{"local dev build", "v0.63.4+5f9ce2a", "v0.0.0-dev", ""},
		{"daemon dev build", "v0.0.0-dev", "v0.63.5+abcdef1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonSkewWarning(tc.daemonV, tc.localV); got != tc.want {
				t.Fatalf("daemonSkewWarning(%q, %q) = %q, want %q", tc.daemonV, tc.localV, got, tc.want)
			}
		})
	}
}
