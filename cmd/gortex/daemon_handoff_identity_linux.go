package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func handoffProcessBirth(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	// comm may contain spaces and ')'; fields after its final ')' start at
	// field3 (state). starttime is field22, measured since system boot.
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return "", fmt.Errorf("process %d identity unavailable", pid)
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("process %d identity incomplete", pid)
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", err
	}
	return fields[19], nil
}
