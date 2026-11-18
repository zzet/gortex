package platform

import (
	"bytes"
	"os"
	"strconv"
)

func processIsZombie(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		// /proc may be restricted or unmounted: preserve the kill probe's result.
		return false
	}
	return procStatIsZombie(stat)
}

func procStatIsZombie(stat []byte) bool {
	// The comm field is parenthesized, but can itself contain spaces and ')'.
	// Its final ')' is the only safe boundary before the process state.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return false
	}
	fields := bytes.Fields(stat[end+1:])
	return len(fields) > 0 && len(fields[0]) == 1 && (fields[0][0] == 'Z' || fields[0][0] == 'X')
}
