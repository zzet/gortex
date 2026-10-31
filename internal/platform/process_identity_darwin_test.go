package platform

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinProcessIdentityDistinguishesEmptyFromUnexpectedRecords(t *testing.T) {
	_, running, err := darwinProcessIdentity(42, nil)
	if running || !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("empty successful PID result: running=%v error=%v", running, err)
	}
	var record unix.KinfoProc
	record.Proc.P_pid = 42
	record.Proc.P_starttime.Sec = 123
	record.Proc.P_starttime.Usec = 456
	record.Proc.P_stat = 2
	birth, running, err := darwinProcessIdentity(42, []unix.KinfoProc{record})
	if err != nil || !running || birth != [2]uint64{123, 456} {
		t.Fatalf("actual record: birth=%v running=%v error=%v", birth, running, err)
	}
	_, _, err = darwinProcessIdentity(42, []unix.KinfoProc{record, record})
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("ambiguous record must stay unknown: %v", err)
	}
	_, _, err = darwinProcessIdentity(43, []unix.KinfoProc{record})
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("wrong PID must stay unknown: %v", err)
	}
	record.Proc.P_stat = darwinProcessZombie
	_, running, err = darwinProcessIdentity(42, []unix.KinfoProc{record})
	if err != nil || running {
		t.Fatalf("unreaped zombie: running=%v error=%v", running, err)
	}
}
