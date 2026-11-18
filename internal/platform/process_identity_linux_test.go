package platform

import (
	"strings"
	"testing"
)

func TestProcStatBirthUsesActualStarttimeAfterComplexComm(t *testing.T) {
	for _, comm := range []string{"daemon", "daemon worker", "daemon) Z )worker)"} {
		// Fields 3..21, then starttime (22), then unrelated later values.
		stat := "42 (" + comm + ") S " + strings.Repeat("1 ", 18) + "987654 999 888"
		birth, err := procStatBirth([]byte(stat))
		if err != nil || birth != [2]uint64{987654, 0} {
			t.Fatalf("comm=%q: birth=%v error=%v", comm, birth, err)
		}
	}
	for _, stat := range []string{"", "42 (no boundary", "42 (daemon)", "42 (daemon) Sleeping " + strings.Repeat("1 ", 19), "42 (daemon) S " + strings.Repeat("1 ", 18) + "0", "42 (daemon) S " + strings.Repeat("1 ", 18) + "-1", "42 (daemon) S " + strings.Repeat("1 ", 18) + "bad"} {
		if _, err := procStatBirth([]byte(stat)); err == nil {
			t.Fatalf("accepted missing/invalid native starttime: %q", stat)
		}
	}
}
