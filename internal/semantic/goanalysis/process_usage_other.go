//go:build !unix

package goanalysis

import "time"

type processUsage struct {
	majorFaults, minorFaults int64
	cpu                      time.Duration
}

func readProcessUsage() processUsage { return processUsage{} }

func (u processUsage) since(processUsage) processUsage { return processUsage{} }
