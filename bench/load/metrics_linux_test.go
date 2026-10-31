package main

import "testing"

func TestLinuxProcCPUCommAndTickUnits(t *testing.T) {
	m := linuxCPU("123 (command with ) spaces) R 0 0 0 0 0 0 0 0 0 0 250 50", 100)
	if m.Value == nil || *m.Value != 3 {
		t.Fatalf("proc CPU parse: %+v", m)
	}
	if m := linuxCPU("malformed", 100); m.Value != nil {
		t.Fatal("malformed proc CPU became zero")
	}
	if m := linuxCPU("123 (cmd) R", 0); m.Value != nil {
		t.Fatal("missing tick rate became zero")
	}
}
